package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
		Eyebrow:      "Yodel identities",
		Heading:      "Profiles",
		Description:  "Each profile has one persistent browser directory and one exclusive OTP source.",
		CreateURL:    "/profiles/new",
		CreateLabel:  "New profile",
		EmptyMessage: "Create an OTP source first, then add a Yodel profile.",
	}
	for _, profile := range profiles {
		status, class := "Disabled", ""
		if profile.Enabled {
			status, class = "Enabled", "ok"
		}
		data.Cards = append(data.Cards, listCard{
			Title:       profile.Name,
			Subtitle:    fmt.Sprintf("Browser identity %d", profile.ID),
			Status:      status,
			StatusClass: class,
			URL:         fmt.Sprintf("/profiles/%d", profile.ID),
			Fields:      []labelValue{{"Vehicle", profile.DefaultVehicle}, {"OTP source", sourceNames[profile.OTPSourceID]}, {"Browser", browserLabel(profile)}},
			Actions:     []cardAction{{"Edit", fmt.Sprintf("/profiles/%d", profile.ID), ""}},
		})
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
	input, err := profileInput(r, false)
	if err == nil {
		_, err = s.userStore(r).UpdateProfile(r.Context(), id, input)
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
	input := store.ProfileInput{
		Name:              r.Form.Get("name"),
		DefaultVehicle:    r.Form.Get("default_vehicle"),
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
	return input, nil
}

func (s *Server) profileForm(w http.ResponseWriter, r *http.Request, profile *model.Profile, formError string) {
	sources, err := s.userStore(r).ListOTPSources(r.Context())
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
		value.Name, value.DefaultVehicle = r.Form.Get("name"), r.Form.Get("default_vehicle")
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
		Description: "The mobile login is write-only, encrypted at rest, and passed to Python only when Yodel displays its sign-in form.",
		CancelURL:   "/profiles",
		ActionURL:   actionURL,
		SubmitLabel: submit,
		FormError:   formError,
	}
	data.Sections = []formSection{
		{
			Title: "Profile",
			Help:  "Buntzen assigns a private persistent browser identity automatically.",
			Fields: []formField{
				{Name: "name", Label: "Name", Type: "text", Value: value.Name, Required: true},
				{Name: "default_vehicle", Label: "Vehicle keyword", Type: "text", Value: value.DefaultVehicle, Required: true},
				{Name: "otp_source_id", Label: "Exclusive OTP source", Type: "select", Required: true, Options: options},
				{Name: "enabled", Label: "Enabled", Type: "checkbox", Checked: value.Enabled},
			},
		},
		{
			Title:  "Yodel sign-in",
			Help:   "Enter the 10-digit Canadian/US mobile number used by Yodel. A leading +1 and common separators are accepted. If this profile predates mobile login support, re-enter the number before enabling it.",
			Fields: []formField{{Name: "yodel_phone", Label: "Mobile phone number", Type: "password", Placeholder: secretPlaceholder(creating), Required: creating}},
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
