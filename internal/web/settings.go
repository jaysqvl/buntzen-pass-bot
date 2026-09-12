package web

import (
	"net/http"
	"strconv"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

type settingsPageData struct {
	BaseData
	Sections  []formSection
	FormError string
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	value, err := s.userStore(r).GetAccountSettings(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	s.renderSettingsPage(w, r, value, "")
}

func (s *Server) settingsUpdate(w http.ResponseWriter, r *http.Request) {
	timeout, err := strconv.Atoi(r.Form.Get("default_timeout_ms"))
	value := model.AccountSettings{Headless: checked(r, "headless"), BrowserChannel: r.Form.Get("browser_channel"), DefaultTimeoutMS: timeout}
	problem := ""
	if err != nil {
		problem = "Action timeout must be a whole number."
	} else if err = value.Validate(); err != nil {
		problem = err.Error()
	}
	if problem == "" {
		if _, err = s.userStore(r).SaveAccountSettings(r.Context(), value); err != nil {
			problem = safeFormError(err)
		}
	}
	if problem != "" {
		s.renderSettingsPage(w, r, value, problem)
		return
	}
	http.Redirect(w, r, "/settings?ok=updated", http.StatusSeeOther)
}

func (s *Server) renderSettingsPage(w http.ResponseWriter, r *http.Request, value model.AccountSettings, problem string) {
	timeout := strconv.Itoa(value.DefaultTimeoutMS)
	if r.Method == http.MethodPost {
		timeout = r.Form.Get("default_timeout_ms")
	}
	data := settingsPageData{BaseData: base(r, "Settings"), FormError: problem, Sections: []formSection{
		{Title: "Browser defaults", Help: "Copied into new lake profiles. Existing profiles retain their browser settings.", Fields: []formField{
			{Name: "browser_channel", Label: "Browser channel", Type: "select", Options: browserChannelOptions(value.BrowserChannel)},
			{Name: "default_timeout_ms", Label: "Action timeout (milliseconds)", Type: "number", Value: timeout, Required: true, Min: "1000", Max: "120000", Step: "1000"},
			{Name: "headless", Label: "Run without a visible browser window", Type: "checkbox", Checked: value.Headless, Wide: true},
		}},
	}}
	s.render(w, formStatus(problem), "settings", data)
}
