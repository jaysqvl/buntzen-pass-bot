package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestApprovalPageShowsRequestedDateVehicleAndSelectedPass(t *testing.T) {
	fixture := newWebFixture(t)
	profile, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "approval-review", true)
	ctx := context.Background()
	job, err := fixture.server.engine.QueueBookingNow(ctx, fixture.admin.ID, booking.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SystemClaimNextDueJob(ctx, "review-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobAwaitingApproval, store.JobTransition{Message: "Waiting for approval: all-day pass."}); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	page := serveForm(fixture, http.MethodGet, fmt.Sprintf("/jobs/%d", job.ID), cookies, nil)
	for _, want := range []string{
		"Waiting for approval: all-day pass.", booking.TargetDate + " · UTC", profile.DefaultVehicle,
		"Pass preference order", "Book now · manual approval", "Expires", `id="approval-panel" class="approval" >`,
	} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("approval page missing %q: status=%d body=%q", want, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), "5559876543") || strings.Contains(page.Body.String(), "synthetic-secret") {
		t.Fatal("approval details exposed credentials")
	}
	if _, err := fixture.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobAwaitingApproval}, model.JobCancelled, store.JobTransition{}); err != nil {
		t.Fatal(err)
	}
	page = serveForm(fixture, http.MethodGet, fmt.Sprintf("/jobs/%d", job.ID), cookies, nil)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), booking.TargetDate+" · UTC") {
		t.Fatal("terminal history must not present current editable booking settings as an issued receipt")
	}
}
