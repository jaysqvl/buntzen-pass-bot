// Package engine joins the durable queue, provider adapters, and isolated
// action processes into the long-running control plane.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/observability"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

var ErrUserCancelled = errors.New("job cancelled by operator")

type Engine struct {
	config      config.Config
	store       *store.Store
	hub         *control.Hub
	workerOwner string

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
		workerOwner: fmt.Sprintf("%s-%d", host, os.Getpid()),
		active:      make(map[int64]context.CancelCauseFunc),
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
	slog.Info("job engine starting",
		"workers", e.config.MaxConcurrentJobs,
		"schedules_enabled", e.config.SchedulesEnabled,
	)
	for worker := 0; worker < e.config.MaxConcurrentJobs; worker++ {
		e.wg.Add(1)
		go e.worker(worker)
	}
	e.wg.Add(1)
	go e.maintenanceLoop()
	if e.config.SchedulesEnabled {
		e.wg.Add(1)
		go e.scheduleLoop()
	}
}

func (e *Engine) Stop() {
	slog.Info("job engine stopping")
	e.mu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
	slog.Info("job engine stopped")
}

func (e *Engine) Hub() *control.Hub { return e.hub }

func (e *Engine) worker(index int) {
	defer e.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := e.store.SystemClaimNextDueJob(e.ctx, fmt.Sprintf("%s-%d", e.workerOwner, index))
		switch {
		case err == nil:
			slog.Info("job claimed",
				"job_id", job.ID,
				"worker_index", index,
				"command", job.Command,
				"mode", job.RunMode,
			)
			e.runClaimed(job)
		case errors.Is(err, store.ErrNotFound):
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
			}
		default:
			slog.Error("job queue claim failed", "worker_index", index, "error", err)
			select {
			case <-e.ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (e *Engine) runClaimed(job model.Job) {
	startedAt := time.Now()
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
		slog.Debug("job worker released",
			"job_id", job.ID,
			"duration", time.Since(startedAt).Round(time.Millisecond),
		)
	}()
	go e.monitorCancellation(monitorCtx, job.ID, cancel)
	go e.monitorArtifacts(monitorCtx, job.ID, cancel)
	defer func() {
		if err := e.enforceArtifactLimit(job.ID); err != nil {
			slog.Error("job artifact cleanup failed", "job_id", job.ID, "error", err)
		}
	}()

	result, runErr := e.execute(jobCtx, job)
	if runErr != nil {
		slog.Error("job execution failed",
			"job_id", job.ID,
			"command", job.Command,
			"error", runErr,
		)
		current, _ := e.store.SystemGetJob(context.Background(), job.ID)
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
		} else if errors.Is(context.Cause(jobCtx), ErrArtifactLimit) {
			message = "Job diagnostics exceeded the per-job storage limit."
		}
		e.finish(job.ID, status, message, nil)
		return
	}

	current, _ := e.store.SystemGetJob(context.Background(), job.ID)
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
	} else if errors.Is(context.Cause(jobCtx), ErrArtifactLimit) && current.ConfirmationStartedAt == nil {
		result.Status = model.JobFailed
		result.Message = "Job diagnostics exceeded the per-job storage limit."
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
			requested, err := e.store.SystemJobCancellationRequested(checkCtx, jobID)
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
	slog.Debug("loading job execution inputs", "job_id", job.ID)
	profile, err := e.store.SystemGetProfile(ctx, job.ProfileID)
	if err != nil {
		return control.RunResult{}, err
	}
	source, err := e.store.SystemGetOTPSource(ctx, job.OTPSourceID)
	if err != nil {
		return control.RunResult{}, err
	}
	booking, err := e.bookingForJob(ctx, job)
	if err != nil {
		return control.RunResult{}, err
	}
	// Validate the operator-owned origin boundary before decrypting either the
	// Yodel credentials or provider configuration. Persisted booking URLs are
	// never authority to choose a credential recipient.
	if err := booking.ValidateForOrigins(e.config.YodelOrigins); err != nil {
		return control.RunResult{}, err
	}
	var bookWindow *scheduler.Window
	if job.Command == model.CommandBook {
		window, err := scheduler.WindowFor(booking)
		if err != nil {
			return control.RunResult{}, err
		}
		now := time.Now()
		if now.Before(window.PrepAt) {
			return control.RunResult{}, errors.New("the booking job became runnable before its bounded preparation window")
		}
		if !now.Before(window.PollEndsAt) {
			return control.RunResult{}, errors.New("the booking release window ended before the action could start")
		}
		bookWindow = &window
	}
	credentials, err := e.store.SystemGetProfileCredentials(ctx, profile.ID)
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
		provider = &supervisedProvider{PairingProvider: pairingProvider, hub: e.hub, store: e.store, jobID: job.ID, sourceID: source.ID, jobKey: strconv.FormatInt(job.ID, 10)}
	}
	slog.Debug("checking selected OTP provider",
		"job_id", job.ID,
		"source_id", source.ID,
		"provider", source.Provider,
		"pairing", pairing,
	)
	if err := provider.Health(ctx); err != nil {
		return control.RunResult{}, fmt.Errorf("selected OTP provider is unavailable: %w", err)
	}
	slog.Debug("selected OTP provider is healthy", "job_id", job.ID, "source_id", source.ID, "provider", source.Provider)

	if err := e.cleanupProfiles(ctx); err != nil {
		return control.RunResult{}, err
	}
	profileDir, err := ensureManagedProfileDirectory(e.config.ProfilesDir, profile)
	if err != nil {
		return control.RunResult{}, err
	}
	artifactDir, err := safeChild(e.config.ArtifactsDir, fmt.Sprintf("job-%d", job.ID))
	if err != nil {
		return control.RunResult{}, err
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return control.RunResult{}, fmt.Errorf("create artifact directory: %w", err)
	}

	startConfig := map[string]any{
		"profile_dir":           profileDir,
		"target_date":           booking.TargetDate,
		"timezone":              booking.Timezone,
		"login_probe_url":       booking.LoginProbeURL,
		"allowed_yodel_origins": append([]string(nil), e.config.YodelOrigins...),
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
		window := *bookWindow
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
	slog.Info("isolated browser action starting",
		"job_id", job.ID,
		"profile_id", profile.ID,
		"source_id", source.ID,
		"provider", source.Provider,
		"command", job.Command,
		"mode", job.RunMode,
		"headless", profile.Headless,
	)
	return control.Run(ctx, control.RunInput{
		JobID: job.ID, Command: job.Command, Mode: job.RunMode,
		StartConfig: startConfig, Credentials: credentials,
		Provider: provider, OTPFilter: filter,
		OTPTimeout:  time.Duration(booking.PollDeadlineSeconds) * time.Second,
		CancelGrace: cancelGrace, Hub: e.hub,
		NewProcess: func(processCtx context.Context) (control.ActionProcess, error) {
			session, err := actionproc.Start(processCtx, actionproc.Config{
				Executable: e.config.PythonExecutable,
				Args:       []string{"-m", e.config.PythonModule},
				Environment: []string{
					"PYTHONUNBUFFERED=1",
					"BUNTZEN_ACTION_LOG_LEVEL=" + e.config.EffectiveLogLevel(),
				},
				CancelGrace: cancelGrace,
				OnStderr: func(line string) {
					observability.LogActionDiagnostic(processCtx, jobKey, line, credentials.Phone)
				},
			})
			if err != nil {
				return nil, err
			}
			slog.Debug("python action process started", "job_id", job.ID)
			return session, nil
		},
		Hooks: control.RunHooks{
			Event: func(kind, message string) {
				slog.Debug("job lifecycle event", "job_id", job.ID, "event", kind, "detail", message)
				e.event(job.ID, kind, message)
				e.hub.Publish(jobKey, control.LiveEvent{Kind: "event", Data: map[string]any{"type": kind, "message": message}})
			},
			Diagnostic: func(operation string, err error) {
				slog.Warn("isolated action diagnostic", "job_id", job.ID, "operation", operation, "error", err)
			},
			AwaitingApproval: func(string) error {
				_, err := e.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobAwaitingApproval, store.JobTransition{Message: "Waiting immediately before final confirmation."})
				return err
			},
			ApprovalResolved: func(decision model.ApprovalDecision) error {
				if decision != model.DecisionApprove {
					return nil
				}
				_, err := e.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobAwaitingApproval}, model.JobRunning, store.JobTransition{Message: "Approval received; final confirmation is resuming."})
				return err
			},
			ConfirmationStarting: func() error {
				return e.store.SystemMarkConfirmationStarted(ctx, job.ID)
			},
		},
	})
}

func (e *Engine) bookingForJob(ctx context.Context, job model.Job) (model.BookingRequest, error) {
	if job.BookingRequestID != nil {
		return e.store.SystemGetBookingRequest(ctx, *job.BookingRequestID)
	}
	bookings, err := e.store.SystemListBookingRequests(ctx)
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
	current, err := e.store.SystemGetJob(ctx, jobID)
	if err != nil || current.Status.Terminal() {
		return
	}
	expected := []model.JobStatus{model.JobRunning}
	if status != model.JobSucceeded {
		expected = append(expected, model.JobAwaitingApproval)
	}
	finished, err := e.store.SystemTransitionJob(ctx, jobID, expected, status,
		store.JobTransition{Message: message, ExitCode: exitCode})
	if err != nil {
		slog.Error("job final transition failed", "job_id", jobID, "status", status, "error", err)
		return
	}
	slog.Info("job finished", "job_id", jobID, "status", finished.Status, "exit_code", exitCodeValue(exitCode))
	e.event(jobID, "job."+string(finished.Status), finished.Message)
	e.hub.Publish(strconv.FormatInt(jobID, 10), control.LiveEvent{Kind: "complete", Data: map[string]any{"status": finished.Status, "message": finished.Message}})
}

func (e *Engine) event(jobID int64, kind, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := e.store.SystemAppendJobEvent(ctx, store.JobEventInput{
		JobID: jobID, Level: "info", Kind: kind, Message: message,
	}); err != nil {
		slog.Error("job event persistence failed", "job_id", jobID, "event", kind)
	}
}

func (e *Engine) CancelJob(ctx context.Context, userID, jobID int64) error {
	resources := e.store.ForUser(userID)
	job, err := resources.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if err := resources.RequestJobCancellation(ctx, jobID); err != nil {
		return err
	}
	slog.Info("job cancellation requested", "job_id", jobID, "status", job.Status)
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

func (e *Engine) Decide(ctx context.Context, userID, jobID int64, decision model.ApprovalDecision) error {
	if _, err := e.store.ForUser(userID).RecordJobDecision(ctx, jobID, decision); err != nil {
		return err
	}
	slog.Info("manual job decision recorded", "job_id", jobID, "decision", decision)
	err := e.hub.Decide(strconv.FormatInt(jobID, 10), string(decision))
	if errors.Is(err, control.ErrDecisionAlreadySet) || errors.Is(err, control.ErrDecisionNotPending) {
		return nil
	}
	return err
}

func (e *Engine) SystemWait(ctx context.Context, jobID int64) (model.Job, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := e.store.SystemGetJob(ctx, jobID)
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

func exitCodeValue(exitCode *int) any {
	if exitCode == nil {
		return nil
	}
	return *exitCode
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
