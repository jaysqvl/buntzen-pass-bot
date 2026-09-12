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

type lakePageData struct {
	BaseData
	Lake       destinations.Lake
	Saved      bool
	FormError  string
	Sections   []formSection
	Profiles   []dashboardCard
	HasSources bool
}

// The catalog owns identity/support; preferences belong only to this account.
func (s *Server) effectiveLakeSettings(r *http.Request, lakeID string) (destinations.Lake, model.LakeSettings, bool, error) {
	lake, err := destinations.Resolve(lakeID)
	if err != nil {
		return destinations.Lake{}, model.LakeSettings{}, false, err
	}
	approvedOrigin := config.DefaultYodelOrigin
	if len(s.config.YodelOrigins) > 0 {
		approvedOrigin = s.config.YodelOrigins[0]
	}
	lake = lake.WithOrigin(approvedOrigin)
	settings, err := s.userStore(r).GetLakeSettings(r.Context(), lake.ID)
	if errors.Is(err, store.ErrNotFound) {
		return lake, model.DefaultLakeSettings(lake), false, nil
	}
	return lake, settings, err == nil, err
}

func (s *Server) lakesPage(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.userStore(r).ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	data := listData{BaseData: base(r, "Lakes"), Eyebrow: "Your destinations", Heading: "Lakes", Description: "Manage each lake’s profiles, vehicles, and booking defaults. Your settings are personal to this account."}
	for _, supported := range destinations.List() {
		lake, settings, saved, err := s.effectiveLakeSettings(r, supported.ID)
		if err != nil {
			s.internal(w)
			return
		}
		count := 0
		for _, profile := range profiles {
			if profile.EffectiveLakeID() == lake.ID {
				count++
			}
		}
		status := "Built-in defaults"
		if saved {
			status = "Personal defaults"
		}
		path := "/lakes/" + url.PathEscape(lake.ID)
		data.Cards = append(data.Cards, listCard{
			Title: lake.Name, Subtitle: "Booking provider: " + providerLabel(lake.ProviderID), Status: status, StatusClass: "active", URL: path,
			Fields:  []labelValue{{"Release", releaseDaysLabel(settings.ReleaseDaysBefore) + " · " + settings.ReleaseTime}, {"Timezone", settings.Timezone}, {"Lake profiles", strconv.Itoa(count)}, {"Pass preferences", strings.Join(passNames(settings.PreferredPasses), " → ")}},
			Actions: []cardAction{{"Manage lake", path, "primary"}, {"New booking", "/bookings/new?lake_id=" + url.QueryEscape(lake.ID), ""}},
		})
	}
	s.render(w, http.StatusOK, "list", data)
}

func providerLabel(id string) string {
	if id == destinations.ProviderYodel {
		return "Yodel"
	}
	return id
}

func releaseDaysLabel(days int) string {
	if days == 0 {
		return "On the visit date"
	}
	if days == 1 {
		return "1 day before your visit"
	}
	return fmt.Sprintf("%d days before your visit", days)
}

func (s *Server) lakePage(w http.ResponseWriter, r *http.Request) { s.renderLakePage(w, r, nil, "") }

func (s *Server) renderLakePage(w http.ResponseWriter, r *http.Request, submitted *model.LakeSettings, formError string) {
	id := r.PathValue("lakeID")
	if _, err := destinations.Resolve(id); err != nil {
		http.NotFound(w, r)
		return
	}
	lake, settings, saved, err := s.effectiveLakeSettings(r, id)
	if err != nil {
		s.internal(w)
		return
	}
	if submitted != nil {
		settings = *submitted
	}
	profiles, err := s.userStore(r).ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	sources, err := s.userStore(r).ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	data := lakePageData{BaseData: base(r, lake.Name), Lake: lake, Saved: saved, FormError: formError, HasSources: len(sources) > 0}
	sourceNames := make(map[int64]string, len(sources))
	for _, source := range sources {
		sourceNames[source.ID] = source.Name
	}
	for _, profile := range profiles {
		if profile.EffectiveLakeID() == lake.ID {
			data.Profiles = append(data.Profiles, dashboardCard{listCard: profileCard(profile, sourceNames[profile.OTPSourceID]), CSRFToken: data.CSRFToken})
		}
	}
	data.Sections = lakeSettingsSections(lake, settings)
	// Preserve submitted number text and pass slots, including None gaps.
	if r.Method == http.MethodPost && submitted != nil {
		for i := range data.Sections {
			for j := range data.Sections[i].Fields {
				field := &data.Sections[i].Fields[j]
				if field.Type == "number" {
					field.Value = r.Form.Get(field.Name)
				}
				if field.Type == "select" {
					selected := r.Form.Get(field.Name)
					found := false
					for k := range field.Options {
						field.Options[k].Selected = field.Options[k].Value == selected
						found = found || field.Options[k].Selected
					}
					if !found {
						field.Options = append(field.Options, selectOption{Value: selected, Label: "Unsupported choice — select a pass", Selected: true})
					}
				}
			}
		}
	}
	s.render(w, formStatus(formError), "lake", data)
}

func (s *Server) lakeUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("lakeID")
	if _, err := destinations.Resolve(id); err != nil {
		http.NotFound(w, r)
		return
	}
	settings, err := lakeSettingsInput(r, id)
	if err == nil {
		err = settings.ValidateForOrigins(s.config.YodelOrigins)
	}
	if err == nil {
		_, err = s.userStore(r).SaveLakeSettings(r.Context(), settings)
	}
	if err != nil {
		s.renderLakePage(w, r, &settings, safeFormError(err))
		return
	}
	http.Redirect(w, r, "/lakes/"+url.PathEscape(id)+"?ok=updated#defaults", http.StatusSeeOther)
}

func (s *Server) lakeReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("lakeID")
	if _, err := destinations.Resolve(id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.userStore(r).ResetLakeSettings(r.Context(), id); err != nil {
		s.internal(w)
		return
	}
	http.Redirect(w, r, "/lakes/"+url.PathEscape(id)+"?notice=lake-defaults-reset#defaults", http.StatusSeeOther)
}

func lakeSettingsInput(r *http.Request, id string) (model.LakeSettings, error) {
	value := model.LakeSettings{LakeID: id, Timezone: strings.TrimSpace(r.Form.Get("timezone")), ReleaseTime: r.Form.Get("release_time"), AllDayPassURL: strings.TrimSpace(r.Form.Get("all_day_pass_url")), HalfDayPassURL: strings.TrimSpace(r.Form.Get("half_day_pass_url")), PreferredPasses: []model.PassType{}}
	var problems []string
	for _, field := range []struct {
		name, label string
		target      *int
	}{
		{"release_days_before", "Release days", &value.ReleaseDaysBefore},
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
	for i := 1; i <= 3; i++ {
		if pass := r.Form.Get(fmt.Sprintf("pass_priority_%d", i)); pass != "" {
			value.PreferredPasses = append(value.PreferredPasses, model.PassType(pass))
		}
	}
	if len(problems) > 0 {
		return value, errors.New(strings.Join(problems, "; "))
	}
	return value, value.Validate()
}

func lakeSettingsSections(lake destinations.Lake, value model.LakeSettings) []formSection {
	passes := make([]formField, 0, 3)
	for i, label := range []string{"First choice", "Second choice", "Third choice"} {
		selected := ""
		if i < len(value.PreferredPasses) {
			selected = string(value.PreferredPasses[i])
		}
		options := []selectOption{}
		supported := selected == ""
		for _, pass := range lake.SupportedPasses {
			options = append(options, selectOption{Value: pass, Label: passOptionLabel(pass), Selected: pass == selected})
			supported = supported || pass == selected
		}
		options = append(options, selectOption{Value: "", Label: "None", Selected: selected == ""})
		if !supported {
			options = append(options, selectOption{Value: selected, Label: "Unsupported choice — select a pass", Selected: true})
		}
		passes = append(passes, formField{Name: fmt.Sprintf("pass_priority_%d", i+1), Label: label, Type: "select", Options: options})
	}
	return []formSection{
		{Title: "Release schedule", Help: "When passes become available for this lake. Existing booking requests keep their saved schedule.", Fields: []formField{
			{Name: "timezone", Label: "Timezone", Type: "text", Value: value.Timezone, Required: true},
			{Name: "release_time", Label: "Release time", Type: "time", Value: value.ReleaseTime, Required: true},
			{Name: "release_days_before", Label: "Days before your visit", Type: "number", Value: strconv.Itoa(value.ReleaseDaysBefore), Required: true, Min: "0", Max: "365", Step: "1", Help: "Use 0 when passes release on the visit date."},
		}},
		{Title: "Pass preferences", Help: "Default order for new requests. Choose None to skip a slot.", Class: "form-grid-pass-preferences", Fields: passes},
		{Title: "Booking site URLs", Help: "Use paths on a booking site approved by your operator.", Fields: []formField{
			{Name: "all_day_pass_url", Label: "All-day pass URL", Type: "url", Value: value.AllDayPassURL},
			{Name: "half_day_pass_url", Label: "Half-day pass URL", Type: "url", Value: value.HalfDayPassURL},
		}},
		{Title: "Preparation and retry timing", Help: "Defaults for preparing a session and checking pass availability.", Fields: []formField{
			{Name: "prep_minutes_before", Label: "Start preparation (minutes before release)", Type: "number", Value: strconv.Itoa(value.PrepMinutesBefore), Required: true, Min: "0", Max: "180", Step: "1"},
			{Name: "auth_deadline_minutes_before", Label: "Sign-in deadline (minutes before release)", Type: "number", Value: strconv.Itoa(value.AuthDeadlineMinutesBefore), Required: true, Min: "0", Max: "180", Step: "1"},
			{Name: "poll_deadline_seconds", Label: "Availability check window (seconds)", Type: "number", Value: strconv.Itoa(value.PollDeadlineSeconds), Required: true, Min: "1", Max: "900", Step: "1"},
			{Name: "poll_min_seconds", Label: "Minimum retry delay (seconds)", Type: "number", Value: strconv.FormatFloat(value.PollMinSeconds, 'f', -1, 64), Required: true, Min: "0.05", Max: "60", Step: "0.05"},
			{Name: "poll_max_seconds", Label: "Maximum retry delay (seconds)", Type: "number", Value: strconv.FormatFloat(value.PollMaxSeconds, 'f', -1, 64), Required: true, Min: "0.05", Max: "60", Step: "0.05"},
		}},
	}
}
