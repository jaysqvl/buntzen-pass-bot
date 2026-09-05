package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestProfileLoginMigrationPreservesExistingLoginSelectionAndBookingHistory(t *testing.T) {
	ctx := context.Background()
	box := testEncryptor(t)
	database, err := Open(ctx, filepath.Join(t.TempDir(), "v4.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"0001_initial.sql", "0002_yodel_phone_login.sql", "0003_booking_reservations.sql", "0004_immediate_bookings.sql"} {
		migration, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.db.ExecContext(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
		if _, err := database.db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (?, ?, ?)`, index+1, name, formatTime(database.now())); err != nil {
			t.Fatal(err)
		}
	}
	user, err := database.SetupAdmin(ctx, "migration-owner", "profile migration test password")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := box.Encrypt([]byte("5559876543"))
	if err != nil {
		t.Fatal(err)
	}
	type oldBooking struct {
		name, url string
		enabled   bool
	}
	cases := []struct {
		name, wantURL string
		bookings      []oldBooking
	}{
		{name: "enabled name ordering", wantURL: "https://example.test/chosen", bookings: []oldBooking{
			{"A disabled", "https://example.test/disabled", false},
			{"Z enabled", "https://example.test/later-name", true},
			{"B enabled", "https://example.test/chosen", true},
		}},
		{name: "disabled fallback", wantURL: "https://example.test/fallback", bookings: []oldBooking{{"Only disabled", "https://example.test/fallback", false}}},
		{name: "no booking requires profile setup"},
		{name: "unapproved URL stays blocked", wantURL: "https://unapproved.example/login", bookings: []oldBooking{{"Unapproved", "https://unapproved.example/login", true}}},
	}
	profileIDs := make([]int64, len(cases))
	legacyURLs := make(map[int64]string)
	for index, test := range cases {
		source, err := database.CreateOTPSource(ctx, user.ID, OTPSourceInput{
			Name: fmt.Sprintf("Inbox %d", index), Provider: model.OTPProviderTwilio, Identity: fmt.Sprintf("twilio:profile-migration-%d", index), ProviderConfig: map[string]string{"auth_token": "synthetic"},
		})
		if err != nil {
			t.Fatal(err)
		}
		now := formatTime(database.now())
		result, err := database.db.ExecContext(ctx, `INSERT INTO profiles(user_id,name,default_vehicle,otp_source_id,yodel_phone_ciphertext,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, user.ID, test.name, "Example Vehicle", source.ID, phone, now, now)
		if err != nil {
			t.Fatal(err)
		}
		profileIDs[index], err = result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		for _, booking := range test.bookings {
			result, err := database.db.ExecContext(ctx, `INSERT INTO booking_requests(user_id,name,profile_id,enabled,target_date,login_probe_url,all_day_pass_url,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, user.ID, booking.name, profileIDs[index], booking.enabled, "2030-01-15", booking.url, "https://example.test/all", now, now)
			if err != nil {
				t.Fatal(err)
			}
			id, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			legacyURLs[id] = booking.url
		}
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for index, test := range cases {
		profile, err := database.GetProfile(ctx, user.ID, profileIDs[index])
		if err != nil || profile.LoginProbeURL != test.wantURL || !profile.Enabled {
			t.Fatalf("%s profile = %+v, %v", test.name, profile, err)
		}
		credentials, err := database.GetProfileCredentials(ctx, user.ID, profile.ID)
		if err != nil || credentials.Phone != "5559876543" {
			t.Fatalf("%s credentials changed: %v", test.name, err)
		}
		if (test.wantURL == "" || test.name == "unapproved URL stays blocked") && profile.ValidateForOrigins([]string{"https://example.test"}) == nil {
			t.Fatalf("%s became runnable without an approved profile login", test.name)
		}
	}
	for id, want := range legacyURLs {
		booking, err := database.GetBookingRequest(ctx, user.ID, id)
		if err != nil || booking.LoginProbeURL != want {
			t.Fatalf("legacy booking URL changed: %+v %v", booking, err)
		}
	}
	profile, err := database.GetProfile(ctx, user.ID, profileIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	profile, err = database.UpdateProfile(ctx, user.ID, profile.ID, ProfileInput{Name: profile.Name, DefaultVehicle: profile.DefaultVehicle, OTPSourceID: profile.OTPSourceID, LoginProbeURL: "https://example.test/edited", Headless: profile.Headless, DefaultTimeoutMS: profile.DefaultTimeoutMS, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := database.SystemGetProfile(ctx, profile.ID)
	if err != nil || persisted.LoginProbeURL != profile.LoginProbeURL {
		t.Fatalf("repeat migration overwrote profile login: %+v %v", persisted, err)
	}
	for _, list := range []func() ([]model.Profile, error){func() ([]model.Profile, error) { return database.ListProfiles(ctx, user.ID) }, func() ([]model.Profile, error) { return database.SystemListProfiles(ctx) }} {
		profiles, err := list()
		if err != nil || len(profiles) != len(cases) {
			t.Fatalf("profile inventory after migration = %+v %v", profiles, err)
		}
		for _, item := range profiles {
			if item.ID == profile.ID && item.LoginProbeURL != profile.LoginProbeURL {
				t.Fatalf("profile list lost edited login URL: %+v", item)
			}
		}
	}
}
