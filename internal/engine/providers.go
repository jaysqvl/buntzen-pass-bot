package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

type supervisedProvider struct {
	otp.PairingProvider
	hub      *control.Hub
	store    *store.Store
	jobID    int64
	sourceID int64
	jobKey   string
}

func (e *Engine) ChoosePairing(ctx context.Context, userID, jobID int64, messageID string) error {
	if _, err := e.store.ForUser(userID).GetJob(ctx, jobID); err != nil {
		return err
	}
	return e.hub.ChoosePairing(strconv.FormatInt(jobID, 10), messageID)
}

func (e *Engine) QueuePairing(ctx context.Context, userID, sourceID int64) (model.Job, error) {
	resources := e.store.ForUser(userID)
	source, err := resources.GetOTPSource(ctx, sourceID)
	if err != nil {
		return model.Job{}, err
	}
	if source.Provider != model.OTPProviderBlueBubbles {
		return model.Job{}, errors.New("only BlueBubbles sources require supervised pairing")
	}
	profiles, err := resources.ListProfiles(ctx)
	if err != nil {
		return model.Job{}, err
	}
	var profile *model.Profile
	for index := range profiles {
		if profiles[index].OTPSourceID == sourceID && profiles[index].Enabled {
			profile = &profiles[index]
			break
		}
	}
	if profile == nil {
		return model.Job{}, errors.New("assign this source to an enabled Yodel profile before pairing")
	}
	bookings, err := resources.ListBookingRequests(ctx)
	if err != nil {
		return model.Job{}, err
	}
	var bookingID int64
	for _, booking := range bookings {
		if booking.ProfileID == profile.ID && booking.Enabled && booking.ValidateForOrigins(e.config.YodelOrigins) == nil {
			bookingID = booking.ID
			break
		}
	}
	if bookingID == 0 {
		return model.Job{}, errors.New("create an enabled booking request for this profile before pairing")
	}
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
		BookingRequestID: &bookingID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
		DedupKey: prefix + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	if err == nil {
		slog.Info("supervised pairing job queued", "job_id", job.ID, "source_id", sourceID, "profile_id", profile.ID)
	}
	return job, err
}

func ProviderForSource(ctx context.Context, database *store.Store, source model.OTPSource) (otp.Provider, error) {
	switch source.Provider {
	case model.OTPProviderBlueBubbles:
		var providerConfig bluebubbles.Config
		if err := database.SystemGetOTPSourceConfig(ctx, source.ID, &providerConfig); err != nil {
			return nil, err
		}
		providerConfig.ChatGUID = source.PairingChatGUID
		providerConfig.Sender = source.PairingSender
		providerConfig.Service = source.PairingService
		return bluebubbles.New(providerConfig)
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
	return selected, nil
}
