package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestBookingPassPriorityRoundTripsAndSynchronizesLegacyFlags(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, legacy := fixtureProfileAndBooking(t, database, "pass priority")
	if !slices.Equal(legacy.PreferredPasses, []model.PassType{model.PassAllDay}) {
		t.Fatalf("legacy input was not normalized: %v", legacy.PreferredPasses)
	}
	input := legacy
	input.ID = 0
	input.Name = "Morning only"
	input.LoginProbeURL = ""
	input.HalfDayPassURL = "https://example.test/half"
	input.PreferredPasses = []model.PassType{model.PassMorning}
	created, err := database.CreateBookingRequest(ctx, testUserID, input)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created.PassOrder(), input.PreferredPasses) || created.CheckAllDay || created.CheckAfternoon || !created.CheckMorning {
		t.Fatalf("created order and legacy flags disagree: %+v", created)
	}
	if created.LoginProbeURL != profile.LoginProbeURL {
		t.Fatal("omitted legacy login URL was not populated from the owned profile")
	}
	created.PreferredPasses = []model.PassType{model.PassMorning, model.PassAllDay, model.PassAfternoon}
	created.LoginProbeURL = ""
	updated, err := database.UpdateBookingRequest(ctx, testUserID, created)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := database.SystemGetBookingRequest(ctx, updated.ID)
	if err != nil || !slices.Equal(loaded.PassOrder(), created.PreferredPasses) || !loaded.CheckAllDay || !loaded.CheckAfternoon || !loaded.CheckMorning {
		t.Fatalf("saved custom order=%+v err=%v", loaded, err)
	}
	if loaded.LoginProbeURL != profile.LoginProbeURL {
		t.Fatal("update cleared the retained historical login URL")
	}
	updated.PreferredPasses = []model.PassType{model.PassMorning, model.PassMorning}
	if _, err := database.UpdateBookingRequest(ctx, testUserID, updated); err == nil {
		t.Fatal("duplicate priority was saved")
	}
	unchanged, err := database.GetBookingRequest(ctx, testUserID, updated.ID)
	if err != nil || !slices.Equal(unchanged.PassOrder(), created.PreferredPasses) {
		t.Fatalf("rejected update changed the order: %+v err=%v", unchanged, err)
	}
}

func TestPassOrderMigrationPreservesEveryLegacySelection(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "v5.db"), testEncryptor(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.db.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"0001_initial.sql", "0002_yodel_phone_login.sql", "0003_booking_reservations.sql", "0004_immediate_bookings.sql", "0005_profile_login_url.sql"} {
		script, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.applyMigration(ctx, index+1, name, script); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := database.SetupAdmin(ctx, "pass-migration", "synthetic migration test password")
	if err != nil {
		t.Fatal(err)
	}
	source, err := database.CreateOTPSource(ctx, admin.ID, OTPSourceInput{
		Name: "Pass migration inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:pass-migration",
		ProviderConfig: map[string]string{"auth_token": "synthetic-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed the v5 schema directly: current profile writes require columns
	// introduced after the migration this fixture is exercising.
	phone, err := database.encryptor.Encrypt([]byte("5559876543"))
	if err != nil {
		t.Fatal(err)
	}
	profileResult, err := database.db.ExecContext(ctx, `
		INSERT INTO profiles(user_id, name, default_vehicle, login_probe_url,
			otp_source_id, yodel_phone_ciphertext, default_timeout_ms, created_at, updated_at)
		VALUES (?, 'Pass migration profile', 'Example Vehicle', 'https://example.test/login', ?, ?, 15000, ?, ?)
	`, admin.ID, source.ID, phone, formatTime(database.now()), formatTime(database.now()))
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := profileResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	wants := make(map[int64][]model.PassType)
	for flags := 1; flags < 8; flags++ {
		legacy := model.BookingRequest{CheckAllDay: flags&1 != 0, CheckAfternoon: flags&2 != 0, CheckMorning: flags&4 != 0}
		result, err := database.db.ExecContext(ctx, `
			INSERT INTO booking_requests(user_id,name,profile_id,target_date,login_probe_url,
				check_all_day,check_afternoon,check_morning,created_at,updated_at)
			VALUES (?,?,?,'2030-01-15','https://example.test/login',?,?,?,?,?)
		`, admin.ID, fmt.Sprintf("Legacy selection %d", flags), profileID,
			legacy.CheckAllDay, legacy.CheckAfternoon, legacy.CheckMorning, formatTime(database.now()), formatTime(database.now()))
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		wants[id] = legacy.PassOrder()
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range wants {
		booking, err := database.GetBookingRequest(ctx, admin.ID, id)
		if err != nil || !slices.Equal(booking.PreferredPasses, want) {
			t.Fatalf("migrated booking %d order=%v, want %v; err=%v", id, booking.PreferredPasses, want, err)
		}
	}
}
