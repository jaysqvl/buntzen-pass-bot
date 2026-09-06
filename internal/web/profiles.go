package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	profiles, err := userStore.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	sources, _ := userStore.ListOTPSources(r.Context())
	sourceNames := map[int64]string{}
	for _, source := range sources {
		sourceNames[source.ID] = source.Name
	}
	data := listData{
		BaseData:     base(r, "Profiles"),
		Eyebrow:      "Step 2 · Yodel login",
		Heading:      "Profiles",
		Description:  "Choose the OTP source that receives codes for this Yodel mobile number. Save the profile and its login URL, then pair BlueBubbles from the linked OTP source. No booking request is needed for pairing.",
		CreateURL:    "/profiles/new",
		CreateLabel:  "New profile",
		EmptyMessage: "Create an OTP source first, then add a Yodel profile.",
	}
	for _, profile := range profiles {
		data.Cards = append(data.Cards, profileCard(profile, sourceNames[profile.OTPSourceID]))
	}
	s.render(w, http.StatusOK, "list", data)
}

func profileCard(profile model.Profile, sourceName string) listCard {
	status, class := "Disabled", ""
	if profile.Enabled {
		status, class = "Enabled", "ok"
	}
	return listCard{
		Title: profile.Name, Subtitle: "Yodel login and vehicle", Status: status, StatusClass: class,
		URL:    fmt.Sprintf("/profiles/%d", profile.ID),
		Fields: []labelValue{{"Vehicle", profile.DefaultVehicle}, {"Linked OTP source", sourceName}},
		Actions: []cardAction{
			{"Edit profile", fmt.Sprintf("/profiles/%d", profile.ID), ""},
			{"View OTP source", fmt.Sprintf("/sources/%d", profile.OTPSourceID), ""},
			{"New booking", fmt.Sprintf("/bookings/new?profile_id=%d", profile.ID), "primary"},
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
	http.Redirect(w, r, "/profiles?ok=created", http.StatusSeeOther)
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
		_, err = s.userStore(r).UpdateProfile(r.Context(), id, input)
	}
	if err != nil {
		s.profileForm(w, r, &current, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/profiles?ok=updated", http.StatusSeeOther)
}

func (s *Server) profileInput(r *http.Request, creating bool) (store.ProfileInput, error) {
	timeout, err := strconv.Atoi(r.Form.Get("default_timeout_ms"))
	if err != nil {
		return store.ProfileInput{}, errors.New("browser timeout must be a number")
	}
	input := store.ProfileInput{
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
	sources, err := s.userStore(r).ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	creating := profile == nil
	yodelOrigin := config.DefaultYodelOrigin
	if len(s.config.YodelOrigins) > 0 {
		yodelOrigin = s.config.YodelOrigins[0]
	}
	value := model.Profile{Headless: true, Enabled: true, DefaultTimeoutMS: 15000, LoginProbeURL: yodelOrigin + "/buntzen-lake"}
	if profile != nil {
		value = *profile
	}
	if creating && r.Method == http.MethodGet {
		requestedSource := parseInt64(r.URL.Query().Get("source_id"))
		for _, source := range sources {
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
	options := make([]selectOption, 0, len(sources))
	for _, source := range sources {
		options = append(options, selectOption{Value: strconv.FormatInt(source.ID, 10), Label: source.Name + " · " + string(source.Provider), Selected: source.ID == value.OTPSourceID})
	}
	actionURL, heading, submit := "/profiles/new", "New Yodel profile", "Create profile"
	if !creating {
		actionURL, heading, submit = fmt.Sprintf("/profiles/%d", profile.ID), "Edit Yodel profile", "Save profile"
	}
	data := formData{
		BaseData:    base(r, heading),
		Eyebrow:     "Browser identity",
		Heading:     heading,
		Description: "Use the phone number on your Yodel account and choose the OTP source that receives its login codes. Save this profile, then choose Pair with Yodel on the linked OTP source. A booking request is not required.",
		CancelURL:   "/profiles",
		ActionURL:   actionURL,
		SubmitLabel: submit,
		FormError:   formError,
	}
	data.Sections = []formSection{
		{
			Title: "Profile",
			Help:  "Choose your saved OTP source. Each source can be linked to only one profile.",
			Fields: []formField{
				{Name: "name", Label: "Name", Type: "text", Value: value.Name, Required: true},
				{Name: "default_vehicle", Label: "Vehicle keyword", Type: "text", Value: value.DefaultVehicle, Required: true},
				{Name: "otp_source_id", Label: "Exclusive OTP source", Type: "select", Required: true, Options: options},
				{Name: "enabled", Label: "Enabled", Type: "checkbox", Checked: value.Enabled},
			},
		},
		{
			Title: "Yodel sign-in",
			Help:  "Enter the 10-digit Canadian/US mobile number used by Yodel. A leading +1 and common separators are accepted. If this profile predates mobile login support, re-enter the number before enabling it.",
			Fields: []formField{
				{Name: "yodel_phone", Label: "Mobile phone number", Type: "password", Placeholder: secretPlaceholder(creating), Required: creating},
				{Name: "login_probe_url", Label: "Yodel login URL", Type: "url", Value: value.LoginProbeURL, Required: true, Help: "Used for pairing and signing in. Keep the default Buntzen Lake URL unless your host uses another approved Yodel site."},
			},
		},
		{
			Title: "Browser",
			Help:  "Native macOS normally uses channel chrome. Docker uses bundled Chromium, so leave channel and executable blank there.",
			Fields: []formField{
				{Name: "headless", Label: "Run headless", Type: "checkbox", Checked: value.Headless},
				{Name: "browser_channel", Label: "Browser channel", Type: "text", Value: value.BrowserChannel, Placeholder: "chrome"},
				{Name: "browser_executable", Label: "Executable path override", Type: "text", Value: value.BrowserExecutable},
				{Name: "default_timeout_ms", Label: "Action timeout (ms)", Type: "number", Value: strconv.Itoa(value.DefaultTimeoutMS), Required: true, Step: "1000"},
			},
		},
	}
	s.render(w, formStatus(formError), "form", data)
}
