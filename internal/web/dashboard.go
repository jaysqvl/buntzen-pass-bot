package web

import "net/http"

type dashboardData struct {
	BaseData
	Sources          []dashboardCard
	Profiles         []dashboardCard
	BookingCount     int
	SchedulesEnabled bool
	Jobs             []jobRow
}

type dashboardCard struct {
	listCard
	CSRFToken string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	profiles, err := userStore.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	bookings, err := userStore.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	sources, err := userStore.ListOTPSources(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	jobs, err := userStore.ListJobs(r.Context(), 10)
	if err != nil {
		s.internal(w)
		return
	}
	data := dashboardData{BaseData: base(r, "Home"), SchedulesEnabled: s.config.SchedulesEnabled, BookingCount: len(bookings)}
	sourceNames := make(map[int64]string, len(sources))
	for _, source := range sources {
		card, err := s.sourceCard(r.Context(), userStore.UserID(), source)
		if err != nil {
			s.internal(w)
			return
		}
		sourceNames[source.ID] = source.Name
		data.Sources = append(data.Sources, dashboardCard{listCard: card, CSRFToken: data.CSRFToken})
	}
	for _, profile := range profiles {
		card := profileCard(profile, sourceNames[profile.OTPSourceID])
		data.Profiles = append(data.Profiles, dashboardCard{listCard: card, CSRFToken: data.CSRFToken})
	}
	data.Jobs = s.jobRows(r.Context(), userStore, jobs)
	s.render(w, http.StatusOK, "dashboard", data)
}
