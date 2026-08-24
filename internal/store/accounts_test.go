package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

const (
	testAdminPassword  = "correct horse battery staple"
	testMemberPassword = "member password long enough"
)

func TestSetupAdminIsOneTimeAndCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	if hasUsers, err := database.HasUsers(ctx); err != nil || hasUsers {
		t.Fatalf("empty database hasUsers=%v err=%v", hasUsers, err)
	}

	admin, err := database.SetupAdmin(ctx, "Primary.Owner", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != model.RoleAdmin || admin.Status != model.UserActive || admin.MustChangePassword {
		t.Fatalf("administrator = %+v", admin)
	}
	if hasUsers, err := database.HasUsers(ctx); err != nil || !hasUsers {
		t.Fatalf("configured database hasUsers=%v err=%v", hasUsers, err)
	}
	if _, err := database.SetupAdmin(ctx, "replacement", "short"); !errors.Is(err, ErrSetupComplete) {
		t.Fatalf("second setup error = %v", err)
	}
	authenticated, ok, err := database.AuthenticateUser(ctx, "PRIMARY.owner", testAdminPassword)
	if err != nil || !ok || authenticated.ID != admin.ID {
		t.Fatalf("case-insensitive authentication user=%+v ok=%v err=%v", authenticated, ok, err)
	}
}

func TestMemberCannotPrecedeInitialAdministrator(t *testing.T) {
	database := testStore(t)
	_, err := database.CreateMember(context.Background(), CreateUserInput{
		Username: "member", Password: testMemberPassword,
	})
	if !errors.Is(err, ErrSetupRequired) {
		t.Fatalf("member before setup error = %v", err)
	}
}

func TestConcurrentSetupCreatesExactlyOneAdministrator(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	start := make(chan struct{})
	type setupResult struct {
		user model.User
		err  error
	}
	results := make(chan setupResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, username := range []string{"first", "second"} {
		go func(name string) {
			ready.Done()
			<-start
			user, err := database.SetupAdmin(ctx, name, testAdminPassword)
			results <- setupResult{user: user, err: err}
		}(username)
	}
	ready.Wait()
	close(start)

	created, rejected := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.user.Role == model.RoleAdmin:
			created++
		case errors.Is(result.err, ErrSetupComplete):
			rejected++
		default:
			t.Fatalf("unexpected setup result user=%+v err=%v", result.user, result.err)
		}
	}
	if created != 1 || rejected != 1 {
		t.Fatalf("created=%d rejected=%d", created, rejected)
	}
	users, err := database.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].Role != model.RoleAdmin {
		t.Fatalf("users=%+v err=%v", users, err)
	}
}

func TestMemberLifecycleAndPermanentAdminGuards(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	admin, err := database.SetupAdmin(ctx, "owner", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	member, err := database.CreateMember(ctx, CreateUserInput{
		Username: "CaseSensitive", Password: testMemberPassword, MustChangePassword: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if member.Role != model.RoleMember || !member.MustChangePassword {
		t.Fatalf("member = %+v", member)
	}
	if _, err := database.CreateMember(ctx, CreateUserInput{
		Username: "casesensitive", Password: testMemberPassword,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("normalized duplicate error = %v", err)
	}

	member, err = database.UpdateUser(ctx, member.ID, UserUpdateInput{
		Username: "renamed", Status: model.UserDisabled,
	})
	if err != nil || member.Username != "renamed" || member.Status != model.UserDisabled {
		t.Fatalf("updated member=%+v err=%v", member, err)
	}
	if _, ok, err := database.AuthenticateUser(ctx, "renamed", testMemberPassword); err != nil || ok {
		t.Fatalf("disabled member authenticated ok=%v err=%v", ok, err)
	}

	if _, err := database.UpdateUser(ctx, admin.ID, UserUpdateInput{
		Username: admin.Username, Status: model.UserDisabled,
	}); !errors.Is(err, ErrProtectedAdmin) {
		t.Fatalf("disable administrator error = %v", err)
	}
	for label, statement := range map[string]string{
		"delete":  "DELETE FROM users WHERE id = ?",
		"demote":  "UPDATE users SET role = 'member' WHERE id = ?",
		"disable": "UPDATE users SET status = 'disabled' WHERE id = ?",
	} {
		if _, err := database.db.ExecContext(ctx, statement, admin.ID); err == nil || !strings.Contains(err.Error(), "permanent administrator") {
			t.Fatalf("raw %s administrator error = %v", label, err)
		}
	}
}

func TestDeleteMemberRequiresDisabledQuiescentAccountAndReleasesOwnedState(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	admin, err := database.SetupAdmin(ctx, "owner", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	member, err := database.CreateMember(ctx, CreateUserInput{
		Username: "deletable-member", Password: testMemberPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := database.ForUser(member.ID)
	source, err := resources.CreateOTPSource(ctx, OTPSourceInput{
		Name: "Member Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://member-messages.example:1234",
		ProviderConfig: map[string]string{"password": "synthetic-member-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(ctx, ProfileInput{
		Name: "Member profile", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID,
		Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: "member@example.test", Password: "synthetic-yodel-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := resources.CreateBookingRequest(ctx, model.BookingRequest{
		Name: "Member booking", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2031-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all-day",
		CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bookingID := booking.ID
	job, err := resources.EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandBook, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.SystemClaimNextDueJobAt(ctx, "deletion-test-worker", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobAwaitingApproval, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemAppendJobEvent(ctx, JobEventInput{JobID: job.ID, Kind: "approval.waiting", Message: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resources.RecordJobDecision(ctx, job.ID, model.DecisionCancel); err != nil {
		t.Fatal(err)
	}
	memberSession, err := database.NewSession(ctx, member.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if err := database.DeleteMember(ctx, admin.ID, admin.Username); !errors.Is(err, ErrProtectedAdmin) {
		t.Fatalf("delete administrator error=%v", err)
	}
	if err := database.DeleteMember(ctx, member.ID, member.Username); !errors.Is(err, ErrMemberMustBeDisabled) {
		t.Fatalf("delete active member error=%v", err)
	}
	member, err = database.UpdateUser(ctx, member.ID, UserUpdateInput{
		Username: member.Username, Status: model.UserDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteMember(ctx, member.ID, strings.ToUpper(member.Username)); !errors.Is(err, ErrMemberDeleteConfirmation) {
		t.Fatalf("delete with inexact confirmation error=%v", err)
	}
	if err := database.DeleteMember(ctx, member.ID, member.Username); !errors.Is(err, ErrMemberHasActiveJobs) {
		t.Fatalf("delete with active job error=%v", err)
	}
	if _, err := database.SystemGetOTPSource(ctx, source.ID); err != nil {
		t.Fatalf("rejected deletion changed member data: %v", err)
	}
	if _, err := database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobAwaitingApproval}, model.JobCancelled, JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteMember(ctx, member.ID, member.Username); err != nil {
		t.Fatal(err)
	}

	if _, err := database.GetUser(ctx, member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted member lookup error=%v", err)
	}
	if _, err := database.GetSession(ctx, memberSession.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted member session error=%v", err)
	}
	for _, table := range []string{"sessions", "otp_sources", "profiles", "booking_requests", "jobs", "job_events", "job_decisions"} {
		var count int
		if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE user_id = ?", member.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("deleted member retained %d rows in %s", count, table)
		}
	}
	if _, err := database.ForUser(admin.ID).CreateOTPSource(ctx, OTPSourceInput{
		Name: "Recovered Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://admin-messages.example:1234",
		ProviderConfig: map[string]string{"password": "synthetic-admin-password"},
	}); err != nil {
		t.Fatalf("released BlueBubbles slot could not be reclaimed: %v", err)
	}
}

func TestPasswordChangesAndResetsRevokeOnlyTargetSessions(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	admin, err := database.SetupAdmin(ctx, "owner", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	member, err := database.CreateMember(ctx, CreateUserInput{
		Username: "member", Password: testMemberPassword, MustChangePassword: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	adminSession, err := database.NewSession(ctx, admin.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	memberSession, err := database.NewSession(ctx, member.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := database.ChangeUserPassword(ctx, member.ID, "wrong password", "new member password long enough"); err != nil || ok {
		t.Fatalf("wrong current password ok=%v err=%v", ok, err)
	}
	if ok, err := database.ChangeUserPassword(ctx, member.ID, testMemberPassword, testMemberPassword); !errors.Is(err, ErrPasswordUnchanged) || ok {
		t.Fatalf("unchanged password ok=%v err=%v", ok, err)
	}
	if _, err := database.GetSession(ctx, memberSession.Token); err != nil {
		t.Fatalf("rejected change revoked session: %v", err)
	}
	member, err = database.GetUser(ctx, member.ID)
	if err != nil || !member.MustChangePassword {
		t.Fatalf("unchanged password cleared reset marker: member=%+v err=%v", member, err)
	}
	if ok, err := database.ChangeUserPassword(ctx, member.ID, testMemberPassword, "new member password long enough"); err != nil || !ok {
		t.Fatalf("password change ok=%v err=%v", ok, err)
	}
	if _, err := database.GetSession(ctx, memberSession.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("changed member session error = %v", err)
	}
	if _, err := database.GetSession(ctx, adminSession.Token); err != nil {
		t.Fatalf("member change revoked admin session: %v", err)
	}
	member, err = database.GetUser(ctx, member.ID)
	if err != nil || member.MustChangePassword {
		t.Fatalf("member after change=%+v err=%v", member, err)
	}

	secondSession, err := database.NewSession(ctx, member.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ResetUserPassword(ctx, member.ID, "temporary password long enough", true); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetSession(ctx, secondSession.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset member session error = %v", err)
	}
	member, err = database.GetUser(ctx, member.ID)
	if err != nil || !member.MustChangePassword {
		t.Fatalf("member after reset=%+v err=%v", member, err)
	}
}

func TestHostRecoveryFindsRenamedAdministrator(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	admin, err := database.SetupAdmin(ctx, "original-owner", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	admin, err = database.UpdateUser(ctx, admin.ID, UserUpdateInput{
		Username: "renamed-owner", Status: model.UserActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.NewSession(ctx, admin.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := database.ResetAdministratorPassword(ctx, "recovered administrator password")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != admin.ID || recovered.Username != "renamed-owner" {
		t.Fatalf("recovered administrator = %+v", recovered)
	}
	if _, err := database.GetSession(ctx, session.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("administrator recovery session error = %v", err)
	}
	if _, ok, err := database.AuthenticateUser(ctx, "renamed-owner", "recovered administrator password"); err != nil || !ok {
		t.Fatalf("recovered authentication ok=%v err=%v", ok, err)
	}
}

func TestSessionIssuanceRejectsAPreviouslyVerifiedPasswordHash(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	admin, err := database.SetupAdmin(ctx, "owner", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, staleHash, err := getUserWith(ctx, database.db, "id = ?", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ResetAdministratorPassword(ctx, "recovered administrator password"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.newSession(ctx, admin.ID, time.Hour, &staleHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale verified hash created a session: %v", err)
	}
	if _, _, ok, err := database.AuthenticateAndCreateSession(
		ctx, admin.Username, testAdminPassword, time.Hour,
	); err != nil || ok {
		t.Fatalf("old password session ok=%v err=%v", ok, err)
	}
	_, credentials, ok, err := database.AuthenticateAndCreateSession(
		ctx, admin.Username, "recovered administrator password", time.Hour,
	)
	if err != nil || !ok || credentials.Token == "" {
		t.Fatalf("recovered password session ok=%v credentials=%+v err=%v", ok, credentials, err)
	}
}

func TestDisablingMemberRevokesScheduledQueuedAndActiveWork(t *testing.T) {
	ctx := context.Background()
	database, _, memberID := ownershipStore(t)
	_, _, booking := createOwnedResources(t, database, memberID, "disabled-member")
	booking.ScheduleEnabled = true
	booking, err := database.ForUser(memberID).UpdateBookingRequest(ctx, booking)
	if err != nil {
		t.Fatal(err)
	}
	bookingID := booking.ID
	queued, err := database.ForUser(memberID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandDryRun, RunMode: model.RunModeDryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := database.ForUser(memberID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandBook, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err = database.SystemTransitionJob(ctx, active.ID,
		[]model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	active, err = database.SystemTransitionJob(ctx, active.ID,
		[]model.JobStatus{model.JobRunning}, model.JobAwaitingApproval, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	member, err := database.GetUser(ctx, memberID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateUser(ctx, memberID, UserUpdateInput{
		Username: member.Username, Status: model.UserDisabled,
	}); err != nil {
		t.Fatal(err)
	}

	queued, err = database.SystemGetJob(ctx, queued.ID)
	if err != nil || queued.Status != model.JobCancelled || !queued.CancelRequested || queued.FinishedAt == nil {
		t.Fatalf("queued job after disable=%+v err=%v", queued, err)
	}
	active, err = database.SystemGetJob(ctx, active.ID)
	if err != nil || active.Status != model.JobAwaitingApproval || !active.CancelRequested {
		t.Fatalf("active job after disable=%+v err=%v", active, err)
	}
	if _, err := database.SystemTransitionJob(ctx, active.ID,
		[]model.JobStatus{model.JobAwaitingApproval}, model.JobRunning, JobTransition{}); !errors.Is(err, ErrTransitionConflict) {
		t.Fatalf("disabled approval transition error=%v", err)
	}
	if err := database.SystemMarkConfirmationStarted(ctx, active.ID); !errors.Is(err, ErrTransitionConflict) {
		t.Fatalf("disabled final-confirmation marker error=%v", err)
	}
	if _, err := database.ForUser(memberID).RecordJobDecision(ctx, active.ID, model.DecisionApprove); !errors.Is(err, ErrTransitionConflict) {
		t.Fatalf("disabled approval decision error=%v", err)
	}
	if _, err := database.ForUser(memberID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandDryRun, RunMode: model.RunModeDryRun,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled enqueue error=%v", err)
	}

	// Defense in depth: even host-side SQL cannot make a disabled account's
	// schedule or queued job executable without re-enabling the account.
	if _, err := database.db.ExecContext(ctx,
		"UPDATE booking_requests SET schedule_enabled = 1 WHERE id = ?", bookingID); err != nil {
		t.Fatal(err)
	}
	if scheduled, err := database.SystemListScheduledBookingRequests(ctx); err != nil || len(scheduled) != 0 {
		t.Fatalf("disabled scheduled requests=%+v err=%v", scheduled, err)
	}
	if _, err := database.db.ExecContext(ctx,
		"UPDATE jobs SET status = 'queued', cancel_requested = 0, finished_at = NULL WHERE id = ?", queued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemClaimNextDueJobAt(ctx, "disabled-test-worker", time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled claim error=%v", err)
	}
	if requested, err := database.SystemJobCancellationRequested(ctx, active.ID); err != nil || !requested {
		t.Fatalf("disabled active cancellation requested=%v err=%v", requested, err)
	}
}

func TestTerminalTransitionsAtomicallyRespectCommittedRevocation(t *testing.T) {
	ctx := context.Background()
	database, adminID, memberID := ownershipStore(t)
	start := func(userID int64, unique string, command model.JobCommand, mode model.RunMode) model.Job {
		t.Helper()
		resources := database.ForUser(userID)
		source, err := resources.CreateOTPSource(ctx, OTPSourceInput{
			Name: unique + " source", Provider: model.OTPProviderTwilio,
			Identity: "twilio:" + unique, ProviderConfig: map[string]string{"auth_token": unique + "-secret"},
		})
		if err != nil {
			t.Fatal(err)
		}
		profile, err := resources.CreateProfile(ctx, ProfileInput{
			Name: unique + " profile", DefaultVehicle: "Example Vehicle",
			OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
			Credentials: &model.ProfileCredentials{Email: unique + "@example.test", Password: unique + "-password"},
		})
		if err != nil {
			t.Fatal(err)
		}
		booking, err := resources.CreateBookingRequest(ctx, model.BookingRequest{
			Name: unique + " booking", ProfileID: profile.ID, Enabled: true,
			TargetDate: "2031-01-15", Timezone: "UTC", ReleaseTime: "07:00",
			PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
			PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
			LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all-day",
			CheckAllDay: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		bookingID := booking.ID
		job, err := resources.EnqueueJob(ctx, EnqueueJobParams{
			BookingRequestID: &bookingID, Command: command, RunMode: mode,
		})
		if err != nil {
			t.Fatal(err)
		}
		job, err = database.SystemTransitionJob(ctx, job.ID,
			[]model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{})
		if err != nil {
			t.Fatal(err)
		}
		return job
	}

	zero := 0
	cancelled := start(adminID, "late-cancel", model.CommandDryRun, model.RunModeDryRun)
	if err := database.ForUser(adminID).RequestJobCancellation(ctx, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := database.SystemTransitionJob(ctx, cancelled.ID,
		[]model.JobStatus{model.JobRunning}, model.JobSucceeded,
		JobTransition{Message: "worker reported success", ExitCode: &zero})
	if err != nil || cancelled.Status != model.JobCancelled || cancelled.ExitCode != nil {
		t.Fatalf("late cancelled result=%+v err=%v", cancelled, err)
	}

	disabled := start(memberID, "late-disable", model.CommandDryRun, model.RunModeDryRun)
	confirmedSuccess := start(memberID, "confirmed-success", model.CommandBook, model.RunModeAuto)
	confirmedFailure := start(memberID, "confirmed-failure", model.CommandBook, model.RunModeAuto)
	if err := database.SystemMarkConfirmationStarted(ctx, confirmedSuccess.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SystemMarkConfirmationStarted(ctx, confirmedFailure.ID); err != nil {
		t.Fatal(err)
	}
	member, err := database.GetUser(ctx, memberID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateUser(ctx, memberID, UserUpdateInput{
		Username: member.Username, Status: model.UserDisabled,
	}); err != nil {
		t.Fatal(err)
	}

	disabled, err = database.SystemTransitionJob(ctx, disabled.ID,
		[]model.JobStatus{model.JobRunning}, model.JobSucceeded,
		JobTransition{Message: "worker reported success", ExitCode: &zero})
	if err != nil || disabled.Status != model.JobCancelled || disabled.ExitCode != nil {
		t.Fatalf("late disabled result=%+v err=%v", disabled, err)
	}
	confirmedSuccess, err = database.SystemTransitionJob(ctx, confirmedSuccess.ID,
		[]model.JobStatus{model.JobRunning}, model.JobSucceeded,
		JobTransition{Message: "booking confirmed", ExitCode: &zero})
	if err != nil || confirmedSuccess.Status != model.JobSucceeded || confirmedSuccess.ExitCode == nil {
		t.Fatalf("confirmed success result=%+v err=%v", confirmedSuccess, err)
	}
	one := 1
	confirmedFailure, err = database.SystemTransitionJob(ctx, confirmedFailure.ID,
		[]model.JobStatus{model.JobRunning}, model.JobFailed,
		JobTransition{Message: "worker failed after confirmation", ExitCode: &one})
	if err != nil || confirmedFailure.Status != model.JobOutcomeUnknown || confirmedFailure.ExitCode != nil {
		t.Fatalf("confirmed failure result=%+v err=%v", confirmedFailure, err)
	}
}
