package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type OTPProvider string

const (
	OTPProviderBlueBubbles OTPProvider = "bluebubbles"
	OTPProviderTwilio      OTPProvider = "twilio"
)

func (p OTPProvider) Valid() bool {
	return p == OTPProviderBlueBubbles || p == OTPProviderTwilio
}

type OTPSource struct {
	ID              int64
	UserID          int64
	Name            string
	Provider        OTPProvider
	Identity        string
	PairingChatGUID string
	PairingSender   string
	PairingService  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (s OTPSource) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("OTP source name is required")
	}
	if len(s.Name) > MaxResourceNameBytes {
		return errors.New("OTP source name is too long")
	}
	if !s.Provider.Valid() {
		return fmt.Errorf("unsupported OTP provider %q", s.Provider)
	}
	if strings.TrimSpace(s.Identity) == "" {
		return errors.New("OTP source identity is required")
	}
	if len(s.Identity) > MaxOTPIdentityBytes {
		return errors.New("OTP source identity is too long")
	}
	if len(s.PairingChatGUID) > MaxPairingChatGUIDBytes || len(s.PairingSender) > MaxPairingSenderBytes ||
		len(s.PairingService) > MaxPairingServiceBytes {
		return errors.New("OTP source pairing fingerprint is too long")
	}
	return nil
}
