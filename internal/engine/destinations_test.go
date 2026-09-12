package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestExecutionRejectsPersistedUnknownLakeBeforeDecryptingCredentials(t *testing.T) {
	for _, command := range []model.JobCommand{model.CommandAuthCheck, model.CommandDryRun} {
		t.Run(string(command), func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			ctx := context.Background()
			job, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{
				BookingRequestID: &fixture.booking.ID, Command: command, RunMode: model.RunModeManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			database, err := sql.Open("sqlite", fixture.databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			// Simulate unsupported persisted state after queueing, and make
			// decryption fail if execution reaches either credential source.
			for _, statement := range []string{
				`UPDATE booking_requests SET lake_id = 'unknown-lake'`,
				`UPDATE profiles SET yodel_phone_ciphertext = 'invalid-ciphertext'`,
				`UPDATE otp_sources SET config_ciphertext = 'invalid-ciphertext'`,
			} {
				if _, err := database.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.engine.execute(ctx, job); err == nil || !strings.Contains(err.Error(), "unsupported lake") {
				t.Fatalf("unsupported persisted destination was not rejected first: %v", err)
			}
		})
	}
}
