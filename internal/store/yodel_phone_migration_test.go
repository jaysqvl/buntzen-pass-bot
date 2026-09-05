package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestYodelPhoneMigrationFailsClosedAndPreservesLinkedWork(t *testing.T) {
	ctx := context.Background()
	const (
		legacyEmailPlain    = "legacy-email@example.test"
		legacyPasswordPlain = "synthetic-legacy-password"
		providerSecret      = "synthetic-provider-token"
	)
	box := testEncryptor(t)
	database, err := Open(ctx, filepath.Join(t.TempDir(), "v1.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	if _, err := database.db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		t.Fatal(err)
	}
	initialSchema, err := migrationFiles.ReadFile("migrations/0001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, string(initialSchema)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO schema_migrations(version, name, applied_at) VALUES (1, '0001_initial.sql', ?)
	`, formatTime(database.now())); err != nil {
		t.Fatal(err)
	}

	admin, err := database.SetupAdmin(ctx, "migration-admin", "a strong migration test password")
	if err != nil {
		t.Fatal(err)
	}
	source, err := database.CreateOTPSource(ctx, admin.ID, OTPSourceInput{
		Name: "Migration source", Provider: model.OTPProviderTwilio,
		Identity:       "synthetic-account:+15550100123",
		ProviderConfig: map[string]string{"auth_token": providerSecret},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyEmail, err := box.Encrypt([]byte(legacyEmailPlain))
	if err != nil {
		t.Fatal(err)
	}
	legacyPassword, err := box.Encrypt([]byte(legacyPasswordPlain))
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(database.now())
	result, err := database.db.ExecContext(ctx, `
		INSERT INTO profiles(
			user_id, name, default_vehicle, otp_source_id,
			yodel_email_ciphertext, yodel_password_ciphertext,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, admin.ID, "Legacy profile", "Example Vehicle", source.ID,
		legacyEmail, legacyPassword, true, "", "", 15_000, true, now, now)
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = database.db.ExecContext(ctx, `
		INSERT INTO booking_requests(user_id, name, profile_id, enabled, schedule_enabled,
			target_date, timezone, release_time, prep_minutes_before, auth_deadline_minutes_before,
			poll_deadline_seconds, poll_min_seconds, poll_max_seconds, confirmation_mode,
			login_probe_url, all_day_pass_url, check_all_day, created_at, updated_at)
		VALUES (?, 'Linked booking', ?, 1, 1, '2030-01-15', 'UTC', '07:00', 30, 5,
			120, 1, 2, 'manual', 'https://example.test/login', 'https://example.test/all', 1, ?, ?)
	`, admin.ID, profileID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	bookingID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	booking := model.BookingRequest{ID: bookingID}

	// Insert the historical row using its historical schema. Current enqueue
	// code intentionally requires columns added by later migrations.
	result, err = database.db.ExecContext(ctx, `
		INSERT INTO jobs(user_id, booking_request_id, profile_id, otp_source_id,
			command, run_mode, status, due_at, dedup_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'book', 'manual', 'cancelled', ?, 'migration-linked-job', ?, ?)
	`, admin.ID, booking.ID, profileID, source.ID,
		formatTime(time.Date(2030, 1, 14, 7, 0, 0, 0, time.UTC)), now, now)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	job := model.Job{ID: jobID}

	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	version, err := database.SchemaVersion(ctx)
	if err != nil || version != 6 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var phoneColumns, legacyCredentialColumns int
	if err := database.db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE name = 'yodel_phone_ciphertext'),
			count(*) FILTER (WHERE name IN ('yodel_email_ciphertext', 'yodel_password_ciphertext'))
		FROM pragma_table_info('profiles')
	`).Scan(&phoneColumns, &legacyCredentialColumns); err != nil {
		t.Fatal(err)
	}
	if phoneColumns != 1 || legacyCredentialColumns != 0 {
		t.Fatalf("profile credential columns: phone=%d legacy=%d", phoneColumns, legacyCredentialColumns)
	}
	var foreignKeysEnabled int
	if err := database.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeysEnabled); err != nil {
		t.Fatal(err)
	}
	if foreignKeysEnabled != 1 {
		t.Fatal("foreign-key enforcement remained disabled after migration")
	}

	profile, err := database.GetProfile(ctx, admin.ID, profileID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Enabled {
		t.Fatal("legacy profile remained enabled after its credentials were discarded")
	}
	var storedPhone string
	if err := database.db.QueryRowContext(ctx,
		"SELECT yodel_phone_ciphertext FROM profiles WHERE id = ?", profileID,
	).Scan(&storedPhone); err != nil {
		t.Fatal(err)
	}
	if storedPhone != "" {
		t.Fatal("legacy email ciphertext was copied into the Yodel mobile-number field")
	}

	preservedBooking, err := database.GetBookingRequest(ctx, admin.ID, booking.ID)
	if err != nil || preservedBooking.ProfileID != profileID {
		t.Fatalf("preserved booking=%+v err=%v", preservedBooking, err)
	}
	preservedJob, err := database.GetJob(ctx, admin.ID, job.ID)
	if err != nil || preservedJob.RunImmediately || preservedJob.ProfileID != profileID || preservedJob.BookingRequestID == nil || *preservedJob.BookingRequestID != booking.ID {
		t.Fatalf("preserved job=%+v err=%v", preservedJob, err)
	}
	event, err := database.SystemAppendJobEvent(ctx, JobEventInput{
		JobID: job.ID, Kind: "migration.checked",
		Message: "provider credential " + providerSecret + " remained encrypted",
		Data:    map[string]any{"provider_credential": providerSecret, "migration": "complete"},
	})
	if err != nil {
		t.Fatalf("append event for migrated legacy job: %v", err)
	}
	serializedEvent := event.Message + event.DataJSON
	for _, secret := range []string{providerSecret, legacyEmailPlain, legacyPasswordPlain} {
		if strings.Contains(serializedEvent, secret) {
			t.Fatalf("migrated job event exposed secret %q: %s", secret, serializedEvent)
		}
	}
	foreignKeyRows, err := database.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if foreignKeyRows.Next() {
		foreignKeyRows.Close()
		t.Fatal("migration left a foreign-key violation")
	}
	if err := foreignKeyRows.Err(); err != nil {
		foreignKeyRows.Close()
		t.Fatal(err)
	}
	if err := foreignKeyRows.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := database.GetProfileCredentials(ctx, admin.ID, profileID); !errors.Is(err, ErrYodelPhoneRequired) {
		t.Fatalf("credential read error=%v", err)
	}
	profile.Enabled = true
	update := ProfileInput{
		Name: profile.Name, DefaultVehicle: profile.DefaultVehicle, OTPSourceID: profile.OTPSourceID,
		LoginProbeURL: profile.LoginProbeURL,
		Headless:      profile.Headless, BrowserChannel: profile.BrowserChannel,
		BrowserExecutable: profile.BrowserExecutable, DefaultTimeoutMS: profile.DefaultTimeoutMS,
		Enabled: true,
	}
	if _, err := database.UpdateProfile(ctx, admin.ID, profile.ID, update); !errors.Is(err, ErrYodelPhoneRequired) {
		t.Fatalf("enable without re-entering phone error=%v", err)
	}
	stillDisabled, err := database.GetProfile(ctx, admin.ID, profileID)
	if err != nil || stillDisabled.Enabled {
		t.Fatalf("profile after rejected enable=%+v err=%v", stillDisabled, err)
	}

	update.Credentials = &model.ProfileCredentials{Phone: "5559876543"}
	updated, err := database.UpdateProfile(ctx, admin.ID, profile.ID, update)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled {
		t.Fatal("profile was not enabled after supplying a mobile number")
	}
	credentials, err := database.GetProfileCredentials(ctx, admin.ID, profileID)
	if err != nil || credentials.Phone != "5559876543" {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
}

func TestNormalizeYodelPhone(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain domestic", input: "5559876543", want: "5559876543"},
		{name: "common formatting", input: " (555) 987-6543 ", want: "5559876543"},
		{name: "dot formatting", input: "555.987.6543", want: "5559876543"},
		{name: "plus one", input: "+1 (555) 987-6543", want: "5559876543"},
		{name: "one without plus", input: "1-555-987-6543", want: "5559876543"},
		{name: "empty", input: "  ", wantErr: true},
		{name: "too short", input: "555010012", wantErr: true},
		{name: "too long", input: "55598765430", wantErr: true},
		{name: "plus without country code", input: "+5559876543", wantErr: true},
		{name: "wrong country code", input: "+2 555 010 0123", wantErr: true},
		{name: "letters", input: "555010O123", wantErr: true},
		{name: "control character", input: "555010\n0123", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeYodelPhone(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("normalizeYodelPhone(%q)=%q, want error", test.input, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("normalizeYodelPhone(%q)=%q, err=%v, want %q", test.input, got, err, test.want)
			}
		})
	}
}
