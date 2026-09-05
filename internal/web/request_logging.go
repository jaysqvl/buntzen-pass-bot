package web

import (
	"log/slog"
	"net/http"
	"time"
)

type observedResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	written, err := w.ResponseWriter.Write(payload)
	w.bytes += written
	return written, err
}

func (w *observedResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		observed := &observedResponseWriter{ResponseWriter: w}
		next.ServeHTTP(observed, r)
		status := observed.status
		if status == 0 {
			status = http.StatusOK
		}
		slog.Debug("HTTP request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"response_bytes", observed.bytes,
			"duration", time.Since(started).Round(time.Millisecond),
		)
	})
}
