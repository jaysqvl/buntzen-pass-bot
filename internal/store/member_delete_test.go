package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

type memberDeleteGraph struct {
	source  model.OTPSource
	profile model.Profile
	booking model.BookingRequest
	job     model.Job
}

func createMemberDeleteGraph(t *testing.T, database *Store, userID int64, unique string) memberDeleteGraph {
	t.Helper()
	ctx := context.Background()
	source, profile, booking := createOwnedResources(t, database, userID, unique)
	bookingID := booking.ID
	job, err := database.ForUser(userID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &bookingID,
		Command:          model.CommandDryRun,
		RunMode:          model.RunModeDryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	return memberDeleteGraph{source: source, profile: profile, booking: booking, job: job}
}

func disableMemberForDelete(t *testing.T, database *Store, userID int64) model.User {
	t.Helper()
	ctx := context.Background()
	user, err := database.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	user, err = database.UpdateUser(ctx, userID, UserUpdateInput{
		Username: user.Username,
		Status:   model.UserDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func memberDeleteCounts(t *testing.T, database *Store, userID int64) map[string]int {
	t.Helper()
	ctx := context.Background()
	counts := make(map[string]int)
	for _, table := range []string{
		"sessions", "otp_sources", "profiles", "booking_requests",
		"jobs", "job_events", "job_decisions",
	} {
		var count int
		if err := database.db.QueryRowContext(
			ctx, "SELECT count(*) FROM "+table+" WHERE user_id = ?", userID,
		).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

func assertNoForeignKeyViolations(t *testing.T, database *Store) {
	t.Helper()
	rows, err := database.db.QueryContext(context.Background(), "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			t.Fatalf("scan foreign-key violation: %v", err)
		}
		t.Fatalf("foreign-key violation table=%s row=%v parent=%s constraint=%d",
			table, rowID, parent, foreignKeyID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("inspect foreign keys: %v", err)
	}
}

func TestSchemaRejectsRawDeletionOfActiveMember(t *testing.T) {
	ctx := context.Background()
	database, _, memberID := ownershipStore(t)
	if _, err := database.db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", memberID); err == nil ||
		!strings.Contains(err.Error(), "must be disabled") {
		t.Fatalf("raw active-member deletion error=%v", err)
	}
	member := disableMemberForDelete(t, database, memberID)
	if err := database.DeleteMember(ctx, member.ID, member.Username); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteMemberSerializesWithTerminalJobTransition(t *testing.T) {
	ctx := context.Background()
	database, _, memberID := ownershipStore(t)
	graph := createMemberDeleteGraph(t, database, memberID, "delete-transition-race")

	claimed, err := database.SystemClaimNextDueJobAt(
		ctx, "member-delete-race-worker", time.Now().UTC().Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != graph.job.ID {
		t.Fatalf("claimed job=%d want=%d", claimed.ID, graph.job.ID)
	}
	member := disableMemberForDelete(t, database, memberID)

	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	type result struct {
		operation string
		err       error
	}
	results := make(chan result, 2)
	go func() {
		ready.Done()
		<-start
		_, transitionErr := database.SystemTransitionJob(
			ctx,
			graph.job.ID,
			[]model.JobStatus{model.JobRunning},
			model.JobCancelled,
			JobTransition{Message: "cancelled after account disable"},
		)
		results <- result{operation: "transition", err: transitionErr}
	}()
	go func() {
		ready.Done()
		<-start
		results <- result{operation: "delete", err: database.DeleteMember(ctx, member.ID, member.Username)}
	}()
	ready.Wait()
	close(start)

	var transitionErr, deleteErr error
	for range 2 {
		result := <-results
		if result.operation == "transition" {
			transitionErr = result.err
		} else {
			deleteErr = result.err
		}
	}
	if transitionErr != nil {
		t.Fatalf("terminal transition lost deletion race: %v", transitionErr)
	}
	if deleteErr != nil && !errors.Is(deleteErr, ErrMemberHasActiveJobs) {
		t.Fatalf("delete race error=%v", deleteErr)
	}
	if errors.Is(deleteErr, ErrMemberHasActiveJobs) {
		job, err := database.SystemGetJob(ctx, graph.job.ID)
		if err != nil || job.Status != model.JobCancelled {
			t.Fatalf("job after rejected delete=%+v err=%v", job, err)
		}
		if err := database.DeleteMember(ctx, member.ID, member.Username); err != nil {
			t.Fatalf("delete after terminal transition: %v", err)
		}
	}
	if _, err := database.GetUser(ctx, member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member remained after quiescent delete: %v", err)
	}
	assertNoForeignKeyViolations(t, database)
}

func TestDeleteMemberSerializesWithConcurrentReenable(t *testing.T) {
	ctx := context.Background()
	database, _, memberID := ownershipStore(t)
	graph := createMemberDeleteGraph(t, database, memberID, "delete-reenable-race")
	if err := database.ForUser(memberID).RequestJobCancellation(ctx, graph.job.ID); err != nil {
		t.Fatal(err)
	}
	member := disableMemberForDelete(t, database, memberID)

	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	type result struct {
		operation string
		err       error
	}
	results := make(chan result, 2)
	go func() {
		ready.Done()
		<-start
		_, updateErr := database.UpdateUser(ctx, member.ID, UserUpdateInput{
			Username: member.Username,
			Status:   model.UserActive,
		})
		results <- result{operation: "reenable", err: updateErr}
	}()
	go func() {
		ready.Done()
		<-start
		results <- result{operation: "delete", err: database.DeleteMember(ctx, member.ID, member.Username)}
	}()
	ready.Wait()
	close(start)

	var updateErr, deleteErr error
	for range 2 {
		result := <-results
		if result.operation == "reenable" {
			updateErr = result.err
		} else {
			deleteErr = result.err
		}
	}
	switch {
	case deleteErr == nil:
		if !errors.Is(updateErr, ErrNotFound) {
			t.Fatalf("delete won but concurrent re-enable error=%v", updateErr)
		}
	case updateErr == nil:
		if !errors.Is(deleteErr, ErrMemberMustBeDisabled) {
			t.Fatalf("re-enable won but delete error=%v", deleteErr)
		}
		member = disableMemberForDelete(t, database, member.ID)
		if err := database.DeleteMember(ctx, member.ID, member.Username); err != nil {
			t.Fatalf("cleanup delete after re-enable race: %v", err)
		}
	default:
		t.Fatalf("invalid race outcome: re-enable error=%v delete error=%v", updateErr, deleteErr)
	}
	if _, err := database.GetUser(ctx, member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member remained after race cleanup: %v", err)
	}
	assertNoForeignKeyViolations(t, database)
}

func TestDeleteMemberRollsBackAllChildrenOnMidDeleteFailure(t *testing.T) {
	ctx := context.Background()
	database, _, memberID := ownershipStore(t)
	graph := createMemberDeleteGraph(t, database, memberID, "delete-rollback")
	if err := database.ForUser(memberID).RequestJobCancellation(ctx, graph.job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemAppendJobEvent(ctx, JobEventInput{
		JobID:   graph.job.ID,
		Kind:    "delete.rollback",
		Message: "synthetic event retained when deletion rolls back",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO job_decisions(job_id, user_id, decision, created_at)
		VALUES (?, ?, 'cancel', ?)
	`, graph.job.ID, memberID, formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	member := disableMemberForDelete(t, database, memberID)
	before := memberDeleteCounts(t, database, memberID)

	trigger := fmt.Sprintf(`
		CREATE TRIGGER member_delete_injected_failure
		BEFORE DELETE ON profiles
		WHEN OLD.user_id = %d
		BEGIN
			SELECT RAISE(ABORT, 'injected member deletion failure');
		END
	`, memberID)
	if _, err := database.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	err := database.DeleteMember(ctx, member.ID, member.Username)
	if err == nil || !strings.Contains(err.Error(), "injected member deletion failure") {
		t.Fatalf("injected deletion error=%v", err)
	}
	after := memberDeleteCounts(t, database, memberID)
	for table, want := range before {
		if got := after[table]; got != want {
			t.Fatalf("%s rows after rollback=%d want=%d", table, got, want)
		}
	}
	retained, err := database.GetUser(ctx, member.ID)
	if err != nil || retained.Status != model.UserDisabled {
		t.Fatalf("member after rollback=%+v err=%v", retained, err)
	}
	assertNoForeignKeyViolations(t, database)

	if _, err := database.db.ExecContext(ctx, "DROP TRIGGER member_delete_injected_failure"); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteMember(ctx, member.ID, member.Username); err != nil {
		t.Fatalf("delete after removing injected failure: %v", err)
	}
}

func TestDeleteMemberPreservesOtherUsersAndForeignKeys(t *testing.T) {
	ctx := context.Background()
	database, adminID, deletedID := ownershipStore(t)
	retained, err := database.CreateMember(ctx, CreateUserInput{
		Username: "retained-member",
		Password: "a strong retained member password",
	})
	if err != nil {
		t.Fatal(err)
	}
	deletedGraph := createMemberDeleteGraph(t, database, deletedID, "deleted-owner")
	retainedGraph := createMemberDeleteGraph(t, database, retained.ID, "retained-owner")
	if err := database.ForUser(deletedID).RequestJobCancellation(ctx, deletedGraph.job.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ForUser(retained.ID).RequestJobCancellation(ctx, retainedGraph.job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemAppendJobEvent(ctx, JobEventInput{
		JobID: retainedGraph.job.ID, Kind: "retained.event", Message: "must survive other member deletion",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO job_decisions(job_id, user_id, decision, created_at)
		VALUES (?, ?, 'cancel', ?)
	`, retainedGraph.job.ID, retained.ID, formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	adminSession, err := database.NewSession(ctx, adminID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	retainedSession, err := database.NewSession(ctx, retained.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	deleted := disableMemberForDelete(t, database, deletedID)
	if err := database.DeleteMember(ctx, deleted.ID, deleted.Username); err != nil {
		t.Fatal(err)
	}

	for label, check := range map[string]func() error{
		"user": func() error { _, err := database.GetUser(ctx, retained.ID); return err },
		"source": func() error {
			_, err := database.ForUser(retained.ID).GetOTPSource(ctx, retainedGraph.source.ID)
			return err
		},
		"profile": func() error {
			_, err := database.ForUser(retained.ID).GetProfile(ctx, retainedGraph.profile.ID)
			return err
		},
		"booking": func() error {
			_, err := database.ForUser(retained.ID).GetBookingRequest(ctx, retainedGraph.booking.ID)
			return err
		},
		"job": func() error {
			_, err := database.ForUser(retained.ID).GetJob(ctx, retainedGraph.job.ID)
			return err
		},
		"decision": func() error {
			_, err := database.ForUser(retained.ID).GetJobDecision(ctx, retainedGraph.job.ID)
			return err
		},
	} {
		if err := check(); err != nil {
			t.Fatalf("retained %s was changed: %v", label, err)
		}
	}
	events, err := database.ForUser(retained.ID).ListJobEvents(ctx, retainedGraph.job.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Kind != "retained.event" {
		t.Fatalf("retained events=%+v err=%v", events, err)
	}
	if _, err := database.GetSession(ctx, adminSession.Token); err != nil {
		t.Fatalf("admin session was revoked: %v", err)
	}
	if _, err := database.GetSession(ctx, retainedSession.Token); err != nil {
		t.Fatalf("other member session was revoked: %v", err)
	}
	for table, count := range memberDeleteCounts(t, database, deleted.ID) {
		if count != 0 {
			t.Fatalf("deleted member retained %d rows in %s", count, table)
		}
	}
	assertNoForeignKeyViolations(t, database)
}
