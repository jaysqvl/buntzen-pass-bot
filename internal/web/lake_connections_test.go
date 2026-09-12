package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func connectionFixture() (destinations.Lake, model.Profile, model.OTPSource, time.Time) {
	lake, _ := destinations.Resolve(destinations.DefaultLakeID)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	profile := model.Profile{ID: 10, UserID: 1, Name: "My account", LoginProbeURL: lake.LoginURL, OTPSourceID: 20, Enabled: true, DefaultTimeoutMS: 15000, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	source := model.OTPSource{ID: 20, UserID: 1, Name: "My inbox", Provider: model.OTPProviderTwilio, Identity: "preview-inbox"}
	return lake, profile, source, now
}

func connectionJob(id int64, profile model.Profile, command model.JobCommand, status model.JobStatus, at time.Time) model.Job {
	job := model.Job{ID: id, UserID: profile.UserID, ProfileID: profile.ID, Command: command, Status: status, CreatedAt: at.Add(-time.Minute), UpdatedAt: at}
	if status.Terminal() {
		job.FinishedAt = &at
	}
	return job
}

func TestLakeConnectionSetupRequiresUsableOwnedAccountAndSource(t *testing.T) {
	lake, profile, source, _ := connectionFixture()
	origins := []string{"https://yodelportal.com"}
	tests := []struct {
		name       string
		profiles   []model.Profile
		source     model.OTPSource
		started    bool
		configured bool
		status     string
	}{
		{name: "empty", source: source, status: "Not connected"},
		{name: "saved phone is not a verified connection", profiles: []model.Profile{profile}, source: source, started: true, configured: true, status: "Sign-in needed"},
		{name: "missing default inbox", profiles: []model.Profile{profile}, started: true, status: "OTP source needed"},
		{name: "foreign inbox", profiles: []model.Profile{profile}, source: func() model.OTPSource { value := source; value.UserID = 2; return value }(), started: true, status: "OTP source needed"},
		{name: "invalid inbox", profiles: []model.Profile{profile}, source: func() model.OTPSource { value := source; value.Provider = "unknown"; return value }(), started: true, status: "OTP source needed"},
		{name: "disabled account", profiles: []model.Profile{func() model.Profile { value := profile; value.Enabled = false; return value }()}, source: source, started: true, status: "Disabled"},
		{name: "invalid saved account", profiles: []model.Profile{func() model.Profile {
			value := profile
			value.LoginProbeURL = "https://unapproved.example/sign-in"
			return value
		}()}, source: source, started: true, status: "Needs update"},
		{name: "foreign account", profiles: []model.Profile{func() model.Profile { value := profile; value.UserID = 2; return value }()}, source: source, status: "Not connected"},
		{name: "different lake", profiles: []model.Profile{func() model.Profile { value := profile; value.LakeID = "future-lake"; return value }()}, source: source, status: "Not connected"},
		{name: "different provider", profiles: []model.Profile{func() model.Profile { value := profile; value.ProviderID = "future-provider"; return value }()}, source: source, status: "Not connected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := buildLakeConnection(lake, 1, test.profiles, nil, test.source, origins)
			if got.SetupStarted != test.started || got.Configured != test.configured || got.Connected || got.Status != test.status {
				t.Fatalf("unexpected readiness: %+v", got)
			}
			if got.URL != "/lakes/buntzen" || got.ActionURL != "/lakes/buntzen#connection" {
				t.Fatalf("setup action escaped lake context: %+v", got)
			}
		})
	}
	// A legacy/global Yodel identity belongs to the original destination only.
	future := lake
	future.ID = "future-yodel-lake"
	if got := buildLakeConnection(future, 1, []model.Profile{profile}, nil, source, origins); got.SetupStarted || got.Configured {
		t.Fatalf("same provider connected an unrelated lake: %+v", got)
	}
}

func TestLakeConnectionVerificationUsesCurrentOwnedCompletedEvidence(t *testing.T) {
	lake, profile, source, now := connectionFixture()
	success := connectionJob(100, profile, model.CommandAuthCheck, model.JobSucceeded, now)
	tests := []struct {
		name      string
		profile   model.Profile
		jobs      []model.Job
		connected bool
		status    string
	}{
		{name: "successful check", profile: profile, jobs: []model.Job{success}, connected: true, status: "Connection verified"},
		{name: "successful booking", profile: profile, jobs: []model.Job{connectionJob(100, profile, model.CommandBook, model.JobSucceeded, now)}, connected: true, status: "Connection verified"},
		{name: "successful rehearsal", profile: profile, jobs: []model.Job{connectionJob(100, profile, model.CommandDryRun, model.JobSucceeded, now)}, connected: true, status: "Connection verified"},
		{name: "credentials edited later", profile: func() model.Profile { value := profile; value.UpdatedAt = now.Add(time.Minute); return value }(), jobs: []model.Job{success}, status: "Sign-in needed"},
		{name: "foreign success", profile: profile, jobs: []model.Job{func() model.Job { value := success; value.UserID = 2; return value }()}, status: "Sign-in needed"},
		{name: "other identity success", profile: profile, jobs: []model.Job{func() model.Job { value := success; value.ProfileID++; return value }()}, status: "Sign-in needed"},
		{name: "missing completion evidence", profile: profile, jobs: []model.Job{func() model.Job { value := success; value.FinishedAt = nil; return value }()}, status: "Sign-in needed"},
		{name: "unrelated future job command", profile: profile, jobs: []model.Job{func() model.Job { value := success; value.Command = "cleanup"; return value }()}, status: "Sign-in needed"},
		{name: "newer auth failure needs review", profile: profile, jobs: []model.Job{success, connectionJob(101, profile, model.CommandAuthCheck, model.JobFailed, now.Add(time.Minute))}, status: "Check connection"},
		{name: "newer interrupted sign-in needs review", profile: profile, jobs: []model.Job{success, connectionJob(101, profile, model.CommandAuthCheck, model.JobInterrupted, now.Add(time.Minute))}, status: "Check connection"},
		{name: "booking failure does not prove disconnected", profile: profile, jobs: []model.Job{success, connectionJob(101, profile, model.CommandBook, model.JobFailed, now.Add(time.Minute))}, connected: true, status: "Connection verified"},
		{name: "cancelled check does not prove disconnected", profile: profile, jobs: []model.Job{success, connectionJob(101, profile, model.CommandAuthCheck, model.JobCancelled, now.Add(time.Minute))}, connected: true, status: "Connection verified"},
		{name: "later successful check clears old error", profile: profile, jobs: []model.Job{success, connectionJob(99, profile, model.CommandAuthCheck, model.JobFailed, now.Add(-time.Minute))}, connected: true, status: "Connection verified"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := buildLakeConnection(lake, 1, []model.Profile{test.profile}, test.jobs, source, []string{"https://yodelportal.com"})
			if !got.Configured || got.Connected != test.connected || got.Status != test.status {
				t.Fatalf("unexpected verification: %+v", got)
			}
			if got.Connected && (!strings.Contains(got.Description, "Last checked") || !strings.Contains(got.Description, "Checked again before booking") || got.Profiles[0].VerifiedAt == "") {
				t.Fatalf("historical evidence presented without its scope: %+v", got)
			}
		})
	}
}

func TestLakeConnectionPendingAndMultipleIdentityStatus(t *testing.T) {
	lake, profile, source, now := connectionFixture()
	queued := connectionJob(1, profile, model.CommandBook, model.JobQueued, now)
	running := connectionJob(2, profile, model.CommandAuthCheck, model.JobRunning, now)
	for _, test := range []struct {
		name   string
		jobs   []model.Job
		status string
		jobURL string
	}{
		{"scheduled booking is not connecting", []model.Job{queued}, "Job queued", "/jobs/1"},
		{"running sign-in", []model.Job{running}, "Connecting", "/jobs/2"},
		{"active job wins over future queue", []model.Job{queued, running}, "Connecting", "/jobs/2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := buildLakeConnection(lake, 1, []model.Profile{profile}, test.jobs, source, []string{"https://yodelportal.com"})
			if !got.Configured || got.Connected || !got.Profiles[0].Pending || got.Status != test.status || got.Profiles[0].JobURL != test.jobURL {
				t.Fatalf("pending job implied verified connection: %+v", got)
			}
		})
	}
	other := profile
	other.ID++
	other.Name = "Second account"
	jobs := []model.Job{connectionJob(100, profile, model.CommandAuthCheck, model.JobSucceeded, now), connectionJob(101, other, model.CommandAuthCheck, model.JobFailed, now.Add(time.Minute))}
	got := buildLakeConnection(lake, 1, []model.Profile{other, profile}, jobs, source, []string{"https://yodelportal.com"})
	if !got.Configured || !got.Connected || got.Status != "Connection verified" || len(got.Profiles) != 2 || got.Profiles[0].Connected || !got.Profiles[1].Connected {
		t.Fatalf("one account's failure overwrote another connection: %+v", got)
	}
}
