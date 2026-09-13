package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/scheduler"
)

func TestGlobalTimingMigrationPreservesPersonalDefaultsAndExecutionRecords(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "v8.db"), testEncryptor(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{
		"0001_initial.sql", "0002_yodel_phone_login.sql", "0003_booking_reservations.sql",
		"0004_immediate_bookings.sql", "0005_profile_login_url.sql", "0006_pass_order.sql",
		"0007_booking_lake.sql", "0008_personal_lake_settings.sql",
	} {
		script, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.applyMigration(ctx, index+1, name, script); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := database.SetupAdmin(ctx, "timing-migration", "synthetic timing migration password")
	if err != nil {
		t.Fatal(err)
	}
	if admin.ID != testUserID {
		t.Fatalf("fixture administrator id=%d", admin.ID)
	}
	profile, booking, job := legacySettingsExecutionFixture(t, database)
	credentials, err := database.GetProfileCredentials(ctx, admin.ID, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	window, err := scheduler.WindowFor(booking)
	if err != nil {
		t.Fatal(err)
	}
	const oldSavedAt = "2025-01-01T12:00:00Z"
	type migrationCase struct {
		username       string
		userID         int64
		browserChannel string
		headless       bool
		timeout        int
		lakeIDs        []string
		prep           int
		auth           int
		pollWindow     int
		pollMin        float64
		pollMax        float64
		hasBrowser     bool
	}
	cases := []migrationCase{
		{username: admin.Username, userID: admin.ID, hasBrowser: true, browserChannel: "chrome-beta", timeout: 45000,
			lakeIDs: []string{"aaa-extra", "buntzen"}, prep: 42, auth: 7, pollWindow: 180, pollMin: 2.25, pollMax: 4.5},
		{username: "timing-only", headless: true, timeout: 15000,
			lakeIDs: []string{"buntzen"}, prep: 0, auth: 0, pollWindow: 300, pollMin: .5, pollMax: 1.5},
		{username: "browser-only", hasBrowser: true, headless: true, browserChannel: "chrome", timeout: 20000,
			prep: 30, auth: 5, pollWindow: 120, pollMin: 1.4, pollMax: 3.6},
		// Extra stored IDs are a database fixture, not advertised lake support.
		{username: "stable-fallback", headless: true, timeout: 15000,
			lakeIDs: []string{"zeta-extra", "alpha-extra"}, prep: 55, auth: 15, pollWindow: 250, pollMin: 3, pollMax: 5},
	}
	for i := range cases {
		c := &cases[i]
		if c.userID == 0 {
			member, err := database.CreateMember(ctx, CreateUserInput{Username: c.username, Password: "synthetic member timing password"})
			if err != nil {
				t.Fatal(err)
			}
			c.userID = member.ID
		}
		if c.hasBrowser {
			if _, err := database.db.ExecContext(ctx, `
				INSERT INTO account_settings(user_id, headless, browser_channel, default_timeout_ms, updated_at)
				VALUES (?, ?, ?, ?, ?)
			`, c.userID, c.headless, c.browserChannel, c.timeout, oldSavedAt); err != nil {
				t.Fatal(err)
			}
		}
		for j, lakeID := range c.lakeIDs {
			prep := c.prep
			if j != len(c.lakeIDs)-1 {
				prep = 100 // Must not win over Buntzen or the stable fallback ID.
			}
			if _, err := database.db.ExecContext(ctx, `
				INSERT INTO lake_settings(user_id, lake_id, timezone, release_time, release_days_before,
					all_day_pass_url, half_day_pass_url, pass_order, prep_minutes_before,
					auth_deadline_minutes_before, poll_deadline_seconds, poll_min_seconds, poll_max_seconds, updated_at)
				VALUES (?, ?, 'America/Vancouver', '08:20', 3,
					'https://yodelportal.com/custom/all', 'https://yodelportal.com/custom/half', 'afternoon,all_day',
					?, ?, ?, ?, ?, ?)
			`, c.userID, lakeID, prep, c.auth, c.pollWindow, c.pollMin, c.pollMax, oldSavedAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 2 {
		if err := database.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		saved, err := database.ForUser(c.userID).GetAccountSettings(ctx)
		if err != nil || saved.UserID != c.userID || saved.Headless != c.headless || saved.BrowserChannel != c.browserChannel ||
			saved.DefaultTimeoutMS != c.timeout || saved.PrepMinutesBefore != c.prep || saved.AuthDeadlineMinutesBefore != c.auth ||
			saved.PollDeadlineSeconds != c.pollWindow || saved.PollMinSeconds != c.pollMin || saved.PollMaxSeconds != c.pollMax ||
			saved.UpdatedAt.Format(time.RFC3339) != oldSavedAt {
			t.Fatalf("%s migrated settings=%+v err=%v", c.username, saved, err)
		}
		for _, lakeID := range c.lakeIDs {
			var timezone, release, allDay, halfDay, order, updated string
			var days int
			if err := database.db.QueryRowContext(ctx, `
				SELECT timezone, release_time, release_days_before, all_day_pass_url, half_day_pass_url, pass_order, updated_at
				FROM lake_settings WHERE user_id = ? AND lake_id = ?
			`, c.userID, lakeID).Scan(&timezone, &release, &days, &allDay, &halfDay, &order, &updated); err != nil ||
				timezone != "America/Vancouver" || release != "08:20" || days != 3 || allDay != "https://yodelportal.com/custom/all" ||
				halfDay != "https://yodelportal.com/custom/half" || order != "afternoon,all_day" || updated != oldSavedAt {
				t.Fatalf("lake defaults changed for %s/%s: %v", c.username, lakeID, err)
			}
		}
	}
	lakeDefaults, err := database.ForUser(admin.ID).GetLakeSettings(ctx, "buntzen")
	if err != nil || lakeDefaults.VehicleKeyword != profile.DefaultVehicle {
		t.Fatalf("unambiguous legacy vehicle was not retained: %+v %v", lakeDefaults, err)
	}
	var remainingTimingColumns int
	if err := database.db.QueryRowContext(ctx, `
		SELECT count(*) FROM pragma_table_info('lake_settings')
		WHERE name IN ('prep_minutes_before', 'auth_deadline_minutes_before', 'poll_deadline_seconds', 'poll_min_seconds', 'poll_max_seconds')
	`).Scan(&remainingTimingColumns); err != nil || remainingTimingColumns != 0 {
		t.Fatalf("lake table still owns timing: count=%d err=%v", remainingTimingColumns, err)
	}
	gotBooking, err := database.ForUser(admin.ID).GetBookingRequest(ctx, booking.ID)
	if err != nil || !reflect.DeepEqual(gotBooking, booking) {
		t.Fatalf("migration changed booking snapshot: %+v, %v", gotBooking, err)
	}
	gotWindow, err := scheduler.WindowFor(gotBooking)
	if err != nil || !reflect.DeepEqual(gotWindow, window) {
		t.Fatalf("migration changed release window: %+v, %v", gotWindow, err)
	}
	gotProfile, err := database.ForUser(admin.ID).GetProfile(ctx, profile.ID)
	if err != nil || !reflect.DeepEqual(gotProfile, profile) {
		t.Fatalf("migration changed profile: %+v, %v", gotProfile, err)
	}
	gotCredentials, err := database.ForUser(admin.ID).GetProfileCredentials(ctx, profile.ID)
	if err != nil || !reflect.DeepEqual(gotCredentials, credentials) {
		t.Fatalf("migration changed credentials: %v", err)
	}
	gotJob, err := database.ForUser(admin.ID).GetJob(ctx, job.ID)
	if err != nil || !reflect.DeepEqual(gotJob, job) {
		t.Fatalf("migration changed queued job: %+v, %v", gotJob, err)
	}
	conflict, err := database.ForUser(admin.ID).BookingConflict(ctx, booking.ID, model.CommandBook)
	if err != nil || !conflict.Reservation || conflict.Job == nil || conflict.Job.ID != job.ID {
		t.Fatalf("migration lost reservation: %+v, %v", conflict, err)
	}
}
