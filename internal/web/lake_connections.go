package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

type lakeConnection struct {
	Lake                                destinations.Lake
	Profiles                            []connectionProfile
	SetupStarted, Configured, Connected bool
	Status, StatusClass, Description    string
	URL, ActionURL, ActionLabel         string
	DefaultSourceName                   string
}

type connectionProfile struct {
	model.Profile
	Configured, Connected, Pending   bool
	Status, StatusClass, Description string
	JobURL, VerifiedAt               string
}

// Connection state is evidence from this account's retained jobs, not a live
// provider-session probe. In particular, saving credentials never verifies a
// connection. Provider sessions can expire after any successful check.
func (s *Server) lakeConnections(r *http.Request) ([]lakeConnection, error) {
	resources := s.userStore(r)
	profiles, err := resources.ListProfiles(r.Context())
	if err != nil {
		return nil, err
	}
	jobs, err := resources.ListJobs(r.Context(), store.MaxRetainedJobsPerUser)
	if err != nil {
		return nil, err
	}
	source, err := resources.GetDefaultOTPSource(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	connections := make([]lakeConnection, 0, len(destinations.List()))
	for _, lake := range destinations.List() {
		connections = append(connections, buildLakeConnection(lake, resources.UserID(), profiles, jobs, source, s.config.YodelOrigins))
	}
	return connections, nil
}

func buildLakeConnection(lake destinations.Lake, userID int64, profiles []model.Profile, jobs []model.Job, source model.OTPSource, origins []string) lakeConnection {
	path := "/lakes/" + url.PathEscape(lake.ID)
	result := lakeConnection{
		Lake: lake, URL: path, ActionURL: path + "#connection", ActionLabel: "Connect lake",
		Status: "Not connected", StatusClass: "muted", Description: "Connect your booking account to get started with this lake.",
	}
	hasSource := source.ID > 0 && source.UserID == userID && source.Validate() == nil
	if hasSource {
		result.DefaultSourceName = source.Name
	}
	bestRank := -1
	for _, profile := range profiles {
		// Global sign-ins created before lake-owned setup have an empty LakeID,
		// which maps only to the original lake. Sharing a provider does not
		// automatically connect a new destination to an existing account.
		if userID <= 0 || profile.UserID != userID || profile.EffectiveLakeID() != lake.ID || profile.EffectiveProviderID() != lake.ProviderID {
			continue
		}
		connection := profileConnection(lake, profile, jobs, hasSource, origins)
		result.Profiles = append(result.Profiles, connection)
		result.SetupStarted = true
		result.Configured = result.Configured || connection.Configured
		result.Connected = result.Connected || connection.Connected
		rank := connectionRank(connection)
		if rank > bestRank {
			bestRank = rank
			result.Status, result.StatusClass, result.Description = connection.Status, connection.StatusClass, connection.Description
		}
	}
	if result.SetupStarted {
		result.ActionLabel = "Manage connection"
	}
	return result
}

func profileConnection(lake destinations.Lake, profile model.Profile, jobs []model.Job, hasSource bool, origins []string) connectionProfile {
	result := connectionProfile{Profile: profile, Status: "Sign-in needed", StatusClass: "muted", Description: "Your account is saved. Sign in to verify its connection to this lake."}
	if !profile.Enabled {
		result.Status, result.Description = "Disabled", "Enable this account to use it for this lake."
		return result
	}
	if err := profile.ValidateForOrigins(origins); err != nil {
		result.Status, result.StatusClass, result.Description = "Needs update", "warn", "Update this account's saved details before connecting."
		return result
	}
	if !hasSource {
		result.Status, result.StatusClass, result.Description = "OTP source needed", "warn", "Choose a default OTP source, then sign in to connect this lake."
		return result
	}
	result.Configured = true
	var verified, failedSignIn, pending *model.Job
	for i := range jobs {
		job := &jobs[i]
		if job.UserID != profile.UserID || job.ProfileID != profile.ID || !job.Command.Valid() {
			continue
		}
		if !job.Status.Terminal() {
			if pending == nil || pendingRank(*job) > pendingRank(*pending) || (pendingRank(*job) == pendingRank(*pending) && job.ID > pending.ID) {
				pending = job
			}
			continue
		}
		// A profile edit may replace its encrypted phone number. Because the
		// presentation model deliberately cannot read those secrets, any edit
		// invalidates older evidence, including a harmless name-only edit.
		if job.CreatedAt.Before(profile.UpdatedAt) {
			continue
		}
		if job.Status == model.JobSucceeded && job.FinishedAt != nil && !job.FinishedAt.IsZero() {
			if verified == nil || job.FinishedAt.After(*verified.FinishedAt) {
				verified = job
			}
		}
		if job.Command == model.CommandAuthCheck && (job.Status == model.JobFailed || job.Status == model.JobInterrupted || job.Status == model.JobOutcomeUnknown) {
			if failedSignIn == nil || connectionJobTime(*job).After(connectionJobTime(*failedSignIn)) {
				failedSignIn = job
			}
		}
	}
	if verified != nil {
		result.Connected = true
		result.Status, result.StatusClass = "Connection verified", "ok"
		location, err := time.LoadLocation(lake.Timezone)
		if err != nil {
			location = time.UTC
		}
		result.VerifiedAt = verified.FinishedAt.In(location).Format("Jan 2, 2006 at 3:04 PM MST")
		result.Description = "Last checked " + result.VerifiedAt + ". Checked again before booking."
		result.JobURL = fmt.Sprintf("/jobs/%d", verified.ID)
	}
	if failedSignIn != nil && (verified == nil || connectionJobTime(*failedSignIn).After(*verified.FinishedAt)) {
		result.Connected = false
		result.Status, result.StatusClass = "Check connection", "warn"
		result.Description = "The latest sign-in did not complete. Review the job before trying again."
		result.JobURL = fmt.Sprintf("/jobs/%d", failedSignIn.ID)
	}
	if pending != nil {
		result.Pending = true
		result.JobURL = fmt.Sprintf("/jobs/%d", pending.ID)
		result.Status, result.StatusClass = "Job in progress", "active"
		result.Description = "A job is using this account. Open it to follow progress."
		if pending.Command == model.CommandAuthCheck {
			result.Status, result.Description = "Connecting", "Follow the sign-in job to finish connecting this lake."
		}
		if pending.Status == model.JobQueued {
			result.Status, result.Description = "Job queued", "A job is queued for this account. Its connection will be checked when it runs."
			if pending.Command == model.CommandAuthCheck {
				result.Status = "Sign-in queued"
			}
		}
	}
	return result
}

func connectionJobTime(job model.Job) time.Time {
	if job.FinishedAt != nil {
		return *job.FinishedAt
	}
	return job.UpdatedAt
}

func pendingRank(job model.Job) int {
	if job.Status == model.JobQueued {
		return 0
	}
	return 1
}

func connectionRank(profile connectionProfile) int {
	if profile.Connected {
		return 4
	}
	if profile.Pending {
		return 3
	}
	if profile.Configured {
		return 2
	}
	if profile.Enabled {
		return 1
	}
	return 0
}
