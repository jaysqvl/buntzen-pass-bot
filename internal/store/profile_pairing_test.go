package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestProfileOnlyPairingAdmissionIsAtomicAndCancellationAllowsRetry(t *testing.T) {
	database := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, database, "profile-pairing")
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 8)
	var workers sync.WaitGroup
	for attempt := 0; attempt < cap(results); attempt++ {
		workers.Add(1)
		go func(attempt int) {
			defer workers.Done()
			<-start
			_, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
				ProfileID: profile.ID, Command: model.CommandAuthCheck,
				DedupKey: fmt.Sprintf("pairing:%d:attempt-%d", profile.OTPSourceID, attempt),
			})
			results <- err
		}(attempt)
	}
	close(start)
	workers.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("pairing admission returned unexpected error: %v", err)
		}
	}
	jobs, err := database.ForUser(testUserID).ListJobs(ctx, 10)
	if err != nil || accepted != 1 || len(jobs) != 1 || jobs[0].BookingRequestID != nil {
		t.Fatalf("concurrent pairing accepted=%d jobs=%+v err=%v", accepted, jobs, err)
	}
	if err := database.ForUser(testUserID).RequestJobCancellation(ctx, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		DedupKey: fmt.Sprintf("pairing:%d:retry", profile.OTPSourceID),
	}); err != nil {
		t.Fatalf("retry after cancelled pairing failed: %v", err)
	}
}
