package store

const (
	MaxOTPSourcesPerUser         = 24
	MaxProfilesPerUser           = 16
	MaxBookingRequestsPerUser    = 64
	MaxPendingJobsPerUser        = 8
	MaxTerminalJobHistoryPerUser = 128
	MaxRetainedJobsPerUser       = 200
	MaxJobEventsPerJob           = 256
	MaxSessionsPerUser           = 8

	MaxProviderConfigJSONBytes        = 4 << 10
	MaxProviderConfigCiphertextBytes  = 8 << 10
	MaxYodelEmailBytes                = 320
	MaxYodelPasswordBytes             = 1024
	MaxYodelCredentialCiphertextBytes = 4 << 10
)
