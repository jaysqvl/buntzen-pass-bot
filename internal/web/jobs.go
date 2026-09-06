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
	ModeLabel    string
	DueLabel     string
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
	bookings, _ := userStore.ListBookingRequests(ctx)
	bookingsByID := make(map[int64]model.BookingRequest, len(bookings))
	for _, booking := range bookings {
		bookingsByID[booking.ID] = booking
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, job := range jobs {
		location := time.UTC
		if job.BookingRequestID != nil {
			location = pendingJobLocation(job, bookingsByID[*job.BookingRequestID])
		}
		rows = append(rows, jobRow{
			ID:           job.ID,
			ShortID:      fmt.Sprintf("#%06d", job.ID),
			ProfileName:  names[job.ProfileID],
			Command:      string(job.Command),
			StatusLabel:  jobStatusLabel(job),
			StatusClass:  statusClass(job.Status),
			ModeLabel:    jobModeLabel(job),
			DueLabel:     formatJobTime(job.DueAt, location),
			CreatedLabel: formatJobTime(job.CreatedAt, location),
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
	location := time.UTC
	var bookingReview []labelValue
	// Pending jobs prevent edits to the linked profile and booking. Terminal
	// history may outlive edits, so do not present current settings as its receipt.
	if !job.Status.Terminal() && job.BookingRequestID != nil && job.Command == model.CommandBook {
		booking, err := userStore.GetBookingRequest(r.Context(), *job.BookingRequestID)
		if err != nil {
			s.internal(w)
			return
		}
		location = pendingJobLocation(job, booking)
		bookingReview = []labelValue{
			{"Target date", booking.TargetDate + " · " + booking.Timezone},
			{"Vehicle", profile.DefaultVehicle},
			{"Pass preference order", strings.Join(passNames(booking.PassOrder()), " → ")},
		}
	}
	view := jobView{
		ID:                job.ID,
		ShortID:           fmt.Sprintf("#%06d", job.ID),
		ProfileName:       profile.Name,
		Command:           string(job.Command),
		StatusLabel:       jobStatusLabel(job),
		StatusClass:       statusClass(job.Status),
		CreatedLabel:      formatJobTime(job.CreatedAt, location),
		StartedLabel:      formatOptionalJobTime(job.StartedAt, location),
		FinishedLabel:     formatOptionalJobTime(job.FinishedAt, location),
		ConfirmationLabel: formatOptionalJobTime(job.ConfirmationStartedAt, location),
		Mode:              jobModeLabel(job),
		DueLabel:          formatJobTime(job.DueAt, location),
		ExpiresLabel:      formatOptionalJobTime(job.ExpiresAt, location),
		BookingReview:     bookingReview,
		Message:           jobDisplayMessage(job, location),
		AwaitingApproval:  job.Status == model.JobAwaitingApproval,
		CanCancel:         !job.Status.Terminal(),
	}
	if job.RunImmediately {
		view.TimingLabel = "Book now · manual approval"
	} else if job.Command == model.CommandBook {
		view.TimingLabel = "Release window"
	}
	data := jobData{BaseData: base(r, "Job "+view.ShortID), Job: view}
	for _, event := range events {
		data.Events = append(data.Events, eventView{Time: event.CreatedAt.In(location).Format("15:04:05 -07:00"), Type: event.Kind, Message: event.Message})
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
	initialJob, err := userStore.GetJob(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	// Keep this stream's timezone stable across completion. Only a pending
	// booking has settings that are still locked to this job.
	location := time.UTC
	if !initialJob.Status.Terminal() && initialJob.Command == model.CommandBook && initialJob.BookingRequestID != nil {
		booking, err := userStore.GetBookingRequest(r.Context(), *initialJob.BookingRequestID)
		if err != nil {
			s.internal(w)
			return
		}
		location = pendingJobLocation(initialJob, booking)
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
				writeSSE(w, "job_event", map[string]any{"id": event.ID, "time": event.CreatedAt.In(location).Format("15:04:05 -07:00"), "type": event.Kind, "message": event.Message})
				afterID = event.ID
			}
			if len(events) < 100 {
				break
			}
		}
		writeJobState(w, job, location)
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

func writeJobState(w http.ResponseWriter, job model.Job, location *time.Location) {
	writeSSE(w, "state", map[string]any{
		"message":              jobDisplayMessage(job, location),
		"label":                jobStatusLabel(job),
		"class_name":           statusClass(job.Status),
		"status":               string(job.Status),
		"started":              formatOptionalJobTime(job.StartedAt, location),
		"finished":             formatOptionalJobTime(job.FinishedAt, location),
		"confirmation_started": formatOptionalJobTime(job.ConfirmationStartedAt, location),
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

func statusLabel(status model.JobStatus) string {
	if status == model.JobQueued {
		return "Waiting to start"
	}
	return strings.ReplaceAll(string(status), "_", " ")
}

func jobStatusLabel(job model.Job) string {
	if job.CancelRequested && !job.Status.Terminal() {
		return "Cancellation requested"
	}
	return statusLabel(job.Status)
}

func jobModeLabel(job model.Job) string {
	if job.Command == model.CommandBook {
		switch job.RunMode {
		case model.RunModeAuto:
			return "Automatic final confirmation"
		case model.RunModeManual:
			return "Manual final approval"
		}
	}
	return "No booking confirmation"
}

func jobDisplayMessage(job model.Job, location *time.Location) string {
	if job.CancelRequested && !job.Status.Terminal() {
		return "Cancellation requested."
	}
	if job.Status == model.JobQueued && job.Message == "" {
		return "Waiting to start. Earliest start: " + formatJobTime(job.DueAt, location) + ". The job may start later if another job is running or its settings need attention."
	}
	return job.Message
}

// Pending booking settings are locked while a job uses them. Completed jobs
// have no saved timezone, so use UTC rather than today's editable settings.
func pendingJobLocation(job model.Job, booking model.BookingRequest) *time.Location {
	if job.Status.Terminal() || job.Command != model.CommandBook || job.BookingRequestID == nil ||
		*job.BookingRequestID != booking.ID || job.ProfileID != booking.ProfileID || job.UserID != booking.UserID {
		return time.UTC
	}
	location, err := time.LoadLocation(booking.Timezone)
	if err != nil {
		return time.UTC
	}
	return location
}

func formatJobTime(value time.Time, location *time.Location) string {
	if location == nil {
		location = time.UTC
	}
	local := value.In(location)
	label := local.Format("Mon, Jan 2, 2006 at 3:04 PM") + " UTC"
	if _, offset := local.Zone(); offset != 0 {
		label += local.Format("-07:00")
	}
	return label
}

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

func formatOptionalJobTime(value *time.Time, location *time.Location) string {
	if value == nil {
		return "—"
	}
	return formatJobTime(*value, location)
}
