package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jaysqvl/lake-pass-bot/internal/config"
	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/#yodel-sign-in", http.StatusSeeOther)
}

func profileCard(profile model.Profile, sourceName string) listCard {
	status, class := "Disabled", ""
	if profile.Enabled {
		status, class = "Enabled", "ok"
	}
	card := listCard{
		Title: profile.Name, Subtitle: "Yodel sign-in", Status: status, StatusClass: class,
		URL:     fmt.Sprintf("/profiles/%d", profile.ID),
		Actions: []cardAction{{"Edit sign-in", fmt.Sprintf("/profiles/%d", profile.ID), ""}},
	}
	if sourceName == "" {
		card.Description = "Choose a default OTP source before signing in."
		card.Actions = append(card.Actions, cardAction{"Choose OTP source", "/sources", "primary"})
	} else if profile.Enabled {
		card.PostActions = []postAction{{Label: "Sign in to Yodel", URL: fmt.Sprintf("/profiles/%d/sign-in", profile.ID), Class: "primary"}}
	}
	return card
}

func (s *Server) profileNew(w http.ResponseWriter, r *http.Request) { s.profileForm(w, r, nil, "") }
func (s *Server) profileCreate(w http.ResponseWriter, r *http.Request) {
	input, err := s.profileInput(r, nil)
	if err == nil {
		_, err = s.userStore(r).CreateProfile(r.Context(), input)
	}
	if err != nil {
		s.profileForm(w, r, nil, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/?ok=created#yodel-sign-in", http.StatusSeeOther)
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
	input, err := s.profileInput(r, &current)
	if err == nil {
		_, err = s.userStore(r).UpdateProfile(r.Context(), id, input)
	}
	if err != nil {
		s.profileForm(w, r, &current, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/?ok=updated#yodel-sign-in", http.StatusSeeOther)
}

func (s *Server) profileSignIn(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := s.engine.QueueProfileSignIn(r.Context(), s.userStore(r).UserID(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		s.renderDashboard(w, r, "A sign-in or booking is already using this account or its OTP source. Check Jobs before trying again.")
		return
	}
	if err != nil {
		s.renderDashboard(w, r, safeFormError(err))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d", job.ID), http.StatusSeeOther)
}

func (s *Server) profileInput(r *http.Request, current *model.Profile) (store.ProfileInput, error) {
	// Browser programs are controlled by the operator even for old clients.
	if strings.TrimSpace(r.Form.Get("browser_executable")) != "" {
		return store.ProfileInput{}, errors.New("browser executable paths are operator-controlled")
	}
	input := store.ProfileInput{ProviderID: destinations.ProviderYodel, Name: strings.TrimSpace(r.Form.Get("name")), Enabled: checked(r, "enabled")}
	if current != nil {
		input.ProviderID, input.LakeID = current.EffectiveProviderID(), current.LakeID
		input.DefaultVehicle, input.LoginProbeURL, input.OTPSourceID = current.DefaultVehicle, current.LoginProbeURL, current.OTPSourceID
		input.Headless, input.BrowserChannel, input.DefaultTimeoutMS = current.Headless, current.BrowserChannel, current.DefaultTimeoutMS
		// Resaving clears the retired executable override. It is no longer
		// editable, and carrying it forward would keep legacy sign-ins invalid.
		input.BrowserExecutable = ""
	} else {
		source, err := s.userStore(r).GetDefaultOTPSource(r.Context())
		if errors.Is(err, store.ErrNotFound) {
			return input, errors.New("choose a default OTP source before adding a Yodel sign-in")
		}
		if err != nil {
			return input, err
		}
		settings, err := s.userStore(r).GetAccountSettings(r.Context())
		if err != nil {
			return input, err
		}
		input.OTPSourceID, input.Headless, input.BrowserChannel, input.DefaultTimeoutMS = source.ID, settings.Headless, settings.BrowserChannel, settings.DefaultTimeoutMS
	}
	// Some older sign-ins have no login URL until they are repaired. Use the
	// approved built-in URL for those records, preserving nonblank saved URLs.
	if strings.TrimSpace(input.LoginProbeURL) == "" {
		approvedOrigin := config.DefaultYodelOrigin
		if len(s.config.YodelOrigins) > 0 {
			approvedOrigin = s.config.YodelOrigins[0]
		}
		lake, _ := destinations.Resolve(destinations.DefaultLakeID)
		input.LoginProbeURL = lake.WithOrigin(approvedOrigin).LoginURL
	}
	phone := strings.TrimSpace(r.Form.Get("yodel_phone"))
	if current == nil || phone != "" {
		if phone == "" {
			return input, errors.New("Yodel mobile number is required")
		}
		input.Credentials = &model.ProfileCredentials{Phone: phone}
	}
	return input, input.ValidateForOrigins(s.config.YodelOrigins)
}

func (s *Server) profileForm(w http.ResponseWriter, r *http.Request, profile *model.Profile, formError string) {
	source, err := s.userStore(r).GetDefaultOTPSource(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w)
		return
	}
	creating := profile == nil
	value := model.Profile{Enabled: true}
	if profile != nil {
		value = *profile
	}
	if r.Method == http.MethodPost {
		value.Name, value.Enabled = r.Form.Get("name"), checked(r, "enabled")
	}
	actionURL, heading, submit := "/profiles/new", "Add Yodel sign-in", "Save sign-in"
	if !creating {
		actionURL, heading = fmt.Sprintf("/profiles/%d", profile.ID), "Edit Yodel sign-in"
	}
	phonePlaceholder := "Enter mobile number"
	phoneHelp := "Enter the 10-digit Canadian or US mobile number you use with Yodel. A leading +1 is accepted."
	if !creating {
		phonePlaceholder = secretPlaceholder(false)
		phoneHelp = "Leave blank to keep your saved mobile number. Enter a new number only to replace it."
	}
	data := formData{
		BaseData: base(r, heading), Eyebrow: "Your account", Heading: heading,
		Description: "Save your Yodel account once and use it across supported lakes.",
		CancelURL:   "/#yodel-sign-in", ActionURL: actionURL, SubmitLabel: submit, FormError: formError,
		SubmitHelp:     "Saving does not start a sign-in. Use Sign in to Yodel on Home when you’re ready.",
		SubmitDisabled: creating && source.ID == 0,
		Sections: []formSection{{Title: "Account details", Fields: []formField{
			{Name: "name", Label: "Sign-in name", Type: "text", Value: value.Name, Required: true, Help: "Use a name you’ll recognize when choosing a booking account."},
			{Name: "yodel_phone", Label: "Mobile phone number", Type: "password", Placeholder: phonePlaceholder, Required: creating, Help: phoneHelp},
			{Name: "enabled", Label: "Enable this sign-in", Type: "checkbox", Checked: value.Enabled, Wide: true, Help: "Allow this account to sign in and run booking jobs."},
		}}},
	}
	message := "Choose a default OTP source on the OTP sources page before adding a Yodel sign-in."
	if source.ID != 0 {
		message = "Default OTP source: " + source.Name + ". Manage the inbox used for login codes on OTP sources."
	}
	data.Flash = &Flash{Kind: "info", Message: message, ActionLabel: "OTP sources", ActionURL: "/sources"}
	if !creating {
		job, err := s.pendingResourceJob(r, func(job model.Job) bool { return job.ProfileID == profile.ID })
		if err != nil {
			s.internal(w)
			return
		}
		if job != nil {
			data.SubmitDisabled = true
			data.SubmitHelp = "This sign-in cannot be changed while its job is pending."
			data.Flash = &Flash{Kind: "info", Message: "A pending job is using this sign-in. View the job to follow progress or cancel before editing.", ActionLabel: "View job", ActionURL: fmt.Sprintf("/jobs/%d", job.ID)}
		}
	}
	s.render(w, formStatus(formError), "form", data)
}

func (s *Server) pendingResourceJob(r *http.Request, matches func(model.Job) bool) (*model.Job, error) {
	// Read the complete account history so an older scheduled job still blocks
	// editing when newer completed jobs appear first in the recent jobs list.
	jobs, err := s.userStore(r).ListJobs(r.Context(), store.MaxRetainedJobsPerUser)
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if !job.Status.Terminal() && matches(job) {
			return &job, nil
		}
	}
	return nil, nil
}

func browserChannelOptions(selected string) []selectOption {
	selected = strings.ToLower(strings.TrimSpace(selected))
	options := []selectOption{{Value: "", Label: "Bundled Chromium", Selected: selected == ""}}
	for _, channel := range []string{"chrome", "chrome-beta", "chrome-dev", "chrome-canary"} {
		options = append(options, selectOption{Value: channel, Label: channel, Selected: channel == selected})
	}
	return options
}
