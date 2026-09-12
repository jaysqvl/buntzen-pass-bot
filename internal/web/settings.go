package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

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
	value, err := accountSettingsInput(r)
	problem := ""
	if err != nil {
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

func accountSettingsInput(r *http.Request) (model.AccountSettings, error) {
	value := model.AccountSettings{Headless: checked(r, "headless"), BrowserChannel: strings.TrimSpace(r.Form.Get("browser_channel"))}
	var problems []string
	for _, field := range []struct {
		name, label string
		target      *int
	}{
		{"default_timeout_ms", "Action timeout", &value.DefaultTimeoutMS},
		{"prep_minutes_before", "Preparation time", &value.PrepMinutesBefore},
		{"auth_deadline_minutes_before", "Sign-in deadline", &value.AuthDeadlineMinutesBefore},
		{"poll_deadline_seconds", "Availability check window", &value.PollDeadlineSeconds},
	} {
		number, err := strconv.Atoi(strings.TrimSpace(r.Form.Get(field.name)))
		if err != nil {
			problems = append(problems, field.label+" must be a whole number")
		} else {
			*field.target = number
		}
	}
	for _, field := range []struct {
		name, label string
		target      *float64
	}{
		{"poll_min_seconds", "Minimum retry delay", &value.PollMinSeconds},
		{"poll_max_seconds", "Maximum retry delay", &value.PollMaxSeconds},
	} {
		number, err := strconv.ParseFloat(strings.TrimSpace(r.Form.Get(field.name)), 64)
		if err != nil {
			problems = append(problems, field.label+" must be a number")
		} else {
			*field.target = number
		}
	}
	if len(problems) > 0 {
		return value, errors.New(strings.Join(problems, "; "))
	}
	return value, value.Validate()
}

func (s *Server) renderSettingsPage(w http.ResponseWriter, r *http.Request, value model.AccountSettings, problem string) {
	data := settingsPageData{BaseData: base(r, "Settings"), FormError: problem, Sections: []formSection{
		{Title: "Booking preparation and retries", Help: "Defaults shared across your lakes and copied into new booking requests. Existing requests keep their saved timing.", Fields: []formField{
			{Name: "prep_minutes_before", Label: "Start preparation (minutes before release)", Type: "number", Value: strconv.Itoa(value.PrepMinutesBefore), Required: true, Min: "0", Max: "180", Step: "1"},
			{Name: "auth_deadline_minutes_before", Label: "Sign-in deadline (minutes before release)", Type: "number", Value: strconv.Itoa(value.AuthDeadlineMinutesBefore), Required: true, Min: "0", Max: "180", Step: "1"},
			{Name: "poll_deadline_seconds", Label: "Availability check window (seconds)", Type: "number", Value: strconv.Itoa(value.PollDeadlineSeconds), Required: true, Min: "1", Max: "900", Step: "1"},
			{Name: "poll_min_seconds", Label: "Minimum retry delay (seconds)", Type: "number", Value: strconv.FormatFloat(value.PollMinSeconds, 'f', -1, 64), Required: true, Min: "0.05", Max: "60", Step: "0.05"},
			{Name: "poll_max_seconds", Label: "Maximum retry delay (seconds)", Type: "number", Value: strconv.FormatFloat(value.PollMaxSeconds, 'f', -1, 64), Required: true, Min: "0.05", Max: "60", Step: "0.05"},
		}},
		{Title: "Browser defaults", Help: "Copied into new Yodel sign-ins. Existing sign-ins retain their browser settings.", Fields: []formField{
			{Name: "browser_channel", Label: "Browser channel", Type: "select", Options: browserChannelOptions(value.BrowserChannel)},
			{Name: "default_timeout_ms", Label: "Action timeout (milliseconds)", Type: "number", Value: strconv.Itoa(value.DefaultTimeoutMS), Required: true, Min: "1000", Max: "120000", Step: "1000"},
			{Name: "headless", Label: "Run without a visible browser window", Type: "checkbox", Checked: value.Headless, Wide: true},
		}},
	}}
	if r.Method == http.MethodPost {
		for i := range data.Sections {
			for j := range data.Sections[i].Fields {
				field := &data.Sections[i].Fields[j]
				if field.Type == "number" {
					field.Value = r.Form.Get(field.Name)
				}
			}
		}
	}
	s.render(w, formStatus(problem), "settings", data)
}
