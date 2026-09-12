package web

import "net/http"

type dashboardData struct {
	BaseData
	BookingCount     int
	SchedulesEnabled bool
	AutoQueueNotice  string
	Jobs             []jobRow
}

type dashboardCard struct {
	listCard
	CSRFToken string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
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
	s.render(w, http.StatusOK, "dashboard", data)
}
