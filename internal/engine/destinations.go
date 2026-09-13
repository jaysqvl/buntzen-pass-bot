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

func executionProvider(providerID string) error {
	if providerID != destinations.ProviderYodel {
		return fmt.Errorf("unsupported sign-in provider %q", providerID)
	}
	return nil
}

func validateProfileBookingProvider(profile model.Profile, booking model.BookingRequest) error {
	lake, err := executionDestination(booking.EffectiveLakeID())
	if err != nil {
		return err
	}
	if profile.EffectiveProviderID() != lake.ProviderID {
		return fmt.Errorf("booking provider must match the selected sign-in provider")
	}
	return nil
}
