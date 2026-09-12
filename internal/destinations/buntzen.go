package destinations

// DefaultLakeID preserves the destination used before explicit lake selection.
const DefaultLakeID = "buntzen"

var buntzen = Lake{
	ID:                DefaultLakeID,
	Name:              "Buntzen Lake",
	ProviderID:        ProviderYodel,
	Timezone:          "America/Vancouver",
	ReleaseTime:       "07:00",
	ReleaseDaysBefore: 1,
	LoginURL:          "https://yodelportal.com/buntzen-lake",
	AllDayPassURL:     "https://yodelportal.com/buntzen-lake/All-Day-Pass",
	HalfDayPassURL:    "https://yodelportal.com/buntzen-lake/Half-Day-Pass",
	SupportedPasses:   []string{"all_day", "afternoon", "morning"},
}
