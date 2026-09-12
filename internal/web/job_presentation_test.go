package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestPendingJobPagesAndStreamExplainStartAndConfirmation(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	ctx := context.Background()
	resources := fixture.store.ForUser(fixture.admin.ID)
	for _, test := range []struct {
		name, due, localLabel, utcLabel, confirmation string
		mode                                          model.RunMode
	}{
		{"summer automatic", "2026-09-07T13:30:00Z", "Mon, Sep 7, 2026 at 6:30 AM UTC-07:00", "Mon, Sep 7, 2026 at 1:30 PM UTC", "Automatic final confirmation", model.RunModeAuto},
		{"winter manual previous day", "2025-01-02T00:30:00Z", "Wed, Jan 1, 2025 at 4:30 PM UTC-08:00", "Thu, Jan 2, 2025 at 12:30 AM UTC", "Manual final approval", model.RunModeManual},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, test.name, true)
			booking.Timezone = "America/Vancouver"
			booking, err := resources.UpdateBookingRequest(ctx, booking)
			if err != nil {
				t.Fatal(err)
			}
			due, err := time.Parse(time.RFC3339, test.due)
			if err != nil {
				t.Fatal(err)
			}
			job, err := resources.EnqueueJob(ctx, store.EnqueueJobParams{
				ProfileID: profile.ID, BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: test.mode, DueAt: due,
			})
			if err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("/jobs/%d", job.ID)
			for _, path := range []string{path, "/jobs"} {
				page := serveForm(fixture, http.MethodGet, path, cookies, nil)
				for _, want := range []string{"Waiting to start", "Earliest start", test.localLabel, test.confirmation} {
					if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
						t.Fatalf("%s missing %q: status=%d body=%q", path, want, page.Code, page.Body.String())
					}
				}
			}
			reader := openJobStream(t, fixture, job.ID, cookies, 0, 0)
			state := readJobEvent(t, reader)
			if state.kind != "state" || state.data["status"] != "queued" || state.data["label"] != "Waiting to start" || state.data["terminal"] != false {
				t.Fatalf("queued wire status changed: %+v", state)
			}
			message, _ := state.data["message"].(string)
			if !strings.Contains(message, "Earliest start: "+test.localLabel) || !strings.Contains(message, "may start later") {
				t.Fatalf("queued stream did not explain the pending start: %q", message)
			}
			if err := resources.RequestJobCancellation(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			booking.Timezone = "Asia/Tokyo"
			if _, err := resources.UpdateBookingRequest(ctx, booking); err != nil {
				t.Fatal(err)
			}
			page := serveForm(fixture, http.MethodGet, path, cookies, nil)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), test.utcLabel) ||
				strings.Contains(page.Body.String(), "JST") || strings.Contains(page.Body.String(), test.localLabel) ||
				strings.Contains(page.Body.String(), "Waiting to start") {
				t.Fatalf("terminal history used current booking settings or still looked queued: status=%d body=%q", page.Code, page.Body.String())
			}
		})
	}
}

func TestCancellationDisplayPreservesPendingWireStatus(t *testing.T) {
	for _, status := range []model.JobStatus{model.JobQueued, model.JobRunning, model.JobAwaitingApproval} {
		t.Run(string(status), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeJobState(recorder, model.Job{Status: status, CancelRequested: true}, time.UTC)
			payload := strings.TrimSpace(strings.TrimPrefix(recorder.Body.String(), "event: state\ndata: "))
			var state map[string]any
			if err := json.Unmarshal([]byte(payload), &state); err != nil {
				t.Fatal(err)
			}
			if state["status"] != string(status) || state["label"] != "Cancellation requested" ||
				state["message"] != "Cancellation requested." || state["terminal"] != false {
				t.Fatalf("cancellation state=%v", state)
			}
		})
	}
}

func TestPendingJobTimezoneFallsBackWithoutMatchingLockedBooking(t *testing.T) {
	bookingID := int64(3)
	job := model.Job{UserID: 1, ProfileID: 2, BookingRequestID: &bookingID, Status: model.JobQueued, Command: model.CommandBook}
	booking := model.BookingRequest{ID: bookingID, UserID: 1, ProfileID: 2, Timezone: "America/Vancouver"}
	for _, test := range []struct {
		name   string
		mutate func(*model.Job, *model.BookingRequest)
	}{
		{"another owner", func(_ *model.Job, b *model.BookingRequest) { b.UserID++ }},
		{"another profile", func(_ *model.Job, b *model.BookingRequest) { b.ProfileID++ }},
		{"another booking", func(_ *model.Job, b *model.BookingRequest) { b.ID++ }},
		{"missing booking", func(j *model.Job, _ *model.BookingRequest) { j.BookingRequestID = nil }},
		{"terminal history", func(j *model.Job, _ *model.BookingRequest) { j.Status = model.JobSucceeded }},
		{"login check", func(j *model.Job, _ *model.BookingRequest) { j.Command = model.CommandAuthCheck }},
		{"invalid timezone", func(_ *model.Job, b *model.BookingRequest) { b.Timezone = "Invalid/Timezone" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, booking := job, booking
			test.mutate(&job, &booking)
			if location := pendingJobLocation(job, booking); location != time.UTC {
				t.Fatalf("unrelated or mutable booking timezone used: %s", location)
			}
		})
	}
}
