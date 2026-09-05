package scheduler

import (
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestWindowUsesPreviousLocalCalendarDayAcrossDST(t *testing.T) {
	for _, test := range []struct {
		targetDate string
		release    string
	}{
		// Historical transitions keep this test stable when future timezone
		// legislation changes the IANA rules used in production.
		{"2025-01-15", "2025-01-14T07:00:00-08:00"},
		{"2025-03-10", "2025-03-09T07:00:00-07:00"},
		{"2025-11-03", "2025-11-02T07:00:00-08:00"},
	} {
		t.Run(test.targetDate, func(t *testing.T) {
			request := validRequest()
			request.TargetDate = test.targetDate
			request.Timezone = "America/Vancouver"
			window, err := WindowFor(request)
			if err != nil {
				t.Fatal(err)
			}
			if got := window.ReleaseAt.Format(time.RFC3339); got != test.release {
				t.Fatalf("release = %s, want %s", got, test.release)
			}
			if got := window.PrepAt.Format("15:04"); got != "06:30" {
				t.Fatalf("prep time = %s", got)
			}
			if got := window.AuthDeadlineAt.Format("15:04"); got != "06:55" {
				t.Fatalf("auth deadline = %s", got)
			}
		})
	}
}

func TestShouldQueueUsesBoundedWindow(t *testing.T) {
	window, err := WindowFor(validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if ShouldQueue(window.PrepAt.Add(-time.Nanosecond), window) {
		t.Fatal("queued before prep")
	}
	if !ShouldQueue(window.PrepAt, window) {
		t.Fatal("did not queue at the inclusive preparation boundary")
	}
	if !ShouldQueue(window.ReleaseAt, window) {
		t.Fatal("did not queue at release")
	}
	if ShouldQueue(window.PollEndsAt, window) {
		t.Fatal("queued at the exclusive poll-window boundary")
	}
	if ShouldQueue(window.PollEndsAt.Add(time.Nanosecond), window) {
		t.Fatal("queued after poll window")
	}
}

func validRequest() model.BookingRequest {
	return model.BookingRequest{
		Name: "test", ProfileID: 1, Enabled: true, TargetDate: "2030-01-15",
		Timezone: "UTC", ReleaseTime: "07:00", PrepMinutesBefore: 30,
		AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120, PollMinSeconds: 1,
		PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.invalid/login", AllDayPassURL: "https://example.invalid/all",
		CheckAllDay: true,
	}
}
