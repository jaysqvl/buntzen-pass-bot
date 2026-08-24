package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	secretcrypto "github.com/jaysqvl/buntzen-pass-bot/internal/crypto"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestMigrateCreatesCleanSchemaAndRefusesLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	box := testEncryptor(t)
	path := filepath.Join(t.TempDir(), "new.db")
	store, err := Open(ctx, path, box)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}

	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(ctx, legacyPath, box)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.db.ExecContext(ctx, "CREATE TABLE instances(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Migrate(ctx); !errors.Is(err, ErrUnversionedDatabase) {
		t.Fatalf("migration error = %v", err)
	}
}

func TestConcurrentMigrationIsSerialized(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	box := testEncryptor(t)
	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			database, err := OpenMigrated(ctx, path, box)
			if err == nil {
				err = database.Close()
			}
			errorsFound <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsFound; err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
}

func TestSecretsRoundTripAndProfileSourceIsExclusive(t *testing.T) {
	ctx := context.Background()
	store := ownedTestStore(t)
	providerConfig := map[string]any{
		"base_url": "http://bluebubbles.example:1234",
		"password": "test-only-provider-password",
	}
	source, err := store.CreateOTPSource(ctx, testUserID, OTPSourceInput{
		Name: "Example Messages", Provider: model.OTPProviderBlueBubbles,
		Identity: "http://bluebubbles.example:1234", ProviderConfig: providerConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProfile(ctx, testUserID, ProfileInput{
		Name: "Empty", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{},
	}); err == nil {
		t.Fatal("empty Yodel credentials were accepted")
	}
	profile, err := store.CreateProfile(ctx, testUserID, ProfileInput{
		Name: "Example", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: "user@example.test", Password: "test-only-yodel-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.GetProfileCredentials(ctx, testUserID, profile.ID)
	if err != nil || credentials.Email != "user@example.test" || credentials.Password != "test-only-yodel-password" {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
	var decoded map[string]any
	if err := store.GetOTPSourceConfig(ctx, testUserID, source.ID, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["password"] != "test-only-provider-password" {
		t.Fatalf("provider config = %#v", decoded)
	}
	_, err = store.CreateProfile(ctx, testUserID, ProfileInput{
		Name: "Second", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID,
		DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: "second@example.test", Password: "password"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second profile error = %v", err)
	}

	var encryptedConfig, encryptedEmail, encryptedPassword string
	if err := store.db.QueryRowContext(ctx, "SELECT config_ciphertext FROM otp_sources WHERE id = ?", source.ID).Scan(&encryptedConfig); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `
		SELECT yodel_email_ciphertext, yodel_password_ciphertext FROM profiles WHERE id = ?
	`, profile.ID).Scan(&encryptedEmail, &encryptedPassword); err != nil {
		t.Fatal(err)
	}
	joined := encryptedConfig + encryptedEmail + encryptedPassword
	for _, secret := range []string{"test-only-provider-password", "user@example.test", "test-only-yodel-password"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("stored ciphertext exposed %q", secret)
		}
	}
}

func TestBookingPreservesExplicitZeroOffsetsAndRejectsRelativeURLs(t *testing.T) {
	store := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, store, "zero-offset-base")
	request := model.BookingRequest{
		Name: "zero offsets", ProfileID: profile.ID, Enabled: true, TargetDate: "2030-01-15",
		Timezone: "UTC", ReleaseTime: "07:00", PrepMinutesBefore: 0,
		AuthDeadlineMinutesBefore: 0, PollDeadlineSeconds: 120, PollMinSeconds: 1,
		PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all",
		CheckAllDay: true,
	}
	created, err := store.CreateBookingRequest(context.Background(), testUserID, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.PrepMinutesBefore != 0 || created.AuthDeadlineMinutesBefore != 0 {
		t.Fatalf("explicit zero offsets were rewritten: %+v", created)
	}
	request.Name = "invalid URL"
	request.LoginProbeURL = "/relative"
	if _, err := store.CreateBookingRequest(context.Background(), testUserID, request); err == nil {
		t.Fatal("relative login URL was accepted")
	}
}

func TestBlueBubblesPairingFingerprintIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	store := ownedTestStore(t)
	_, err := store.CreateOTPSource(ctx, testUserID, OTPSourceInput{
		Name: "partial", Provider: model.OTPProviderBlueBubbles, Identity: "http://mac.test:1234",
		ProviderConfig: map[string]string{"password": "secret"}, PairingChatGUID: "chat-only",
	})
	if err == nil || !strings.Contains(err.Error(), "chat, sender, and service") {
		t.Fatalf("partial pairing error = %v", err)
	}
	source, err := store.CreateOTPSource(ctx, testUserID, OTPSourceInput{
		Name: "complete", Provider: model.OTPProviderBlueBubbles, Identity: "http://mac.test:1234",
		ProviderConfig: map[string]string{"password": "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.CreateProfile(ctx, testUserID, ProfileInput{
		Name: "pairing profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: "pairing@example.test", Password: "password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := store.CreateBookingRequest(ctx, testUserID, model.BookingRequest{
		Name: "pairing booking", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2030-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all", CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bookingID := booking.ID
	job, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandAuthCheck, RunMode: model.RunModeManual,
		DedupKey: fmt.Sprintf("pairing:%d:test", source.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.SystemClaimNextDueJobAt(ctx, "pairing-worker", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SystemPersistOTPSourcePairing(ctx, job.ID, source.ID, "chat", "+15550100123", "SMS"); err != nil {
		t.Fatal(err)
	}
	paired, err := store.GetOTPSource(ctx, testUserID, source.ID)
	if err != nil || paired.PairingChatGUID != "chat" || paired.PairingSender != "+15550100123" || paired.PairingService != "SMS" {
		t.Fatalf("paired source=%+v err=%v", paired, err)
	}
	if _, err := store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobSucceeded, JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateOTPSource(ctx, testUserID, source.ID, OTPSourceInput{
		Name: "complete", Provider: model.OTPProviderBlueBubbles,
		Identity: "http://different-mac.test:1234", ProviderConfig: map[string]string{"password": "secret"},
	}); err == nil || !strings.Contains(err.Error(), "newly supplied password") {
		t.Fatalf("identity change with retained password error = %v", err)
	}
	unchanged, err := store.GetOTPSource(ctx, testUserID, source.ID)
	if err != nil || unchanged.Identity != source.Identity {
		t.Fatalf("source changed after rejected password retention: %+v err=%v", unchanged, err)
	}
	changed, err := store.UpdateOTPSource(ctx, testUserID, source.ID, OTPSourceInput{
		Name: "complete", Provider: model.OTPProviderBlueBubbles,
		Identity: "http://different-mac.test:1234", ProviderConfig: map[string]string{"password": "rotated"},
		SecretProvided:  true,
		PairingChatGUID: paired.PairingChatGUID, PairingSender: paired.PairingSender, PairingService: paired.PairingService,
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed.PairingChatGUID != "" || changed.PairingSender != "" || changed.PairingService != "" {
		t.Fatalf("pairing survived an inbox identity change: %#v", changed)
	}
}

func TestPairingPersistenceHonorsCommittedRevocation(t *testing.T) {
	t.Run("job cancellation", func(t *testing.T) {
		ctx := context.Background()
		database := ownedTestStore(t)
		source, job := claimedBlueBubblesPairingJob(t, database, testUserID, "cancel")
		if err := database.RequestJobCancellation(ctx, testUserID, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := database.SystemPersistOTPSourcePairing(ctx, job.ID, source.ID, "chat", "+15550100123", "SMS"); !errors.Is(err, ErrTransitionConflict) {
			t.Fatalf("pairing after cancellation error = %v", err)
		}
		assertSourceUnpaired(t, database, testUserID, source.ID)
	})

	t.Run("account disable", func(t *testing.T) {
		ctx := context.Background()
		database := ownedTestStore(t)
		member, err := database.CreateMember(ctx, CreateUserInput{Username: "pairing-member", Password: "member pairing password"})
		if err != nil {
			t.Fatal(err)
		}
		source, job := claimedBlueBubblesPairingJob(t, database, member.ID, "disable")
		if _, err := database.UpdateUser(ctx, member.ID, UserUpdateInput{Username: member.Username, Status: model.UserDisabled}); err != nil {
			t.Fatal(err)
		}
		if err := database.SystemPersistOTPSourcePairing(ctx, job.ID, source.ID, "chat", "+15550100123", "SMS"); !errors.Is(err, ErrTransitionConflict) {
			t.Fatalf("pairing after account disable error = %v", err)
		}
		assertSourceUnpaired(t, database, member.ID, source.ID)
	})
}

func TestJobsClaimExclusivelyAndRecoverSafely(t *testing.T) {
	ctx := context.Background()
	store := ownedTestStore(t)
	firstProfile, firstBooking := fixtureProfileAndBooking(t, store, "first")
	_, secondBooking := fixtureProfileAndBooking(t, store, "second")
	now := time.Date(2030, 1, 14, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	firstID := firstBooking.ID
	first, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &firstID, Command: model.CommandBook, RunMode: model.RunModeManual,
		DueAt: now, DedupKey: "first-booking",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &firstID, Command: model.CommandBook, RunMode: model.RunModeManual,
		DueAt: now, DedupKey: "first-booking",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate scheduled enqueue error = %v", err)
	}
	_, err = store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		ProfileID: firstProfile.ID, Command: model.CommandAuthCheck, RunMode: model.RunModeManual, DueAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondID := secondBooking.ID
	second, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &secondID, Command: model.CommandBook, RunMode: model.RunModeAuto, DueAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	claimedFirst, err := store.SystemClaimNextDueJobAt(ctx, "worker-1", now)
	if err != nil || claimedFirst.ID != first.ID {
		t.Fatalf("first claim=%+v err=%v", claimedFirst, err)
	}
	claimedSecond, err := store.SystemClaimNextDueJobAt(ctx, "worker-2", now)
	if err != nil || claimedSecond.ID != second.ID {
		t.Fatalf("second claim=%+v err=%v", claimedSecond, err)
	}
	if _, err := store.SystemClaimNextDueJobAt(ctx, "worker-3", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("same-profile claim error = %v", err)
	}
	if err := store.SystemMarkConfirmationStarted(ctx, claimedSecond.ID); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.SystemRecoverInterruptedJobs(ctx)
	if err != nil || recovered != 2 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	firstAfter, _ := store.GetJob(ctx, testUserID, claimedFirst.ID)
	secondAfter, _ := store.GetJob(ctx, testUserID, claimedSecond.ID)
	if firstAfter.Status != model.JobInterrupted {
		t.Fatalf("first status = %s", firstAfter.Status)
	}
	if secondAfter.Status != model.JobOutcomeUnknown {
		t.Fatalf("second status = %s", secondAfter.Status)
	}
	queued, err := store.SystemClaimNextDueJobAt(ctx, "worker-3", now)
	if err != nil || queued.ProfileID != firstProfile.ID {
		t.Fatalf("queued job was not retained: %+v err=%v", queued, err)
	}
}

func TestQueuedJobGuardsProfileSourceAndBookingConfiguration(t *testing.T) {
	ctx := context.Background()
	store := ownedTestStore(t)
	profile, booking := fixtureProfileAndBooking(t, store, "guarded")
	source, err := store.GetOTPSource(ctx, testUserID, profile.OTPSourceID)
	if err != nil {
		t.Fatal(err)
	}
	bookingID := booking.ID
	job, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandBook, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.UpdateProfile(ctx, testUserID, profile.ID, ProfileInput{
		Name: profile.Name, DefaultVehicle: "New vehicle",
		OTPSourceID: profile.OTPSourceID, Headless: profile.Headless,
		BrowserChannel: profile.BrowserChannel, BrowserExecutable: profile.BrowserExecutable,
		DefaultTimeoutMS: profile.DefaultTimeoutMS, Enabled: profile.Enabled,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("profile mutation error = %v", err)
	}
	booking.Name = "changed booking"
	if _, err := store.UpdateBookingRequest(ctx, testUserID, booking); !errors.Is(err, ErrConflict) {
		t.Fatalf("booking mutation error = %v", err)
	}
	_, err = store.UpdateOTPSource(ctx, testUserID, source.ID, OTPSourceInput{
		Name: source.Name, Provider: source.Provider, Identity: source.Identity,
		ProviderConfig: map[string]string{"password": "replacement"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("source mutation error = %v", err)
	}
	if _, err := store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobQueued},
		model.JobCancelled, JobTransition{Message: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	profile.DefaultVehicle = "New vehicle"
	updated, err := store.UpdateProfile(ctx, testUserID, profile.ID, ProfileInput{
		Name: profile.Name, DefaultVehicle: profile.DefaultVehicle,
		OTPSourceID: profile.OTPSourceID, Headless: profile.Headless,
		BrowserChannel: profile.BrowserChannel, BrowserExecutable: profile.BrowserExecutable,
		DefaultTimeoutMS: profile.DefaultTimeoutMS, Enabled: profile.Enabled,
	})
	if err != nil || updated.DefaultVehicle != "New vehicle" {
		t.Fatalf("post-cancel profile update=%+v err=%v", updated, err)
	}
}

func TestTransitionsDecisionsAndEventRedaction(t *testing.T) {
	ctx := context.Background()
	store := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, store, "approval")
	bookingID := booking.ID
	job, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandBook, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.SystemClaimNextDueJob(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning},
		model.JobAwaitingApproval, JobTransition{Message: "Waiting for approval"})
	if err != nil || job.Status != model.JobAwaitingApproval {
		t.Fatalf("awaiting transition=%+v err=%v", job, err)
	}
	decision, err := store.RecordJobDecision(ctx, testUserID, job.ID, model.DecisionApprove)
	if err != nil || decision.Decision != model.DecisionApprove {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if _, err := store.RecordJobDecision(ctx, testUserID, job.ID, model.DecisionApprove); err != nil {
		t.Fatalf("idempotent approval: %v", err)
	}
	if _, err := store.RecordJobDecision(ctx, testUserID, job.ID, model.DecisionCancel); !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("conflicting decision error = %v", err)
	}

	event, err := store.SystemAppendJobEvent(ctx, JobEventInput{
		JobID: job.ID, Kind: "otp.received", Message: "code=654321 was submitted with password",
		Data: map[string]any{
			"otp_code": "654321", "detail": "received 654321", "numeric": 654321,
			"concrete_map":   map[string]string{"otp": "654321", "note": "code 654321"},
			"concrete_slice": []string{"654321"},
			"innocuous_key":  "the credential was password",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(event)
	for _, secret := range []string{"654321", "password", "approval@example.test"} {
		if bytes.Contains(serialized, []byte(secret)) {
			t.Fatalf("secret %q leaked into event: %s", secret, serialized)
		}
	}

	job, err = store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobAwaitingApproval},
		model.JobRunning, JobTransition{ConfirmationStarted: true})
	if err != nil || job.ConfirmationStartedAt == nil {
		t.Fatalf("approval transition=%+v err=%v", job, err)
	}
	job, err = store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning},
		model.JobSucceeded, JobTransition{Message: "Booked", ExitCode: intPointer(0)})
	if err != nil || job.Status != model.JobSucceeded || job.FinishedAt == nil {
		t.Fatalf("terminal transition=%+v err=%v", job, err)
	}
	if _, err := store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning},
		model.JobFailed, JobTransition{}); !errors.Is(err, ErrTransitionConflict) {
		t.Fatalf("terminal transition was not protected: %v", err)
	}
}

func TestConcurrentApprovalRaceHasSingleDurableWinner(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "approval-race")
	bookingID := booking.ID
	job, err := database.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandBook, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.SystemClaimNextDueJob(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning},
		model.JobAwaitingApproval, JobTransition{Message: "Waiting for approval"})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		decision model.ApprovalDecision
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, decision := range []model.ApprovalDecision{model.DecisionApprove, model.DecisionCancel} {
		go func(decision model.ApprovalDecision) {
			<-start
			_, err := database.RecordJobDecision(ctx, testUserID, job.ID, decision)
			results <- outcome{decision: decision, err: err}
		}(decision)
	}
	close(start)

	var winner model.ApprovalDecision
	conflicts := 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			if winner != "" {
				t.Fatalf("both approval decisions succeeded: %s and %s", winner, result.decision)
			}
			winner = result.decision
		case errors.Is(result.err, ErrDecisionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected approval race error for %s: %v", result.decision, result.err)
		}
	}
	if winner == "" || conflicts != 1 {
		t.Fatalf("approval race winner=%q conflicts=%d", winner, conflicts)
	}
	recorded, err := database.GetJobDecision(ctx, testUserID, job.ID)
	if err != nil || recorded.Decision != winner {
		t.Fatalf("recorded decision=%+v winner=%q err=%v", recorded, winner, err)
	}
}

func TestConcurrentClaimsAcquireOnlyOneProfileLease(t *testing.T) {
	ctx := context.Background()
	store := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, store, "parallel")
	now := time.Date(2030, 1, 14, 12, 0, 0, 0, time.UTC)
	for range 8 {
		if _, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
			ProfileID: profile.ID, Command: model.CommandAuthCheck,
			RunMode: model.RunModeManual, DueAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	type claim struct {
		job model.Job
		err error
	}
	results := make(chan claim, 8)
	start := make(chan struct{})
	for worker := range 8 {
		go func() {
			<-start
			job, err := store.SystemClaimNextDueJobAt(ctx, fmt.Sprintf("worker-%d", worker), now)
			results <- claim{job: job, err: err}
		}()
	}
	close(start)
	claimed := 0
	for range 8 {
		result := <-results
		if result.err == nil {
			claimed++
			continue
		}
		if !errors.Is(result.err, ErrNotFound) {
			t.Fatalf("unexpected claim error: %v", result.err)
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed %d jobs for one profile, want 1", claimed)
	}
}

func TestDurableCancellationStopsQueuedAndFlagsRunningJobs(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, database, "cancel")
	queued, err := database.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RequestJobCancellation(ctx, testUserID, queued.ID); err != nil {
		t.Fatal(err)
	}
	queued, _ = database.GetJob(ctx, testUserID, queued.ID)
	if queued.Status != model.JobCancelled || !queued.CancelRequested {
		t.Fatalf("queued cancellation = %+v", queued)
	}

	running, err := database.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err = database.SystemClaimNextDueJob(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RequestJobCancellation(ctx, testUserID, running.ID); err != nil {
		t.Fatal(err)
	}
	requested, err := database.SystemJobCancellationRequested(ctx, running.ID)
	if err != nil || !requested {
		t.Fatalf("running cancellation requested=%v err=%v", requested, err)
	}
}

func TestExpiredQueuedJobNeverStarts(t *testing.T) {
	ctx := context.Background()
	store := ownedTestStore(t)
	profile, booking := fixtureProfileAndBooking(t, store, "expired")
	now := time.Date(2030, 1, 14, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Minute)
	bookingID := booking.ID
	stale, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandBook, RunMode: model.RunModeAuto,
		DueAt: now, ExpiresAt: &expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := store.EnqueueJob(ctx, testUserID, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck, RunMode: model.RunModeManual,
		DueAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.SystemClaimNextDueJobAt(ctx, "worker", expires)
	if err != nil || claimed.ID != valid.ID {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	stale, err = store.GetJob(ctx, testUserID, stale.ID)
	if err != nil || stale.Status != model.JobFailed || stale.StartedAt != nil {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
}

func TestAccountSessionsAndRateLimit(t *testing.T) {
	ctx := context.Background()
	store := emptyTestStore(t)
	admin, err := store.SetupAdmin(ctx, "admin", "a strong local admin password")
	if err != nil {
		t.Fatalf("setup=%+v err=%v", admin, err)
	}
	if _, err = store.SetupAdmin(ctx, "admin", "ignored because it already exists"); !errors.Is(err, ErrSetupComplete) {
		t.Fatalf("second setup err=%v", err)
	}
	if _, ok, err := store.AuthenticateUser(ctx, "admin", "wrong password"); err != nil || ok {
		t.Fatalf("wrong password ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.AuthenticateUser(ctx, "admin", "a strong local admin password"); err != nil || !ok {
		t.Fatalf("correct password ok=%v err=%v", ok, err)
	}

	credentials, err := store.NewSession(ctx, admin.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(credentials.Session.ID, credentials.Token) || !ValidateCSRF(credentials.Session, credentials.CSRFToken) {
		t.Fatal("raw session material was stored or CSRF validation failed")
	}
	loaded, err := store.GetSession(ctx, credentials.Token)
	if err != nil || loaded.User.ID != admin.ID {
		t.Fatalf("session=%+v err=%v", loaded, err)
	}

	now := time.Date(2030, 1, 14, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	for range 5 {
		if err := store.RecordLoginAttempt(ctx, "192.0.2.1", false); err != nil {
			t.Fatal(err)
		}
	}
	allowed, retryAt, err := store.LoginRateLimit(ctx, "192.0.2.1", now, 15*time.Minute, 5)
	if err != nil || allowed || !retryAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("allowed=%v retry=%v err=%v", allowed, retryAt, err)
	}
	if err := store.RecordLoginAttempt(ctx, "192.0.2.1", true); err != nil {
		t.Fatal(err)
	}
	allowed, _, err = store.LoginRateLimit(ctx, "192.0.2.1", now, 15*time.Minute, 5)
	if err != nil || !allowed {
		t.Fatalf("successful login did not clear limit: allowed=%v err=%v", allowed, err)
	}
}

func testStore(t *testing.T) *Store {
	return emptyTestStore(t)
}

const testUserID int64 = 1

func ownedTestStore(t *testing.T) *Store {
	t.Helper()
	store := emptyTestStore(t)
	user, err := store.SetupAdmin(context.Background(), "test-admin", "a strong test password")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != testUserID {
		t.Fatalf("test user ID = %d, want %d", user.ID, testUserID)
	}
	return store
}

func emptyTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenMigrated(context.Background(), filepath.Join(t.TempDir(), "buntzen.db"), testEncryptor(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func testEncryptor(t *testing.T) *secretcrypto.Encryptor {
	t.Helper()
	box, err := secretcrypto.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func fixtureProfileAndBooking(t *testing.T, store *Store, name string) (model.Profile, model.BookingRequest) {
	t.Helper()
	ctx := context.Background()
	source, err := store.CreateOTPSource(ctx, testUserID, OTPSourceInput{
		Name: name + " source", Provider: model.OTPProviderTwilio,
		Identity: "twilio:" + name, ProviderConfig: map[string]string{"auth_token": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.CreateProfile(ctx, testUserID, ProfileInput{
		Name: name + " profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: name + "@example.test", Password: "password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := store.CreateBookingRequest(ctx, testUserID, model.BookingRequest{
		Name: name + " booking", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2030-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all",
		CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile, booking
}

func claimedBlueBubblesPairingJob(t *testing.T, database *Store, userID int64, name string) (model.OTPSource, model.Job) {
	t.Helper()
	ctx := context.Background()
	source, err := database.CreateOTPSource(ctx, userID, OTPSourceInput{
		Name: name + " Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://" + name + ".example.test:1234",
		ProviderConfig: map[string]string{"password": "synthetic-provider-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := database.CreateProfile(ctx, userID, ProfileInput{
		Name: name + " profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: name + "@example.test", Password: "synthetic-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := database.CreateBookingRequest(ctx, userID, model.BookingRequest{
		Name: name + " booking", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2030-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all", CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bookingID := booking.ID
	job, err := database.EnqueueJob(ctx, userID, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandAuthCheck, RunMode: model.RunModeManual,
		DedupKey: fmt.Sprintf("pairing:%d:%s", source.ID, name),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.SystemClaimNextDueJobAt(ctx, "pairing-worker", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return source, job
}

func assertSourceUnpaired(t *testing.T, database *Store, userID, sourceID int64) {
	t.Helper()
	source, err := database.GetOTPSource(context.Background(), userID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if source.PairingChatGUID != "" || source.PairingSender != "" || source.PairingService != "" {
		t.Fatalf("revoked pairing write persisted: %+v", source)
	}
}

func intPointer(value int) *int { return &value }

func TestEncryptedSecretsNeverReachDatabaseFiles(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "buntzen.db")
	store, err := OpenMigrated(ctx, path, testEncryptor(t))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.SetupAdmin(ctx, "test-admin", "a strong test password")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.CreateOTPSource(ctx, admin.ID, OTPSourceInput{
		Name: "source", Provider: model.OTPProviderTwilio, Identity: "acct:+15550100123",
		ProviderConfig: map[string]string{"auth_token": "never-persist-this-plaintext"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateProfile(ctx, admin.ID, ProfileInput{
		Name: "profile", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID,
		DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: "hidden@example.test", Password: "also-never-persist"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"never-persist-this-plaintext", "hidden@example.test", "also-never-persist"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("%s contains plaintext secret %q", entry.Name(), secret)
			}
		}
	}
}
