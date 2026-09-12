package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestDefaultOTPSourcePageSelectionIsOwnerScopedAndKeepsQueuedSource(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	profile, _ := createImmediateWebBooking(t, f, f.admin.ID, "Original setup", true)
	original, err := resources.GetDefaultOTPSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Alternative inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:alternative-default",
		ProviderConfig: map[string]string{"auth_token": "synthetic-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "other-default", Password: "another long password"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.store.ForUser(member.ID).CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Private inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:private-default",
		ProviderConfig: map[string]string{"auth_token": "synthetic-private"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	path := fmt.Sprintf("/sources/%d/default", alternate.ID)
	missingCSRF := serveForm(f, http.MethodPost, path, cookies, url.Values{})
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF=%d", missingCSRF.Code)
	}
	response := serveForm(f, http.MethodPost, path, cookies, url.Values{"csrf_token": {csrfFrom(cookies)}, "user_id": {fmt.Sprint(member.ID)}})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/sources?notice=otp-default-updated" {
		t.Fatalf("default save=%d %s", response.Code, response.Body.String())
	}
	selected, err := resources.GetDefaultOTPSource(ctx)
	if err != nil || selected.ID != alternate.ID {
		t.Fatalf("default=%+v err=%v", selected, err)
	}
	queued, err := resources.GetJob(ctx, job.ID)
	if err != nil || queued.OTPSourceID != original.ID {
		t.Fatalf("queued source changed: %+v err=%v", queued, err)
	}
	page := serveForm(f, http.MethodGet, "/sources", cookies, nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, ">Default</span>") || !strings.Contains(body, ">Configured</span>") || !strings.Contains(body, fmt.Sprintf(`action="/sources/%d/default"`, original.ID)) || strings.Contains(body, fmt.Sprintf(`action="/sources/%d/default"`, alternate.ID)) || strings.Contains(body, foreign.Name) {
		t.Fatalf("default source page=%d %s", page.Code, body)
	}
	denied := serveForm(f, http.MethodPost, fmt.Sprintf("/sources/%d/default", foreign.ID), cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
	if denied.Code != http.StatusNotFound {
		t.Fatalf("foreign default=%d", denied.Code)
	}
	other, err := f.store.ForUser(member.ID).GetDefaultOTPSource(ctx)
	if err != nil || other.ID != foreign.ID {
		t.Fatalf("other default changed: %+v %v", other, err)
	}
}
