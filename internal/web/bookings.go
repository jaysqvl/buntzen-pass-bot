package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/config"
	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/engine"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/scheduler"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func (s *Server) bookings(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	bookings, err := userStore.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	profiles, _ := userStore.ListProfiles(r.Context())
	jobs, err := userStore.ListPendingBookingJobs(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	pending := make(map[int64]*model.Job, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		if job.BookingRequestID != nil && pending[*job.BookingRequestID] == nil {
			pending[*job.BookingRequestID] = job
		}
	}
	names := map[int64]string{}
	for _, profile := range profiles {
		names[profile.ID] = profile.Name
	}
	data := listData{
		BaseData:     base(r, "Bookings"),
		Eyebrow:      "Pass bookings",
		Heading:      "Booking requests",
		Description:  "Queue for release creates a job that waits until preparation starts. Book now checks already released passes and requires your approval; it expires after 15 minutes.",
		CreateURL:    "/bookings/new",
		CreateLabel:  "New booking",
		EmptyMessage: "Create a profile, then add a booking request.",
	}
	if data.Flash != nil {
		switch r.URL.Query().Get("notice") {
		case "queue-pending", "queue-review":
			if job, err := userStore.GetJob(r.Context(), parseInt64(r.URL.Query().Get("job"))); err == nil {
				data.Flash.ActionLabel = "View existing job"
				data.Flash.ActionURL = fmt.Sprintf("/jobs/%d", job.ID)
			}
		case "queue-full", "queue-unavailable":
			data.Flash.ActionLabel, data.Flash.ActionURL = "View jobs", "/jobs"
		}
	}
	if !s.config.SchedulesEnabled {
		data.Notice = autoQueueOffNotice
	}
	for _, booking := range bookings {
		data.Cards = append(data.Cards, bookingCard(booking, names[booking.ProfileID], s.config.SchedulesEnabled, pending[booking.ID]))
	}
	s.render(w, http.StatusOK, "list", data)
}

const autoQueueOffNotice = "Auto-queueing is off for this server. New jobs must be queued manually. Already queued jobs remain scheduled; use Cancel job to stop one."

func lakeName(id string) string {
	lake, err := destinations.Resolve(id)
	if err != nil {
		return "Unsupported lake"
	}
	return lake.Name
}

func lakeReleasePolicy(lake destinations.Lake) string {
	unit := "days"
	if lake.ReleaseDaysBefore == 1 {
		unit = "day"
	}
	return fmt.Sprintf("Passes release %d %s before your visit at the configured local time.", lake.ReleaseDaysBefore, unit)
}

func passOptionLabel(pass string) string {
	switch model.PassType(pass) {
	case model.PassAllDay:
		return "All-day"
	case model.PassAfternoon:
		return "Afternoon"
	case model.PassMorning:
		return "Morning"
	default:
		return strings.ReplaceAll(pass, "_", " ")
	}
}

type lakePassOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type lakeFormDefaults struct {
	ID             string           `json:"id"`
	Timezone       string           `json:"timezone"`
	ReleaseTime    string           `json:"releaseTime"`
	AllDayPassURL  string           `json:"allDayPassURL"`
	HalfDayPassURL string           `json:"halfDayPassURL"`
	ReleasePolicy  string           `json:"releasePolicy"`
	Passes         []lakePassOption `json:"passes"`
}

func lakeSelectOption(lake destinations.Lake, selectedID string) selectOption {
	defaults := lakeFormDefaults{
		ID: lake.ID, Timezone: lake.Timezone, ReleaseTime: lake.ReleaseTime,
		AllDayPassURL: lake.AllDayPassURL, HalfDayPassURL: lake.HalfDayPassURL,
		ReleasePolicy: lakeReleasePolicy(lake),
	}
	for _, pass := range lake.SupportedPasses {
		defaults.Passes = append(defaults.Passes, lakePassOption{Value: pass, Label: passOptionLabel(pass)})
	}
	// Only strings and slices are encoded; html/template escapes the result
	// as an attribute, and the external client parses it as data, never code.
	encoded, _ := json.Marshal(defaults)
	return selectOption{Value: lake.ID, Label: lake.Name, Selected: lake.ID == selectedID, LakeDefaults: string(encoded)}
}

func bookingCard(booking model.BookingRequest, profileName string, schedulesEnabled bool, job *model.Job) listCard {
	status, class := "No booking queued", ""
	autoQueue := "Off for this booking"
	if !booking.Enabled {
		status = "Disabled"
		autoQueue = "Off · booking disabled"
	} else if !schedulesEnabled {
		autoQueue = "Off for this server"
	} else if booking.ScheduleEnabled {
		autoQueue = "On for this booking"
	}
	confirmation := jobModeLabel(model.Job{Command: model.CommandBook, RunMode: booking.ConfirmationMode})
	if job != nil {
		// The saved job controls checkout. Book now always requires approval,
		// even when the booking request specifies automatic confirmation.
		confirmation = jobModeLabel(*job)
	}
	release := booking.ReleaseTime + " · " + booking.Timezone
	if window, err := scheduler.WindowFor(booking); err == nil {
		release = formatJobTime(window.ReleaseAt, window.ReleaseAt.Location())
	}
	url := fmt.Sprintf("/bookings/%d", booking.ID)
	card := listCard{
		Title:       booking.Name,
		Subtitle:    profileName,
		Status:      status,
		StatusClass: class,
		URL:         url,
		Fields: []labelValue{
			{"Lake", lakeName(booking.EffectiveLakeID())},
			{"Target date", booking.TargetDate + " · " + booking.Timezone},
			{"Pass release", release},
			{"Auto-queueing", autoQueue},
			{"Final confirmation", confirmation},
			{"Pass order", strings.Join(passNames(booking.PassOrder()), " → ")},
		},
		Actions: []cardAction{{"Edit", url, ""}},
		PostActions: []postAction{
			{Label: "Auth check", URL: url + "/run", Fields: []hiddenField{{"command", "auth-check"}}},
			{Label: "Dry run", URL: url + "/run", Fields: []hiddenField{{"command", "dry-run"}}},
			{Label: "Queue for release", URL: url + "/run", Fields: []hiddenField{{"command", "book"}}},
			{
				Label: "Book now · manual approval", URL: url + "/run", Class: "primary",
				Fields: []hiddenField{{"command", "book"}, {"timing", "now"}},
			},
		},
	}
	if job != nil {
		card.Status, card.StatusClass = jobStatusLabel(*job), statusClass(job.Status)
		card.Fields = append(card.Fields,
			labelValue{"Booking job", fmt.Sprintf("Job %d · %s", job.ID, card.Status)},
			labelValue{"Earliest start", formatJobTime(job.DueAt, pendingJobLocation(*job, booking))},
		)
		card.Actions = append(card.Actions, cardAction{"View job", fmt.Sprintf("/jobs/%d", job.ID), ""})
	}
	return card
}

func passNames(values []model.PassType) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = strings.ReplaceAll(string(value), "_", " ")
	}
	return result
}

func (s *Server) bookingNew(w http.ResponseWriter, r *http.Request) { s.bookingForm(w, r, nil, "") }
func (s *Server) bookingCreate(w http.ResponseWriter, r *http.Request) {
	request, err := s.bookingInput(r, 0)
	if err == nil {
		_, err = s.userStore(r).CreateBookingRequest(r.Context(), request)
	}
	if err != nil {
		s.bookingForm(w, r, nil, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/bookings?ok=created", http.StatusSeeOther)
}
func (s *Server) bookingEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	booking, err := s.userStore(r).GetBookingRequest(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	s.bookingForm(w, r, &booking, "")
}
func (s *Server) bookingUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.userStore(r).GetBookingRequest(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	request, err := s.bookingInput(r, id)
	if err == nil {
		_, err = s.userStore(r).UpdateBookingRequest(r.Context(), request)
	}
	if err != nil {
		s.bookingForm(w, r, &current, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/bookings?ok=updated", http.StatusSeeOther)
}
func (s *Server) bookingRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	booking, err := s.userStore(r).GetBookingRequest(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	command := model.JobCommand(r.Form.Get("command"))
	if !command.Valid() {
		redirectNotice(w, r, "/bookings", "booking-action")
		return
	}
	mode := model.RunMode("")
	if command == model.CommandDryRun {
		mode = model.RunModeDryRun
	}
	timing := r.Form.Get("timing")
	if timing != "" && (timing != "now" || command != model.CommandBook) {
		redirectNotice(w, r, "/bookings", "booking-action")
		return
	}
	userID := requestAuth(r).Authenticated.User.ID
	var job model.Job
	if timing == "now" {
		job, err = s.engine.QueueBookingNow(r.Context(), userID, id)
	} else {
		job, err = s.engine.QueueBooking(r.Context(), userID, id, command, mode)
	}
	if err != nil {
		slog.Warn("booking job could not be queued", "booking_id", id, "command", command, "error", err)
		s.bookingRunFailure(w, r, booking, command, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d?ok=queued", job.ID), http.StatusSeeOther)
}

func (s *Server) bookingRunFailure(w http.ResponseWriter, r *http.Request, booking model.BookingRequest, command model.JobCommand, cause error) {
	code := "queue-unavailable"
	var existingJob *model.Job
	switch {
	case errors.Is(cause, store.ErrResourceLimit):
		// Resource limits wrap ErrConflict, but do not mean a duplicate exists.
		code = "queue-full"
	case errors.Is(cause, engine.ErrBookingNotReleased):
		code = "booking-not-released"
	case errors.Is(cause, engine.ErrBookingDatePassed):
		code = "booking-date-passed"
	case errors.Is(cause, engine.ErrBookingWindowEnded):
		code = "booking-window-ended"
	case errors.Is(cause, store.ErrConflict):
		conflict, err := s.userStore(r).BookingConflict(r.Context(), booking.ID, command)
		if err != nil {
			slog.Warn("booking conflict lookup failed", "booking_id", booking.ID, "error", err)
			break
		}
		existingJob = conflict.Job
		if existingJob != nil && !existingJob.Status.Terminal() {
			code = "queue-pending"
		} else if conflict.Reservation && (existingJob == nil || existingJob.Status == model.JobSucceeded || existingJob.Status == model.JobOutcomeUnknown || existingJob.ConfirmationStartedAt != nil) {
			code = "queue-review"
		}
	default:
		if !booking.Enabled {
			code = "booking-disabled"
		} else if profile, err := s.userStore(r).GetProfile(r.Context(), booking.ProfileID); err == nil && !profile.Enabled {
			code = "profile-disabled"
		}
	}
	query := url.Values{"notice": {code}}
	if existingJob != nil && (code == "queue-pending" || code == "queue-review") {
		query.Set("job", strconv.FormatInt(existingJob.ID, 10))
	}
	location := url.URL{Path: "/bookings", RawQuery: query.Encode()}
	http.Redirect(w, r, location.String(), http.StatusSeeOther)
}

func (s *Server) bookingInput(r *http.Request, id int64) (model.BookingRequest, error) {
	intField := func(name string) (int, error) { return strconv.Atoi(strings.TrimSpace(r.Form.Get(name))) }
	floatField := func(name string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(r.Form.Get(name)), 64) }
	prep, err := intField("prep_minutes_before")
	if err != nil {
		return model.BookingRequest{}, errors.New("prep minutes must be a number")
	}
	authDeadline, err := intField("auth_deadline_minutes_before")
	if err != nil {
		return model.BookingRequest{}, errors.New("auth deadline must be a number")
	}
	pollDeadline, err := intField("poll_deadline_seconds")
	if err != nil {
		return model.BookingRequest{}, errors.New("poll deadline must be a number")
	}
	pollMin, err := floatField("poll_min_seconds")
	if err != nil {
		return model.BookingRequest{}, errors.New("minimum poll delay must be a number")
	}
	pollMax, err := floatField("poll_max_seconds")
	if err != nil {
		return model.BookingRequest{}, errors.New("maximum poll delay must be a number")
	}
	request := model.BookingRequest{
		ID:                        id,
		LakeID:                    r.Form.Get("lake_id"),
		Name:                      r.Form.Get("name"),
		ProfileID:                 parseInt64(r.Form.Get("profile_id")),
		Enabled:                   checked(r, "enabled"),
		ScheduleEnabled:           checked(r, "schedule_enabled"),
		TargetDate:                r.Form.Get("target_date"),
		Timezone:                  r.Form.Get("timezone"),
		ReleaseTime:               r.Form.Get("release_time"),
		PrepMinutesBefore:         prep,
		AuthDeadlineMinutesBefore: authDeadline,
		PollDeadlineSeconds:       pollDeadline,
		PollMinSeconds:            pollMin,
		PollMaxSeconds:            pollMax,
		ConfirmationMode:          model.RunMode(r.Form.Get("confirmation_mode")),
		AllDayPassURL:             r.Form.Get("all_day_pass_url"),
		HalfDayPassURL:            r.Form.Get("half_day_pass_url"),
		CheckAllDay:               checked(r, "check_all_day"),
		CheckAfternoon:            checked(r, "check_afternoon"),
		CheckMorning:              checked(r, "check_morning"),
	}
	for slot := 1; slot <= 3; slot++ {
		if r.Form.Has(fmt.Sprintf("pass_priority_%d", slot)) {
			request.PreferredPasses = []model.PassType{}
			break
		}
	}
	if request.PreferredPasses != nil {
		for slot := 1; slot <= 3; slot++ {
			if value := r.Form.Get(fmt.Sprintf("pass_priority_%d", slot)); value != "" {
				request.PreferredPasses = append(request.PreferredPasses, model.PassType(value))
			}
		}
	}
	return request, request.ValidateForOrigins(s.config.YodelOrigins)
}

func (s *Server) bookingForm(w http.ResponseWriter, r *http.Request, booking *model.BookingRequest, formError string) {
	profiles, err := s.userStore(r).ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	creating := booking == nil
	defaultYodelOrigin := config.DefaultYodelOrigin
	if len(s.config.YodelOrigins) > 0 {
		defaultYodelOrigin = s.config.YodelOrigins[0]
	}
	lakeID := r.URL.Query().Get("lake_id")
	if booking != nil {
		lakeID = booking.EffectiveLakeID()
	}
	if r.Method == http.MethodPost {
		lakeID = r.Form.Get("lake_id")
	}
	lake, err := destinations.Resolve(lakeID)
	if err != nil {
		if r.Method == http.MethodGet {
			http.Error(w, "unsupported lake", http.StatusBadRequest)
			return
		}
		// Keep the rejected choice visible while rendering validation errors.
		lake, _ = destinations.Resolve(destinations.DefaultLakeID)
	}
	lake = lake.WithOrigin(defaultYodelOrigin)
	localTimezone, err := time.LoadLocation(lake.Timezone)
	if err != nil {
		localTimezone = time.Local
	}
	value := model.BookingRequest{
		LakeID:                    lake.ID,
		Enabled:                   true,
		TargetDate:                time.Now().In(localTimezone).AddDate(0, 0, 1).Format(time.DateOnly),
		Timezone:                  lake.Timezone,
		ReleaseTime:               lake.ReleaseTime,
		PrepMinutesBefore:         30,
		AuthDeadlineMinutesBefore: 5,
		PollDeadlineSeconds:       120,
		PollMinSeconds:            1.4,
		PollMaxSeconds:            3.6,
		ConfirmationMode:          model.RunModeManual,
		AllDayPassURL:             lake.AllDayPassURL,
		HalfDayPassURL:            lake.HalfDayPassURL,
	}
	for _, pass := range lake.SupportedPasses {
		value.PreferredPasses = append(value.PreferredPasses, model.PassType(pass))
	}
	if booking != nil {
		value = *booking
	} else if r.Method == http.MethodGet {
		selectedID := parseInt64(r.URL.Query().Get("profile_id"))
		for _, profile := range profiles {
			if profile.Enabled && profile.ID == selectedID {
				value.ProfileID = selectedID
				break
			}
		}
	}
	if r.Method == http.MethodPost {
		parsed, _ := s.bookingInput(r, value.ID)
		value = parsed
	}
	lakeOptions := make([]selectOption, 0, len(destinations.List()))
	for _, supported := range destinations.List() {
		lakeOptions = append(lakeOptions, lakeSelectOption(supported.WithOrigin(defaultYodelOrigin), value.EffectiveLakeID()))
	}
	if _, err := destinations.Resolve(value.LakeID); err != nil {
		lakeOptions = append(lakeOptions, selectOption{Value: value.LakeID, Label: "Unsupported lake — choose a supported lake", Selected: true})
	}
	profileOptions := []selectOption{{Value: "", Label: "Choose a profile", Selected: value.ProfileID == 0}}
	for _, profile := range profiles {
		if !profile.Enabled && (booking == nil || profile.ID != booking.ProfileID) {
			continue
		}
		profileOptions = append(profileOptions, selectOption{Value: strconv.FormatInt(profile.ID, 10), Label: profile.Name, Selected: profile.ID == value.ProfileID})
	}
	passFields := make([]formField, 3)
	order := value.PassOrder()
	for i, label := range []string{"First choice", "Second choice", "Third choice"} {
		selected := ""
		if i < len(order) {
			selected = string(order[i])
		}
		options := make([]selectOption, 0, len(lake.SupportedPasses)+1)
		for _, pass := range lake.SupportedPasses {
			options = append(options, selectOption{Value: pass, Label: passOptionLabel(pass)})
		}
		options = append(options, selectOption{Value: "", Label: "None"})
		for j := range options {
			options[j].Selected = options[j].Value == selected
		}
		passFields[i] = formField{Name: fmt.Sprintf("pass_priority_%d", i+1), Label: label, Type: "select", Options: options}
	}
	actionURL, heading, submit := "/bookings/new", "New booking request", "Create booking"
	if !creating {
		actionURL, heading, submit = fmt.Sprintf("/bookings/%d", booking.ID), "Edit booking request", "Save booking"
	}
	data := formData{
		BaseData:      base(r, heading),
		Eyebrow:       "Booking policy",
		Heading:       heading,
		Description:   lakeReleasePolicy(lake),
		CancelURL:     "/bookings",
		ActionURL:     actionURL,
		SubmitLabel:   submit,
		FormError:     formError,
		LakeSelection: true,
	}
	autoQueueHelp := "Turning this off does not cancel jobs already queued; cancel them from Jobs."
	if !s.config.SchedulesEnabled {
		autoQueueHelp = "Auto-queueing is currently off for this server, so this option will not create jobs. " + autoQueueHelp
	}
	data.Sections = []formSection{
		{
			Title: "Request",
			Fields: []formField{
				{Name: "lake_id", Label: "Lake", Type: "select", Required: true, Options: lakeOptions, Help: "Choose where you want to book a pass."},
				{Name: "name", Label: "Name", Type: "text", Value: value.Name, Required: true},
				{Name: "profile_id", Label: "Yodel profile", Type: "select", Required: true, Options: profileOptions},
				{Name: "target_date", Label: "Target date", Type: "date", Value: value.TargetDate, Required: true},
				{Name: "timezone", Label: "Timezone", Type: "text", Value: value.Timezone, Required: true},
				{Name: "release_time", Label: "Release time", Type: "time", Value: value.ReleaseTime, Required: true},
				{
					Name:     "confirmation_mode",
					Label:    "Final confirmation for release jobs",
					Help:     "Automatic confirms the booking without asking. Manual waits for your approval. Book now always requires approval.",
					Type:     "select",
					Required: true,
					Options: []selectOption{
						{Value: "manual", Label: "Manual approval", Selected: value.ConfirmationMode == model.RunModeManual},
						{Value: "auto", Label: "Automatic final confirmation", Selected: value.ConfirmationMode == model.RunModeAuto},
					},
				},
				{Name: "enabled", Label: "Enabled", Type: "checkbox", Checked: value.Enabled},
				{Name: "schedule_enabled", Label: "Automatically create a job when preparation starts", Type: "checkbox", Checked: value.ScheduleEnabled, Help: autoQueueHelp},
			},
		},
		{
			Title: "Pass URLs",
			Help:  "Defaults come from the selected lake. Custom URLs must stay on an operator-approved booking site.",
			Fields: []formField{
				{Name: "all_day_pass_url", Label: "All-day pass URL", Type: "url", Value: value.AllDayPassURL},
				{Name: "half_day_pass_url", Label: "Half-day pass URL", Type: "url", Value: value.HalfDayPassURL},
			},
		},
		{
			Title:  "Pass order",
			Help:   "Try choices in order. Choose None to skip a slot; select each pass only once.",
			Fields: passFields,
		},
		{
			Title: "Timing",
			Fields: []formField{
				{
					Name:     "prep_minutes_before",
					Label:    "Prep minutes before",
					Type:     "number",
					Value:    strconv.Itoa(value.PrepMinutesBefore),
					Required: true,
					Step:     "1",
					Min:      "0",
					Max:      strconv.Itoa(model.MaxPrepMinutesBefore),
				},
				{
					Name:     "auth_deadline_minutes_before",
					Label:    "Auth deadline minutes before",
					Type:     "number",
					Value:    strconv.Itoa(value.AuthDeadlineMinutesBefore),
					Required: true,
					Step:     "1",
					Min:      "0",
					Max:      strconv.Itoa(model.MaxPrepMinutesBefore),
				},
				{
					Name:     "poll_deadline_seconds",
					Label:    "Poll deadline seconds",
					Type:     "number",
					Value:    strconv.Itoa(value.PollDeadlineSeconds),
					Required: true,
					Step:     "1",
					Min:      "1",
					Max:      "900",
				},
				{
					Name:     "poll_min_seconds",
					Label:    "Minimum poll delay",
					Type:     "number",
					Value:    strconv.FormatFloat(value.PollMinSeconds, 'f', -1, 64),
					Required: true,
					Step:     "0.1",
					Min:      "0.1",
					Max:      "60",
				},
				{
					Name:     "poll_max_seconds",
					Label:    "Maximum poll delay",
					Type:     "number",
					Value:    strconv.FormatFloat(value.PollMaxSeconds, 'f', -1, 64),
					Required: true,
					Step:     "0.1",
					Min:      "0.1",
					Max:      "60",
				},
			},
		},
	}
	if r.Method == http.MethodPost {
		// Render submitted values even when parsing an earlier numeric field
		// failed. In particular, keep None gaps and duplicate choices visible.
		for i := range data.Sections {
			for j := range data.Sections[i].Fields {
				field := &data.Sections[i].Fields[j]
				if field.Type == "select" {
					selected := r.Form.Get(field.Name)
					for k := range field.Options {
						field.Options[k].Selected = field.Options[k].Value == selected
					}
				} else if field.Type == "checkbox" {
					field.Checked = checked(r, field.Name)
				} else {
					field.Value = r.Form.Get(field.Name)
				}
			}
		}
	}
	s.render(w, formStatus(formError), "form", data)
}
