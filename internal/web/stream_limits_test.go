package web

import (
	"bufio"
	"context"
	"fmt"
	"github.com/jaysqvl/lake-pass-bot/internal/control"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestJobStreamAdmissionRejectsNinthConnection(t *testing.T) {
	f := newWebFixture(t)
	job := newLiveJob(t, f)
	cookies := loginCookies(t, f)
	secondSession := loginCookies(t, f)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	var responses []*http.Response
	defer func() {
		for _, r := range responses {
			r.Body.Close()
		}
	}()
	for i := 0; i < 9; i++ {
		request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/jobs/%d/events", server.URL, job.ID), nil)
		if err != nil {
			t.Fatal(err)
		}
		selectedCookies := cookies
		if i%2 != 0 {
			selectedCookies = secondSession
		}
		for _, c := range selectedCookies {
			request.AddCookie(c)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
		want := http.StatusOK
		if i == 8 {
			want = http.StatusTooManyRequests
		}
		if response.StatusCode != want {
			t.Fatalf("stream %d = %d, want %d", i+1, response.StatusCode, want)
		}
		if i == 8 && response.Header.Get("Retry-After") == "" {
			t.Fatal("missing retry hint")
		}
	}
	responses[0].Body.Close()
	waitStreamTotal(t, &f.server.streams, 7)
	r, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/jobs/%d/events", server.URL, job.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	responses = append(responses, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("released slot unusable: %d", response.StatusCode)
	}
}

func waitStreamTotal(t *testing.T, a *streamAdmission, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		got := a.total
		a.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream count=%d want=%d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamAdmissionGlobalLimitAndConcurrentCleanup(t *testing.T) {
	var a streamAdmission
	releases := make(chan func(), maxStreams)
	var tasks sync.WaitGroup
	for i := 1; i <= maxStreams; i++ {
		tasks.Add(1)
		go func(id int64) { defer tasks.Done(); releases <- a.acquire(id) }(int64(i))
	}
	tasks.Wait()
	close(releases)
	if release := a.acquire(maxStreams + 1); release != nil {
		release()
		t.Fatal("global budget exceeded")
	}
	for release := range releases {
		if release == nil {
			t.Fatal("legitimate per-user admission rejected")
		}
		tasks.Add(1)
		go func(release func()) { defer tasks.Done(); release(); release() }(release)
	}
	tasks.Wait()
	if a.total != 0 || len(a.users) != 0 {
		t.Fatalf("leaked admission state: %d/%d", a.total, len(a.users))
	}
	if release := a.acquire(1); release == nil {
		t.Fatal("budget not reusable")
	} else {
		release()
	}
}

func TestFullStreamBudgetDoesNotExposeAnotherOwnersJob(t *testing.T) {
	f := newWebFixture(t)
	job := newLiveJob(t, f)
	member, err := f.store.CreateMember(context.Background(), store.CreateUserInput{Username: "member", Password: "long-member-password"})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := f.store.NewSession(context.Background(), member.ID, sessionLifetime)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxStreamsPerUser; i++ {
		release := f.server.streams.acquire(member.ID)
		if release == nil {
			t.Fatal("unexpected full budget")
		}
		defer release()
	}
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://example.test/jobs/%d/events", job.ID), nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: credentials.Token})
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: credentials.CSRFToken})
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign job lookup leaked through admission: %d", w.Code)
	}
}

func TestStreamOutlivesOrdinaryResponseWriteTimeout(t *testing.T) {
	f := newWebFixture(t)
	job := newLiveJob(t, f)
	cookies := loginCookies(t, f)
	server := httptest.NewUnstartedServer(f.handler)
	server.Config.WriteTimeout = 30 * time.Second
	server.Start()
	defer server.Close()
	client := server.Client()
	client.Timeout = 40 * time.Second
	r, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/jobs/%d/events", server.URL, job.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream=%d", response.StatusCode)
	}
	timer := time.NewTimer(31 * time.Second)
	defer timer.Stop()
	<-timer.C
	key := strconv.FormatInt(job.ID, 10)
	f.server.engine.Hub().Publish(key, control.LiveEvent{Kind: "otp", Data: map[string]any{"active": true, "code": "123456"}})
	f.server.engine.Hub().Publish(key, control.LiveEvent{Kind: "pairing", Data: map[string]any{"active": false}})
	if err := f.store.ForUser(f.admin.ID).RequestJobCancellation(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	final := appendLiveJobEvent(t, f, job.ID, "job.cancelled")
	f.server.engine.Hub().Publish(key, control.LiveEvent{Kind: "complete"})
	reader := bufio.NewReader(response.Body)
	seenOTP, seenPairing, seenFinal := false, false, false
	for {
		event := readJobEvent(t, reader)
		switch event.kind {
		case "otp":
			seenOTP = true
		case "pairing":
			seenPairing = true
		case "job_event":
			if event.id == strconv.FormatInt(final.ID, 10) {
				seenFinal = true
			}
		case "complete":
			if !seenOTP || !seenPairing || !seenFinal {
				t.Fatalf("long stream lost events: otp=%v pairing=%v final=%v", seenOTP, seenPairing, seenFinal)
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatalf("unclean stream completion: %v", err)
			}
			waitStreamTotal(t, &f.server.streams, 0)
			return
		}
	}
}
