package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestPendingResourceEditFormsFollowJobSnapshotsAndReleaseAfterCompletion(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	createSource := func(name, number string) model.OTPSource {
		t.Helper()
		source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
			Name: name, Provider: model.OTPProviderTwilio, Identity: "twilio:" + number,
			ProviderConfig: twilio.Config{AccountSID: "synthetic-account", AuthToken: "synthetic-secret", ToNumber: number},
		})
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	original := createSource("Original inbox", "+15550100123")
	snapshot := createSource("Job inbox", "+15550100124")
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{
		Name: "Saved Yodel sign-in", OTPSourceID: original.ID, LoginProbeURL: "https://example.test/login",
		Headless: true, DefaultTimeoutMS: 15000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.SetDefaultOTPSource(ctx, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	job, err := resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck})
	if err != nil || job.OTPSourceID != snapshot.ID {
		t.Fatalf("job snapshot=%+v error=%v", job, err)
	}
	// Changing the default affects future work; editing must still follow the
	// source saved on this job, independent of the profile's legacy source.
	if err := resources.SetDefaultOTPSource(ctx, original.ID); err != nil {
		t.Fatal(err)
	}
	other, err := resources.CreateProfile(ctx, store.ProfileInput{
		Name: "Other Yodel sign-in", OTPSourceID: original.ID, LoginProbeURL: "https://example.test/login",
		Headless: true, DefaultTimeoutMS: 15000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// An older pending job must remain visible past the usual 100-job listing.
	for range 101 {
		recent, err := resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: other.ID, Command: model.CommandAuthCheck})
		if err != nil {
			t.Fatal(err)
		}
		if err := resources.RequestJobCancellation(ctx, recent.ID); err != nil {
			t.Fatal(err)
		}
	}
	cookies := loginCookies(t, f)
	jobLink := fmt.Sprintf(`href="/jobs/%d"`, job.ID)
	targets := []struct {
		path, submit, help string
		form               url.Values
	}{
		{fmt.Sprintf("/profiles/%d", profile.ID), "Save sign-in", "This sign-in cannot be changed while its job is pending.", url.Values{"csrf_token": {csrfFrom(cookies)}, "name": {"Attempted rename"}, "enabled": {"1"}}},
		{fmt.Sprintf("/sources/%d", snapshot.ID), "Save source", "This OTP source cannot be changed while its job is pending.", url.Values{"csrf_token": {csrfFrom(cookies)}, "name": {"Attempted rename"}, "provider": {"twilio"}}},
	}
	for _, status := range []model.JobStatus{model.JobQueued, model.JobRunning, model.JobAwaitingApproval} {
		if job.Status != status {
			job, err = f.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{job.Status}, status, store.JobTransition{})
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, target := range targets {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				response := serveForm(f, method, target.path, cookies, target.form)
				wantStatus := http.StatusOK
				if method == http.MethodPost {
					wantStatus = http.StatusUnprocessableEntity
				}
				body := response.Body.String()
				if response.Code != wantStatus || !strings.Contains(body, target.help) || !strings.Contains(body, jobLink) || !strings.Contains(body, `type="submit" disabled>`+target.submit+`</button>`) {
					t.Fatalf("%s %s while %s=%d body=%s", method, target.path, status, response.Code, body)
				}
				if strings.Contains(body, "synthetic-secret") || strings.Contains(body, "5559876543") {
					t.Fatal("busy form exposed a saved credential")
				}
			}
		}
	}
	retained, err := resources.GetProfile(ctx, profile.ID)
	if err != nil || retained.Name != profile.Name || retained.OTPSourceID != original.ID {
		t.Fatalf("busy profile was modified: %+v error=%v", retained, err)
	}
	source, err := resources.GetOTPSource(ctx, snapshot.ID)
	if err != nil || source.Name != snapshot.Name {
		t.Fatalf("busy source was modified: %+v error=%v", source, err)
	}
	for _, path := range []string{fmt.Sprintf("/profiles/%d", other.ID), fmt.Sprintf("/sources/%d", original.ID), "/profiles/new", "/sources/new"} {
		response := serveForm(f, http.MethodGet, path, cookies, nil)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `type="submit" disabled`) || strings.Contains(response.Body.String(), jobLink) {
			t.Fatalf("unrelated or new resource incorrectly blocked %s=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	if _, err := f.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{job.Status}, model.JobCancelled, store.JobTransition{}); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		response := serveForm(f, http.MethodGet, target.path, cookies, nil)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `type="submit" disabled`) || strings.Contains(response.Body.String(), jobLink) {
			t.Fatalf("completed job still blocks %s=%d body=%s", target.path, response.Code, response.Body.String())
		}
		response = serveForm(f, http.MethodPost, target.path, cookies, target.form)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("unblocked save %s=%d body=%s", target.path, response.Code, response.Body.String())
		}
	}
}

func TestPendingResourceEditNoticesAreAccountScoped(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	own, _ := createImmediateWebBooking(t, f, f.admin.ID, "own", true)
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "private-job-owner", Password: "private job owner password"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := createImmediateWebBooking(t, f, member.ID, "foreign", true)
	job, err := f.store.ForUser(member.ID).EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: foreign.ID, Command: model.CommandAuthCheck})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	for _, path := range []string{fmt.Sprintf("/profiles/%d", own.ID), fmt.Sprintf("/sources/%d", own.OTPSourceID)} {
		response := serveForm(f, http.MethodGet, path, cookies, nil)
		body := response.Body.String()
		if response.Code != http.StatusOK || strings.Contains(body, `type="submit" disabled`) || strings.Contains(body, fmt.Sprintf(`href="/jobs/%d"`, job.ID)) || strings.Contains(body, foreign.Name) {
			t.Fatalf("foreign job leaked into %s=%d body=%s", path, response.Code, body)
		}
	}
	for _, path := range []string{fmt.Sprintf("/profiles/%d", foreign.ID), fmt.Sprintf("/sources/%d", foreign.OTPSourceID)} {
		response := serveForm(f, http.MethodGet, path, cookies, nil)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), fmt.Sprintf(`href="/jobs/%d"`, job.ID)) {
			t.Fatalf("foreign busy resource %s=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

func TestYodelSignInPhonePlaceholderExplainsCreationAndRetention(t *testing.T) {
	f := newWebFixture(t)
	profile, _ := createImmediateWebBooking(t, f, f.admin.ID, "phone guidance", true)
	cookies := loginCookies(t, f)
	for _, test := range []struct{ path, placeholder, help string }{
		{"/profiles/new", "Enter mobile number", "A leading +1 is accepted."},
		{fmt.Sprintf("/profiles/%d", profile.ID), "Leave blank to keep existing", "Leave blank to keep your saved mobile number."},
	} {
		response := serveForm(f, http.MethodGet, test.path, cookies, nil)
		body := html.UnescapeString(response.Body.String())
		if response.Code != http.StatusOK || !strings.Contains(body, `placeholder="`+test.placeholder+`"`) || !strings.Contains(body, test.help) || strings.Contains(body, "Required when selected") || strings.Contains(body, "5559876543") {
			t.Fatalf("phone guidance %s=%d body=%s", test.path, response.Code, body)
		}
	}
}
