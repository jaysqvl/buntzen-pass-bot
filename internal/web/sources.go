package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.userStore(r).ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	data := listData{
		BaseData:     base(r, "OTP Sources"),
		Eyebrow:      "Provider isolation",
		Heading:      "OTP Sources",
		Description:  "Each inbox is exclusive to one Yodel profile. Providers never fall back to one another.",
		CreateURL:    "/sources/new",
		CreateLabel:  "New OTP source",
		EmptyMessage: "Create a BlueBubbles or Twilio inbox.",
	}
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
	provider, err := engine.ProviderForSource(r.Context(), s.store, source)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		err = provider.Health(ctx)
	}
	if err != nil {
		slog.Warn("OTP provider health check failed", "source_id", source.ID, "provider", source.Provider, "error", err)
		http.Error(w, "provider health check failed", http.StatusBadGateway)
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
	job, err := s.engine.QueuePairing(r.Context(), requestAuth(r).Authenticated.User.ID, id)
	if err != nil {
		slog.Warn("supervised pairing could not be queued", "source_id", id, "error", err)
		http.Error(w, safeFormError(err), http.StatusConflict)
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
		if _, err := bluebubbles.New(cfg); err != nil {
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
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("BlueBubbles URL must be a server root such as http://bluebubbles.example:1234")
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	port := parsed.Port()
	if hostname == "" {
		return "", errors.New("BlueBubbles URL must include a server hostname")
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host, nil
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
		BaseData:    base(r, heading),
		Eyebrow:     "Provider configuration",
		Heading:     heading,
		Description: "Only the selected adapter can read this inbox. Secrets are encrypted in SQLite and are never rendered back into this form.",
		CancelURL:   "/sources",
		ActionURL:   actionURL,
		SubmitLabel: submit,
		FormError:   formError,
	}
	data.Sections = []formSection{
		{
			Title: "Identity",
			Fields: []formField{
				{Name: "name", Label: "Name", Type: "text", Value: name, Required: true},
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
			},
		},
		{
			Title: "BlueBubbles",
			Help:  "Used only when BlueBubbles is selected. The password is write-only.",
			Fields: []formField{
				{Name: "bb_base_url", Label: "Server URL", Type: "url", Value: bbURL, Placeholder: "http://bluebubbles.example:1234"},
				{Name: "bb_password", Label: "Server password", Type: "password", Placeholder: secretPlaceholder(creating)},
			},
		},
		{
			Title: "Twilio",
			Help:  "Used only when Twilio is selected. This adapter reads inbound messages only.",
			Fields: []formField{
				{Name: "twilio_account_sid", Label: "Account SID", Type: "password", Placeholder: secretPlaceholder(creating)},
				{Name: "twilio_auth_token", Label: "Auth token", Type: "password", Placeholder: secretPlaceholder(creating)},
				{Name: "twilio_to_number", Label: "Receiving number", Type: "text", Value: twilioTo, Placeholder: "+15550100123"},
				{Name: "twilio_sender", Label: "Expected sender (optional)", Type: "text", Value: twilioSender},
			},
		},
	}
	s.render(w, formStatus(formError), "form", data)
}

func secretPlaceholder(creating bool) string {
	if creating {
		return "Required when selected"
	}
	return "Leave blank to keep existing"
}
