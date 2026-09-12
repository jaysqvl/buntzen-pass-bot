package web

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

type dashboardData struct {
	BaseData
	BookingCount     int
	SchedulesEnabled bool
	AutoQueueNotice  string
	Jobs             []jobRow
	Connections      []lakeConnection
	Bookings         []dashboardBooking
}

type dashboardBooking struct{ Name, URL, Date, Lake string }

type dashboardCard struct {
	listCard
	CSRFToken string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.renderDashboard(w, r, "")
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, problem string) {
	connections, err := s.lakeConnections(r)
	if err != nil {
		s.internal(w)
		return
	}
	configured := false
	for _, connection := range connections {
		configured = configured || connection.Configured
	}
	if !configured {
		http.Redirect(w, r, "/lakes", http.StatusSeeOther)
		return
	}
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
	data := dashboardData{BaseData: base(r, "Home"), SchedulesEnabled: s.config.SchedulesEnabled, BookingCount: len(bookings), AutoQueueNotice: autoQueueOffNotice, Jobs: s.jobRows(r.Context(), userStore, jobs), Connections: connections}
	for _, booking := range upcomingBookings(bookings, time.Now()) {
		lake, _ := destinations.Resolve(booking.EffectiveLakeID())
		data.Bookings = append(data.Bookings, dashboardBooking{Name: booking.Name, URL: fmt.Sprintf("/bookings/%d", booking.ID), Date: booking.TargetDate, Lake: lake.Name})
	}
	if problem != "" {
		data.Flash = &Flash{Kind: "error", Message: problem}
	}
	s.render(w, formStatus(problem), "dashboard", data)
}

func upcomingBookings(bookings []model.BookingRequest, now time.Time) []model.BookingRequest {
	upcoming := make([]model.BookingRequest, 0, len(bookings))
	for _, booking := range bookings {
		location, err := time.LoadLocation(booking.Timezone)
		if err != nil {
			location = time.UTC
		}
		if booking.Enabled && booking.TargetDate >= now.In(location).Format(time.DateOnly) {
			upcoming = append(upcoming, booking)
		}
	}
	sort.SliceStable(upcoming, func(i, j int) bool { return upcoming[i].TargetDate < upcoming[j].TargetDate })
	if len(upcoming) > 3 {
		upcoming = upcoming[:3]
	}
	return upcoming
}
