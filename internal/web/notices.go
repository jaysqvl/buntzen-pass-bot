package web

import (
	"net/http"
	"net/url"
	"strings"
)

// Only fixed application messages are accepted from redirect URLs. Provider
// errors, credentials, arbitrary text and return URLs never enter notifications.
func noticeFor(code string) *Flash {
	if code == "otp-default-updated" {
		return &Flash{Kind: "success", Message: "Default OTP source updated. Already queued jobs keep their original source."}
	}
	if code == "lake-defaults-reset" {
		return &Flash{Kind: "success", Message: "Lake defaults reset. Existing Yodel sign-ins, requests, and queued jobs keep their saved settings."}
	}
	if code == "queue-pending" {
		return &Flash{Kind: "info", Message: "A job already exists for this booking. No second job was created."}
	}
	messages := map[string]string{
		"preview-read-only":    "This action is unavailable in the sample preview. You can test queueing from Bookings; other changes belong in your main app.",
		"queue-review":         "An earlier attempt may already have booked a pass for this sign-in and visit date. Check Yodel before retrying.",
		"queue-full":           "This account has reached its job limit. Review existing jobs before starting another.",
		"queue-unavailable":    "We couldn't confirm that the job was queued. Check Jobs before trying again.",
		"booking-not-released": "Passes for this date have not been released. Use Queue for release instead.",
		"booking-date-passed":  "This booking date has passed. Choose today or a future date.",
		"booking-window-ended": "The release window has ended. Choose another date, or use Book now to check released passes.",
		"booking-disabled":     "Enable this booking request before starting a job.",
		"profile-disabled":     "Enable the selected account from its lake's Connection section before starting a job.",
		"booking-action":       "That booking action is unavailable. Choose an action from the booking card.",
		"provider-unavailable": "The OTP source connection test failed. Check its server address and credentials, and make sure the provider is running.",
		"pairing-unavailable":  "Pairing could not start. Check your account in the lake's Connection section, then try again.",
	}
	if message := messages[code]; message != "" {
		return &Flash{Kind: "error", Message: message}
	}
	return nil
}

// path is a fixed in-app destination chosen by the handler, never a submitted
// return URL or Referer. Redirecting after POST also makes refresh safe.
func redirectNotice(w http.ResponseWriter, r *http.Request, path, code string) {
	path, fragment, _ := strings.Cut(path, "#")
	location := url.URL{Path: path, Fragment: fragment, RawQuery: url.Values{"notice": {code}}.Encode()}
	http.Redirect(w, r, location.String(), http.StatusSeeOther)
}
