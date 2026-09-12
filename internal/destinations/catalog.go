// Package destinations defines the supported places and their booking rules.
// Browser automation belongs to the provider named by each destination.
package destinations

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

const ProviderYodel = "yodel"

type Lake struct {
	ID                string
	Name              string
	ProviderID        string
	Timezone          string
	ReleaseTime       string
	ReleaseDaysBefore int
	LoginURL          string
	AllDayPassURL     string
	HalfDayPassURL    string
	SupportedPasses   []string
}

// WithOrigin preserves the destination paths while applying the operator's
// approved provider origin. The caller remains responsible for origin policy.
func (lake Lake) WithOrigin(origin string) Lake {
	lake.SupportedPasses = slices.Clone(lake.SupportedPasses)
	for _, target := range []*string{&lake.LoginURL, &lake.AllDayPassURL, &lake.HalfDayPassURL} {
		parsed, err := url.Parse(*target)
		if err == nil && *target != "" {
			*target = strings.TrimRight(origin, "/") + parsed.RequestURI()
		}
	}
	return lake
}

func List() []Lake {
	return []Lake{clone(buntzen)}
}

// Resolve accepts an omitted ID for callers and records predating lake
// selection. Explicit unknown IDs never silently select another destination.
func Resolve(id string) (Lake, error) {
	if id == "" {
		id = DefaultLakeID
	}
	for _, lake := range List() {
		if lake.ID == id {
			return lake, nil
		}
	}
	return Lake{}, fmt.Errorf("unsupported lake %q", id)
}

func clone(lake Lake) Lake {
	lake.SupportedPasses = slices.Clone(lake.SupportedPasses)
	return lake
}
