package model

import (
	"slices"
	"testing"
)

func TestPreferredPassesOverrideLegacyFlagsWithoutSharingStorage(t *testing.T) {
	booking := validBooking()
	booking.PreferredPasses = []PassType{PassMorning, PassAllDay, PassAfternoon}
	booking.HalfDayPassURL = "https://example.test/half"
	if err := booking.Validate(); err != nil {
		t.Fatal(err)
	}
	order := booking.PassOrder()
	if !slices.Equal(order, booking.PreferredPasses) {
		t.Fatalf("custom priority changed: %v", order)
	}
	order[0] = PassAllDay
	if booking.PassOrder()[0] != PassMorning {
		t.Fatal("mutating the returned order changed the booking")
	}
	booking.PreferredPasses = []PassType{PassMorning}
	booking.AllDayPassURL = ""
	if err := booking.Validate(); err != nil {
		t.Fatalf("legacy all-day flag overrode the selected morning pass: %v", err)
	}
	booking.HalfDayPassURL = ""
	if err := booking.Validate(); err == nil {
		t.Fatal("selected morning pass did not require its URL")
	}
}

func TestPassPreferencesRejectEmptyDuplicateAndUnknownSelections(t *testing.T) {
	for _, test := range []struct {
		name   string
		passes []PassType
	}{
		{"explicitly empty despite legacy flags", []PassType{}},
		{"duplicate", []PassType{PassMorning, PassMorning}},
		{"unknown", []PassType{"evening"}},
		{"too many", []PassType{PassMorning, PassAllDay, PassAfternoon, PassMorning}},
	} {
		t.Run(test.name, func(t *testing.T) {
			booking := validBooking()
			booking.HalfDayPassURL = "https://example.test/half"
			booking.PreferredPasses = test.passes
			if err := booking.Validate(); err == nil {
				t.Fatalf("accepted invalid preference list %v", test.passes)
			}
		})
	}
}
