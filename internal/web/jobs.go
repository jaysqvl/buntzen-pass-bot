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

type dashboardData struct {
	BaseData
	Stats struct {
		Profiles  int
		Scheduled int
		Active    int
		Waiting   int
	}
	Jobs []jobRow
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	profiles, err := userStore.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	bookings, err := userStore.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	jobs, err := userStore.ListJobs(r.Context(), 10)
	if err != nil {
		s.internal(w)
		return
	}
	data := dashboardData{BaseData: base(r, "Dashboard")}
	data.Stats.Profiles = len(profiles)
	for _, booking := range bookings {
		if booking.Enabled && booking.ScheduleEnabled {
			data.Stats.Scheduled++
		}
	}
	for _, job := range jobs {
		if job.Status == model.JobRunning || job.Status == model.JobAwaitingApproval {
			data.Stats.Active++
		}
		if job.Status == model.JobAwaitingApproval {
			data.Stats.Waiting++
		}
	}
	data.Jobs = s.jobRows(r.Context(), userStore, jobs)
	s.render(w, http.StatusOK, "dashboard", data)
}

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
	ID               int64
	ShortID          string
	ProfileName      string
	Command          string
	StatusLabel      string
	StatusClass      string
	CreatedLabel     string
	Message          string
	AwaitingApproval bool
	CanCancel        bool
	Fields           []labelValue
}
type eventView struct{ Time, Type, Message string }
type jobData struct {
	BaseData
	Job    jobView
	Events []eventView
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
	profile, _ := userStore.GetProfile(r.Context(), job.ProfileID)
	events, err := userStore.ListJobEvents(r.Context(), id, 0, 500)
	if err != nil {
		s.internal(w)
		return
	}
	view := jobView{
		ID:               job.ID,
		ShortID:          fmt.Sprintf("#%06d", job.ID),
		ProfileName:      profile.Name,
		Command:          string(job.Command),
		StatusLabel:      statusLabel(job.Status),
		StatusClass:      statusClass(job.Status),
		CreatedLabel:     job.CreatedAt.Local().Format(time.RFC1123),
		Message:          job.Message,
		AwaitingApproval: job.Status == model.JobAwaitingApproval,
		CanCancel:        job.Status == model.JobQueued || job.Status == model.JobRunning || job.Status == model.JobAwaitingApproval,
	}
	view.Fields = []labelValue{
		{"Mode", string(job.RunMode)},
		{"Status", string(job.Status)},
		{"Due", job.DueAt.Local().Format(time.RFC1123)},
		{"Started", optionalTime(job.StartedAt)},
		{"Finished", optionalTime(job.FinishedAt)},
		{"Final confirmation", optionalTime(job.ConfirmationStartedAt)},
	}
	data := jobData{BaseData: base(r, "Job "+view.ShortID), Job: view}
	for _, event := range events {
		data.Events = append(data.Events, eventView{Time: event.CreatedAt.Local().Format("15:04:05"), Type: event.Kind, Message: event.Message})
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

	existing, _ := userStore.ListJobEvents(r.Context(), id, 0, 1000)
	var afterID int64
	if len(existing) > 0 {
		afterID = existing[len(existing)-1].ID
	}
	s.writeJobState(w, userStore, id)
	flusher.Flush()
	jobKey := strconv.FormatInt(id, 10)
	live, unsubscribe := s.engine.Hub().Subscribe(jobKey)
	defer unsubscribe()
	poll := time.NewTicker(2 * time.Second)
	keepalive := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer keepalive.Stop()
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
			} else {
				s.writeJobState(w, userStore, id)
			}
			flusher.Flush()
		case <-poll.C:
			if !streamAuthorized() {
				expireStream()
				return
			}
			events, err := userStore.ListJobEvents(r.Context(), id, afterID, 100)
			if err != nil {
				return
			}
			for _, event := range events {
				writeSSE(w, "job_event", map[string]any{"time": event.CreatedAt.Local().Format("15:04:05"), "type": event.Kind, "message": event.Message})
				afterID = event.ID
			}
			job, err := userStore.GetJob(r.Context(), id)
			if err != nil {
				return
			}
			s.writeJobState(w, userStore, id)
			flusher.Flush()
			if job.Status.Terminal() {
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

func (s *Server) writeJobState(w http.ResponseWriter, userStore store.UserStore, id int64) {
	job, err := userStore.GetJob(context.Background(), id)
	if err != nil {
		return
	}
	writeSSE(w, "state", map[string]any{
		"message":           job.Message,
		"label":             statusLabel(job.Status),
		"class_name":        statusClass(job.Status),
		"awaiting_approval": job.Status == model.JobAwaitingApproval,
		"terminal":          job.Status.Terminal(),
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
