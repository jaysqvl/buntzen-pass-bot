// Package model contains the persistence-neutral control-plane domain types.
package model

const (
	MaxResourceNameBytes      = 128
	MaxOTPIdentityBytes       = 2048
	MaxPairingChatGUIDBytes   = 1024
	MaxPairingSenderBytes     = 320
	MaxPairingServiceBytes    = 64
	MaxDefaultVehicleBytes    = 256
	MaxBrowserChannelBytes    = 64
	MaxBrowserExecutableBytes = 2048
	MaxTimezoneBytes          = 128
	MaxPrepMinutesBefore      = 180
)
