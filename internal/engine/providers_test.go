package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/egress"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/otp"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestPairingPrerequisitesIdentifyTheProfileToCorrect(t *testing.T) {
	for _, test := range []struct {
		name             string
		profile, enabled bool
		loginURL         string
		wantErr          error
	}{
		{name: "source without a profile", wantErr: ErrPairingProfileRequired},
		{name: "disabled linked profile", profile: true, loginURL: "https://example.test/login", wantErr: ErrPairingProfileDisabled},
		{name: "unapproved profile login page", profile: true, enabled: true, loginURL: "https://unapproved.example/login", wantErr: ErrPairingProfileInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			ctx := context.Background()
			if !test.profile {
				if err := fixture.resources.DeleteBookingRequest(ctx, fixture.booking.ID); err != nil {
					t.Fatal(err)
				}
				if err := fixture.resources.DeleteProfile(ctx, fixture.booking.ProfileID); err != nil {
					t.Fatal(err)
				}
			}
			source := createPairingTestSource(t, fixture.resources, "http://127.0.0.1:2234")
			var want PairingSetup
			if test.profile {
				profile := createPairingTestProfile(t, fixture.resources, source.ID, test.loginURL, test.enabled)
				want = PairingSetup{ProfileID: profile.ID, ProfileName: profile.Name}
			}
			setup, err := fixture.engine.CheckPairingSetup(ctx, fixture.user.ID, source.ID)
			if setup != want || !errors.Is(err, test.wantErr) {
				t.Fatalf("pairing setup = %+v, %v; want %+v, %v", setup, err, want, test.wantErr)
			}
			if test.wantErr == ErrPairingProfileInvalid && !strings.Contains(err.Error(), "Yodel login URL must use an approved Yodel origin") {
				t.Fatalf("profile error lacks validation detail: %v", err)
			}
			if _, err := fixture.engine.QueuePairing(ctx, fixture.user.ID, source.ID); !errors.Is(err, test.wantErr) {
				t.Fatalf("pairing admission does not match setup: %v", err)
			}
			jobs, err := fixture.resources.ListJobs(ctx, 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("failed setup queued jobs = %v, %v", jobs, err)
			}
		})
	}
}

func TestPairingQueuesAnOwnedProfileWithoutAnyBooking(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	if err := fixture.resources.DeleteBookingRequest(ctx, fixture.booking.ID); err != nil {
		t.Fatal(err)
	}
	source := createPairingTestSource(t, fixture.resources, "http://127.0.0.1:2234")
	profile := createPairingTestProfile(t, fixture.resources, source.ID, "https://example.test/profile-login", true)
	setup, err := fixture.engine.CheckPairingSetup(ctx, fixture.user.ID, source.ID)
	if err != nil || setup != (PairingSetup{ProfileID: profile.ID, ProfileName: profile.Name}) {
		t.Fatalf("ready pairing setup = %+v, %v", setup, err)
	}
	job, err := fixture.engine.QueuePairing(ctx, fixture.user.ID, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.BookingRequestID != nil || job.Command != model.CommandAuthCheck || job.RunMode != model.RunModeManual || job.ProfileID != profile.ID || job.OTPSourceID != source.ID {
		t.Fatalf("pairing did not queue a profile-only auth check: %+v", job)
	}
	if _, err := fixture.engine.QueuePairing(ctx, fixture.user.ID, source.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second pending pairing error = %v", err)
	}
	otherUserID := fixture.user.ID + 100
	setup, err = fixture.engine.CheckPairingSetup(ctx, otherUserID, source.ID)
	if !errors.Is(err, store.ErrNotFound) || setup != (PairingSetup{}) {
		t.Fatalf("foreign pairing setup = %+v, %v", setup, err)
	}
	if _, err := fixture.engine.QueuePairing(ctx, otherUserID, source.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign pairing queue error = %v", err)
	}
}

type pairingCandidateProvider struct {
	otp.PairingProvider
	candidate otp.Message
}

func (p pairingCandidateProvider) WaitForPairingCandidates(context.Context, otp.Armed) ([]otp.Message, error) {
	return []otp.Message{p.candidate}, nil
}

func TestProfileOnlyPairingPersistsTheSelectedFingerprint(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source := createPairingTestSource(t, fixture.resources, "http://127.0.0.1:2234")
	createPairingTestProfile(t, fixture.resources, source.ID, "https://example.test/login", true)
	job, err := fixture.engine.QueuePairing(ctx, fixture.user.ID, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err = fixture.store.SystemClaimNextDueJob(ctx, "pairing-selection-test")
	if err != nil {
		t.Fatal(err)
	}
	jobKey := strconv.FormatInt(job.ID, 10)
	candidate := otp.Message{ID: "message-1", Code: "123456", ChatGUID: "SMS;-;+15550100123", Sender: "+15550100123", Service: "SMS"}
	provider := supervisedProvider{
		PairingProvider: pairingCandidateProvider{candidate: candidate}, hub: fixture.engine.hub,
		store: fixture.store, jobID: job.ID, sourceID: source.ID, jobKey: jobKey,
	}
	events, unsubscribe := fixture.engine.hub.Subscribe(jobKey)
	defer unsubscribe()
	finished := make(chan error, 1)
	go func() {
		message, err := provider.WaitForCode(ctx, otp.Armed{})
		if err == nil && message.ID != candidate.ID {
			err = errors.New("selected pairing message was not returned to the worker")
		}
		finished <- err
	}()
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("pairing candidates were not published")
	}
	if err := fixture.engine.ChoosePairing(ctx, fixture.user.ID, job.ID, candidate.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("pairing selection did not complete")
	}
	persisted, err := fixture.resources.GetOTPSource(ctx, source.ID)
	if err != nil || persisted.PairingChatGUID != candidate.ChatGUID || persisted.PairingSender != candidate.Sender || persisted.PairingService != candidate.Service || !provider.selected.Load() {
		t.Fatalf("profile-only pairing did not persist successful selection: %+v, %v", persisted, err)
	}
}

func TestProfileLoginExecutionConfigAndCachedSessionPairing(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the worker transport regression")
	}
	workerDir := t.TempDir()
	// This real subprocess verifies the wire config and credential exchange. It
	// models a cached session: authentication succeeds without requesting OTP.
	worker := `import json, sys
from datetime import datetime, timezone
from pathlib import Path

def emit(kind, **values):
    print(json.dumps(dict(v=2, type=kind, **values)), flush=True)

emit("worker.ready", action="yodel", protocol=2)
start = json.loads(sys.stdin.readline())
assert start["type"] == "run.start"
config = start["config"]
assert config["provider_id"] == "yodel"
assert config["login_probe_url"] == "https://example.test/profile-login"
if start["command"] == "auth-check":
    assert not ({"lake_id", "vehicle_keyword", "target_date", "timezone", "all_day_pass_url", "half_day_pass_url", "pass_order", "release_at", "auth_deadline_at"} & config.keys())
else:
    assert config["lake_id"] == "buntzen"
    assert config["vehicle_keyword"] == "Request vehicle snapshot"
    assert start["command"] in {"dry-run", "book"}
    if start["command"] == "dry-run":
        assert config["target_date"] == "2031-01-15"
    else:
        assert start["mode"] == "manual" and "release_at" not in config
        assert config["target_date"] == datetime.now(timezone.utc).date().isoformat()
        Path(__file__).with_suffix(".json").write_text(json.dumps(config))
    assert config["pass_order"] == ["morning", "all_day", "afternoon"]
    assert config["all_day_pass_url"] == "https://example.test/all-day"
    assert config["half_day_pass_url"] == "https://example.test/half-day"
emit("credentials.request", request_id="profile-login")
credentials = json.loads(sys.stdin.readline())
assert credentials["type"] == "credentials.provide" and credentials["phone"] == "5559876543"
emit("run.complete", status="cancelled" if start["command"] == "book" else "succeeded", message="Profile configuration verified")
`
	workerPath := filepath.Join(workerDir, "profile_auth_probe.py")
	if err := os.WriteFile(workerPath, []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	launcher := filepath.Join(workerDir, "run-probe")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+quote(python)+" "+quote(workerPath)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                        string
		pairing, booking, immediate bool
	}{
		{name: "auth without booking"}, {name: "cached pairing without OTP", pairing: true}, {name: "booking uses profile login and ordered passes", booking: true},
		{name: "book now uses profile login and bounded manual timing", booking: true, immediate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := fixture.resources.DeleteBookingRequest(ctx, fixture.booking.ID); err != nil {
				t.Fatal(err)
			}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/ping" {
					t.Errorf("unexpected provider request: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":200,"data":"pong"}`))
			}))
			defer provider.Close()
			fixture.engine.config.BlueBubblesPolicy, err = egress.NewPolicy([]egress.Rule{{Origin: provider.URL, Networks: []string{"127.0.0.1/32"}}})
			if err != nil {
				t.Fatal(err)
			}
			source := createPairingTestSource(t, fixture.resources, provider.URL)
			profile := createPairingTestProfile(t, fixture.resources, source.ID, "https://example.test/profile-login", true)
			if err := fixture.resources.SetDefaultOTPSource(ctx, source.ID); err != nil {
				t.Fatal(err)
			}
			fixture.engine.config.PythonExecutable, fixture.engine.config.PythonModule = launcher, "profile_auth_probe"
			var job model.Job
			if test.pairing {
				job, err = fixture.engine.QueuePairing(ctx, fixture.user.ID, source.ID)
			} else if test.booking {
				booking := fixture.booking
				booking.VehicleKeyword = "Request vehicle snapshot"
				booking.ProfileID = profile.ID
				booking.LoginProbeURL = "https://unapproved.example/legacy-login"
				booking.HalfDayPassURL = "https://example.test/half-day"
				booking.PreferredPasses = []model.PassType{model.PassMorning, model.PassAllDay, model.PassAfternoon}
				if test.immediate {
					booking.TargetDate = time.Now().UTC().Format(time.DateOnly)
				}
				booking, err = fixture.resources.CreateBookingRequest(ctx, booking)
				if err != nil {
					t.Fatal(err)
				}
				if test.immediate {
					job, err = fixture.engine.QueueBookingNow(ctx, fixture.user.ID, booking.ID)
				} else {
					job, err = fixture.engine.QueueBooking(ctx, fixture.user.ID, booking.ID, model.CommandDryRun, model.RunModeDryRun)
				}
			} else {
				job, err = fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck})
			}
			if err != nil {
				t.Fatal(err)
			}
			job, err = fixture.store.SystemClaimNextDueJob(ctx, "profile-auth-test")
			if err != nil {
				t.Fatal(err)
			}
			result, err := fixture.engine.execute(ctx, job)
			if err != nil {
				t.Fatal(err)
			}
			if !test.pairing && !test.immediate && result.Status != model.JobSucceeded {
				t.Fatalf("profile auth result = %+v", result)
			}
			if test.immediate {
				if result.Status != model.JobCancelled {
					t.Fatalf("immediate config probe result = %+v", result)
				}
				raw, err := os.ReadFile(filepath.Join(workerDir, "profile_auth_probe.json"))
				if err != nil {
					t.Fatal(err)
				}
				var config map[string]any
				if err := json.Unmarshal(raw, &config); err != nil {
					t.Fatal(err)
				}
				// Parse the wire timestamp here: the transport-only probe can use
				// system Python versions that cannot parse RFC3339 nanoseconds.
				deadline, err := time.Parse(time.RFC3339Nano, config["auth_deadline_at"].(string))
				if err != nil || job.ExpiresAt == nil || !deadline.Equal(*job.ExpiresAt) || !deadline.After(time.Now()) {
					t.Fatalf("worker deadline %v must match the future original expiry %v: %v", deadline, job.ExpiresAt, err)
				}
			}
			if test.pairing && (result.Status != model.JobFailed || !strings.Contains(result.Message, "no new verification message was selected")) {
				t.Fatalf("cached session falsely reported successful pairing: %+v", result)
			}
		})
	}
}

func createPairingTestSource(t *testing.T, resources store.UserStore, baseURL string) model.OTPSource {
	t.Helper()
	source, err := resources.CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Messages", Provider: model.OTPProviderBlueBubbles, Identity: baseURL,
		ProviderConfig: bluebubbles.Config{BaseURL: baseURL, Password: "synthetic-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func createPairingTestProfile(t *testing.T, resources store.UserStore, sourceID int64, loginURL string, enabled bool) model.Profile {
	t.Helper()
	profile, err := resources.CreateProfile(context.Background(), store.ProfileInput{
		Name: "Pairing profile", DefaultVehicle: "Example Vehicle", OTPSourceID: sourceID,
		LoginProbeURL: loginURL, Headless: true, DefaultTimeoutMS: 15_000, Enabled: enabled,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
