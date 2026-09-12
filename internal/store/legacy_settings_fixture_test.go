package store

import (
	"context"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/scheduler"
)

// Seed v8/v9 through their original columns, since current store methods now
// require provider and vehicle snapshots introduced by migration 10.
func legacySettingsExecutionFixture(t *testing.T, database *Store) (model.Profile, model.BookingRequest, model.Job) {
	t.Helper()
	ctx := context.Background()
	source, err := database.CreateOTPSource(ctx, testUserID, OTPSourceInput{
		Name: "Migration inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:shared-migration",
		ProviderConfig: map[string]string{"auth_token": "synthetic-migration-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	phone, err := database.encryptor.Encrypt([]byte("5559876543"))
	if err != nil {
		t.Fatal(err)
	}
	now := database.now().UTC()
	profile := model.Profile{
		UserID: testUserID, ProviderID: "yodel", LakeID: "buntzen", Name: "Migration identity",
		DefaultVehicle: "Legacy car", LoginProbeURL: "https://yodelportal.com/buntzen-lake",
		OTPSourceID: source.ID, Headless: true, BrowserChannel: "chrome", DefaultTimeoutMS: 20000,
		Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	result, err := database.db.ExecContext(ctx, `
		INSERT INTO profiles(user_id,name,default_vehicle,otp_source_id,yodel_phone_ciphertext,
			headless,browser_channel,default_timeout_ms,enabled,created_at,updated_at,login_probe_url,lake_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, profile.UserID, profile.Name, profile.DefaultVehicle, profile.OTPSourceID, phone, profile.Headless,
		profile.BrowserChannel, profile.DefaultTimeoutMS, profile.Enabled, formatTime(now), formatTime(now), profile.LoginProbeURL, profile.LakeID)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID, err = result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	days := 1
	booking := model.BookingRequest{
		UserID: testUserID, LakeID: "buntzen", ProfileID: profile.ID, Name: "Migration visit",
		VehicleKeyword: profile.DefaultVehicle, Enabled: true, ScheduleEnabled: true,
		TargetDate: "2030-08-12", Timezone: "America/Vancouver", ReleaseTime: "07:00", ReleaseDaysBefore: &days,
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120, PollMinSeconds: 1.4, PollMaxSeconds: 3.6,
		ConfirmationMode: model.RunModeManual, LoginProbeURL: profile.LoginProbeURL,
		AllDayPassURL: "https://yodelportal.com/buntzen-lake/All-Day-Pass", HalfDayPassURL: "https://yodelportal.com/buntzen-lake/Half-Day-Pass",
		PreferredPasses: []model.PassType{model.PassAllDay}, CheckAllDay: true, CreatedAt: now, UpdatedAt: now,
	}
	result, err = database.db.ExecContext(ctx, `
		INSERT INTO booking_requests(user_id,name,profile_id,enabled,schedule_enabled,target_date,timezone,release_time,
			prep_minutes_before,auth_deadline_minutes_before,poll_deadline_seconds,poll_min_seconds,poll_max_seconds,
			confirmation_mode,login_probe_url,all_day_pass_url,half_day_pass_url,check_all_day,check_afternoon,check_morning,
			pass_order,created_at,updated_at,lake_id,release_days_before)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, booking.UserID, booking.Name, booking.ProfileID, booking.Enabled, booking.ScheduleEnabled, booking.TargetDate,
		booking.Timezone, booking.ReleaseTime, booking.PrepMinutesBefore, booking.AuthDeadlineMinutesBefore,
		booking.PollDeadlineSeconds, booking.PollMinSeconds, booking.PollMaxSeconds, booking.ConfirmationMode,
		booking.LoginProbeURL, booking.AllDayPassURL, booking.HalfDayPassURL, true, false, false,
		"all_day", formatTime(now), formatTime(now), booking.LakeID, days)
	if err != nil {
		t.Fatal(err)
	}
	booking.ID, err = result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	window, err := scheduler.WindowFor(booking)
	if err != nil {
		t.Fatal(err)
	}
	result, err = database.db.ExecContext(ctx, `
		INSERT INTO jobs(user_id,booking_request_id,profile_id,otp_source_id,command,run_mode,status,due_at,expires_at,created_at,updated_at)
		VALUES(?,?,?,?,'book','manual','queued',?,?,?,?)
	`, testUserID, booking.ID, profile.ID, source.ID, formatTime(window.PrepAt), formatTime(window.PollEndsAt), formatTime(now), formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.GetJob(ctx, testUserID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return profile, booking, job
}
