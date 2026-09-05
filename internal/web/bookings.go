package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func (s *Server) bookings(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	bookings, err := userStore.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	profiles, _ := userStore.ListProfiles(r.Context())
	names := map[int64]string{}
	for _, profile := range profiles {
		names[profile.ID] = profile.Name
	}
	data := listData{
		BaseData:     base(r, "Bookings"),
		Eyebrow:      "Release schedule",
		Heading:      "Booking requests",
		Description:  "Target dates release one day earlier. Session warming begins at the preparation time.",
		CreateURL:    "/bookings/new",
		CreateLabel:  "New booking",
		EmptyMessage: "Create a profile, then add a booking request.",
	}
	for _, booking := range bookings {
		status, class := "Manual queue", ""
		if booking.Enabled && booking.ScheduleEnabled {
			status, class = "Scheduled", "ok"
		} else if !booking.Enabled {
			status = "Disabled"
		}
		url := fmt.Sprintf("/bookings/%d", booking.ID)
		data.Cards = append(data.Cards, listCard{
			Title:       booking.Name,
			Subtitle:    names[booking.ProfileID],
			Status:      status,
			StatusClass: class,
			URL:         url,
			Fields: []labelValue{
				{"Target date", booking.TargetDate},
				{"Release", booking.ReleaseTime + " · " + booking.Timezone},
				{"Confirmation", string(booking.ConfirmationMode)},
				{"Pass order", strings.Join(passNames(booking.PassOrder()), " → ")},
			},
			Actions: []cardAction{{"Edit", url, ""}},
			PostActions: []postAction{
				{Label: "Auth check", URL: url + "/run", Fields: []hiddenField{{"command", "auth-check"}}},
				{Label: "Dry run", URL: url + "/run", Fields: []hiddenField{{"command", "dry-run"}}},
				{Label: "Queue booking", URL: url + "/run", Class: "primary", Fields: []hiddenField{{"command", "book"}}},
			},
		})
	}
	s.render(w, http.StatusOK, "list", data)
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
	if _, err := s.userStore(r).GetBookingRequest(r.Context(), id); err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	command := model.JobCommand(r.Form.Get("command"))
	if !command.Valid() {
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	}
	mode := model.RunMode("")
	if command == model.CommandDryRun {
		mode = model.RunModeDryRun
	}
	job, err := s.engine.QueueBooking(r.Context(), requestAuth(r).Authenticated.User.ID, id, command, mode)
	if err != nil {
		slog.Warn("booking job could not be queued", "booking_id", id, "command", command, "error", err)
		http.Error(w, "job could not be queued", http.StatusConflict)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d?ok=queued", job.ID), http.StatusSeeOther)
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
		LoginProbeURL:             r.Form.Get("login_probe_url"),
		AllDayPassURL:             r.Form.Get("all_day_pass_url"),
		HalfDayPassURL:            r.Form.Get("half_day_pass_url"),
		CheckAllDay:               checked(r, "check_all_day"),
		CheckAfternoon:            checked(r, "check_afternoon"),
		CheckMorning:              checked(r, "check_morning"),
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
	yodelBaseURL := strings.TrimRight(defaultYodelOrigin, "/") + "/buntzen-lake"
	localTimezone, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		localTimezone = time.Local
	}
	value := model.BookingRequest{
		Enabled:                   true,
		TargetDate:                time.Now().In(localTimezone).AddDate(0, 0, 1).Format(time.DateOnly),
		Timezone:                  "America/Vancouver",
		ReleaseTime:               "07:00",
		PrepMinutesBefore:         30,
		AuthDeadlineMinutesBefore: 5,
		PollDeadlineSeconds:       120,
		PollMinSeconds:            1.4,
		PollMaxSeconds:            3.6,
		ConfirmationMode:          model.RunModeManual,
		LoginProbeURL:             yodelBaseURL,
		AllDayPassURL:             yodelBaseURL + "/All-Day-Pass",
		HalfDayPassURL:            yodelBaseURL + "/Half-Day-Pass",
		CheckAllDay:               true,
		CheckAfternoon:            true,
		CheckMorning:              true,
	}
	if booking != nil {
		value = *booking
	}
	if r.Method == http.MethodPost {
		parsed, _ := s.bookingInput(r, value.ID)
		value = parsed
	}
	profileOptions := make([]selectOption, 0, len(profiles))
	for _, profile := range profiles {
		profileOptions = append(profileOptions, selectOption{Value: strconv.FormatInt(profile.ID, 10), Label: profile.Name, Selected: profile.ID == value.ProfileID})
	}
	actionURL, heading, submit := "/bookings/new", "New booking request", "Create booking"
	if !creating {
		actionURL, heading, submit = fmt.Sprintf("/bookings/%d", booking.ID), "Edit booking request", "Save booking"
	}
	data := formData{
		BaseData:    base(r, heading),
		Eyebrow:     "Booking policy",
		Heading:     heading,
		Description: "The release occurs at the selected time on target date minus one day.",
		CancelURL:   "/bookings",
		ActionURL:   actionURL,
		SubmitLabel: submit,
		FormError:   formError,
	}
	data.Sections = []formSection{
		{
			Title: "Request",
			Fields: []formField{
				{Name: "name", Label: "Name", Type: "text", Value: value.Name, Required: true},
				{Name: "profile_id", Label: "Yodel profile", Type: "select", Required: true, Options: profileOptions},
				{Name: "target_date", Label: "Target date", Type: "date", Value: value.TargetDate, Required: true},
				{Name: "timezone", Label: "Timezone", Type: "text", Value: value.Timezone, Required: true},
				{Name: "release_time", Label: "Release time", Type: "time", Value: value.ReleaseTime, Required: true},
				{
					Name:     "confirmation_mode",
					Label:    "Confirmation",
					Type:     "select",
					Required: true,
					Options: []selectOption{
						{Value: "manual", Label: "Manual approval", Selected: value.ConfirmationMode == model.RunModeManual},
						{Value: "auto", Label: "Automatic confirmation", Selected: value.ConfirmationMode == model.RunModeAuto},
					},
				},
				{Name: "enabled", Label: "Enabled", Type: "checkbox", Checked: value.Enabled},
				{Name: "schedule_enabled", Label: "Auto-queue in prep window", Type: "checkbox", Checked: value.ScheduleEnabled},
			},
		},
		{
			Title: "Yodel URLs",
			Fields: []formField{
				{Name: "login_probe_url", Label: "Login probe URL", Type: "url", Value: value.LoginProbeURL, Required: true},
				{Name: "all_day_pass_url", Label: "All-day pass URL", Type: "url", Value: value.AllDayPassURL},
				{Name: "half_day_pass_url", Label: "Half-day pass URL", Type: "url", Value: value.HalfDayPassURL},
			},
		},
		{
			Title: "Pass order",
			Help:  "Selection order is fixed: all-day, then afternoon, then morning.",
			Fields: []formField{
				{Name: "check_all_day", Label: "All-day", Type: "checkbox", Checked: value.CheckAllDay},
				{Name: "check_afternoon", Label: "Afternoon", Type: "checkbox", Checked: value.CheckAfternoon},
				{Name: "check_morning", Label: "Morning", Type: "checkbox", Checked: value.CheckMorning},
			},
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
	s.render(w, formStatus(formError), "form", data)
}
