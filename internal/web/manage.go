package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

type cardAction struct{ Label, URL, Class string }
type hiddenField struct{ Name, Value string }
type postAction struct {
	Label, URL, Class string
	Fields            []hiddenField
}
type listCard struct {
	Title, Subtitle, Status, StatusClass, URL string
	Fields                                    []labelValue
	Actions                                   []cardAction
	PostActions                               []postAction
}
type listData struct {
	BaseData
	Eyebrow, Heading, Description, CreateURL, CreateLabel, EmptyMessage string
	Cards                                                               []listCard
}

type selectOption struct {
	Value, Label string
	Selected     bool
}
type formField struct {
	Name, Label, Type, Value, Placeholder, Help, Step string
	Required, Checked                                 bool
	Options                                           []selectOption
}
type formSection struct {
	Title, Help string
	Fields      []formField
}
type formData struct {
	BaseData
	Eyebrow, Heading, Description, CancelURL, ActionURL, SubmitLabel, FormError string
	Sections                                                                    []formSection
}

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	data := listData{BaseData: base(r, "OTP Sources"), Eyebrow: "Provider isolation", Heading: "OTP Sources", Description: "Each inbox is exclusive to one Yodel profile. Providers never fall back to one another.", CreateURL: "/sources/new", CreateLabel: "New OTP source", EmptyMessage: "Create a BlueBubbles or Twilio inbox."}
	for _, source := range sources {
		paired := "Ready"
		class := "ok"
		if source.Provider == model.OTPProviderBlueBubbles && (source.PairingChatGUID == "" || source.PairingSender == "" || source.PairingService == "") {
			paired, class = "Needs pairing", "warn"
		}
		postActions := []postAction{{Label: "Test connection", URL: fmt.Sprintf("/sources/%d/health", source.ID)}}
		if source.Provider == model.OTPProviderBlueBubbles {
			pairLabel := "Pair with Yodel"
			if paired == "Ready" {
				pairLabel = "Re-pair"
			}
			postActions = append(postActions, postAction{Label: pairLabel, URL: fmt.Sprintf("/sources/%d/pair", source.ID), Class: "primary"})
		}
		data.Cards = append(data.Cards, listCard{
			Title: source.Name, Subtitle: string(source.Provider), Status: paired, StatusClass: class, URL: fmt.Sprintf("/sources/%d", source.ID),
			Fields:      []labelValue{{"Inbox identity", source.Identity}, {"Paired sender", maskStoredSender(source.PairingSender)}},
			Actions:     []cardAction{{"Edit", fmt.Sprintf("/sources/%d", source.ID), ""}},
			PostActions: postActions,
		})
	}
	s.render(w, http.StatusOK, "list", data)
}

func maskStoredSender(value string) string {
	if value == "" {
		return "Not paired"
	}
	if at := strings.LastIndex(value, "@"); at > 0 {
		return value[:1] + "***" + value[at:]
	}
	digits := make([]rune, 0, len(value))
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) > 4 {
		return "***" + string(digits[len(digits)-4:])
	}
	return "***"
}

func (s *Server) sourceNew(w http.ResponseWriter, r *http.Request) {
	s.sourceForm(w, r, nil, "")
}

func (s *Server) sourceCreate(w http.ResponseWriter, r *http.Request) {
	input, err := s.sourceInput(r, nil)
	if err == nil {
		_, err = s.store.CreateOTPSource(r.Context(), input)
	}
	if err != nil {
		s.sourceForm(w, r, nil, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/sources?ok=created", http.StatusSeeOther)
}

func (s *Server) sourceEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	source, err := s.store.GetOTPSource(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	s.sourceForm(w, r, &source, "")
}

func (s *Server) sourceUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetOTPSource(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	input, err := s.sourceInput(r, &current)
	if err == nil {
		_, err = s.store.UpdateOTPSource(r.Context(), id, input)
	}
	if err != nil {
		s.sourceForm(w, r, &current, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/sources?ok=updated", http.StatusSeeOther)
}

func (s *Server) sourceHealth(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	source, err := s.store.GetOTPSource(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	provider, err := engine.ProviderForSource(r.Context(), s.store, source)
	if err == nil {
		ctx, cancel := contextWithTimeout(r, 15*time.Second)
		defer cancel()
		err = provider.Health(ctx)
	}
	if err != nil {
		http.Error(w, "provider health check failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/sources?ok=healthy", http.StatusSeeOther)
}

func (s *Server) sourcePair(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := s.engine.QueuePairing(r.Context(), id)
	if err != nil {
		http.Error(w, "pairing could not be started; assign the source to an enabled profile and booking first", http.StatusConflict)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d?ok=queued", job.ID), http.StatusSeeOther)
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

func (s *Server) sourceInput(r *http.Request, current *model.OTPSource) (store.OTPSourceInput, error) {
	provider := model.OTPProvider(strings.TrimSpace(r.Form.Get("provider")))
	input := store.OTPSourceInput{Name: r.Form.Get("name"), Provider: provider}
	if current != nil && provider == current.Provider {
		input.PairingChatGUID, input.PairingSender, input.PairingService = current.PairingChatGUID, current.PairingSender, current.PairingService
	}
	switch provider {
	case model.OTPProviderBlueBubbles:
		var cfg bluebubbles.Config
		if current != nil && current.Provider == provider {
			if err := s.store.GetOTPSourceConfig(r.Context(), current.ID, &cfg); err != nil {
				return input, err
			}
		}
		if value := strings.TrimSpace(r.Form.Get("bb_base_url")); value != "" {
			cfg.BaseURL = value
		}
		if value := r.Form.Get("bb_password"); value != "" {
			cfg.Password = value
		}
		identity, err := blueBubblesIdentity(cfg.BaseURL)
		if err != nil {
			return input, err
		}
		if _, err := bluebubbles.New(cfg); err != nil {
			return input, err
		}
		input.Identity, input.ProviderConfig = identity, cfg
	case model.OTPProviderTwilio:
		var cfg twilio.Config
		if current != nil && current.Provider == provider {
			if err := s.store.GetOTPSourceConfig(r.Context(), current.ID, &cfg); err != nil {
				return input, err
			}
		}
		if value := strings.TrimSpace(r.Form.Get("twilio_account_sid")); value != "" {
			cfg.AccountSID = value
		}
		if value := r.Form.Get("twilio_auth_token"); value != "" {
			cfg.AuthToken = value
		}
		if value := strings.TrimSpace(r.Form.Get("twilio_to_number")); value != "" {
			cfg.ToNumber = value
		}
		if value := strings.TrimSpace(r.Form.Get("twilio_sender")); value != "" {
			cfg.Sender = value
		}
		if cfg.AccountSID == "" || cfg.ToNumber == "" {
			return input, errors.New("Twilio account SID and receiving number are required")
		}
		if _, err := twilio.New(cfg); err != nil {
			return input, err
		}
		identity := sha256.Sum256([]byte(strings.ToUpper(cfg.AccountSID) + ":" + phoneIdentity(cfg.ToNumber)))
		input.Identity = fmt.Sprintf("twilio:%x", identity[:12])
		input.ProviderConfig = cfg
	default:
		return input, errors.New("select BlueBubbles or Twilio")
	}
	return input, nil
}

func blueBubblesIdentity(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("BlueBubbles URL must be a server root such as http://bluebubbles.example:1234")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func phoneIdentity(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	return digits.String()
}

func (s *Server) sourceForm(w http.ResponseWriter, r *http.Request, source *model.OTPSource, formError string) {
	creating := source == nil
	provider, name, bbURL, twilioTo, twilioSender := string(model.OTPProviderBlueBubbles), "", s.config.BlueBubblesURL, "", ""
	if source != nil {
		provider, name = string(source.Provider), source.Name
		if source.Provider == model.OTPProviderBlueBubbles {
			var cfg bluebubbles.Config
			if s.store.GetOTPSourceConfig(r.Context(), source.ID, &cfg) == nil {
				bbURL = cfg.BaseURL
			}
		} else {
			var cfg twilio.Config
			if s.store.GetOTPSourceConfig(r.Context(), source.ID, &cfg) == nil {
				twilioTo, twilioSender = cfg.ToNumber, cfg.Sender
			}
		}
	}
	if r.Method == http.MethodPost {
		name, provider = r.Form.Get("name"), r.Form.Get("provider")
		if value := r.Form.Get("bb_base_url"); value != "" {
			bbURL = value
		}
		if value := r.Form.Get("twilio_to_number"); value != "" {
			twilioTo = value
		}
		if value := r.Form.Get("twilio_sender"); value != "" {
			twilioSender = value
		}
	}
	actionURL, heading, submit := "/sources/new", "New OTP source", "Create source"
	if !creating {
		actionURL, heading, submit = fmt.Sprintf("/sources/%d", source.ID), "Edit OTP source", "Save source"
	}
	data := formData{BaseData: base(r, heading), Eyebrow: "Provider configuration", Heading: heading, Description: "Only the selected adapter can read this inbox. Secrets are encrypted in SQLite and are never rendered back into this form.", CancelURL: "/sources", ActionURL: actionURL, SubmitLabel: submit, FormError: formError}
	data.Sections = []formSection{
		{Title: "Identity", Fields: []formField{{Name: "name", Label: "Name", Type: "text", Value: name, Required: true}, {Name: "provider", Label: "Provider", Type: "select", Required: true, Options: []selectOption{{Value: "bluebubbles", Label: "BlueBubbles", Selected: provider == "bluebubbles"}, {Value: "twilio", Label: "Twilio", Selected: provider == "twilio"}}}}},
		{Title: "BlueBubbles", Help: "Used only when BlueBubbles is selected. The password is write-only.", Fields: []formField{{Name: "bb_base_url", Label: "Server URL", Type: "url", Value: bbURL, Placeholder: "http://bluebubbles.example:1234"}, {Name: "bb_password", Label: "Server password", Type: "password", Placeholder: secretPlaceholder(creating)}}},
		{Title: "Twilio", Help: "Used only when Twilio is selected. This adapter reads inbound messages only.", Fields: []formField{{Name: "twilio_account_sid", Label: "Account SID", Type: "password", Placeholder: secretPlaceholder(creating)}, {Name: "twilio_auth_token", Label: "Auth token", Type: "password", Placeholder: secretPlaceholder(creating)}, {Name: "twilio_to_number", Label: "Receiving number", Type: "text", Value: twilioTo, Placeholder: "+15550100123"}, {Name: "twilio_sender", Label: "Expected sender (optional)", Type: "text", Value: twilioSender}}},
	}
	s.render(w, formStatus(formError), "form", data)
}

func secretPlaceholder(creating bool) string {
	if creating {
		return "Required when selected"
	}
	return "Leave blank to keep existing"
}

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.store.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	sources, _ := s.store.ListOTPSources(r.Context())
	sourceNames := map[int64]string{}
	for _, source := range sources {
		sourceNames[source.ID] = source.Name
	}
	data := listData{BaseData: base(r, "Profiles"), Eyebrow: "Yodel identities", Heading: "Profiles", Description: "Each profile has one persistent browser directory and one exclusive OTP source.", CreateURL: "/profiles/new", CreateLabel: "New profile", EmptyMessage: "Create an OTP source first, then add a Yodel profile."}
	for _, profile := range profiles {
		status, class := "Disabled", ""
		if profile.Enabled {
			status, class = "Enabled", "ok"
		}
		data.Cards = append(data.Cards, listCard{Title: profile.Name, Subtitle: profile.BrowserProfile, Status: status, StatusClass: class, URL: fmt.Sprintf("/profiles/%d", profile.ID), Fields: []labelValue{{"Vehicle", profile.DefaultVehicle}, {"OTP source", sourceNames[profile.OTPSourceID]}, {"Browser", browserLabel(profile)}}, Actions: []cardAction{{"Edit", fmt.Sprintf("/profiles/%d", profile.ID), ""}}})
	}
	s.render(w, http.StatusOK, "list", data)
}

func browserLabel(profile model.Profile) string {
	if profile.BrowserExecutable != "" {
		return profile.BrowserExecutable
	}
	if profile.BrowserChannel != "" {
		return profile.BrowserChannel
	}
	return "Bundled Chromium"
}

func (s *Server) profileNew(w http.ResponseWriter, r *http.Request) { s.profileForm(w, r, nil, "") }
func (s *Server) profileCreate(w http.ResponseWriter, r *http.Request) {
	input, err := profileInput(r, true)
	if err == nil {
		_, err = s.store.CreateProfile(r.Context(), input)
	}
	if err != nil {
		s.profileForm(w, r, nil, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/profiles?ok=created", http.StatusSeeOther)
}
func (s *Server) profileEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	profile, err := s.store.GetProfile(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	s.profileForm(w, r, &profile, "")
}
func (s *Server) profileUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetProfile(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	input, err := profileInput(r, false)
	if err == nil {
		_, err = s.store.UpdateProfile(r.Context(), id, input)
	}
	if err != nil {
		s.profileForm(w, r, &current, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/profiles?ok=updated", http.StatusSeeOther)
}

func profileInput(r *http.Request, creating bool) (store.ProfileInput, error) {
	timeout, err := strconv.Atoi(r.Form.Get("default_timeout_ms"))
	if err != nil {
		return store.ProfileInput{}, errors.New("browser timeout must be a number")
	}
	input := store.ProfileInput{Name: r.Form.Get("name"), BrowserProfile: r.Form.Get("browser_profile"), DefaultVehicle: r.Form.Get("default_vehicle"), OTPSourceID: parseInt64(r.Form.Get("otp_source_id")), Headless: checked(r, "headless"), BrowserChannel: r.Form.Get("browser_channel"), BrowserExecutable: r.Form.Get("browser_executable"), DefaultTimeoutMS: timeout, Enabled: checked(r, "enabled")}
	if !validDirectoryName(input.BrowserProfile) {
		return input, errors.New("browser profile must be one safe directory name")
	}
	email, password := strings.TrimSpace(r.Form.Get("yodel_email")), r.Form.Get("yodel_password")
	if creating || email != "" || password != "" {
		if email == "" || password == "" {
			return input, errors.New("Yodel email and password must both be supplied")
		}
		input.Credentials = &model.ProfileCredentials{Email: email, Password: password}
	}
	return input, nil
}

func validDirectoryName(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, `/\\`)
}

func (s *Server) profileForm(w http.ResponseWriter, r *http.Request, profile *model.Profile, formError string) {
	sources, err := s.store.ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	creating := profile == nil
	value := model.Profile{Headless: true, Enabled: true, DefaultTimeoutMS: 15000}
	if profile != nil {
		value = *profile
	}
	if r.Method == http.MethodPost {
		value.Name, value.BrowserProfile, value.DefaultVehicle = r.Form.Get("name"), r.Form.Get("browser_profile"), r.Form.Get("default_vehicle")
		value.OTPSourceID, value.Headless, value.Enabled = parseInt64(r.Form.Get("otp_source_id")), checked(r, "headless"), checked(r, "enabled")
		value.BrowserChannel, value.BrowserExecutable = r.Form.Get("browser_channel"), r.Form.Get("browser_executable")
		value.DefaultTimeoutMS, _ = strconv.Atoi(r.Form.Get("default_timeout_ms"))
	}
	options := make([]selectOption, 0, len(sources))
	for _, source := range sources {
		options = append(options, selectOption{Value: strconv.FormatInt(source.ID, 10), Label: source.Name + " · " + string(source.Provider), Selected: source.ID == value.OTPSourceID})
	}
	actionURL, heading, submit := "/profiles/new", "New Yodel profile", "Create profile"
	if !creating {
		actionURL, heading, submit = fmt.Sprintf("/profiles/%d", profile.ID), "Edit Yodel profile", "Save profile"
	}
	data := formData{BaseData: base(r, heading), Eyebrow: "Browser identity", Heading: heading, Description: "Credentials are write-only and are passed to Python only when Yodel displays a login form.", CancelURL: "/profiles", ActionURL: actionURL, SubmitLabel: submit, FormError: formError}
	data.Sections = []formSection{
		{Title: "Profile", Fields: []formField{{Name: "name", Label: "Name", Type: "text", Value: value.Name, Required: true}, {Name: "browser_profile", Label: "Browser profile directory", Type: "text", Value: value.BrowserProfile, Required: true, Placeholder: "home"}, {Name: "default_vehicle", Label: "Vehicle keyword", Type: "text", Value: value.DefaultVehicle, Required: true}, {Name: "otp_source_id", Label: "Exclusive OTP source", Type: "select", Required: true, Options: options}, {Name: "enabled", Label: "Enabled", Type: "checkbox", Checked: value.Enabled}}},
		{Title: "Yodel credentials", Fields: []formField{{Name: "yodel_email", Label: "Email", Type: "password", Placeholder: secretPlaceholder(creating)}, {Name: "yodel_password", Label: "Password", Type: "password", Placeholder: secretPlaceholder(creating)}}},
		{Title: "Browser", Help: "Native macOS normally uses channel chrome. Docker uses bundled Chromium, so leave channel and executable blank there.", Fields: []formField{{Name: "headless", Label: "Run headless", Type: "checkbox", Checked: value.Headless}, {Name: "browser_channel", Label: "Browser channel", Type: "text", Value: value.BrowserChannel, Placeholder: "chrome"}, {Name: "browser_executable", Label: "Executable path override", Type: "text", Value: value.BrowserExecutable}, {Name: "default_timeout_ms", Label: "Action timeout (ms)", Type: "number", Value: strconv.Itoa(value.DefaultTimeoutMS), Required: true, Step: "1000"}}},
	}
	s.render(w, formStatus(formError), "form", data)
}

func (s *Server) bookings(w http.ResponseWriter, r *http.Request) {
	bookings, err := s.store.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	profiles, _ := s.store.ListProfiles(r.Context())
	names := map[int64]string{}
	for _, profile := range profiles {
		names[profile.ID] = profile.Name
	}
	data := listData{BaseData: base(r, "Bookings"), Eyebrow: "Release schedule", Heading: "Booking requests", Description: "Target dates release one day earlier. Session warming begins at the preparation time.", CreateURL: "/bookings/new", CreateLabel: "New booking", EmptyMessage: "Create a profile, then add a booking request."}
	for _, booking := range bookings {
		status, class := "Manual queue", ""
		if booking.Enabled && booking.ScheduleEnabled {
			status, class = "Scheduled", "ok"
		} else if !booking.Enabled {
			status = "Disabled"
		}
		url := fmt.Sprintf("/bookings/%d", booking.ID)
		data.Cards = append(data.Cards, listCard{Title: booking.Name, Subtitle: names[booking.ProfileID], Status: status, StatusClass: class, URL: url, Fields: []labelValue{{"Target date", booking.TargetDate}, {"Release", booking.ReleaseTime + " · " + booking.Timezone}, {"Confirmation", string(booking.ConfirmationMode)}, {"Pass order", strings.Join(passNames(booking.PassOrder()), " → ")}}, Actions: []cardAction{{"Edit", url, ""}}, PostActions: []postAction{{Label: "Auth check", URL: url + "/run", Fields: []hiddenField{{"command", "auth-check"}}}, {Label: "Dry run", URL: url + "/run", Fields: []hiddenField{{"command", "dry-run"}}}, {Label: "Queue booking", URL: url + "/run", Class: "primary", Fields: []hiddenField{{"command", "book"}}}}})
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
	request, err := bookingInput(r, 0)
	if err == nil {
		_, err = s.store.CreateBookingRequest(r.Context(), request)
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
	booking, err := s.store.GetBookingRequest(r.Context(), id)
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
	current, err := s.store.GetBookingRequest(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	request, err := bookingInput(r, id)
	if err == nil {
		_, err = s.store.UpdateBookingRequest(r.Context(), request)
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
	command := model.JobCommand(r.Form.Get("command"))
	if !command.Valid() {
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	}
	mode := model.RunMode("")
	if command == model.CommandDryRun {
		mode = model.RunModeDryRun
	}
	job, err := s.engine.QueueBooking(r.Context(), id, command, mode)
	if err != nil {
		http.Error(w, "job could not be queued", http.StatusConflict)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d?ok=queued", job.ID), http.StatusSeeOther)
}

func bookingInput(r *http.Request, id int64) (model.BookingRequest, error) {
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
	request := model.BookingRequest{ID: id, Name: r.Form.Get("name"), ProfileID: parseInt64(r.Form.Get("profile_id")), Enabled: checked(r, "enabled"), ScheduleEnabled: checked(r, "schedule_enabled"), TargetDate: r.Form.Get("target_date"), Timezone: r.Form.Get("timezone"), ReleaseTime: r.Form.Get("release_time"), PrepMinutesBefore: prep, AuthDeadlineMinutesBefore: authDeadline, PollDeadlineSeconds: pollDeadline, PollMinSeconds: pollMin, PollMaxSeconds: pollMax, ConfirmationMode: model.RunMode(r.Form.Get("confirmation_mode")), LoginProbeURL: r.Form.Get("login_probe_url"), AllDayPassURL: r.Form.Get("all_day_pass_url"), HalfDayPassURL: r.Form.Get("half_day_pass_url"), CheckAllDay: checked(r, "check_all_day"), CheckAfternoon: checked(r, "check_afternoon"), CheckMorning: checked(r, "check_morning")}
	return request, request.Validate()
}

func (s *Server) bookingForm(w http.ResponseWriter, r *http.Request, booking *model.BookingRequest, formError string) {
	profiles, err := s.store.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	creating := booking == nil
	value := model.BookingRequest{Enabled: true, TargetDate: time.Now().In(time.Local).AddDate(0, 0, 1).Format(time.DateOnly), Timezone: "UTC", ReleaseTime: "07:00", PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120, PollMinSeconds: 1.4, PollMaxSeconds: 3.6, ConfirmationMode: model.RunModeManual, CheckAllDay: true}
	if booking != nil {
		value = *booking
	}
	if r.Method == http.MethodPost {
		parsed, _ := bookingInput(r, value.ID)
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
	data := formData{BaseData: base(r, heading), Eyebrow: "Booking policy", Heading: heading, Description: "The release occurs at the selected time on target date minus one day.", CancelURL: "/bookings", ActionURL: actionURL, SubmitLabel: submit, FormError: formError}
	data.Sections = []formSection{
		{Title: "Request", Fields: []formField{{Name: "name", Label: "Name", Type: "text", Value: value.Name, Required: true}, {Name: "profile_id", Label: "Yodel profile", Type: "select", Required: true, Options: profileOptions}, {Name: "target_date", Label: "Target date", Type: "date", Value: value.TargetDate, Required: true}, {Name: "timezone", Label: "Timezone", Type: "text", Value: value.Timezone, Required: true}, {Name: "release_time", Label: "Release time", Type: "time", Value: value.ReleaseTime, Required: true}, {Name: "confirmation_mode", Label: "Confirmation", Type: "select", Required: true, Options: []selectOption{{Value: "manual", Label: "Manual approval", Selected: value.ConfirmationMode == model.RunModeManual}, {Value: "auto", Label: "Automatic confirmation", Selected: value.ConfirmationMode == model.RunModeAuto}}}, {Name: "enabled", Label: "Enabled", Type: "checkbox", Checked: value.Enabled}, {Name: "schedule_enabled", Label: "Auto-queue in prep window", Type: "checkbox", Checked: value.ScheduleEnabled}}},
		{Title: "Yodel URLs", Fields: []formField{{Name: "login_probe_url", Label: "Login probe URL", Type: "url", Value: value.LoginProbeURL, Required: true}, {Name: "all_day_pass_url", Label: "All-day pass URL", Type: "url", Value: value.AllDayPassURL}, {Name: "half_day_pass_url", Label: "Half-day pass URL", Type: "url", Value: value.HalfDayPassURL}}},
		{Title: "Pass order", Help: "Selection order is fixed: all-day, then afternoon, then morning.", Fields: []formField{{Name: "check_all_day", Label: "All-day", Type: "checkbox", Checked: value.CheckAllDay}, {Name: "check_afternoon", Label: "Afternoon", Type: "checkbox", Checked: value.CheckAfternoon}, {Name: "check_morning", Label: "Morning", Type: "checkbox", Checked: value.CheckMorning}}},
		{Title: "Timing", Fields: []formField{{Name: "prep_minutes_before", Label: "Prep minutes before", Type: "number", Value: strconv.Itoa(value.PrepMinutesBefore), Required: true, Step: "1"}, {Name: "auth_deadline_minutes_before", Label: "Auth deadline minutes before", Type: "number", Value: strconv.Itoa(value.AuthDeadlineMinutesBefore), Required: true, Step: "1"}, {Name: "poll_deadline_seconds", Label: "Poll deadline seconds", Type: "number", Value: strconv.Itoa(value.PollDeadlineSeconds), Required: true, Step: "1"}, {Name: "poll_min_seconds", Label: "Minimum poll delay", Type: "number", Value: strconv.FormatFloat(value.PollMinSeconds, 'f', -1, 64), Required: true, Step: "0.1"}, {Name: "poll_max_seconds", Label: "Maximum poll delay", Type: "number", Value: strconv.FormatFloat(value.PollMaxSeconds, 'f', -1, 64), Required: true, Step: "0.1"}}},
	}
	s.render(w, formStatus(formError), "form", data)
}

func checked(r *http.Request, name string) bool {
	return r.Form.Get(name) == "1" || r.Form.Get(name) == "on" || r.Form.Get(name) == "true"
}
func parseInt64(value string) int64 {
	result, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return result
}
func formStatus(err string) int {
	if err != "" {
		return http.StatusUnprocessableEntity
	}
	return http.StatusOK
}
func safeFormError(err error) string {
	if errors.Is(err, store.ErrConflict) {
		return "That name, inbox, browser profile, or exclusive source is already in use."
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 300 {
		return "The submitted values were not accepted."
	}
	for _, marker := range []string{"password", "token", "secret", "cipher", "decrypt", "encrypt"} {
		if strings.Contains(strings.ToLower(message), marker) {
			return "The submitted provider or credential values were not accepted."
		}
	}
	return message
}
