package web

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// browserErrorWriter discards plain error bodies without retaining potentially
// sensitive diagnostics. Successful responses and streams pass through directly.
type browserErrorWriter struct {
	http.ResponseWriter
	status     int
	plainError bool
}

func (w *browserErrorWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	mediaType, _, _ := mime.ParseMediaType(w.Header().Get("Content-Type"))
	w.plainError = status >= 400 && mediaType == "text/plain"
	if !w.plainError {
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *browserErrorWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.plainError {
		return len(payload), nil
	}
	return w.ResponseWriter.Write(payload)
}

func (w *browserErrorWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.plainError {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
}

func (w *browserErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) browserErrorPages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsBrowserHTML(r) {
			next.ServeHTTP(w, r)
			return
		}
		response := &browserErrorWriter{ResponseWriter: w}
		next.ServeHTTP(response, r)
		if !response.plainError {
			return
		}
		// These describe the discarded representation, not the replacement page.
		// Keep security headers, Allow, Retry-After and the original error status.
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
		w.Header().Del("ETag")
		w.Header().Del("Content-Range")
		s.render(w, response.status, "error", browserErrorData(r, response.status))
	})
}

func acceptsBrowserHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/event-stream") {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mediaType, parameters, err := mime.ParseMediaType(part)
		if err != nil || mediaType != "text/html" {
			continue
		}
		if q, exists := parameters["q"]; exists {
			weight, err := strconv.ParseFloat(q, 64)
			if err != nil || !(weight > 0 && weight <= 1) {
				continue
			}
		}
		return true
	}
	return false
}

type browserErrorPage struct {
	BaseData
	Message     string
	ReturnURL   string
	ReturnLabel string
}

func browserErrorData(r *http.Request, status int) browserErrorPage {
	title, message := "Something went wrong", "Return to the app and try again. If the problem continues, contact the administrator."
	switch status {
	case http.StatusBadRequest:
		title, message = "The request couldn't be completed", "Return to the app and try again from the current page."
	case http.StatusUnauthorized:
		title, message = "Please sign in", "Sign in again to continue."
	case http.StatusForbidden:
		title, message = "That action isn't available", "Your session may need refreshing, or you may not have access. Return to the app and try again."
	case http.StatusNotFound:
		title, message = "Page not found", "This page is unavailable or you don't have access to it."
	case http.StatusMethodNotAllowed:
		title, message = "That action isn't available", "Return to the app and use the actions shown on the page."
	case http.StatusRequestEntityTooLarge:
		title, message = "The submission is too large", "Return to the form and reduce the amount of information submitted."
	case http.StatusTooManyRequests:
		title, message = "Please wait a moment", "Too many requests were received. Wait before trying again."
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		title, message = "Temporarily unavailable", "The app couldn't complete this request right now. Please try again later."
	}
	returnURL, returnLabel := browserErrorReturn(r.URL.Path)
	if status >= http.StatusInternalServerError && (returnURL == "/bookings" || returnURL == "/jobs") {
		message = "We could not confirm the result of this action. Check Jobs before trying again."
		returnURL, returnLabel = "/jobs", "Back to Jobs"
	}
	if status == http.StatusUnauthorized {
		returnURL, returnLabel = "/login", "Sign in"
	}
	// Authentication is attached to an inner request context. Do not fabricate
	// an authenticated layout or accept notification/return text from this URL.
	return browserErrorPage{BaseData: BaseData{Title: title}, Message: message, ReturnURL: returnURL, ReturnLabel: returnLabel}
}

func browserErrorReturn(path string) (string, string) {
	section := func(prefix string) bool { return path == prefix || strings.HasPrefix(path, prefix+"/") }
	switch {
	case section("/bookings"):
		return "/bookings", "Back to Bookings"
	case section("/jobs"):
		return "/jobs", "Back to Jobs"
	case section("/account"):
		return "/account", "Back to Account"
	case section("/admin/users"):
		return "/admin/users", "Back to Users"
	case section("/login"), section("/logout"):
		return "/login", "Sign in"
	case section("/setup"):
		return "/setup", "Back to setup"
	default:
		return "/", "Back to Setup"
	}
}
