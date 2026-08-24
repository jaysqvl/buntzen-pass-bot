package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestWindowUsesDayBeforeTargetInLocalTimezone(t *testing.T) {
	request := validRequest()
	request.TargetDate = "2030-01-15"
	window, err := WindowFor(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := window.ReleaseAt.Format(time.RFC3339); got != "2030-01-14T07:00:00Z" {
		t.Fatalf("release = %s", got)
	}
	if got := window.PrepAt.Format("15:04"); got != "06:30" {
		t.Fatalf("prep time = %s", got)
	}
	if got := window.AuthDeadlineAt.Format("15:04"); got != "06:55" {
		t.Fatalf("auth deadline = %s", got)
	}
}

func TestPassOrderIsFixed(t *testing.T) {
	request := validRequest()
	request.CheckMorning = true
	request.CheckAfternoon = true
	want := []model.PassType{model.PassAllDay, model.PassAfternoon, model.PassMorning}
	if got := request.PassOrder(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pass order = %v", got)
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
