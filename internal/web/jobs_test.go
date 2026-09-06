package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func newLiveJob(t *testing.T, fixture webFixture) model.Job {
	t.Helper()
	ctx := context.Background()
	resources := fixture.store.ForUser(fixture.admin.ID)
	source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Live events inbox", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://127.0.0.1:2234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:2234", Password: "test-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{
		Name: "Live events profile", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID,
		LoginProbeURL: "https://example.test/login",
		Headless:      true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

type streamedJobEvent struct {
	kind string
	id   string
	data map[string]any
}

func readJobEvent(t *testing.T, reader *bufio.Reader) streamedJobEvent {
	t.Helper()
	var event streamedJobEvent
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read %s event: %v", event.kind, err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event.kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			event.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.data); err != nil {
				t.Fatal(err)
			}
		case line == "" && event.kind != "":
			return event
		}
	}
}

func openJobStream(t *testing.T, fixture webFixture, jobID int64, cookies []*http.Cookie, afterID, lastEventID int64) *bufio.Reader {
	t.Helper()
	server := httptest.NewServer(fixture.handler)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/jobs/%d/events?after=%d", server.URL, jobID, afterID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID > 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d", response.StatusCode)
	}
	return bufio.NewReader(response.Body)
}

func appendLiveJobEvent(t *testing.T, fixture webFixture, jobID int64, kind string) model.JobEvent {
	t.Helper()
	event, err := fixture.store.SystemAppendJobEvent(context.Background(), store.JobEventInput{
		JobID: jobID, Kind: kind, Message: "Sanitized job progress",
	})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestJobStreamDeliversFinalEventsBeforeClosingAndUpdatesDetails(t *testing.T) {
	fixture := newWebFixture(t)
	job := newLiveJob(t, fixture)
	cookies := loginCookies(t, fixture)
	reader := openJobStream(t, fixture, job.ID, cookies, 0, 0)
	initial := readJobEvent(t, reader)
	if initial.kind != "state" || initial.data["status"] != "queued" || initial.data["can_cancel"] != true {
		t.Fatalf("initial state=%+v", initial)
	}
	ctx := context.Background()
	if _, err := fixture.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, store.JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SystemMarkConfirmationStarted(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// More than one page must be drained when the job finishes between polls.
	for range 105 {
		appendLiveJobEvent(t, fixture, job.ID, "progress")
	}
	finished, err := fixture.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobSucceeded, store.JobTransition{Message: "Confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	key := strconv.FormatInt(job.ID, 10)
	fixture.server.engine.Hub().Publish(key, control.LiveEvent{Kind: "event"})
	var state streamedJobEvent
	progressCount := 0
	for {
		event := readJobEvent(t, reader)
		if event.kind == "job_event" && event.data["type"] == "progress" && event.id != "" {
			progressCount++
		} else if event.kind != "state" {
			t.Fatalf("unexpected event before terminal state: %+v", event)
		}
		if event.kind == "state" && event.data["terminal"] == true {
			state = event
			break
		}
	}
	if progressCount != 105 {
		t.Fatalf("received %d progress events, want 105", progressCount)
	}
	for key, want := range map[string]any{
		"status": "succeeded", "label": "succeeded", "message": "Confirmed", "can_cancel": false,
		"awaiting_approval": false, "terminal": true, "started": optionalTime(finished.StartedAt),
		"finished": optionalTime(finished.FinishedAt), "confirmation_started": optionalTime(finished.ConfirmationStartedAt),
	} {
		if state.kind != "state" || state.data[key] != want {
			t.Fatalf("terminal state %s=%v, want %v: %+v", key, state.data[key], want, state)
		}
	}
	// Model the real status-before-event ordering. The browser must still be
	// subscribed when this final durable event becomes available.
	last := appendLiveJobEvent(t, fixture, job.ID, "job.succeeded")
	fixture.server.engine.Hub().Publish(key, control.LiveEvent{Kind: "complete"})
	if event := readJobEvent(t, reader); event.kind != "job_event" || event.id != strconv.FormatInt(last.ID, 10) {
		t.Fatalf("missing final event: %+v", event)
	}
	if event := readJobEvent(t, reader); event.kind != "state" {
		t.Fatalf("missing final state: %+v", event)
	}
	if event := readJobEvent(t, reader); event.kind != "complete" {
		t.Fatalf("stream closed before completion marker: %+v", event)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("completed stream remained open: %v", err)
	}
}

func TestJobStreamResumesFromRenderedAndReconnectedCursors(t *testing.T) {
	fixture := newWebFixture(t)
	job := newLiveJob(t, fixture)
	cookies := loginCookies(t, fixture)
	rendered := appendLiveJobEvent(t, fixture, job.ID, "rendered")
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet,
		fmt.Sprintf("http://example.test/jobs/%d", job.ID), cookies, nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), fmt.Sprintf(`data-last-event-id="%d"`, rendered.ID)) {
		t.Fatalf("rendered page missing event cursor: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	between := appendLiveJobEvent(t, fixture, job.ID, "after_render")
	reader := openJobStream(t, fixture, job.ID, cookies, rendered.ID, 0)
	if event := readJobEvent(t, reader); event.kind != "job_event" || event.id != strconv.FormatInt(between.ID, 10) {
		t.Fatalf("lost event between HTML and stream: %+v", event)
	}
	if event := readJobEvent(t, reader); event.kind != "state" {
		t.Fatalf("duplicate history before initial state: %+v", event)
	}
	reconnected := appendLiveJobEvent(t, fixture, job.ID, "after_disconnect")
	reader = openJobStream(t, fixture, job.ID, cookies, rendered.ID, between.ID)
	if event := readJobEvent(t, reader); event.kind != "job_event" || event.id != strconv.FormatInt(reconnected.ID, 10) {
		t.Fatalf("reconnection did not resume: %+v", event)
	}
	if event := readJobEvent(t, reader); event.kind != "state" {
		t.Fatalf("reconnection replayed acknowledged events: %+v", event)
	}
}

func TestTerminalJobStreamClosesWhenLiveCompletionWasMissed(t *testing.T) {
	fixture := newWebFixture(t)
	job := newLiveJob(t, fixture)
	cookies := loginCookies(t, fixture)
	if err := fixture.store.ForUser(fixture.admin.ID).RequestJobCancellation(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	reader := openJobStream(t, fixture, job.ID, cookies, 0, 0)
	if event := readJobEvent(t, reader); event.kind != "state" || event.data["terminal"] != true {
		t.Fatalf("missing terminal snapshot: %+v", event)
	}
	last := appendLiveJobEvent(t, fixture, job.ID, "job.cancelled")
	seenFinal := false
	for {
		event := readJobEvent(t, reader)
		if event.kind == "job_event" && event.id == strconv.FormatInt(last.ID, 10) {
			seenFinal = true
		}
		if event.kind == "complete" {
			if !seenFinal {
				t.Fatal("terminal reconnect closed before the delayed final event")
			}
			break
		}
	}
}
