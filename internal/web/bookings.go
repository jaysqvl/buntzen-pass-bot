package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	profiles, err := userStore.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	profilesByID := map[int64]model.Profile{}
	for _, profile := range profiles {
		profilesByID[profile.ID] = profile
	}
	data := listData{
		BaseData:     base(r, "Bookings"),
		Eyebrow:      "Pass bookings",
		Heading:      "Booking requests",
		Description:  "Choose a lake and visit date, then queue your request for release or check passes that are already available.",
		CreateURL:    "/bookings/new",
		CreateLabel:  "New booking",
		EmptyMessage: "Add a Yodel sign-in on Home, then create a booking request.",
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
	now := time.Now()
	for _, booking := range bookings {
		conflict, err := bookingPresentationConflict(r.Context(), userStore, booking.ID)
		if err != nil {
			s.internal(w)
			return
		}
		data.Cards = append(data.Cards, bookingCard(booking, profilesByID[booking.ProfileID], s.config.SchedulesEnabled, conflict, now))
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
	return releaseDaysPolicy(lake.ReleaseDaysBefore)
}

func releaseDaysPolicy(days int) string {
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	return fmt.Sprintf("Passes release %d %s before your visit at the configured local time.", days, unit)
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
	ID                string           `json:"id"`
	Timezone          string           `json:"timezone"`
	ReleaseTime       string           `json:"releaseTime"`
	ReleaseDaysBefore int              `json:"releaseDaysBefore"`
	AllDayPassURL     string           `json:"allDayPassURL"`
	HalfDayPassURL    string           `json:"halfDayPassURL"`
	ReleasePolicy     string           `json:"releasePolicy"`
	Passes            []lakePassOption `json:"passes"`
	PreferredPasses   []string         `json:"preferredPasses"`
	VehicleKeyword    string           `json:"vehicleKeyword"`
	Profiles          []lakePassOption `json:"profiles"`
	SettingsURL       string           `json:"settingsURL"`
}

func lakeSelectOption(lake destinations.Lake, settings model.LakeSettings, selectedID string, profiles []model.Profile, currentProfileID int64) selectOption {
	defaults := lakeFormDefaults{
		ID: lake.ID, Timezone: settings.Timezone, ReleaseTime: settings.ReleaseTime,
		ReleaseDaysBefore: settings.ReleaseDaysBefore,
		AllDayPassURL:     settings.AllDayPassURL, HalfDayPassURL: settings.HalfDayPassURL,
		ReleasePolicy:  releaseDaysPolicy(settings.ReleaseDaysBefore),
		VehicleKeyword: settings.VehicleKeyword,
		Profiles:       []lakePassOption{}, SettingsURL: "/lakes/" + url.PathEscape(lake.ID),
	}
	for _, pass := range lake.SupportedPasses {
		defaults.Passes = append(defaults.Passes, lakePassOption{Value: pass, Label: passOptionLabel(pass)})
	}
	for _, pass := range settings.PreferredPasses {
		defaults.PreferredPasses = append(defaults.PreferredPasses, string(pass))
	}
	for _, profile := range profiles {
		if profile.EffectiveProviderID() == lake.ProviderID && (profile.Enabled || profile.ID == currentProfileID) {
			defaults.Profiles = append(defaults.Profiles, lakePassOption{Value: strconv.FormatInt(profile.ID, 10), Label: bookingProfileLabel(profile)})
		}
	}
	// Only values and slices are encoded; html/template escapes the result
	// as an attribute, and the external client parses it as data, never code.
	encoded, _ := json.Marshal(defaults)
	return selectOption{Value: lake.ID, Label: lake.Name, Selected: lake.ID == selectedID, LakeDefaults: string(encoded)}
}

func bookingProfileLabel(profile model.Profile) string {
	if !profile.Enabled {
		return profile.Name + " · disabled"
	}
	return profile.Name
}

func bookingPresentationConflict(ctx context.Context, resources store.UserStore, bookingID int64) (store.BookingConflict, error) {
	bookingConflict, err := resources.BookingConflict(ctx, bookingID, model.CommandBook)
	if err != nil || (bookingConflict.Job != nil && !bookingConflict.Job.Status.Terminal()) {
		return bookingConflict, err
	}
	for _, command := range []model.JobCommand{model.CommandDryRun, model.CommandAuthCheck} {
		conflict, err := resources.BookingConflict(ctx, bookingID, command)
		if err != nil || conflict.Job != nil {
			return conflict, err
		}
	}
	return bookingConflict, nil
}

func bookingCard(booking model.BookingRequest, profile model.Profile, schedulesEnabled bool, conflict store.BookingConflict, now time.Time) listCard {
	job := conflict.Job
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
	if job != nil && !job.Status.Terminal() && job.Command == model.CommandBook {
		// The saved job controls checkout. Book now always requires approval,
		// even when the booking request specifies automatic confirmation.
		confirmation = jobModeLabel(*job)
	}
	release := booking.ReleaseTime + " · " + booking.Timezone
	window, windowErr := scheduler.WindowFor(booking)
	if windowErr == nil {
		release = formatJobTime(window.ReleaseAt, window.ReleaseAt.Location())
	}
	url := fmt.Sprintf("/bookings/%d", booking.ID)
	card := listCard{
		Title:       booking.Name,
		Subtitle:    profile.Name,
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
	}
	if job != nil && !job.Status.Terminal() {
		card.Status, card.StatusClass = jobStatusLabel(*job), statusClass(job.Status)
		card.Fields = append(card.Fields,
			labelValue{"Current job", fmt.Sprintf("Job %d · %s", job.ID, card.Status)},
			labelValue{"Earliest start", formatJobTime(job.DueAt, pendingJobLocation(*job, booking))},
		)
		card.URL = fmt.Sprintf("/jobs/%d", job.ID)
		card.Actions = []cardAction{{"View job", card.URL, "primary"}}
		card.Description = "This request has a pending job. View it to follow progress or cancel before making changes."
		return card
	}
	if conflict.Reservation {
		card.Description = "This sign-in already has a confirmed or unresolved booking for this visit date. Review it before trying again."
		if job != nil {
			card.Status, card.StatusClass = jobStatusLabel(*job), statusClass(job.Status)
			card.Actions = append(card.Actions, cardAction{"View booking job", fmt.Sprintf("/jobs/%d", job.ID), "primary"})
		} else {
			card.Status, card.StatusClass = "Review needed", "warn"
		}
		return card
	}
	if !booking.Enabled {
		card.Description = "Enable this request to queue a booking."
		return card
	}
	if !profile.Enabled {
		card.Description = "Enable the booking sign-in or choose another sign-in before queueing this request."
		card.Actions = append(card.Actions, cardAction{"Edit sign-in", fmt.Sprintf("/profiles/%d", profile.ID), ""})
		return card
	}
	if windowErr != nil {
		card.Description = "Review the request settings before queueing a job."
		return card
	}
	if now.In(window.ReleaseAt.Location()).Format(time.DateOnly) > booking.TargetDate {
		card.Status, card.StatusClass = "Visit date passed", ""
		card.Description = "Choose a new visit date to use this request again."
		return card
	}
	if now.Before(window.ReleaseAt) {
		card.Description = "Passes have not released yet. Queue now to start preparation at the scheduled time."
		card.PostActions = append(card.PostActions, postAction{Label: "Queue for release", URL: url + "/run", Class: "primary", Fields: []hiddenField{{"command", "book"}}})
	} else {
		card.Description = "Check released passes now. Final confirmation requires your approval; the job expires after 15 minutes."
		card.PostActions = append(card.PostActions, postAction{Label: "Book now · manual approval", URL: url + "/run", Class: "primary", Fields: []hiddenField{{"command", "book"}, {"timing", "now"}}})
	}
	card.PostActions = append(card.PostActions,
		postAction{Label: "Booking rehearsal", URL: url + "/run", Fields: []hiddenField{{"command", "dry-run"}}},
		postAction{Label: "Sign-in check", URL: url + "/run", Fields: []hiddenField{{"command", "auth-check"}}},
	)
	return card
}

func passNames(values []model.PassType) []string {
	result := make([]string, len(values))
	for i, value := range values {
		switch value {
		case model.PassAllDay:
			result[i] = "All-day"
		case model.PassMorning:
			result[i] = "Morning"
		case model.PassAfternoon:
			result[i] = "Afternoon"
		default:
			result[i] = strings.ReplaceAll(string(value), "_", " ")
		}
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
		if !r.Form.Has("release_days_before") && request.EffectiveLakeID() == current.EffectiveLakeID() {
			request.ReleaseDaysBefore = current.ReleaseDaysBefore
		}
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
	var releaseDaysBefore *int
	if r.Form.Has("release_days_before") {
		days, err := intField("release_days_before")
		if err != nil {
			return model.BookingRequest{}, errors.New("release days before visit must be a number")
		}
		releaseDaysBefore = &days
	}
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
		ReleaseDaysBefore:         releaseDaysBefore,
		PrepMinutesBefore:         prep,
		AuthDeadlineMinutesBefore: authDeadline,
		PollDeadlineSeconds:       pollDeadline,
		PollMinSeconds:            pollMin,
		PollMaxSeconds:            pollMax,
		ConfirmationMode:          model.RunMode(r.Form.Get("confirmation_mode")),
		AllDayPassURL:             r.Form.Get("all_day_pass_url"),
		HalfDayPassURL:            r.Form.Get("half_day_pass_url"),
		VehicleKeyword:            r.Form.Get("vehicle_keyword"),
		CheckAllDay:               checked(r, "check_all_day"),
		CheckAfternoon:            checked(r, "check_afternoon"),
		CheckMorning:              checked(r, "check_morning"),
	}
	if !r.Form.Has("vehicle_keyword") {
		if id > 0 {
			current, err := s.userStore(r).GetBookingRequest(r.Context(), id)
			if err != nil {
				return request, err
			}
			request.VehicleKeyword = current.VehicleKeyword
		} else if profile, err := s.userStore(r).GetProfile(r.Context(), request.ProfileID); err == nil {
			// Older clients carried the vehicle only on their sign-in profile.
			request.VehicleKeyword = profile.DefaultVehicle
		}
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
	lakeID := r.URL.Query().Get("lake_id")
	if booking != nil {
		lakeID = booking.EffectiveLakeID()
	}
	if r.Method == http.MethodPost {
		lakeID = r.Form.Get("lake_id")
	}
	if _, err := destinations.Resolve(lakeID); err != nil {
		if r.Method == http.MethodGet {
			http.Error(w, "unsupported lake", http.StatusBadRequest)
			return
		}
		// Keep the rejected choice visible while rendering validation errors.
		lakeID = destinations.DefaultLakeID
	}
	lake, settings, _, err := s.effectiveLakeSettings(r, lakeID)
	if err != nil {
		s.internal(w)
		return
	}
	localTimezone, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		localTimezone = time.Local
	}
	value := settings.ApplyTo(model.BookingRequest{
		LakeID:           lake.ID,
		Enabled:          true,
		TargetDate:       time.Now().In(localTimezone).AddDate(0, 0, 1).Format(time.DateOnly),
		ConfirmationMode: model.RunModeManual,
	})
	if booking != nil {
		value = *booking
	} else if r.Method == http.MethodGet {
		accountSettings, err := s.userStore(r).GetAccountSettings(r.Context())
		if err != nil {
			s.internal(w)
			return
		}
		value = accountSettings.ApplyToBooking(value)
		selectedID := parseInt64(r.URL.Query().Get("profile_id"))
		for _, profile := range profiles {
			if profile.Enabled && profile.ID == selectedID && profile.EffectiveProviderID() == lake.ProviderID {
				value.ProfileID = selectedID
				break
			}
		}
	}
	if r.Method == http.MethodPost {
		parsed, _ := s.bookingInput(r, value.ID)
		value = parsed
		if booking != nil && !r.Form.Has("vehicle_keyword") {
			value.VehicleKeyword = booking.VehicleKeyword
		}
		// The full parser can stop at an earlier invalid field. Keep the
		// visible policy in sync with a valid submitted day offset regardless.
		if r.Form.Has("release_days_before") {
			if days, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("release_days_before"))); err == nil && days >= 0 && days <= model.MaxReleaseDaysBefore {
				value.ReleaseDaysBefore = &days
			}
		} else if booking != nil && lake.ID == booking.EffectiveLakeID() {
			value.ReleaseDaysBefore = booking.ReleaseDaysBefore
		}
	}
	selectedLakeID, selectedProfileID := value.EffectiveLakeID(), value.ProfileID
	if r.Method == http.MethodPost {
		selectedLakeID, selectedProfileID = r.Form.Get("lake_id"), parseInt64(r.Form.Get("profile_id"))
		if selectedLakeID == "" {
			selectedLakeID = destinations.DefaultLakeID
		}
	}
	currentProfileID := int64(0)
	if booking != nil {
		currentProfileID = booking.ProfileID
	}
	lakeOptions := make([]selectOption, 0, len(destinations.List()))
	for _, supported := range destinations.List() {
		definition, defaults, _, err := s.effectiveLakeSettings(r, supported.ID)
		if err != nil {
			s.internal(w)
			return
		}
		lakeOptions = append(lakeOptions, lakeSelectOption(definition, defaults, selectedLakeID, profiles, currentProfileID))
	}
	if _, err := destinations.Resolve(selectedLakeID); err != nil {
		lakeOptions = append(lakeOptions, selectOption{Value: selectedLakeID, Label: "Unsupported lake — choose a supported lake", Selected: true})
	}
	profileOptions := []selectOption{{Value: "", Label: "Choose a sign-in", Selected: selectedProfileID == 0}}
	for _, profile := range profiles {
		if profile.EffectiveProviderID() != lake.ProviderID || (!profile.Enabled && profile.ID != currentProfileID) {
			continue
		}
		profileOptions = append(profileOptions, selectOption{Value: strconv.FormatInt(profile.ID, 10), Label: bookingProfileLabel(profile), Selected: profile.ID == selectedProfileID})
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
	actionURL, heading, submit := "/bookings/new", "New booking request", "Create request"
	if !creating {
		actionURL, heading, submit = fmt.Sprintf("/bookings/%d", booking.ID), "Edit booking request", "Save changes"
	}
	data := formData{
		BaseData:        base(r, heading),
		Eyebrow:         "Bookings",
		Heading:         heading,
		Description:     releaseDaysPolicy(value.EffectiveReleaseDaysBefore()),
		CancelURL:       "/bookings",
		ActionURL:       actionURL,
		SubmitLabel:     submit,
		FormError:       formError,
		LakeSelection:   true,
		LakeSettingsURL: "/lakes/" + url.PathEscape(lake.ID),
		AdvancedHelp:    "Release rules, preparation timing, and booking URLs are copied from your defaults. Override them here for this request only.",
		SubmitHelp:      "Save this request, then manage its jobs from Bookings.",
	}
	if len(profileOptions) == 1 {
		data.Flash = &Flash{Kind: "info", Message: "Add or enable a Yodel sign-in on Home before saving a booking request.", ActionLabel: "Manage sign-ins", ActionURL: "/#yodel-sign-in"}
	}
	if creating {
		hasEnabledSignIn := false
		for _, profile := range profiles {
			hasEnabledSignIn = hasEnabledSignIn || profile.Enabled
		}
		data.SubmitDisabled = !hasEnabledSignIn
		if data.SubmitDisabled {
			data.SubmitHelp = "Add or enable a booking sign-in on Home, then return to create this request."
		}
	} else {
		conflict, err := bookingPresentationConflict(r.Context(), s.userStore(r), booking.ID)
		if err != nil {
			s.internal(w)
			return
		}
		if job := conflict.Job; job != nil && !job.Status.Terminal() && job.BookingRequestID != nil && *job.BookingRequestID == booking.ID {
			data.SubmitDisabled = true
			data.SubmitHelp = "This request cannot be changed while its job is pending."
			data.Flash = &Flash{Kind: "info", Message: "A pending job is using this request. View the job to follow progress or cancel before editing.", ActionLabel: "View job", ActionURL: fmt.Sprintf("/jobs/%d", job.ID)}
		}
	}
	autoQueueHelp := "Turning this off does not cancel jobs already queued; cancel them from Jobs."
	if !s.config.SchedulesEnabled {
		autoQueueHelp = "Auto-queueing is currently off for this server, so this option will not create jobs. " + autoQueueHelp
	}
	data.Sections = []formSection{
		{
			Title: "Visit details",
			Fields: []formField{
				{Name: "lake_id", Label: "Lake", Type: "select", Required: true, Options: lakeOptions},
				{Name: "target_date", Label: "Visit date", Type: "date", Value: value.TargetDate, Required: true},
				{Name: "name", Label: "Request name", Type: "text", Value: value.Name, Required: true, Wide: true, Help: "A name to help you recognize this visit in Bookings."},
			},
		},
		{
			Title:   "Booking account and vehicle",
			Help:    "Choose the account to book with and a vehicle saved in that account.",
			HelpURL: "/#yodel-sign-in", HelpLabel: "Manage Yodel sign-ins",
			Fields: []formField{
				{Name: "profile_id", Label: "Booking sign-in", Type: "select", Required: true, Options: profileOptions},
				{Name: "vehicle_keyword", Label: "Vehicle keyword", Type: "text", Value: value.VehicleKeyword, Required: true, Help: "Use a unique vehicle name or licence plate. Your lake default is filled in automatically."},
			},
		},
		{
			Title:  "Pass preferences",
			Help:   "Try choices in order. Choose None to skip a slot; select each pass only once.",
			Class:  "form-grid-pass-preferences",
			Fields: passFields,
		},
		{
			Title:    "Release rules",
			Help:     "Copied from the selected lake. Use its local timezone and pass release schedule.",
			Advanced: true,
			Fields: []formField{
				{Name: "timezone", Label: "Timezone", Type: "text", Value: value.Timezone, Required: true},
				{Name: "release_time", Label: "Release time", Type: "time", Value: value.ReleaseTime, Required: true},
				{Name: "release_days_before", Label: "Days before visit", Type: "number", Value: strconv.Itoa(value.EffectiveReleaseDaysBefore()), Required: true, Min: "0", Max: strconv.Itoa(model.MaxReleaseDaysBefore), Step: "1", Help: "How many days before your visit passes become available."},
			},
		},
		{
			Title: "Automation and confirmation",
			Fields: []formField{
				{
					Name:     "confirmation_mode",
					Label:    "Booking confirmation",
					Help:     "For release jobs: wait for your approval or confirm automatically. Book now always requires approval.",
					Type:     "select",
					Required: true,
					Wide:     true,
					Options: []selectOption{
						{Value: "manual", Label: "Manual approval", Selected: value.ConfirmationMode == model.RunModeManual},
						{Value: "auto", Label: "Automatic final confirmation", Selected: value.ConfirmationMode == model.RunModeAuto},
					},
				},
				{Name: "enabled", Label: "Enable this request", Type: "checkbox", Checked: value.Enabled, Wide: true, Help: "Enabled requests can be queued manually or automatically."},
				{Name: "schedule_enabled", Label: "Automatically queue at preparation time", Type: "checkbox", Checked: value.ScheduleEnabled, Help: autoQueueHelp, Wide: true},
			},
		},
		{
			Title:    "Booking site URLs",
			Help:     "Defaults come from the selected lake. Custom URLs must stay on an operator-approved booking site.",
			Advanced: true,
			Fields: []formField{
				{Name: "all_day_pass_url", Label: "All-day pass URL", Type: "url", Value: value.AllDayPassURL},
				{Name: "half_day_pass_url", Label: "Half-day pass URL", Type: "url", Value: value.HalfDayPassURL},
			},
		},
		{
			Title:   "Preparation and retry timing",
			Help:    "Copied from your general Settings when this request is created. Changes here apply only to this request.",
			HelpURL: "/settings", HelpLabel: "Manage global defaults",
			Advanced: true,
			Fields: []formField{
				{
					Name:     "prep_minutes_before",
					Label:    "Start preparation (minutes before release)",
					Type:     "number",
					Value:    strconv.Itoa(value.PrepMinutesBefore),
					Required: true,
					Step:     "1",
					Min:      "0",
					Max:      strconv.Itoa(model.MaxPrepMinutesBefore),
				},
				{
					Name:     "auth_deadline_minutes_before",
					Label:    "Sign-in deadline (minutes before release)",
					Type:     "number",
					Value:    strconv.Itoa(value.AuthDeadlineMinutesBefore),
					Required: true,
					Step:     "1",
					Min:      "0",
					Max:      strconv.Itoa(model.MaxPrepMinutesBefore),
				},
				{
					Name:     "poll_deadline_seconds",
					Label:    "Availability check window (seconds)",
					Type:     "number",
					Value:    strconv.Itoa(value.PollDeadlineSeconds),
					Required: true,
					Step:     "1",
					Min:      "1",
					Max:      "900",
				},
				{
					Name:     "poll_min_seconds",
					Label:    "Minimum retry delay (seconds)",
					Type:     "number",
					Value:    strconv.FormatFloat(value.PollMinSeconds, 'f', -1, 64),
					Required: true,
					Step:     "0.05",
					Min:      "0.05",
					Max:      "60",
				},
				{
					Name:     "poll_max_seconds",
					Label:    "Maximum retry delay (seconds)",
					Type:     "number",
					Value:    strconv.FormatFloat(value.PollMaxSeconds, 'f', -1, 64),
					Required: true,
					Step:     "0.05",
					Min:      "0.05",
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
					if field.Name == "vehicle_keyword" && !r.Form.Has(field.Name) {
						continue
					}
					if field.Name == "release_days_before" && !r.Form.Has(field.Name) {
						continue // Older clients predate the per-request policy field.
					}
					field.Value = r.Form.Get(field.Name)
				}
			}
		}
	}
	s.render(w, formStatus(formError), "form", data)
}
