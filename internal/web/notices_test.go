package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestBookingFailureExplainsRetainedConfirmationAndLinksOwnedJob(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	resources := fixture.store.ForUser(fixture.admin.ID)
	_, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "retained-notice", true)
	booking.TargetDate = "2030-09-10"
	booking, err := resources.UpdateBookingRequest(ctx, booking)
	if err != nil {
		t.Fatal(err)
	}
	job, err := fixture.server.engine.QueueBooking(ctx, fixture.admin.ID, booking.ID, model.CommandBook, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, store.JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SystemMarkConfirmationStarted(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobOutcomeUnknown, store.JobTransition{}); err != nil {
		t.Fatal(err)
	}
	other := booking
	other.ID, other.Name, other.ConfirmationMode = 0, "Another request for the same date", model.RunModeManual
	other, err = resources.CreateBookingRequest(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/bookings/%d/run", other.ID), cookies,
		url.Values{"csrf_token": {csrfFrom(cookies)}, "command": {"book"}})
	destination, err := url.Parse(response.Header().Get("Location"))
	if err != nil || response.Code != http.StatusSeeOther || destination.Path != "/bookings" || destination.Query().Get("notice") != "queue-review" || destination.Query().Get("job") != fmt.Sprint(job.ID) {
		t.Fatalf("retained confirmation response=%d destination=%s", response.Code, response.Header().Get("Location"))
	}
	page := serveForm(fixture, http.MethodGet, destination.String(), cookies, nil)
	for _, want := range []string{`role="alert"`, "Check Yodel before retrying", "View existing job", fmt.Sprintf(`href="/jobs/%d"`, job.ID)} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("retained booking missing %q: %d %s", want, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), "No second job was created") {
		t.Fatal("uncertain completed checkout was described as a pending duplicate")
	}
	if jobs, err := resources.ListJobs(ctx, 10); err != nil || len(jobs) != 1 || jobs[0].Status != model.JobOutcomeUnknown {
		t.Fatalf("notification changed retained booking protection: jobs=%+v err=%v", jobs, err)
	}
}

func TestFullQueueDoesNotClaimThatBookingIsAlreadyQueued(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	profile, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "capacity-notice", true)
	for range store.MaxPendingJobsPerUser {
		if _, err := fixture.store.ForUser(fixture.admin.ID).EnqueueJob(ctx, store.EnqueueJobParams{
			ProfileID: profile.ID, Command: model.CommandAuthCheck, DueAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cookies := loginCookies(t, fixture)
	response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/bookings/%d/run", booking.ID), cookies,
		url.Values{"csrf_token": {csrfFrom(cookies)}, "command": {"book"}, "timing": {"now"}})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/bookings?notice=queue-full" {
		t.Fatalf("capacity error=%d location=%s", response.Code, response.Header().Get("Location"))
	}
	page := serveForm(fixture, http.MethodGet, response.Header().Get("Location"), cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "reached its job limit") || strings.Contains(page.Body.String(), "View existing job") {
		t.Fatalf("capacity notification=%d %s", page.Code, page.Body.String())
	}
}

func TestNotificationCannotLinkAnotherOwnersJobOrEchoArbitraryText(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	member, err := fixture.store.CreateMember(ctx, store.CreateUserInput{Username: "notice-member", Password: "long member password"})
	if err != nil {
		t.Fatal(err)
	}
	_, booking := createImmediateWebBooking(t, fixture, member.ID, "private-notice", true)
	job, err := fixture.server.engine.QueueBookingNow(ctx, member.ID, booking.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	for _, code := range []string{"queue-pending", "queue-review", `<script>alert("injected")</script>`} {
		query := url.Values{"notice": {code}, "job": {fmt.Sprint(job.ID)}, "return_to": {"https://attacker.invalid/"}}
		page := serveForm(fixture, http.MethodGet, "/bookings?"+query.Encode(), cookies, nil)
		if page.Code != http.StatusOK {
			t.Fatalf("notice page=%d", page.Code)
		}
		for _, unwanted := range []string{fmt.Sprintf(`href="/jobs/%d"`, job.ID), "View existing job", "private-notice", "attacker.invalid", "injected"} {
			if strings.Contains(page.Body.String(), unwanted) {
				t.Fatalf("notification exposed %q", unwanted)
			}
		}
	}
}
