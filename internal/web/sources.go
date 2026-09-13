package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/egress"
	"github.com/jaysqvl/lake-pass-bot/internal/engine"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.userStore(r).ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	defaultSource, err := s.userStore(r).GetDefaultOTPSource(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w)
		return
	}
	data := listData{BaseData: base(r, "OTP sources"), Eyebrow: "Your connections", Heading: "OTP sources", Description: "Configure BlueBubbles or Twilio and choose the default inbox for your sign-in codes. Newly queued jobs use your default source.", CreateURL: "/sources/new", CreateLabel: "New OTP source", EmptyMessage: "Add a BlueBubbles or Twilio source. Your first source becomes the default."}
	for _, source := range sources {
		card, err := s.sourceCard(r.Context(), s.userStore(r).UserID(), source)
		if err != nil {
			s.internal(w)
			return
		}
		if source.ID == defaultSource.ID {
			card.Default = true
		} else {
			card.PostActions = append(card.PostActions, postAction{Label: "Make default", URL: fmt.Sprintf("/sources/%d/default", source.ID)})
		}
		data.Cards = append(data.Cards, card)
	}
	s.render(w, http.StatusOK, "list", data)
}

func (s *Server) sourceDefault(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.userStore(r).SetDefaultOTPSource(r.Context(), id); err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	redirectNotice(w, r, "/sources", "otp-default-updated")
}

func (s *Server) sourceCard(ctx context.Context, userID int64, source model.OTPSource) (listCard, error) {
	providerName := string(source.Provider)
	switch source.Provider {
	case model.OTPProviderBlueBubbles:
		providerName = "BlueBubbles"
	case model.OTPProviderTwilio:
		providerName = "Twilio"
	}
	card := listCard{
		Title: source.Name, Subtitle: providerName, Status: "Configured", StatusClass: "", URL: fmt.Sprintf("/sources/%d", source.ID),
		Actions:     []cardAction{{"Edit", fmt.Sprintf("/sources/%d", source.ID), ""}},
		PostActions: []postAction{{Label: "Test connection", URL: fmt.Sprintf("/sources/%d/health", source.ID)}},
	}
	if source.Provider != model.OTPProviderBlueBubbles {
		return card, nil
	}
	card.Status, card.StatusClass = "Paired", "ok"
	card.Fields = append(card.Fields, labelValue{"Paired sender", maskStoredSender(source.PairingSender)})
	if source.PairingChatGUID == "" || source.PairingSender == "" || source.PairingService == "" {
		card.Status, card.StatusClass = "Needs pairing", "warn"
	}
	setup, err := s.engine.CheckPairingSetup(ctx, userID, source.ID)
	if setup.ProfileID != 0 {
		card.Fields = append(card.Fields, labelValue{"Yodel sign-in", setup.ProfileName})
	}
	if err != nil {
		if errors.Is(err, engine.ErrPairingProfileAmbiguous) {
			profiles, listErr := s.store.ForUser(userID).ListProfiles(ctx)
			if listErr != nil {
				return listCard{}, listErr
			}
			options := []selectOption{{Label: "Choose a Yodel sign-in", Selected: true}}
			for _, profile := range profiles {
				if profile.Enabled && profile.EffectiveProviderID() == destinations.ProviderYodel && profile.ValidateForOrigins(s.config.YodelOrigins) == nil {
					options = append(options, selectOption{Value: strconv.FormatInt(profile.ID, 10), Label: profile.Name})
				}
			}
			if len(options) > 1 {
				card.Description = "Choose the Yodel sign-in to pair with this inbox. Your default OTP source stays unchanged."
				card.PostActions = append(card.PostActions, postAction{
					Label: sourcePairLabel(source), URL: fmt.Sprintf("/sources/%d/pair", source.ID), Class: "primary",
					SelectName: "profile_id", SelectLabel: "Yodel sign-in for pairing", SelectOptions: options,
				})
				return card, nil
			}
		}
		action := cardAction{Class: "primary"}
		switch {
		case errors.Is(err, engine.ErrPairingProfileRequired), errors.Is(err, engine.ErrPairingProfileAmbiguous):
			action.Label, action.URL = "Set up lake connection", "/lakes/"+destinations.DefaultLakeID+"#connection"
		case errors.Is(err, engine.ErrPairingProfileDisabled):
			action.Label, action.URL = "Enable sign-in", "/lakes/"+destinations.DefaultLakeID+"#connection"
		case errors.Is(err, engine.ErrPairingProfileInvalid):
			action.Label, action.URL = "Review sign-in", "/lakes/"+destinations.DefaultLakeID+"#connection"
		default:
			return listCard{}, err
		}
		card.Description = "Pairing uses your saved Yodel sign-in. To continue, " + safeFormError(err) + "."
		card.Actions = append(card.Actions, action)
		if card.Status == "Needs pairing" {
			card.Status = "Setup needed"
		}
		return card, nil
	}
	card.PostActions = append(card.PostActions, postAction{Label: sourcePairLabel(source), URL: fmt.Sprintf("/sources/%d/pair", source.ID), Class: "primary"})
	return card, nil
}

func sourcePairLabel(source model.OTPSource) string {
	if source.PairingChatGUID != "" && source.PairingSender != "" && source.PairingService != "" {
		return "Re-pair"
	}
	return "Pair with Yodel"
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
		_, err = s.userStore(r).CreateOTPSource(r.Context(), input)
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
	source, err := s.userStore(r).GetOTPSource(r.Context(), id)
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
	current, err := s.userStore(r).GetOTPSource(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	input, err := s.sourceInput(r, &current)
	if err == nil {
		_, err = s.userStore(r).UpdateOTPSource(r.Context(), id, input)
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
	source, err := s.userStore(r).GetOTPSource(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	provider, err := engine.ProviderForSource(r.Context(), s.store, source, s.config.BlueBubblesPolicy)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		err = provider.Health(ctx)
	}
	if err != nil {
		slog.Warn("OTP provider health check failed", "source_id", source.ID, "provider", source.Provider, "error", err)
		redirectNotice(w, r, "/sources", "provider-unavailable")
		return
	}
	slog.Info("OTP provider health check succeeded", "source_id", source.ID, "provider", source.Provider)
	http.Redirect(w, r, "/sources?ok=healthy", http.StatusSeeOther)
}

func (s *Server) sourcePair(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.userStore(r).GetOTPSource(r.Context(), id); err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	var selectedProfile []int64
	if r.Form.Has("profile_id") {
		profileID, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("profile_id")), 10, 64)
		if err != nil || profileID <= 0 {
			redirectNotice(w, r, "/sources", "pairing-unavailable")
			return
		}
		selectedProfile = []int64{profileID}
	}
	job, err := s.engine.QueuePairing(r.Context(), requestAuth(r).Authenticated.User.ID, id, selectedProfile...)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFoundOrInternal(w, err)
			return
		}
		slog.Warn("supervised pairing could not be queued", "source_id", id, "error", err)
		code := "pairing-unavailable"
		if errors.Is(err, store.ErrResourceLimit) {
			code = "queue-full"
		}
		redirectNotice(w, r, "/sources", code)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d?ok=queued", job.ID), http.StatusSeeOther)
}

func (s *Server) sourceInput(r *http.Request, current *model.OTPSource) (store.OTPSourceInput, error) {
	userStore := s.userStore(r)
	provider := model.OTPProvider(strings.TrimSpace(r.Form.Get("provider")))
	input := store.OTPSourceInput{Name: r.Form.Get("name"), Provider: provider}
	if current != nil && provider == current.Provider {
		input.PairingChatGUID, input.PairingSender, input.PairingService = current.PairingChatGUID, current.PairingSender, current.PairingService
	}
	switch provider {
	case model.OTPProviderBlueBubbles:
		var cfg bluebubbles.Config
		if current != nil && current.Provider == provider {
			if err := userStore.GetOTPSourceConfig(r.Context(), current.ID, &cfg); err != nil {
				return input, err
			}
		}
		if value := strings.TrimSpace(r.Form.Get("bb_base_url")); value != "" {
			cfg.BaseURL = value
		}
		passwordProvided := r.Form.Get("bb_password") != ""
		if value := r.Form.Get("bb_password"); passwordProvided {
			cfg.Password = value
		}
		identity, err := blueBubblesIdentity(cfg.BaseURL)
		if err != nil {
			return input, err
		}
		if current != nil && current.Provider == provider && identity != current.Identity && !passwordProvided {
			// Do not let a retained write-only password cross the old inbox
			// boundary, even transiently in the replacement config.
			cfg.Password = ""
			return input, errors.New("re-enter the BlueBubbles password when changing its server URL")
		}
		cfg.BaseURL = identity
		if _, err := bluebubbles.New(cfg, s.config.BlueBubblesPolicy); err != nil {
			return input, err
		}
		input.Identity, input.ProviderConfig, input.SecretProvided = identity, cfg, passwordProvided
	case model.OTPProviderTwilio:
		var cfg twilio.Config
		if current != nil && current.Provider == provider {
			if err := userStore.GetOTPSourceConfig(r.Context(), current.ID, &cfg); err != nil {
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
	canonical, err := egress.CanonicalOrigin(value)
	if err != nil {
		return "", errors.New("BlueBubbles URL must be a server root such as http://bluebubbles.example:1234")
	}
	return canonical, nil
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
	userStore := s.userStore(r)
	creating := source == nil
	provider, name, bbURL, twilioTo, twilioSender := string(model.OTPProviderBlueBubbles), "", s.config.BlueBubblesURL, "", ""
	if source != nil {
		provider, name = string(source.Provider), source.Name
		if source.Provider == model.OTPProviderBlueBubbles {
			var cfg bluebubbles.Config
			if userStore.GetOTPSourceConfig(r.Context(), source.ID, &cfg) == nil {
				bbURL = cfg.BaseURL
			}
		} else {
			var cfg twilio.Config
			if userStore.GetOTPSourceConfig(r.Context(), source.ID, &cfg) == nil {
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
	data := formData{
		BaseData:        base(r, heading),
		Eyebrow:         "OTP sources",
		Heading:         heading,
		Description:     "Connect the inbox that receives your Yodel login codes. Your sources work across all lakes.",
		CancelURL:       "/sources",
		ActionURL:       actionURL,
		SubmitLabel:     submit,
		SubmitHelp:      "After saving, test the connection from OTP sources. BlueBubbles also needs pairing with a Yodel sign-in.",
		FormError:       formError,
		SourceSelection: true,
	}
	data.Sections = []formSection{
		{
			Title: "Source details",
			Fields: []formField{
				{
					Name:     "provider",
					Label:    "Provider",
					Type:     "select",
					Required: true,
					Options: []selectOption{
						{Value: "bluebubbles", Label: "BlueBubbles", Selected: provider == "bluebubbles"},
						{Value: "twilio", Label: "Twilio", Selected: provider == "twilio"},
					},
				},
				{Name: "name", Label: "Source name", Type: "text", Value: name, Required: true, Help: "A name you will recognize when choosing your default inbox."},
			},
		},
		{
			Title:    "BlueBubbles",
			Help:     "Use the server that receives your Messages. Saved passwords stay hidden; leave the password blank to keep it when editing this provider.",
			Provider: "bluebubbles",
			Fields: []formField{
				{Name: "bb_base_url", Label: "Server URL", Type: "url", Value: bbURL, Placeholder: "http://bluebubbles.example:1234", Help: "Use the server root. Re-enter the password if you change this address."},
				{Name: "bb_password", Label: "Server password", Type: "password", Placeholder: secretPlaceholder(creating)},
			},
		},
		{
			Title:    "Twilio",
			Help:     "Only incoming messages are read. Saved credentials stay hidden; leave them blank to keep them when editing this provider. Changing providers requires new credentials.",
			Provider: "twilio",
			Fields: []formField{
				{Name: "twilio_to_number", Label: "Receiving number", Type: "tel", Value: twilioTo, Placeholder: "+15550100123"},
				{Name: "twilio_sender", Label: "Expected sender (optional)", Type: "text", Value: twilioSender, Help: "Limit codes to this sender. Leave blank to keep the saved sender when editing."},
				{Name: "twilio_account_sid", Label: "Account SID", Type: "password", Placeholder: secretPlaceholder(creating)},
				{Name: "twilio_auth_token", Label: "Auth token", Type: "password", Placeholder: secretPlaceholder(creating)},
			},
		},
	}
	if !creating {
		job, err := s.pendingResourceJob(r, func(job model.Job) bool { return job.OTPSourceID == source.ID })
		if err != nil {
			s.internal(w)
			return
		}
		if job != nil {
			data.SubmitDisabled = true
			data.SubmitHelp = "This OTP source cannot be changed while its job is pending."
			data.Flash = &Flash{Kind: "info", Message: "A pending job is using this OTP source. View the job to follow progress or cancel before editing.", ActionLabel: "View job", ActionURL: fmt.Sprintf("/jobs/%d", job.ID)}
		}
	}
	s.render(w, formStatus(formError), "form", data)
}

func secretPlaceholder(creating bool) string {
	if creating {
		return "Required when selected"
	}
	return "Leave blank to keep existing"
}
