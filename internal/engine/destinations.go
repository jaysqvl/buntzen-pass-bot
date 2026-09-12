package engine

import (
	"fmt"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

// executionDestination runs before credential decryption. Existing profiles
// contain Yodel credentials; a newly registered provider must add its profile
// and OTP support before the engine may dispatch to it.
func executionDestination(lakeID string) (destinations.Lake, error) {
	lake, err := destinations.Resolve(lakeID)
	if err != nil {
		return destinations.Lake{}, err
	}
	if lake.ProviderID != destinations.ProviderYodel {
		return destinations.Lake{}, fmt.Errorf("selected lake provider %q is incompatible with the Yodel profile", lake.ProviderID)
	}
	return lake, nil
}

func validateProfileBookingLake(profile model.Profile, booking model.BookingRequest) error {
	if profile.EffectiveLakeID() != booking.EffectiveLakeID() {
		return fmt.Errorf("booking lake must match the selected profile's lake")
	}
	return nil
}
