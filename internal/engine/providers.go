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
	ErrPairingProfileRequired = errors.New("create a Yodel profile and assign this OTP source before pairing")
	ErrPairingProfileDisabled = errors.New("enable the Yodel profile linked to this OTP source before pairing")
	ErrPairingProfileInvalid  = errors.New("review the linked Yodel profile before pairing")
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
func (e *Engine) CheckPairingSetup(ctx context.Context, userID, sourceID int64) (PairingSetup, error) {
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
	for index := range profiles {
		if profiles[index].OTPSourceID == sourceID {
			profile = &profiles[index]
			break
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

func (e *Engine) QueuePairing(ctx context.Context, userID, sourceID int64) (model.Job, error) {
	setup, err := e.CheckPairingSetup(ctx, userID, sourceID)
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
		ProfileID: setup.ProfileID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
		DedupKey: prefix + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	if err == nil {
		slog.Info("supervised pairing job queued", "job_id", job.ID, "source_id", sourceID, "profile_id", setup.ProfileID)
	}
	return job, err
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
