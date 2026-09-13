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
	Lake            destinations.Lake
	Saved           bool
	FormError       string
	Sections        []formSection
	Connection      lakeConnection
	Profiles        []dashboardCard
	ProviderName    string
	ConnectionError string
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
		settings = model.DefaultLakeSettings(lake)
		// Preserve an unambiguous legacy vehicle when no lake override exists.
		// New global sign-ins have no vehicle, and multiple choices need input.
		profiles, profileErr := s.userStore(r).ListProfiles(r.Context())
		if profileErr != nil {
			return lake, settings, false, profileErr
		}
		for _, profile := range profiles {
			keyword := strings.TrimSpace(profile.DefaultVehicle)
			if profile.EffectiveLakeID() != lake.ID || keyword == "" {
				continue
			}
			if settings.VehicleKeyword != "" && settings.VehicleKeyword != keyword {
				settings.VehicleKeyword = ""
				break
			}
			settings.VehicleKeyword = keyword
		}
		return lake, settings, false, nil
	}
	return lake, settings, err == nil, err
}

type lakeOverview struct {
	lakeConnection
	Fields []labelValue
}

type lakesPageData struct {
	BaseData
	Configured  bool
	Connections []lakeOverview
}

func (s *Server) lakesPage(w http.ResponseWriter, r *http.Request) {
	connections, err := s.lakeConnections(r)
	if err != nil {
		s.internal(w)
		return
	}
	data := lakesPageData{BaseData: base(r, "Lakes")}
	for _, connection := range connections {
		_, settings, _, err := s.effectiveLakeSettings(r, connection.Lake.ID)
		if err != nil {
			s.internal(w)
			return
		}
		data.Configured = data.Configured || connection.Configured
		data.Connections = append(data.Connections, lakeOverview{lakeConnection: connection, Fields: []labelValue{
			{"Release", releaseDaysLabel(settings.ReleaseDaysBefore) + " · " + settings.ReleaseTime},
			{"Timezone", settings.Timezone},
			{"Vehicle keyword", vehicleKeywordLabel(settings.VehicleKeyword)},
		}})
	}
	s.render(w, http.StatusOK, "lakes", data)
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
	data := lakePageData{BaseData: base(r, lake.Name), Lake: lake, Saved: saved, FormError: formError, ProviderName: providerLabel(lake.ProviderID)}
	connections, err := s.lakeConnections(r)
	if err != nil {
		s.internal(w)
		return
	}
	for _, connection := range connections {
		if connection.Lake.ID == lake.ID {
			data.Connection = connection
			break
		}
	}
	for _, profile := range data.Connection.Profiles {
		card := profileCard(profile.Profile, data.Connection.DefaultSourceName)
		card.Status, card.StatusClass, card.Description = profile.Status, profile.StatusClass, profile.Description
		if !profile.Configured {
			card.PostActions = nil
		}
		if profile.Pending {
			card.PostActions = nil
			card.Actions = []cardAction{{"View job", profile.JobURL, "primary"}}
		} else if profile.JobURL != "" {
			card.Actions = append(card.Actions, cardAction{"View last check", profile.JobURL, ""})
		}
		data.Profiles = append(data.Profiles, dashboardCard{listCard: card, CSRFToken: data.CSRFToken})
	}
	if submitted == nil && formError != "" {
		data.ConnectionError, data.FormError = formError, ""
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
	value := model.LakeSettings{LakeID: id, VehicleKeyword: strings.TrimSpace(r.Form.Get("vehicle_keyword")), Timezone: strings.TrimSpace(r.Form.Get("timezone")), ReleaseTime: r.Form.Get("release_time"), AllDayPassURL: strings.TrimSpace(r.Form.Get("all_day_pass_url")), HalfDayPassURL: strings.TrimSpace(r.Form.Get("half_day_pass_url")), PreferredPasses: []model.PassType{}}
	var problems []string
	days, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("release_days_before")))
	if err != nil {
		problems = append(problems, "Release days must be a whole number")
	} else {
		value.ReleaseDaysBefore = days
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
		{Title: "Vehicle selection", Fields: []formField{
			{Name: "vehicle_keyword", Label: "Vehicle keyword", Type: "text", Value: value.VehicleKeyword, Wide: true, Help: "A unique name or licence plate that matches a vehicle saved in your booking account."},
		}},
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
	}
}

func vehicleKeywordLabel(value string) string {
	if value == "" {
		return "Not set"
	}
	return value
}
