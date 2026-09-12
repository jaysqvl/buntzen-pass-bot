package model

import (
	"errors"
	"strings"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
)

type Profile struct {
	ID                int64
	UserID            int64
	LakeID            string
	Name              string
	DefaultVehicle    string
	LoginProbeURL     string
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

func (p Profile) EffectiveLakeID() string {
	if p.LakeID == "" {
		return destinations.DefaultLakeID
	}
	return p.LakeID
}

func (p Profile) Validate() error {
	if _, err := destinations.Resolve(p.LakeID); err != nil {
		return err
	}
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
	if len(p.BrowserChannel) > MaxBrowserChannelBytes || len(p.BrowserExecutable) > MaxBrowserExecutableBytes {
		return errors.New("browser selection is too long")
	}
	switch strings.ToLower(strings.TrimSpace(p.BrowserChannel)) {
	case "", "chrome", "chrome-beta", "chrome-dev", "chrome-canary":
	default:
		return errors.New("choose bundled Chromium or a supported Chrome channel")
	}
	// Retain the legacy field to reject unsafe saved profiles at execution as
	// well as new writes. Only deployment configuration can select a program.
	if strings.TrimSpace(p.BrowserExecutable) != "" {
		return errors.New("browser executable paths are operator-controlled; clear the profile override")
	}
	if p.OTPSourceID <= 0 {
		return errors.New("OTP source is required")
	}
	if p.DefaultTimeoutMS < 1_000 || p.DefaultTimeoutMS > 120_000 {
		return errors.New("default timeout must be between 1000 and 120000 milliseconds")
	}
	return validateHTTPURL(p.LoginProbeURL, "Yodel login URL")
}

func (p Profile) ValidateForOrigins(allowedOrigins []string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return validateYodelURLs(allowedOrigins, yodelURL{p.LoginProbeURL, "Yodel login URL"})
}
