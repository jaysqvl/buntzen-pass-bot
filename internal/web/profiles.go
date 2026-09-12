package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jaysqvl/lake-pass-bot/internal/config"
	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/lakes", http.StatusSeeOther)
}

func profileCard(profile model.Profile, sourceName string) listCard {
	lake, _ := destinations.Resolve(profile.EffectiveLakeID())
	status, class := "Disabled", ""
	if profile.Enabled {
		status, class = "Enabled", "ok"
	}
	return listCard{
		Title: profile.Name, Subtitle: lake.Name + " · Sign-in and vehicle", Status: status, StatusClass: class,
		URL:    fmt.Sprintf("/profiles/%d", profile.ID),
		Fields: []labelValue{{"Vehicle", profile.DefaultVehicle}, {"Linked OTP source", sourceName}},
		Actions: []cardAction{
			{"Edit profile", fmt.Sprintf("/profiles/%d", profile.ID), ""},
			{"View OTP source", fmt.Sprintf("/sources/%d", profile.OTPSourceID), ""},
			{"New booking", fmt.Sprintf("/bookings/new?lake_id=%s&profile_id=%d", url.QueryEscape(profile.EffectiveLakeID()), profile.ID), "primary"},
		},
	}
}

func (s *Server) profileNew(w http.ResponseWriter, r *http.Request) { s.profileForm(w, r, nil, "") }
func (s *Server) profileCreate(w http.ResponseWriter, r *http.Request) {
	input, err := s.profileInput(r, true)
	if err == nil {
		_, err = s.userStore(r).CreateProfile(r.Context(), input)
	}
	if err != nil {
		s.profileForm(w, r, nil, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/lakes/"+url.PathEscape(input.LakeID)+"?ok=created#profiles", http.StatusSeeOther)
}
func (s *Server) profileEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	profile, err := s.userStore(r).GetProfile(r.Context(), id)
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
	current, err := s.userStore(r).GetProfile(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	input, err := s.profileInput(r, false)
	if err == nil {
		if strings.TrimSpace(r.Form.Get("lake_id")) == "" {
			input.LakeID = current.EffectiveLakeID()
		} else if input.LakeID != current.EffectiveLakeID() {
			err = errors.New("a profile's lake cannot be changed; create a profile in the other lake's settings")
		}
	}
	if err == nil {
		_, err = s.userStore(r).UpdateProfile(r.Context(), id, input)
	}
	if err != nil {
		s.profileForm(w, r, &current, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/lakes/"+url.PathEscape(current.EffectiveLakeID())+"?ok=updated#profiles", http.StatusSeeOther)
}

func (s *Server) profileInput(r *http.Request, creating bool) (store.ProfileInput, error) {
	lake, err := destinations.Resolve(strings.TrimSpace(r.Form.Get("lake_id")))
	if err != nil {
		return store.ProfileInput{}, err
	}
	timeout, err := strconv.Atoi(r.Form.Get("default_timeout_ms"))
	if err != nil {
		return store.ProfileInput{}, errors.New("browser timeout must be a number")
	}
	input := store.ProfileInput{
		LakeID:            lake.ID,
		Name:              r.Form.Get("name"),
		DefaultVehicle:    r.Form.Get("default_vehicle"),
		LoginProbeURL:     strings.TrimSpace(r.Form.Get("login_probe_url")),
		OTPSourceID:       parseInt64(r.Form.Get("otp_source_id")),
		Headless:          checked(r, "headless"),
		BrowserChannel:    r.Form.Get("browser_channel"),
		BrowserExecutable: r.Form.Get("browser_executable"),
		DefaultTimeoutMS:  timeout,
		Enabled:           checked(r, "enabled"),
	}
	phone := strings.TrimSpace(r.Form.Get("yodel_phone"))
	if creating || phone != "" {
		if phone == "" {
			return input, errors.New("Yodel mobile number is required")
		}
		input.Credentials = &model.ProfileCredentials{Phone: phone}
	}
	return input, input.ValidateForOrigins(s.config.YodelOrigins)
}

func (s *Server) profileForm(w http.ResponseWriter, r *http.Request, profile *model.Profile, formError string) {
	requestedLakeID := strings.TrimSpace(r.URL.Query().Get("lake_id"))
	if r.Method == http.MethodPost {
		requestedLakeID = strings.TrimSpace(r.Form.Get("lake_id"))
	}
	if profile != nil {
		requestedLakeID = profile.EffectiveLakeID()
	}
	lake, err := destinations.Resolve(requestedLakeID)
	if err != nil {
		if r.Method == http.MethodPost {
			http.Error(w, "Choose a supported lake before creating a profile.", http.StatusUnprocessableEntity)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	sources, err := s.userStore(r).ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	profiles, err := s.userStore(r).ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	assignedSources := make(map[int64]bool, len(profiles))
	for _, linked := range profiles {
		if profile == nil || linked.ID != profile.ID {
			assignedSources[linked.OTPSourceID] = true
		}
	}
	availableSources := make([]model.OTPSource, 0, len(sources))
	for _, source := range sources {
		if !assignedSources[source.ID] {
			availableSources = append(availableSources, source)
		}
	}
	creating := profile == nil
	yodelOrigin := config.DefaultYodelOrigin
	if len(s.config.YodelOrigins) > 0 {
		yodelOrigin = s.config.YodelOrigins[0]
	}
	value := model.Profile{LakeID: lake.ID, Enabled: true, LoginProbeURL: lake.WithOrigin(yodelOrigin).LoginURL}
	if profile != nil {
		value = *profile
	} else {
		settings, err := s.userStore(r).GetAccountSettings(r.Context())
		if err != nil {
			s.internal(w)
			return
		}
		value.Headless, value.BrowserChannel, value.DefaultTimeoutMS = settings.Headless, settings.BrowserChannel, settings.DefaultTimeoutMS
	}
	if creating && r.Method == http.MethodGet {
		requestedSource := parseInt64(r.URL.Query().Get("source_id"))
		for _, source := range availableSources {
			if source.ID == requestedSource {
				value.OTPSourceID = source.ID
				break
			}
		}
	}
	if r.Method == http.MethodPost {
		value.Name, value.DefaultVehicle = r.Form.Get("name"), r.Form.Get("default_vehicle")
		value.LoginProbeURL = r.Form.Get("login_probe_url")
		value.OTPSourceID, value.Headless, value.Enabled = parseInt64(r.Form.Get("otp_source_id")), checked(r, "headless"), checked(r, "enabled")
		value.BrowserChannel, value.BrowserExecutable = r.Form.Get("browser_channel"), r.Form.Get("browser_executable")
		value.DefaultTimeoutMS, _ = strconv.Atoi(r.Form.Get("default_timeout_ms"))
	}
	selectedSourceAvailable := false
	options := make([]selectOption, 0, len(availableSources)+1)
	for _, source := range availableSources {
		options = append(options, selectOption{Value: strconv.FormatInt(source.ID, 10), Label: source.Name + " · " + string(source.Provider), Selected: source.ID == value.OTPSourceID})
		selectedSourceAvailable = selectedSourceAvailable || source.ID == value.OTPSourceID
	}
	placeholder := "Choose an OTP source"
	if len(availableSources) == 0 {
		placeholder = "No available OTP sources"
	}
	options = append([]selectOption{{Value: "", Label: placeholder, Selected: !selectedSourceAvailable}}, options...)
	timeoutValue := strconv.Itoa(value.DefaultTimeoutMS)
	if r.Method == http.MethodPost {
		timeoutValue = r.Form.Get("default_timeout_ms")
	}
	actionURL, heading, submit := "/profiles/new", "New "+lake.Name+" profile", "Create profile"
	if !creating {
		actionURL, heading, submit = fmt.Sprintf("/profiles/%d", profile.ID), "Edit "+lake.Name+" profile", "Save profile"
	}
	data := formData{
		BaseData:     base(r, heading),
		Eyebrow:      "Lake settings",
		Heading:      heading,
		Description:  "Save the sign-in and vehicle details used for " + lake.Name + ". Link an OTP source from your account to receive login codes.",
		CancelURL:    "/lakes/" + url.PathEscape(lake.ID) + "#profiles",
		ActionURL:    actionURL,
		SubmitLabel:  submit,
		FormError:    formError,
		HiddenFields: []hiddenField{{Name: "lake_id", Value: lake.ID}},
		AdvancedHelp: "Browser options are copied from Settings when a profile is created. Change them here only for this profile.",
	}
	if len(availableSources) == 0 {
		message := "Add an OTP source before creating a lake profile."
		if len(sources) > 0 {
			message = "All your OTP sources are already linked to profiles. Add another source to create a new lake profile."
		}
		data.Flash = &Flash{Kind: "info", Message: message, ActionLabel: "Add OTP source", ActionURL: "/sources/new"}
	} else if r.Method == http.MethodPost && value.OTPSourceID != 0 && !selectedSourceAvailable {
		data.Flash = &Flash{Kind: "info", Message: "The selected OTP source is unavailable. Choose an available source or add another.", ActionLabel: "Add OTP source", ActionURL: "/sources/new"}
	}
	data.Sections = []formSection{
		{
			Title: "Profile",
			Help:  "Choose an available OTP source. Each source can be linked to only one profile.",
			Fields: []formField{
				{Name: "name", Label: "Name", Type: "text", Value: value.Name, Required: true},
				{Name: "otp_source_id", Label: "Exclusive OTP source", Type: "select", Required: true, Options: options},
				{Name: "enabled", Label: "Enabled", Type: "checkbox", Checked: value.Enabled},
			},
		},
		{
			Title: lake.Name + " vehicle",
			Help:  "Use a unique name or licence plate from the vehicle saved in your Yodel account.",
			Fields: []formField{
				{Name: "default_vehicle", Label: "Vehicle keyword", Type: "text", Value: value.DefaultVehicle, Required: true, Wide: true},
			},
		},
		{
			Title: "Yodel sign-in",
			Help:  "Enter the 10-digit Canadian/US mobile number used by Yodel. A leading +1 and common separators are accepted. If this profile predates mobile login support, re-enter the number before enabling it.",
			Fields: []formField{
				{Name: "yodel_phone", Label: "Mobile phone number", Type: "password", Placeholder: secretPlaceholder(creating), Required: creating},
				{Name: "login_probe_url", Label: "Yodel login URL", Type: "url", Value: value.LoginProbeURL, Required: true, Help: "Used for pairing and signing in to the booking provider. Keep the default unless your operator has approved another provider site."},
			},
		},
		{
			Title:    "Browser",
			Advanced: true,
			Help:     "Use Chrome for native macOS or bundled Chromium for Docker. Custom browser installations are managed by your host operator.",
			Fields: []formField{
				{Name: "browser_channel", Label: "Browser channel", Type: "select", Options: browserChannelOptions(value.BrowserChannel)},
				{Name: "default_timeout_ms", Label: "Action timeout (ms)", Type: "number", Value: timeoutValue, Required: true, Min: "1000", Max: "120000", Step: "1000"},
				{Name: "headless", Label: "Run without a visible browser window", Type: "checkbox", Checked: value.Headless, Wide: true},
			},
		},
	}
	s.render(w, formStatus(formError), "form", data)
}

func browserChannelOptions(selected string) []selectOption {
	selected = strings.ToLower(strings.TrimSpace(selected))
	options := []selectOption{{Value: "", Label: "Bundled Chromium", Selected: selected == ""}}
	for _, channel := range []string{"chrome", "chrome-beta", "chrome-dev", "chrome-canary"} {
		options = append(options, selectOption{Value: channel, Label: channel, Selected: channel == selected})
	}
	return options
}
