package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestBookingPageSeparatesAutoQueueingFromExistingJob(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	_, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "queue-status", true)
	booking.Timezone = "America/Vancouver"
	booking.TargetDate = "2030-09-10"
	var err error
	booking, err = fixture.store.ForUser(fixture.admin.ID).UpdateBookingRequest(ctx, booking)
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	form := serveForm(fixture, http.MethodGet, fmt.Sprintf("/bookings/%d", booking.ID), cookies, nil)
	for _, want := range []string{"Auto-queueing is currently off for this server", "Turning this off does not cancel jobs already queued", "For release jobs: wait for your approval or confirm automatically", "Book now always requires approval"} {
		if form.Code != http.StatusOK || !strings.Contains(form.Body.String(), want) {
			t.Fatalf("booking form missing %q: %d %s", want, form.Code, form.Body.String())
		}
	}
	before := serveForm(fixture, http.MethodGet, "/bookings", cookies, nil)
	if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), "No booking queued") || !strings.Contains(before.Body.String(), "Off for this server") {
		t.Fatalf("booking without a job: %d %s", before.Code, before.Body.String())
	}
	job, err := fixture.server.engine.QueueBooking(ctx, fixture.admin.ID, booking.ID, model.CommandBook, "")
	if err != nil {
		t.Fatal(err)
	}
	page := serveForm(fixture, http.MethodGet, "/bookings", cookies, nil)
	for _, want := range []string{"Waiting to start", "Off for this server", "Already queued jobs remain scheduled", "Earliest start", "Mon, Sep 9, 2030 at 6:30 AM", "Mon, Sep 9, 2030 at 7:00 AM", "Automatic final confirmation", fmt.Sprintf(`href="/jobs/%d"`, job.ID)} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("queued booking missing %q: %d %s", want, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), "No booking queued") || strings.Contains(page.Body.String(), "Schedule paused") {
		t.Fatal("queued booking still described as absent or paused")
	}
	unchanged, err := fixture.store.ForUser(fixture.admin.ID).GetJob(ctx, job.ID)
	if err != nil || unchanged.Status != model.JobQueued || unchanged.RunMode != model.RunModeAuto || !unchanged.DueAt.Equal(time.Date(2030, 9, 9, 13, 30, 0, 0, time.UTC)) {
		t.Fatalf("viewing the page changed the saved release job: %+v err=%v", unchanged, err)
	}
	if err := fixture.store.ForUser(fixture.admin.ID).RequestJobCancellation(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	page = serveForm(fixture, http.MethodGet, "/bookings", cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "No booking queued") || strings.Contains(page.Body.String(), fmt.Sprintf(`href="/jobs/%d"`, job.ID)) {
		t.Fatal("cancelled booking still appears pending")
	}
}

func TestBookingCardUsesImmediateJobsSavedManualMode(t *testing.T) {
	fixture := newWebFixture(t)
	_, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "manual-status", true)
	job, err := fixture.server.engine.QueueBookingNow(context.Background(), fixture.admin.ID, booking.ID)
	if err != nil {
		t.Fatal(err)
	}
	page := serveForm(fixture, http.MethodGet, "/bookings", loginCookies(t, fixture), nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Manual final approval") || strings.Contains(page.Body.String(), "Automatic final confirmation") || !strings.Contains(page.Body.String(), fmt.Sprintf(`href="/jobs/%d"`, job.ID)) {
		t.Fatalf("immediate job shown with wrong confirmation policy: %d %s", page.Code, page.Body.String())
	}
}
