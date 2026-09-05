package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

type jobRow struct {
	ID           int64
	ShortID      string
	ProfileName  string
	Command      string
	StatusLabel  string
	StatusClass  string
	CreatedLabel string
}

type jobsData struct {
	BaseData
	Jobs []jobRow
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	jobs, err := userStore.ListJobs(r.Context(), 200)
	if err != nil {
		s.internal(w)
		return
	}
	s.render(w, http.StatusOK, "jobs", jobsData{BaseData: base(r, "Jobs"), Jobs: s.jobRows(r.Context(), userStore, jobs)})
}

func (s *Server) jobRows(ctx context.Context, userStore store.UserStore, jobs []model.Job) []jobRow {
	profiles, _ := userStore.ListProfiles(ctx)
	names := make(map[int64]string, len(profiles))
	for _, profile := range profiles {
		names[profile.ID] = profile.Name
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, job := range jobs {
		rows = append(rows, jobRow{
			ID:           job.ID,
			ShortID:      fmt.Sprintf("#%06d", job.ID),
			ProfileName:  names[job.ProfileID],
			Command:      string(job.Command),
			StatusLabel:  statusLabel(job.Status),
			StatusClass:  statusClass(job.Status),
			CreatedLabel: job.CreatedAt.Local().Format("Jan 2, 15:04"),
		})
	}
	return rows
}

type labelValue struct{ Label, Value string }
type jobView struct {
	ID                int64
	ShortID           string
	ProfileName       string
	Command           string
	StatusLabel       string
	StatusClass       string
	Status            string
	CreatedLabel      string
	StartedLabel      string
	FinishedLabel     string
	ConfirmationLabel string
	Mode              string
	DueLabel          string
	ExpiresLabel      string
	TimingLabel       string
	BookingReview     []labelValue
	Message           string
	AwaitingApproval  bool
	CanCancel         bool
}
type eventView struct{ Time, Type, Message string }
type jobData struct {
	BaseData
	Job         jobView
	Events      []eventView
	LastEventID int64
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userStore := s.userStore(r)
	job, err := userStore.GetJob(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	profile, err := userStore.GetProfile(r.Context(), job.ProfileID)
	if err != nil {
		s.internal(w)
		return
	}
	events, err := userStore.ListJobEvents(r.Context(), id, 0, 500)
	if err != nil {
		s.internal(w)
		return
	}
	view := jobView{
		ID:                job.ID,
		ShortID:           fmt.Sprintf("#%06d", job.ID),
		ProfileName:       profile.Name,
		Command:           string(job.Command),
		StatusLabel:       statusLabel(job.Status),
		StatusClass:       statusClass(job.Status),
		Status:            string(job.Status),
		CreatedLabel:      job.CreatedAt.Local().Format(time.RFC1123),
		StartedLabel:      optionalTime(job.StartedAt),
		FinishedLabel:     optionalTime(job.FinishedAt),
		ConfirmationLabel: optionalTime(job.ConfirmationStartedAt),
		Mode:              string(job.RunMode),
		DueLabel:          job.DueAt.Local().Format(time.RFC1123),
		ExpiresLabel:      optionalTime(job.ExpiresAt),
		Message:           job.Message,
		AwaitingApproval:  job.Status == model.JobAwaitingApproval,
		CanCancel:         !job.Status.Terminal(),
	}
	if job.RunImmediately {
		view.TimingLabel = "Book now · manual approval"
	} else if job.Command == model.CommandBook {
		view.TimingLabel = "Release window"
	}
	// Pending jobs prevent edits to the linked profile and booking. Terminal
	// history may outlive edits, so do not present current settings as its receipt.
	if !job.Status.Terminal() && job.BookingRequestID != nil && job.Command == model.CommandBook {
		booking, err := userStore.GetBookingRequest(r.Context(), *job.BookingRequestID)
		if err != nil {
			s.internal(w)
			return
		}
		view.BookingReview = []labelValue{
			{"Target date", booking.TargetDate + " · " + booking.Timezone},
			{"Vehicle", profile.DefaultVehicle},
			{"Pass preference order", strings.Join(passNames(booking.PassOrder()), " → ")},
		}
	}
	data := jobData{BaseData: base(r, "Job "+view.ShortID), Job: view}
	for _, event := range events {
		data.Events = append(data.Events, eventView{Time: event.CreatedAt.Local().Format("15:04:05"), Type: event.Kind, Message: event.Message})
		data.LastEventID = event.ID
	}
	s.render(w, http.StatusOK, "job", data)
}

func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userStore := s.userStore(r)
	if _, err := userStore.GetJob(r.Context(), id); err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	var afterID int64
	for _, cursor := range []string{r.URL.Query().Get("after"), r.Header.Get("Last-Event-ID")} {
		if cursor == "" {
			continue
		}
		parsed, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || parsed < 0 {
			http.Error(w, "invalid event cursor", http.StatusBadRequest)
			return
		}
		if parsed > afterID {
			afterID = parsed
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	sessionToken, err := r.Cookie(sessionCookie)
	if err != nil || sessionToken.Value == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	streamAuthorized := func() bool {
		_, err := s.store.GetSession(r.Context(), sessionToken.Value)
		return err == nil
	}
	expireStream := func() {
		writeSSE(w, "auth_expired", map[string]any{})
		flusher.Flush()
	}

	jobKey := strconv.FormatInt(id, 10)
	live, unsubscribe := s.engine.Hub().Subscribe(jobKey)
	defer unsubscribe()
	const pollInterval = 2 * time.Second
	poll := time.NewTicker(pollInterval)
	keepalive := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer keepalive.Stop()
	var terminalObservedAt time.Time
	writeSnapshot := func(completed bool) bool {
		if !streamAuthorized() {
			expireStream()
			return true
		}
		job, err := userStore.GetJob(r.Context(), id)
		if err != nil {
			return true
		}
		if job.Status.Terminal() && terminalObservedAt.IsZero() {
			terminalObservedAt = time.Now()
		}
		// Finish writes the terminal event after the status transition. Give that
		// bounded write time to finish when its live completion signal was missed.
		finished := job.Status.Terminal() && (completed || time.Since(terminalObservedAt) >= 2*pollInterval)
		for {
			events, err := userStore.ListJobEvents(r.Context(), id, afterID, 100)
			if err != nil {
				return true
			}
			for _, event := range events {
				_, _ = fmt.Fprintf(w, "id: %d\n", event.ID)
				writeSSE(w, "job_event", map[string]any{"id": event.ID, "time": event.CreatedAt.Local().Format("15:04:05"), "type": event.Kind, "message": event.Message})
				afterID = event.ID
			}
			if len(events) < 100 {
				break
			}
		}
		writeJobState(w, job)
		if finished {
			writeSSE(w, "complete", map[string]any{})
		}
		flusher.Flush()
		return finished
	}
	if writeSnapshot(false) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-live:
			if !open {
				return
			}
			if !streamAuthorized() {
				expireStream()
				return
			}
			if event.Kind == "otp" || event.Kind == "pairing" {
				writeSSE(w, event.Kind, event.Data)
			} else if writeSnapshot(event.Kind == "complete") {
				return
			}
			flusher.Flush()
		case <-poll.C:
			if writeSnapshot(false) {
				return
			}
		case <-keepalive.C:
			if !streamAuthorized() {
				expireStream()
				return
			}
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeJobState(w http.ResponseWriter, job model.Job) {
	writeSSE(w, "state", map[string]any{
		"message":              job.Message,
		"label":                statusLabel(job.Status),
		"class_name":           statusClass(job.Status),
		"status":               string(job.Status),
		"started":              optionalTime(job.StartedAt),
		"finished":             optionalTime(job.FinishedAt),
		"confirmation_started": optionalTime(job.ConfirmationStartedAt),
		"can_cancel":           !job.Status.Terminal(),
		"awaiting_approval":    job.Status == model.JobAwaitingApproval,
		"terminal":             job.Status.Terminal(),
	})
}

func writeSSE(w http.ResponseWriter, event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
}

func (s *Server) jobDecision(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.userStore(r).GetJob(r.Context(), id); err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	decision := r.Form.Get("decision")
	userID := requestAuth(r).Authenticated.User.ID
	var err error
	switch decision {
	case "approve":
		err = s.engine.Decide(r.Context(), userID, id, model.DecisionApprove)
	case "cancel":
		err = s.engine.Decide(r.Context(), userID, id, model.DecisionCancel)
	case "cancel-job":
		err = s.engine.CancelJob(r.Context(), userID, id)
	case "pair":
		err = s.engine.ChoosePairing(r.Context(), userID, id, r.Form.Get("message_id"))
	default:
		http.Error(w, "unsupported decision", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "decision was no longer available", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func statusLabel(status model.JobStatus) string { return strings.ReplaceAll(string(status), "_", " ") }
func statusClass(status model.JobStatus) string {
	switch status {
	case model.JobSucceeded:
		return "ok"
	case model.JobQueued, model.JobRunning:
		return "active"
	case model.JobAwaitingApproval:
		return "warn"
	case model.JobFailed, model.JobOutcomeUnknown:
		return "error"
	default:
		return ""
	}
}

func optionalTime(value *time.Time) string {
	if value == nil {
		return "—"
	}
	return value.Local().Format(time.RFC1123)
}
