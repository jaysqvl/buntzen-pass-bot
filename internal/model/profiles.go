package model

import (
	"errors"
	"strings"
	"time"
)

type Profile struct {
	ID                int64
	UserID            int64
	Name              string
	DefaultVehicle    string
	OTPSourceID       int64
	Headless          bool
	BrowserChannel    string
	BrowserExecutable string
	DefaultTimeoutMS  int
	Enabled           bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ProfileCredentials struct {
	// Phone is the normalized ten-digit North American mobile number used by
	// Yodel's passwordless sign-in flow. It remains encrypted at rest and only
	// crosses the action boundary in response to credentials.request.
	Phone string
}

func (p Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("profile name is required")
	}
	if len(p.Name) > MaxResourceNameBytes {
		return errors.New("profile name is too long")
	}
	if strings.TrimSpace(p.DefaultVehicle) == "" {
		return errors.New("default vehicle is required")
	}
	if len(p.DefaultVehicle) > MaxDefaultVehicleBytes {
		return errors.New("default vehicle is too long")
	}
	if len(p.BrowserChannel) > MaxBrowserChannelBytes {
		return errors.New("browser channel is too long")
	}
	if len(p.BrowserExecutable) > MaxBrowserExecutableBytes {
		return errors.New("browser executable is too long")
	}
	if p.OTPSourceID <= 0 {
		return errors.New("OTP source is required")
	}
	if p.DefaultTimeoutMS < 1_000 || p.DefaultTimeoutMS > 120_000 {
		return errors.New("default timeout must be between 1000 and 120000 milliseconds")
	}
	return nil
}
