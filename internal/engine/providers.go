package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/control"
	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/egress"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/otp"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

type supervisedProvider struct {
	otp.PairingProvider
	hub      *control.Hub
	store    *store.Store
	jobID    int64
	sourceID int64
	jobKey   string
	selected atomic.Bool
}

var (
	ErrPairingProfileRequired  = errors.New("add a Yodel sign-in on Home before pairing")
	ErrPairingProfileDisabled  = errors.New("enable the Yodel sign-in on Home before pairing")
	ErrPairingProfileInvalid   = errors.New("review the Yodel sign-in on Home before pairing")
	ErrPairingProfileAmbiguous = errors.New("choose a Yodel sign-in on Home before pairing this OTP source")
)

// PairingSetup identifies the owner's profile, including the resource to correct
// when CheckPairingSetup returns a prerequisite error.
type PairingSetup struct {
	ProfileID   int64
	ProfileName string
}

func (e *Engine) ChoosePairing(ctx context.Context, userID, jobID int64, messageID string) error {
	if _, err := e.store.ForUser(userID).GetJob(ctx, jobID); err != nil {
		return err
	}
	return e.hub.ChoosePairing(strconv.FormatInt(jobID, 10), messageID)
}

// CheckPairingSetup shares pairing prerequisites between queue admission and UI
// guidance. Authentication belongs to the profile and needs no booking request.
func (e *Engine) CheckPairingSetup(ctx context.Context, userID, sourceID int64, selectedProfileID ...int64) (PairingSetup, error) {
	var setup PairingSetup
	resources := e.store.ForUser(userID)
	source, err := resources.GetOTPSource(ctx, sourceID)
	if err != nil {
		return setup, err
	}
	if source.Provider != model.OTPProviderBlueBubbles {
		return setup, errors.New("only BlueBubbles sources require supervised pairing")
	}
	profiles, err := resources.ListProfiles(ctx)
	if err != nil {
		return setup, err
	}
	var profile *model.Profile
	if len(selectedProfileID) > 0 && selectedProfileID[0] > 0 {
		for index := range profiles {
			if profiles[index].ID == selectedProfileID[0] {
				profile = &profiles[index]
				break
			}
		}
		if profile == nil {
			return setup, store.ErrNotFound
		}
	} else {
		var linked, enabled []*model.Profile
		for index := range profiles {
			candidate := &profiles[index]
			if candidate.EffectiveProviderID() != destinations.ProviderYodel {
				continue
			}
			if candidate.OTPSourceID == sourceID {
				linked = append(linked, candidate)
			}
			if candidate.Enabled {
				enabled = append(enabled, candidate)
			}
		}
		switch {
		case len(linked) == 1:
			profile = linked[0]
		case len(linked) > 1:
			return setup, ErrPairingProfileAmbiguous
		case len(enabled) == 1:
			profile = enabled[0]
		case len(enabled) > 1:
			return setup, ErrPairingProfileAmbiguous
		}
	}
	if profile == nil {
		return setup, ErrPairingProfileRequired
	}
	setup.ProfileID, setup.ProfileName = profile.ID, profile.Name
	if !profile.Enabled {
		return setup, ErrPairingProfileDisabled
	}
	if err := profile.ValidateForOrigins(e.config.YodelOrigins); err != nil {
		return setup, fmt.Errorf("%w: %v", ErrPairingProfileInvalid, err)
	}
	return setup, nil
}

func (e *Engine) QueuePairing(ctx context.Context, userID, sourceID int64, selectedProfileID ...int64) (model.Job, error) {
	return e.queuePairing(ctx, userID, sourceID, false, selectedProfileID...)
}

func (e *Engine) queuePairing(ctx context.Context, userID, sourceID int64, deduplicate bool, selectedProfileID ...int64) (model.Job, error) {
	setup, err := e.CheckPairingSetup(ctx, userID, sourceID, selectedProfileID...)
	if err != nil {
		return model.Job{}, err
	}
	resources := e.store.ForUser(userID)
	jobs, err := resources.ListJobs(ctx, 500)
	if err != nil {
		return model.Job{}, err
	}
	prefix := fmt.Sprintf("pairing:%d:", sourceID)
	for _, job := range jobs {
		if job.OTPSourceID == sourceID && strings.HasPrefix(job.DedupKey, prefix) && !job.Status.Terminal() {
			return model.Job{}, store.ErrConflict
		}
	}
	job, err := resources.EnqueueJob(ctx, store.EnqueueJobParams{
		ProfileID: setup.ProfileID, OTPSourceID: sourceID, Command: model.CommandAuthCheck, DeduplicateProfileSignIn: deduplicate,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
		DedupKey: prefix + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	if err == nil {
		slog.Info("supervised pairing job queued", "job_id", job.ID, "source_id", sourceID, "profile_id", setup.ProfileID)
	}
	return job, err
}

// QueueProfileSignIn starts a provider session from Home. The selected source
// is captured by the job, so later OTP preference changes cannot reroute it.
func (e *Engine) QueueProfileSignIn(ctx context.Context, userID, profileID int64) (model.Job, error) {
	resources := e.store.ForUser(userID)
	profile, err := resources.GetProfile(ctx, profileID)
	if err != nil {
		return model.Job{}, err
	}
	if !profile.Enabled {
		return model.Job{}, ErrPairingProfileDisabled
	}
	if err := profile.ValidateForOrigins(e.config.YodelOrigins); err != nil {
		return model.Job{}, fmt.Errorf("%w: %v", ErrPairingProfileInvalid, err)
	}
	source, err := resources.GetDefaultOTPSource(ctx)
	if errors.Is(err, store.ErrNotFound) && profile.OTPSourceID > 0 {
		source, err = resources.GetOTPSource(ctx, profile.OTPSourceID)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Job{}, errors.New("choose a default OTP source before signing in")
		}
		return model.Job{}, err
	}
	if source.Provider == model.OTPProviderBlueBubbles {
		return e.queuePairing(ctx, userID, source.ID, true, profile.ID)
	}
	if source.Provider != model.OTPProviderTwilio {
		return model.Job{}, errors.New("unsupported OTP provider")
	}
	return resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: profile.ID, OTPSourceID: source.ID, Command: model.CommandAuthCheck, RunMode: model.RunModeManual, DueAt: time.Now().UTC(), DeduplicateProfileSignIn: true})
}

func ProviderForSource(ctx context.Context, database *store.Store, source model.OTPSource, policy *egress.Policy) (otp.Provider, error) {
	switch source.Provider {
	case model.OTPProviderBlueBubbles:
		var providerConfig bluebubbles.Config
		if err := database.SystemGetOTPSourceConfig(ctx, source.ID, &providerConfig); err != nil {
			return nil, err
		}
		providerConfig.ChatGUID = source.PairingChatGUID
		providerConfig.Sender = source.PairingSender
		providerConfig.Service = source.PairingService
		return bluebubbles.New(providerConfig, policy)
	case model.OTPProviderTwilio:
		var providerConfig twilio.Config
		if err := database.SystemGetOTPSourceConfig(ctx, source.ID, &providerConfig); err != nil {
			return nil, err
		}
		return twilio.New(providerConfig)
	default:
		return nil, fmt.Errorf("unsupported OTP provider %q", source.Provider)
	}
}

func (p *supervisedProvider) Arm(ctx context.Context, filter otp.Filter) (otp.Armed, error) {
	filter.Pairing = true
	filter.RequireYodel = true
	filter.ChatGUID, filter.Sender, filter.Service = "", "", ""
	return p.PairingProvider.Arm(ctx, filter)
}

func (p *supervisedProvider) WaitForCode(ctx context.Context, armed otp.Armed) (otp.Message, error) {
	candidates, err := p.PairingProvider.WaitForPairingCandidates(ctx, armed)
	if err != nil {
		return otp.Message{}, err
	}
	if err := p.hub.SetPairingCandidates(p.jobKey, candidates); err != nil {
		return otp.Message{}, err
	}
	defer p.hub.ClearPairing(p.jobKey)
	selected, err := p.hub.WaitPairing(ctx, p.jobKey)
	if err != nil {
		return otp.Message{}, err
	}
	if err := p.store.SystemPersistOTPSourcePairing(ctx, p.jobID, p.sourceID, selected.ChatGUID, selected.Sender, selected.Service); err != nil {
		return otp.Message{}, err
	}
	p.selected.Store(true)
	return selected, nil
}
