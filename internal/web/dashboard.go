package web

import (
	"errors"
	"net/http"

	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

type dashboardData struct {
	BaseData
	BookingCount      int
	SchedulesEnabled  bool
	AutoQueueNotice   string
	Jobs              []jobRow
	Profiles          []dashboardCard
	DefaultSourceName string
}

type dashboardCard struct {
	listCard
	CSRFToken string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.renderDashboard(w, r, "")
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, problem string) {
	userStore := s.userStore(r)
	bookings, err := userStore.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	jobs, err := userStore.ListJobs(r.Context(), 10)
	if err != nil {
		s.internal(w)
		return
	}
	data := dashboardData{BaseData: base(r, "Home"), SchedulesEnabled: s.config.SchedulesEnabled, BookingCount: len(bookings), AutoQueueNotice: autoQueueOffNotice, Jobs: s.jobRows(r.Context(), userStore, jobs)}
	profiles, err := userStore.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	source, err := userStore.GetDefaultOTPSource(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w)
		return
	}
	data.DefaultSourceName = source.Name
	for _, profile := range profiles {
		data.Profiles = append(data.Profiles, dashboardCard{listCard: profileCard(profile, source.Name), CSRFToken: data.CSRFToken})
	}
	if problem != "" {
		data.Flash = &Flash{Kind: "error", Message: problem}
	}
	s.render(w, formStatus(problem), "dashboard", data)
}
