package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type flushFailureWriter struct{ *httptest.ResponseRecorder }

var errSyntheticFlush = errors.New("synthetic flush failure")

func (w *flushFailureWriter) FlushError() error { return errSyntheticFlush }

func TestResponseWrappersPreserveFlushErrors(t *testing.T) {
	for _, wrap := range []func(http.ResponseWriter) http.ResponseWriter{
		func(w http.ResponseWriter) http.ResponseWriter { return &observedResponseWriter{ResponseWriter: w} },
		func(w http.ResponseWriter) http.ResponseWriter { return &browserErrorWriter{ResponseWriter: w} },
		func(w http.ResponseWriter) http.ResponseWriter {
			return &browserErrorWriter{ResponseWriter: &observedResponseWriter{ResponseWriter: w}}
		},
	} {
		w := wrap(&flushFailureWriter{httptest.NewRecorder()})
		if err := http.NewResponseController(w).Flush(); !errors.Is(err, errSyntheticFlush) {
			t.Fatalf("flush failure hidden: %v", err)
		}
	}
}

type oneConnectionListener struct {
	conn             net.Conn
	first, closeOnce sync.Once
	closed           chan struct{}
}

func (l *oneConnectionListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.first.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *oneConnectionListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed); l.conn.Close() })
	return nil
}
func (l *oneConnectionListener) Addr() net.Addr { return l.conn.LocalAddr() }

func TestStalledSSEWritesAndFinalChunkHaveDeadlines(t *testing.T) {
	for _, finalChunk := range []bool{false, true} {
		t.Run(fmt.Sprintf("final-chunk=%v", finalChunk), func(t *testing.T) {
			f := newWebFixture(t)
			job := newLiveJob(t, f)
			cookies := loginCookies(t, f)
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(previous)
			serverConn, clientConn := net.Pipe()
			defer clientConn.Close()
			listener := &oneConnectionListener{conn: serverConn, closed: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			closed := make(chan struct{}, 1)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.handler.ServeHTTP(w, r.WithContext(ctx)) }), ConnState: func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}}
			defer server.Close()
			go func() { _ = server.Serve(listener) }()
			request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://example.test/jobs/%d/events", job.ID), nil)
			request.Header.Set("Accept", "text/html") // Exercise both response wrappers.
			for _, c := range cookies {
				request.AddCookie(c)
			}
			_ = clientConn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if err := request.Write(clientConn); err != nil {
				t.Fatal(err)
			}
			if finalChunk {
				_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
				reader := bufio.NewReader(clientConn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						t.Fatal(err)
					}
					if line == "\r\n" {
						break
					}
				}
				sizeLine, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
				if err != nil || size <= 0 {
					t.Fatalf("invalid initial chunk %q: %v", sizeLine, err)
				}
				if _, err := io.CopyN(io.Discard, reader, size+2); err != nil {
					t.Fatal(err)
				}
				_ = clientConn.SetReadDeadline(time.Time{})
				// net/http writes the final chunk only after our handler returns. Stop
				// reading first, then cancel the otherwise healthy stream.
				cancel()
			}
			select {
			case <-closed:
			case <-time.After(7 * time.Second):
				clientConn.Close()
				t.Fatal("stalled stream connection outlived its write deadline")
			}
		})
	}
}

type transportFailureWriter struct {
	*httptest.ResponseRecorder
	writeFailure, flushFailure bool
	deadlines                  []time.Time
}

func (w *transportFailureWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}
func (w *transportFailureWriter) Write(p []byte) (int, error) {
	if w.writeFailure {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}
func (w *transportFailureWriter) FlushError() error {
	if w.flushFailure {
		return errSyntheticFlush
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestStreamTransportErrorsReleaseAdmissionThroughWrappers(t *testing.T) {
	for _, flush := range []bool{false, true} {
		t.Run(fmt.Sprintf("flush=%v", flush), func(t *testing.T) {
			f := newWebFixture(t)
			job := newLiveJob(t, f)
			cookies := loginCookies(t, f)
			r := authenticatedRequest(http.MethodGet, fmt.Sprintf("http://example.test/jobs/%d/events", job.ID), cookies, nil)
			underlying := &transportFailureWriter{ResponseRecorder: httptest.NewRecorder(), writeFailure: !flush, flushFailure: flush}
			wrapped := &browserErrorWriter{ResponseWriter: &observedResponseWriter{ResponseWriter: underlying}}
			done := make(chan struct{})
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			go func() { f.handler.ServeHTTP(wrapped, r.WithContext(ctx)); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("transport failure did not end stream")
			}
			waitStreamTotal(t, &f.server.streams, 0)
			if len(underlying.deadlines) == 0 || underlying.deadlines[len(underlying.deadlines)-1].IsZero() {
				t.Fatal("final chunk left without a deadline")
			}
		})
	}
}

func TestSuccessfulStreamBatchClearsDeadlineUntilNextWrite(t *testing.T) {
	w := &transportFailureWriter{ResponseRecorder: httptest.NewRecorder()}
	stream, err := newEventStream(w)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := stream.batch(func() error {
			if last := w.deadlines[len(w.deadlines)-1]; last.IsZero() || time.Until(last) <= 0 {
				t.Fatal("write started without a fresh deadline")
			}
			return writeSSE(w, "state", map[string]any{"status": "running"})
		}); err != nil {
			t.Fatal(err)
		}
		if !w.deadlines[len(w.deadlines)-1].IsZero() {
			t.Fatal("continued stream kept stale deadline")
		}
	}
	stream.finish()
	if w.deadlines[len(w.deadlines)-1].IsZero() {
		t.Fatal("exit did not rearm trailer deadline")
	}
}
