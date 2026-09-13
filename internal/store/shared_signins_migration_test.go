package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestSharedSignInMigrationKeepsIdentitiesVehiclesJobsAndIDHighWater(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "v9.db"), testEncryptor(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,name TEXT NOT NULL,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		var version int
		if _, err := fmt.Sscanf(entry.Name(), "%04d_", &version); err != nil {
			t.Fatal(err)
		}
		if version > 9 {
			continue
		}
		script, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := database.applyMigration(ctx, version, entry.Name(), script); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := database.SetupAdmin(ctx, "shared-migration", "synthetic shared identity migration password")
	if err != nil {
		t.Fatal(err)
	}
	if admin.ID != testUserID {
		t.Fatalf("fixture owner ID=%d", admin.ID)
	}
	unused, err := database.CreateOTPSource(ctx, admin.ID, OTPSourceInput{Name: "Earlier unlinked source", Provider: model.OTPProviderTwilio, Identity: "twilio:earlier-unlinked", ProviderConfig: map[string]string{"token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	profile, booking, job := legacySettingsExecutionFixture(t, database)
	credentials, err := database.GetProfileCredentials(ctx, admin.ID, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondSource, err := database.CreateOTPSource(ctx, admin.ID, OTPSourceInput{Name: "Second legacy source", Provider: model.OTPProviderTwilio, Identity: "twilio:legacy-second", ProviderConfig: map[string]string{"token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	// Retain two historical identities even when their encrypted phone is the
	// same. Their separate IDs continue identifying separate browser storage.
	result, err := database.db.ExecContext(ctx, `
		INSERT INTO profiles(user_id,name,default_vehicle,otp_source_id,yodel_phone_ciphertext,
			headless,browser_channel,browser_executable,default_timeout_ms,enabled,created_at,updated_at,login_probe_url,lake_id)
		SELECT user_id,'Second historical identity',default_vehicle,?,yodel_phone_ciphertext,
			headless,browser_channel,browser_executable,default_timeout_ms,enabled,created_at,updated_at,login_probe_url,'previous-lake'
		FROM profiles WHERE id=?`, secondSource.ID, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO profiles(user_id,name,default_vehicle,otp_source_id,yodel_phone_ciphertext,
			headless,browser_channel,browser_executable,default_timeout_ms,enabled,created_at,updated_at,login_probe_url,lake_id)
		SELECT user_id,'Third historical identity','Another legacy car',?,yodel_phone_ciphertext,
			headless,browser_channel,browser_executable,default_timeout_ms,enabled,created_at,updated_at,login_probe_url,lake_id
		FROM profiles WHERE id=?`, unused.ID, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO lake_settings(user_id,lake_id,timezone,release_time,release_days_before,
			all_day_pass_url,half_day_pass_url,pass_order,updated_at)
		VALUES(?,'buntzen','America/Vancouver','07:00',1,?,?,'all_day',?)
	`, admin.ID, booking.AllDayPassURL, booking.HalfDayPassURL, formatTime(profile.UpdatedAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE sqlite_sequence SET seq=500 WHERE name='profiles'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := database.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	profiles, err := database.ForUser(admin.ID).ListProfiles(ctx)
	if err != nil || len(profiles) != 3 {
		t.Fatalf("historical identities merged/lost: %+v %v", profiles, err)
	}
	gotProfile, err := database.ForUser(admin.ID).GetProfile(ctx, profile.ID)
	if err != nil || !reflect.DeepEqual(gotProfile, profile) {
		t.Fatalf("first identity changed: %+v %v", gotProfile, err)
	}
	gotSecond, err := database.ForUser(admin.ID).GetProfile(ctx, secondID)
	if err != nil || gotSecond.OTPSourceID != secondSource.ID || gotSecond.ProviderID != "yodel" || gotSecond.LakeID != "previous-lake" {
		t.Fatalf("second identity changed: %+v %v", gotSecond, err)
	}
	for _, id := range []int64{profile.ID, secondID} {
		got, err := database.ForUser(admin.ID).GetProfileCredentials(ctx, id)
		if err != nil || !reflect.DeepEqual(got, credentials) {
			t.Fatalf("identity credentials changed: %v", err)
		}
	}
	gotBooking, err := database.ForUser(admin.ID).GetBookingRequest(ctx, booking.ID)
	if err != nil || !reflect.DeepEqual(gotBooking, booking) {
		t.Fatalf("booking vehicle/timing snapshot changed: %+v %v", gotBooking, err)
	}
	lakeDefaults, err := database.ForUser(admin.ID).GetLakeSettings(ctx, "buntzen")
	if err != nil || lakeDefaults.VehicleKeyword != "" {
		t.Fatalf("migration guessed an ambiguous lake vehicle: %+v %v", lakeDefaults, err)
	}
	gotJob, err := database.ForUser(admin.ID).GetJob(ctx, job.ID)
	if err != nil || !reflect.DeepEqual(gotJob, job) {
		t.Fatalf("queued source/job changed: %+v %v", gotJob, err)
	}
	defaultSource, err := database.ForUser(admin.ID).GetDefaultOTPSource(ctx)
	if err != nil || defaultSource.ID != profile.OTPSourceID || defaultSource.ID == unused.ID {
		t.Fatalf("migration did not prefer first identity source: %+v %v", defaultSource, err)
	}
	if _, err := database.ForUser(admin.ID).EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual}); !errors.Is(err, ErrConflict) {
		t.Fatalf("profile/date reservation lost: %v", err)
	}
	created, err := database.ForUser(admin.ID).CreateProfile(ctx, ProfileInput{Name: "New shared identity", LoginProbeURL: profile.LoginProbeURL, DefaultTimeoutMS: 15000, Enabled: true, Credentials: &model.ProfileCredentials{Phone: "5559876545"}})
	if err != nil || created.ID <= 500 || created.DefaultVehicle != "" || created.OTPSourceID != profile.OTPSourceID {
		t.Fatalf("new identity reused history or could not share source: %+v %v", created, err)
	}
}
