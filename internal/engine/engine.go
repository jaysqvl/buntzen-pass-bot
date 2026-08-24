// Package engine joins the durable queue, provider adapters, and isolated
// action processes into the long-running control plane.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

var ErrUserCancelled = errors.New("job cancelled by operator")

type Engine struct {
	config config.Config
	store  *store.Store
	hub    *control.Hub
	owner  string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	active map[int64]context.CancelCauseFunc
}

func New(cfg config.Config, database *store.Store, hub *control.Hub) *Engine {
	host, _ := os.Hostname()
	return &Engine{
		config: cfg, store: database, hub: hub,
		owner:  fmt.Sprintf("%s-%d", host, os.Getpid()),
		active: make(map[int64]context.CancelCauseFunc),
	}
}

func (e *Engine) Start(parent context.Context) {
	e.mu.Lock()
	if e.cancel != nil {
		e.mu.Unlock()
		return
	}
	e.ctx, e.cancel = context.WithCancel(parent)
	e.mu.Unlock()
	for worker := 0; worker < e.config.MaxConcurrentJobs; worker++ {
		e.wg.Add(1)
		go e.worker(worker)
	}
	if e.config.SchedulesEnabled {
		e.wg.Add(1)
		go e.scheduleLoop()
	}
}

func (e *Engine) Stop() {
	e.mu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *Engine) Hub() *control.Hub { return e.hub }

func (e *Engine) worker(index int) {
	defer e.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := e.store.ClaimNextDueJob(e.ctx, fmt.Sprintf("%s-%d", e.owner, index))
		switch {
		case err == nil:
			e.runClaimed(job)
		case errors.Is(err, store.ErrNotFound):
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
			}
		default:
			log.Printf("job queue claim failed: %v", err)
			select {
			case <-e.ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (e *Engine) runClaimed(job model.Job) {
	jobCtx, cancel := context.WithCancelCause(e.ctx)
	monitorCtx, stopMonitor := context.WithCancel(e.ctx)
	e.mu.Lock()
	e.active[job.ID] = cancel
	e.mu.Unlock()
	defer func() {
		stopMonitor()
		cancel(nil)
		e.mu.Lock()
		delete(e.active, job.ID)
		e.mu.Unlock()
	}()
	go e.monitorCancellation(monitorCtx, job.ID, cancel)

	result, runErr := e.execute(jobCtx, job)
	if runErr != nil {
		current, _ := e.store.GetJob(context.Background(), job.ID)
		status := model.JobFailed
		message := "The control plane could not run the isolated action."
		if current.ConfirmationStartedAt != nil {
			status = model.JobOutcomeUnknown
			message = "The action ended after final confirmation may have started; booking outcome is unknown."
		} else if e.ctx.Err() != nil {
			status = model.JobInterrupted
			message = "Interrupted by control-plane shutdown."
		} else if errors.Is(context.Cause(jobCtx), ErrUserCancelled) {
			status = model.JobCancelled
			message = "Cancelled by the operator."
		}
		e.finish(job.ID, status, message, nil)
		return
	}

	current, _ := e.store.GetJob(context.Background(), job.ID)
	if current.ConfirmationStartedAt != nil && result.Status != model.JobSucceeded {
		result.Status = model.JobOutcomeUnknown
		result.Message = "The action ended after final confirmation may have started; booking outcome is unknown."
	}
	if e.ctx.Err() != nil && current.ConfirmationStartedAt == nil {
		result.Status = model.JobInterrupted
		result.Message = "Interrupted by control-plane shutdown."
	} else if errors.Is(context.Cause(jobCtx), ErrUserCancelled) && current.ConfirmationStartedAt == nil {
		result.Status = model.JobCancelled
		result.Message = "Cancelled by the operator."
	}
	e.finish(job.ID, result.Status, result.Message, &result.ExitCode)
}

func (e *Engine) monitorCancellation(ctx context.Context, jobID int64, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
			requested, err := e.store.JobCancellationRequested(checkCtx, jobID)
			stop()
			if err == nil && requested {
				e.hub.CancelJob(strconv.FormatInt(jobID, 10))
				cancel(ErrUserCancelled)
				return
			}
		}
	}
}

func (e *Engine) execute(ctx context.Context, job model.Job) (control.RunResult, error) {
	profile, err := e.store.GetProfile(ctx, job.ProfileID)
	if err != nil {
		return control.RunResult{}, err
	}
	source, err := e.store.GetOTPSource(ctx, job.OTPSourceID)
	if err != nil {
		return control.RunResult{}, err
	}
	credentials, err := e.store.GetProfileCredentials(ctx, profile.ID)
	if err != nil {
		return control.RunResult{}, err
	}
	booking, err := e.bookingForJob(ctx, job)
	if err != nil {
		return control.RunResult{}, err
	}
	provider, err := ProviderForSource(ctx, e.store, source)
	if err != nil {
		return control.RunResult{}, err
	}
	pairing := strings.HasPrefix(job.DedupKey, fmt.Sprintf("pairing:%d:", source.ID))
	if pairing {
		pairingProvider, ok := provider.(otp.PairingProvider)
		if !ok || source.Provider != model.OTPProviderBlueBubbles {
			return control.RunResult{}, errors.New("only BlueBubbles supports supervised pairing")
		}
		provider = &supervisedProvider{PairingProvider: pairingProvider, hub: e.hub, store: e.store, sourceID: source.ID, jobKey: strconv.FormatInt(job.ID, 10)}
	}
	if err := provider.Health(ctx); err != nil {
		return control.RunResult{}, fmt.Errorf("selected OTP provider is unavailable: %w", err)
	}

	profileDir, err := safeChild(e.config.ProfilesDir, profile.BrowserProfile)
	if err != nil {
		return control.RunResult{}, err
	}
	artifactDir, err := safeChild(e.config.ArtifactsDir, fmt.Sprintf("job-%d", job.ID))
	if err != nil {
		return control.RunResult{}, err
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return control.RunResult{}, fmt.Errorf("create browser profile: %w", err)
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return control.RunResult{}, fmt.Errorf("create artifact directory: %w", err)
	}

	startConfig := map[string]any{
		"profile_dir":           profileDir,
		"target_date":           booking.TargetDate,
		"timezone":              booking.Timezone,
		"login_probe_url":       booking.LoginProbeURL,
		"all_day_pass_url":      nullable(booking.AllDayPassURL),
		"half_day_pass_url":     nullable(booking.HalfDayPassURL),
		"vehicle_keyword":       profile.DefaultVehicle,
		"pass_order":            passStrings(booking.PassOrder()),
		"headless":              profile.Headless,
		"browser_channel":       nullable(profile.BrowserChannel),
		"executable_path":       nullable(profile.BrowserExecutable),
		"default_timeout_ms":    profile.DefaultTimeoutMS,
		"poll_deadline_seconds": booking.PollDeadlineSeconds,
		"poll_min_seconds":      booking.PollMinSeconds,
		"poll_max_seconds":      booking.PollMaxSeconds,
		"artifacts_dir":         artifactDir,
	}
	if job.Command == model.CommandBook {
		window, err := scheduler.WindowFor(booking)
		if err != nil {
			return control.RunResult{}, err
		}
		if time.Now().Before(window.ReleaseAt) {
			startConfig["release_at"] = window.ReleaseAt.Format(time.RFC3339)
		}
		startConfig["auth_deadline_at"] = window.AuthDeadlineAt.Format(time.RFC3339)
		if job.ExpiresAt != nil {
			remaining := int(math.Ceil(time.Until(*job.ExpiresAt).Seconds()))
			if remaining < 1 {
				return control.RunResult{}, errors.New("the scheduled booking window expired before the action could start")
			}
			if remaining < booking.PollDeadlineSeconds {
				startConfig["poll_deadline_seconds"] = remaining
			}
		}
	}

	filter := otp.Filter{RequireYodel: true}
	if source.Provider == model.OTPProviderBlueBubbles {
		if pairing {
			filter.Pairing = true
		} else {
			filter.ChatGUID = source.PairingChatGUID
			filter.Sender = source.PairingSender
			filter.Service = source.PairingService
		}
	}
	cancelGrace := time.Duration(profile.DefaultTimeoutMS)*time.Millisecond + 5*time.Second
	if cancelGrace < 20*time.Second {
		cancelGrace = 20 * time.Second
	}
	jobKey := strconv.FormatInt(job.ID, 10)
	e.event(job.ID, "job.started", "The isolated Yodel action started.")
	return control.Run(ctx, control.RunInput{
		JobID: job.ID, Command: job.Command, Mode: job.RunMode,
		StartConfig: startConfig, Credentials: credentials,
		Provider: provider, OTPFilter: filter,
		OTPTimeout:  time.Duration(booking.PollDeadlineSeconds) * time.Second,
		CancelGrace: cancelGrace, Hub: e.hub,
		NewProcess: func(processCtx context.Context) (control.ActionProcess, error) {
			return actionproc.Start(processCtx, actionproc.Config{
				Executable: e.config.PythonExecutable,
				Args:       []string{"-m", e.config.PythonModule},
				Environment: []string{
					"PYTHONUNBUFFERED=1",
				},
				CancelGrace: cancelGrace,
			})
		},
		Hooks: control.RunHooks{
			Event: func(kind, message string) {
				e.event(job.ID, kind, message)
				e.hub.Publish(jobKey, control.LiveEvent{Kind: "event", Data: map[string]any{"type": kind, "message": message}})
			},
			AwaitingApproval: func(string) error {
				_, err := e.store.TransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobAwaitingApproval, store.JobTransition{Message: "Waiting immediately before final confirmation."})
				return err
			},
			ApprovalResolved: func(decision model.ApprovalDecision) error {
				if decision != model.DecisionApprove {
					return nil
				}
				_, err := e.store.TransitionJob(ctx, job.ID, []model.JobStatus{model.JobAwaitingApproval}, model.JobRunning, store.JobTransition{Message: "Approval received; final confirmation is resuming."})
				return err
			},
			ConfirmationStarting: func() error {
				return e.store.MarkConfirmationStarted(ctx, job.ID)
			},
		},
	})
}

func (e *Engine) bookingForJob(ctx context.Context, job model.Job) (model.BookingRequest, error) {
	if job.BookingRequestID != nil {
		return e.store.GetBookingRequest(ctx, *job.BookingRequestID)
	}
	bookings, err := e.store.ListBookingRequests(ctx)
	if err != nil {
		return model.BookingRequest{}, err
	}
	for _, booking := range bookings {
		if booking.ProfileID == job.ProfileID && booking.Enabled {
			return booking, nil
		}
	}
	return model.BookingRequest{}, errors.New("the profile needs an enabled booking request to supply its Yodel login URL")
}

func (e *Engine) finish(jobID int64, status model.JobStatus, message string, exitCode *int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := e.store.GetJob(ctx, jobID)
	if err != nil || current.Status.Terminal() {
		return
	}
	_, err = e.store.TransitionJob(ctx, jobID,
		[]model.JobStatus{model.JobRunning, model.JobAwaitingApproval}, status,
		store.JobTransition{Message: message, ExitCode: exitCode})
	if err != nil {
		log.Printf("job %d final transition failed: %v", jobID, err)
		return
	}
	e.event(jobID, "job."+string(status), message)
	e.hub.Publish(strconv.FormatInt(jobID, 10), control.LiveEvent{Kind: "complete", Data: map[string]any{"status": status, "message": message}})
}

func (e *Engine) event(jobID int64, kind, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := e.store.AppendJobEvent(ctx, store.JobEventInput{
		JobID: jobID, Level: "info", Kind: kind, Message: message,
	}); err != nil {
		log.Printf("job %d event persistence failed", jobID)
	}
}

func (e *Engine) CancelJob(ctx context.Context, jobID int64) error {
	job, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if err := e.store.RequestJobCancellation(ctx, jobID); err != nil {
		return err
	}
	if job.Status == model.JobQueued {
		return nil
	}
	e.mu.Lock()
	cancel := e.active[jobID]
	e.mu.Unlock()
	if cancel == nil {
		// A CLI attached to the same database can request cancellation while the
		// serving process owns the browser. Its monitor will observe the flag.
		return nil
	}
	e.hub.CancelJob(strconv.FormatInt(jobID, 10))
	cancel(ErrUserCancelled)
	return nil
}

func (e *Engine) Decide(ctx context.Context, jobID int64, decision model.ApprovalDecision) error {
	if _, err := e.store.RecordJobDecision(ctx, jobID, decision); err != nil {
		return err
	}
	err := e.hub.Decide(strconv.FormatInt(jobID, 10), string(decision))
	if errors.Is(err, control.ErrDecisionAlreadySet) || errors.Is(err, control.ErrDecisionNotPending) {
		return nil
	}
	return err
}

func (e *Engine) ChoosePairing(jobID int64, messageID string) error {
	return e.hub.ChoosePairing(strconv.FormatInt(jobID, 10), messageID)
}

func (e *Engine) QueuePairing(ctx context.Context, sourceID int64) (model.Job, error) {
	source, err := e.store.GetOTPSource(ctx, sourceID)
	if err != nil {
		return model.Job{}, err
	}
	if source.Provider != model.OTPProviderBlueBubbles {
		return model.Job{}, errors.New("only BlueBubbles sources require supervised pairing")
	}
	profiles, err := e.store.ListProfiles(ctx)
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
	bookings, err := e.store.ListBookingRequests(ctx)
	if err != nil {
		return model.Job{}, err
	}
	var bookingID int64
	for _, booking := range bookings {
		if booking.ProfileID == profile.ID && booking.Enabled {
			bookingID = booking.ID
			break
		}
	}
	if bookingID == 0 {
		return model.Job{}, errors.New("create an enabled booking request for this profile before pairing")
	}
	jobs, err := e.store.ListJobs(ctx, 500)
	if err != nil {
		return model.Job{}, err
	}
	prefix := fmt.Sprintf("pairing:%d:", sourceID)
	for _, job := range jobs {
		if job.OTPSourceID == sourceID && strings.HasPrefix(job.DedupKey, prefix) && !job.Status.Terminal() {
			return model.Job{}, store.ErrConflict
		}
	}
	return e.store.EnqueueJob(ctx, store.EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
		DedupKey: prefix + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
}

func (e *Engine) QueueBooking(ctx context.Context, bookingID int64, command model.JobCommand, mode model.RunMode) (model.Job, error) {
	booking, err := e.store.GetBookingRequest(ctx, bookingID)
	if err != nil {
		return model.Job{}, err
	}
	if command == model.CommandDryRun {
		mode = model.RunModeDryRun
	} else if command == model.CommandBook && !mode.Valid() {
		mode = booking.ConfirmationMode
	} else if command == model.CommandAuthCheck {
		mode = model.RunModeManual
	}
	return e.store.EnqueueJob(ctx, store.EnqueueJobParams{
		BookingRequestID: &bookingID, Command: command, RunMode: mode, DueAt: time.Now().UTC(),
	})
}

func (e *Engine) Wait(ctx context.Context, jobID int64) (model.Job, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := e.store.GetJob(ctx, jobID)
		if err != nil {
			return model.Job{}, err
		}
		if job.Status.Terminal() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return model.Job{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *Engine) scheduleLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		e.queueScheduled(e.ctx, time.Now())
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) queueScheduled(ctx context.Context, now time.Time) {
	requests, err := e.store.ListScheduledBookingRequests(ctx)
	if err != nil {
		log.Printf("scheduled booking scan failed: %v", err)
		return
	}
	for _, request := range requests {
		window, err := scheduler.WindowFor(request)
		if err != nil || !scheduler.ShouldQueue(now, window) {
			continue
		}
		_, err = e.store.EnqueueJob(ctx, store.EnqueueJobParams{
			BookingRequestID: &request.ID, Command: model.CommandBook,
			RunMode: request.ConfirmationMode, DueAt: now.UTC(), ExpiresAt: &window.PollEndsAt,
			DedupKey: scheduler.DedupKey(request),
		})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			log.Printf("scheduled booking %d could not be queued: %v", request.ID, err)
		}
	}
}

func ProviderForSource(ctx context.Context, database *store.Store, source model.OTPSource) (otp.Provider, error) {
	switch source.Provider {
	case model.OTPProviderBlueBubbles:
		var providerConfig bluebubbles.Config
		if err := database.GetOTPSourceConfig(ctx, source.ID, &providerConfig); err != nil {
			return nil, err
		}
		providerConfig.ChatGUID = source.PairingChatGUID
		providerConfig.Sender = source.PairingSender
		providerConfig.Service = source.PairingService
		return bluebubbles.New(providerConfig)
	case model.OTPProviderTwilio:
		var providerConfig twilio.Config
		if err := database.GetOTPSourceConfig(ctx, source.ID, &providerConfig); err != nil {
			return nil, err
		}
		return twilio.New(providerConfig)
	default:
		return nil, fmt.Errorf("unsupported OTP provider %q", source.Provider)
	}
}

func safeChild(parent, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", errors.New("browser profile name must be one directory name")
	}
	path := filepath.Join(parent, name)
	return filepath.Abs(path)
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func passStrings(values []model.PassType) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

type supervisedProvider struct {
	otp.PairingProvider
	hub      *control.Hub
	store    *store.Store
	sourceID int64
	jobKey   string
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
	if err := p.store.UpdateOTPSourcePairing(ctx, p.sourceID, selected.ChatGUID, selected.Sender, selected.Service); err != nil {
		return otp.Message{}, err
	}
	return selected, nil
}
