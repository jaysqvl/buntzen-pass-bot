package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/scheduler"
)

func TestPersonalSettingsOwnershipResetAndCascade(t *testing.T) {
	ctx := context.Background()
	database, firstID, secondID := ownershipStore(t)
	first, second := database.ForUser(firstID), database.ForUser(secondID)
	lake, _ := destinations.Resolve("")
	if _, err := first.GetLakeSettings(ctx, lake.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing personal defaults: %v", err)
	}
	defaults := model.DefaultLakeSettings(lake)
	defaults.UserID = secondID // Binding, not submitted ownership, wins.
	defaults.ReleaseDaysBefore = 0
	defaults.PreferredPasses = []model.PassType{model.PassAfternoon, model.PassAllDay}
	saved, err := first.SaveLakeSettings(ctx, defaults)
	if err != nil || saved.UserID != firstID || saved.ReleaseDaysBefore != 0 || saved.UpdatedAt.IsZero() ||
		!reflect.DeepEqual(saved.PreferredPasses, defaults.PreferredPasses) {
		t.Fatalf("save lake settings: %+v, %v", saved, err)
	}
	if _, err := second.GetLakeSettings(ctx, lake.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account saw saved settings: %v", err)
	}
	if err := second.ResetLakeSettings(ctx, lake.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := first.GetLakeSettings(ctx, lake.ID); err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("another account reset settings: %+v, %v", got, err)
	}
	defaults.ReleaseTime = "08:15"
	if _, err := second.SaveLakeSettings(ctx, defaults); err != nil {
		t.Fatal(err)
	}
	if err := first.ResetLakeSettings(ctx, lake.ID); err != nil {
		t.Fatal(err)
	}
	if err := first.ResetLakeSettings(ctx, lake.ID); err != nil {
		t.Fatalf("reset is not idempotent: %v", err)
	}
	if _, err := first.GetLakeSettings(ctx, lake.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset retained lake override: %v", err)
	}
	if got, err := second.GetLakeSettings(ctx, lake.ID); err != nil || got.ReleaseTime != "08:15" {
		t.Fatalf("reset changed another account: %+v, %v", got, err)
	}

	account, err := first.GetAccountSettings(ctx)
	if err != nil || !account.Headless || account.DefaultTimeoutMS != 15000 || account.UserID != firstID || !account.UpdatedAt.IsZero() {
		t.Fatalf("absent account defaults: %+v, %v", account, err)
	}
	account.UserID, account.Headless, account.BrowserChannel, account.DefaultTimeoutMS = secondID, false, " Chrome ", 20000
	account, err = first.SaveAccountSettings(ctx, account)
	if err != nil || account.UserID != firstID || account.Headless || account.BrowserChannel != "chrome" || account.UpdatedAt.IsZero() {
		t.Fatalf("save account settings: %+v, %v", account, err)
	}
	other, err := second.GetAccountSettings(ctx)
	if err != nil || !other.Headless || other.BrowserChannel != "" || other.DefaultTimeoutMS != 15000 {
		t.Fatalf("another account inherited defaults: %+v, %v", other, err)
	}
	if _, err := second.SaveAccountSettings(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE users SET status = 'disabled' WHERE id = ?", secondID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", secondID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"account_settings", "lake_settings"} {
		var remaining int
		if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE user_id = ?", secondID).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("%s did not cascade: count=%d err=%v", table, remaining, err)
		}
	}
}

func TestSettingsDoNotChangeSavedBookingsProfilesOrJobs(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	resources := database.ForUser(testUserID)
	profile, booking := fixtureProfileAndBooking(t, database, "settings-snapshot")
	if booking.ReleaseDaysBefore == nil || booking.EffectiveReleaseDaysBefore() != 1 || profile.LakeID != destinations.DefaultLakeID {
		t.Fatalf("legacy values were not snapshotted: booking=%+v profile=%+v", booking, profile)
	}
	window, err := scheduler.WindowFor(booking)
	if err != nil {
		t.Fatal(err)
	}
	job, err := resources.EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual,
		DueAt: window.PrepAt, ExpiresAt: &window.PollEndsAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	lake, _ := destinations.Resolve(booking.LakeID)
	settings := model.DefaultLakeSettings(lake)
	settings.ReleaseDaysBefore, settings.ReleaseTime = 0, "09:15"
	settings, err = resources.SaveLakeSettings(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	account := model.DefaultAccountSettings()
	account.Headless, account.DefaultTimeoutMS = !profile.Headless, 30000
	if _, err := resources.SaveAccountSettings(ctx, account); err != nil {
		t.Fatal(err)
	}
	gotBooking, err := resources.GetBookingRequest(ctx, booking.ID)
	if err != nil || !reflect.DeepEqual(gotBooking, booking) {
		t.Fatalf("changing defaults changed existing booking: %+v, %v", gotBooking, err)
	}
	gotWindow, err := scheduler.WindowFor(gotBooking)
	if err != nil || !reflect.DeepEqual(gotWindow, window) {
		t.Fatalf("changing defaults changed schedule: %+v, %v", gotWindow, err)
	}
	gotJob, err := resources.GetJob(ctx, job.ID)
	if err != nil || !reflect.DeepEqual(gotJob, job) {
		t.Fatalf("changing defaults changed queued job: %+v, %v", gotJob, err)
	}
	gotProfile, err := resources.GetProfile(ctx, profile.ID)
	if err != nil || !reflect.DeepEqual(gotProfile, profile) {
		t.Fatalf("changing account defaults changed profile: %+v, %v", gotProfile, err)
	}
	updated := settings.ApplyTo(booking)
	if _, err := resources.UpdateBookingRequest(ctx, updated); !errors.Is(err, ErrConflict) {
		t.Fatalf("active job guard was bypassed: %v", err)
	}
	updated.ID, updated.Name = 0, "new visit with lake defaults"
	created, err := resources.CreateBookingRequest(ctx, updated)
	if err != nil || created.ReleaseDaysBefore == nil || created.EffectiveReleaseDaysBefore() != 0 || created.ReleaseTime != "09:15" {
		t.Fatalf("new booking did not snapshot explicit same-day release: %+v, %v", created, err)
	}
	settings.ReleaseDaysBefore = 3
	if saved, err := resources.SaveLakeSettings(ctx, settings); err != nil || saved.ReleaseDaysBefore != 3 {
		t.Fatalf("updating defaults failed: %+v, %v", saved, err)
	}
	if savedBooking, err := resources.GetBookingRequest(ctx, created.ID); err != nil || !reflect.DeepEqual(savedBooking, created) {
		t.Fatalf("updating defaults changed the booking snapshot: %+v, %v", savedBooking, err)
	}
	if err := resources.ResetLakeSettings(ctx, lake.ID); err != nil {
		t.Fatal(err)
	}
	createdAgain, err := resources.GetBookingRequest(ctx, created.ID)
	if err != nil || !reflect.DeepEqual(createdAgain, created) {
		t.Fatalf("reset changed existing booking: %+v, %v", createdAgain, err)
	}
}

func TestLakeSettingsInvalidInputsAndUserScope(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	invalid := database.ForUser(0)
	lake, _ := destinations.Resolve("")
	settings := model.DefaultLakeSettings(lake)
	for name, operation := range map[string]func() error{
		"get lake":     func() error { _, err := invalid.GetLakeSettings(ctx, lake.ID); return err },
		"save lake":    func() error { _, err := invalid.SaveLakeSettings(ctx, settings); return err },
		"reset lake":   func() error { return invalid.ResetLakeSettings(ctx, lake.ID) },
		"get account":  func() error { _, err := invalid.GetAccountSettings(ctx); return err },
		"save account": func() error { _, err := invalid.SaveAccountSettings(ctx, model.DefaultAccountSettings()); return err },
	} {
		if err := operation(); !errors.Is(err, ErrUserRequired) {
			t.Fatalf("%s did not fail closed: %v", name, err)
		}
	}
	settings.ReleaseDaysBefore = -1
	if _, err := database.ForUser(testUserID).SaveLakeSettings(ctx, settings); err == nil {
		t.Fatal("negative release offset was persisted")
	}
	if err := database.ForUser(testUserID).ResetLakeSettings(ctx, "unknown"); err == nil {
		t.Fatal("unknown lake accepted")
	}
}

func TestProfileLakeIsImmutableAndBookingAssociationIsEnforced(t *testing.T) {
	ctx := context.Background()
	database, firstID, secondID := ownershipStore(t)
	_, profile, booking := createOwnedResources(t, database, firstID, "lake-owner")
	_, otherProfile, _ := createOwnedResources(t, database, secondID, "lake-other")
	if _, err := database.db.ExecContext(ctx, `UPDATE profiles SET lake_id = 'future-lake' WHERE id = ?`, profile.ID); err == nil {
		t.Fatal("raw profile write changed its lake")
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE booking_requests SET lake_id = 'future-lake' WHERE id = ?`, booking.ID); err == nil {
		t.Fatal("raw booking write allowed a profile from another lake")
	}
	booking.ProfileID = otherProfile.ID
	if _, err := database.ForUser(firstID).UpdateBookingRequest(ctx, booking); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-owner profile update was accepted: %v", err)
	}
	booking.ID, booking.Name = 0, "cross-owner profile"
	if _, err := database.ForUser(firstID).CreateBookingRequest(ctx, booking); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-owner profile creation was accepted: %v", err)
	}
}
