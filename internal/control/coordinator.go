package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
)

var standaloneCode = regexp.MustCompile(`(^|[^0-9])[0-9]{4,8}([^0-9]|$)`)

// ActionProcess is the small portion of an isolated process used by the
// coordinator. Keeping the interface here makes protocol behavior testable
// without launching a browser.
type ActionProcess interface {
	Events() <-chan actionproc.Frame
	Done() <-chan actionproc.Result
	Send(string, map[string]any) error
	Cancel(time.Duration)
}

type ProcessFactory func(context.Context) (ActionProcess, error)

type RunInput struct {
	JobID       int64
	Command     model.JobCommand
	Mode        model.RunMode
	StartConfig map[string]any
	Credentials model.ProfileCredentials
	Provider    otp.Provider
	OTPFilter   otp.Filter
	OTPTimeout  time.Duration
	CancelGrace time.Duration
	Hub         *Hub
	NewProcess  ProcessFactory
	Hooks       RunHooks
}

// RunHooks bridge transient protocol activity to sanitized durable state.
// Implementations must never store a raw OTP or provider/credential material.
type RunHooks struct {
	Event                func(kind, message string)
	Diagnostic           func(operation string, err error)
	AwaitingApproval     func(approvalID string) error
	ApprovalResolved     func(decision model.ApprovalDecision) error
	ConfirmationStarting func() error
}

type RunResult struct {
	Status   model.JobStatus
	Message  string
	ExitCode int
}

type pendingChallenge struct {
	armed  otp.Armed
	cancel context.CancelFunc
}

type asyncResult struct {
	kind        string
	correlation string
	message     otp.Message
	decision    string
	err         error
}

// Run drives the Python protocol until both a terminal frame and process exit.
// A crash after confirmation.starting is always upgraded to outcome_unknown.
func Run(ctx context.Context, input RunInput) (RunResult, error) {
	if input.NewProcess == nil || input.Provider == nil || input.Hub == nil {
		return RunResult{}, errors.New("action process, OTP provider, and live hub are required")
	}
	// The coordinator owns every process and asynchronous wait it starts. A
	// terminal worker must release them even when its parent context stays alive.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if input.OTPTimeout <= 0 {
		input.OTPTimeout = 2 * time.Minute
	}
	if input.CancelGrace <= 0 {
		input.CancelGrace = 20 * time.Second
	}
	process, err := input.NewProcess(ctx)
	if err != nil {
		return RunResult{}, err
	}
	jobKey := strconv.FormatInt(input.JobID, 10)
	defer input.Hub.ClearOTP(jobKey)

	events := input.eventSink()
	async := make(chan asyncResult, 8)
	challenges := make(map[string]*pendingChallenge)
	var challengeMu sync.Mutex
	cancelChallenges := func() {
		challengeMu.Lock()
		defer challengeMu.Unlock()
		for _, challenge := range challenges {
			if challenge.cancel != nil {
				challenge.cancel()
			}
		}
	}
	defer cancelChallenges()

	ready := false
	confirmationStarted := false
	confirmationID := ""
	var terminal *RunResult
	eventStream := process.Events()
	doneStream := process.Done()
	ctxDone := ctx.Done()
	var processResult *actionproc.Result
	for {
		select {
		case <-ctxDone:
			input.Hub.CancelJob(jobKey)
			process.Cancel(input.CancelGrace)
			ctxDone = nil
		case frame, ok := <-eventStream:
			if !ok {
				eventStream = nil
				if processResult != nil {
					return finishRun(ctx, terminal, confirmationStarted, *processResult)
				}
				continue
			}
			if !ready {
				protocol, protocolOK := frame.Payload["protocol"].(float64)
				if frame.Type != "worker.ready" ||
					stringValue(frame.Payload, "action") != "yodel" ||
					!protocolOK || protocol != float64(actionproc.ProtocolVersion) {
					process.Cancel(input.CancelGrace)
					return RunResult{}, errors.New("action worker did not negotiate the Yodel v2 protocol")
				}
				ready = true
				if err := process.Send("run.start", map[string]any{
					"run_id":  jobKey,
					"command": string(input.Command),
					"mode":    string(input.Mode),
					"config":  input.StartConfig,
				}); err != nil {
					process.Cancel(input.CancelGrace)
					return RunResult{}, err
				}
				continue
			}
			if err := handleFrame(ctx, input, process, jobKey, frame, challenges, &challengeMu, async, &confirmationStarted, &confirmationID, &terminal); err != nil {
				process.Cancel(input.CancelGrace)
				return RunResult{}, err
			}
		case result := <-async:
			switch result.kind {
			case "otp":
				challengeMu.Lock()
				_, stillActive := challenges[result.correlation]
				challengeMu.Unlock()
				if !stillActive {
					continue
				}
				if result.err != nil {
					input.diagnostic("otp.wait", result.err)
					responseKind := "otp.error"
					message := "OTP provider failed while waiting for a fresh code."
					if errors.Is(result.err, context.DeadlineExceeded) {
						responseKind = "otp.expired"
						message = "No fresh OTP arrived before the challenge expired."
					}
					_ = process.Send(responseKind, map[string]any{"challenge_id": result.correlation})
					events(responseKind, message)
					continue
				}
				expires := time.Now().Add(input.OTPTimeout)
				input.Hub.SetOTP(jobKey, result.message.Code, expires)
				if err := process.Send("otp.provide", map[string]any{
					"challenge_id": result.correlation,
					"code":         result.message.Code,
				}); err != nil {
					input.Hub.ClearOTP(jobKey)
					return RunResult{}, err
				}
				events("otp.received", "A fresh OTP was delivered to the browser action.")
			case "approval":
				decision := model.ApprovalDecision(result.decision)
				if result.err != nil || !decision.Valid() {
					_ = process.Send("approval.cancel", map[string]any{"approval_id": result.correlation})
					continue
				}
				if input.Hooks.ApprovalResolved != nil {
					if err := input.Hooks.ApprovalResolved(decision); err != nil {
						return RunResult{}, err
					}
				}
				kind := "approval.cancel"
				if decision == model.DecisionApprove {
					kind = "approval.approve"
				}
				if err := process.Send(kind, map[string]any{"approval_id": result.correlation}); err != nil {
					return RunResult{}, err
				}
			}
		case result, ok := <-doneStream:
			if !ok {
				doneStream = nil
				continue
			}
			processResult = &result
			if result.Err != nil {
				input.diagnostic("process.exit", result.Err)
			}
			doneStream = nil
			cancelChallenges()
			input.Hub.ClearOTP(jobKey)
			if eventStream == nil {
				return finishRun(ctx, terminal, confirmationStarted, result)
			}
		}
	}
}

func finishRun(ctx context.Context, terminal *RunResult, confirmationStarted bool, processResult actionproc.Result) (RunResult, error) {
	if terminal != nil {
		terminal.ExitCode = processResult.ExitCode
		if confirmationStarted && terminal.Status == model.JobFailed {
			terminal.Status = model.JobOutcomeUnknown
			terminal.Message = "The action ended after final confirmation may have started; booking outcome is unknown."
		}
		return *terminal, nil
	}
	if confirmationStarted {
		return RunResult{Status: model.JobOutcomeUnknown, Message: "The action process ended after final confirmation may have started; booking outcome is unknown.", ExitCode: processResult.ExitCode}, nil
	}
	if ctx.Err() != nil {
		return RunResult{Status: model.JobInterrupted, Message: "The action was interrupted before completion.", ExitCode: processResult.ExitCode}, nil
	}
	return RunResult{Status: model.JobFailed, Message: "The action process ended without a terminal result.", ExitCode: processResult.ExitCode}, nil
}

func (input RunInput) eventSink() func(string, string) {
	if input.Hooks.Event != nil {
		return input.Hooks.Event
	}
	return func(string, string) {}
}

func (input RunInput) diagnostic(operation string, err error) {
	if input.Hooks.Diagnostic != nil && err != nil {
		input.Hooks.Diagnostic(operation, err)
	}
}

func handleFrame(
	ctx context.Context,
	input RunInput,
	process ActionProcess,
	jobKey string,
	frame actionproc.Frame,
	challenges map[string]*pendingChallenge,
	challengeMu *sync.Mutex,
	async chan<- asyncResult,
	confirmationStarted *bool,
	confirmationID *string,
	terminal **RunResult,
) error {
	events := input.eventSink()
	switch frame.Type {
	case "run.status":
		phase := safeToken(stringValue(frame.Payload, "phase"))
		message := sanitizeMessage(stringValue(frame.Payload, "message"), input.Credentials)
		events("run."+phase, message)
		input.Hub.Publish(jobKey, LiveEvent{Kind: "status", Data: map[string]any{"phase": phase, "message": message}})
	case "heartbeat", "booking.poll", "release.ready":
		input.Hub.Publish(jobKey, LiveEvent{Kind: frame.Type, Data: map[string]any{}})
	case "credentials.request":
		requestID, err := correlation(frame.Payload, "request_id")
		if err != nil {
			return err
		}
		return process.Send("credentials.provide", map[string]any{
			"request_id": requestID,
			"phone":      input.Credentials.Phone,
		})
	case "otp.prepare":
		challengeID, err := correlation(frame.Payload, "challenge_id")
		if err != nil {
			return err
		}
		challengeMu.Lock()
		_, exists := challenges[challengeID]
		challengeMu.Unlock()
		if exists {
			return errors.New("duplicate OTP challenge identifier")
		}
		armed, err := input.Provider.Arm(ctx, input.OTPFilter)
		if err != nil {
			input.diagnostic("otp.arm", err)
			_ = process.Send("otp.error", map[string]any{"challenge_id": challengeID})
			events("otp.arm_failed", "OTP provider could not be armed.")
			return nil
		}
		challengeMu.Lock()
		challenges[challengeID] = &pendingChallenge{armed: armed}
		challengeMu.Unlock()
		events("otp.armed", "OTP inbox cursor was captured before the login action.")
		return process.Send("otp.ready", map[string]any{"challenge_id": challengeID})
	case "otp.triggered":
		challengeID, err := correlation(frame.Payload, "challenge_id")
		if err != nil {
			return err
		}
		challengeMu.Lock()
		challenge, ok := challenges[challengeID]
		if !ok || challenge.cancel != nil {
			challengeMu.Unlock()
			return errors.New("OTP was triggered without a unique armed challenge")
		}
		waitCtx, cancel := context.WithTimeout(ctx, input.OTPTimeout)
		challenge.cancel = cancel
		armed := challenge.armed
		challengeMu.Unlock()
		go func() {
			message, waitErr := input.Provider.WaitForCode(waitCtx, armed)
			select {
			case async <- asyncResult{kind: "otp", correlation: challengeID, message: message, err: waitErr}:
			case <-ctx.Done():
			}
		}()
	case "otp.submitted", "otp.not_required", "otp.failed":
		challengeID, err := correlation(frame.Payload, "challenge_id")
		if err != nil {
			return err
		}
		input.Hub.ClearOTP(jobKey)
		challengeMu.Lock()
		if challenge, ok := challenges[challengeID]; ok && challenge.cancel != nil {
			challenge.cancel()
		}
		delete(challenges, challengeID)
		challengeMu.Unlock()
		events(frame.Type, "OTP challenge ended and transient code state was cleared.")
	case "approval.request":
		approvalID, err := correlation(frame.Payload, "approval_id")
		if err != nil {
			return err
		}
		if err := input.Hub.BeginDecision(jobKey); err != nil {
			return err
		}
		if input.Hooks.AwaitingApproval != nil {
			if err := input.Hooks.AwaitingApproval(approvalID); err != nil {
				return err
			}
		}
		events("approval.requested", "The browser is waiting immediately before final confirmation.")
		input.Hub.Publish(jobKey, LiveEvent{Kind: "approval", Data: map[string]any{"active": true}})
		go func() {
			decision, waitErr := input.Hub.WaitDecision(ctx, jobKey)
			select {
			case async <- asyncResult{kind: "approval", correlation: approvalID, decision: decision, err: waitErr}:
			case <-ctx.Done():
			}
		}()
	case "approval.approved", "approval.cancelled", "approval.expired":
		input.Hub.Publish(jobKey, LiveEvent{Kind: "approval", Data: map[string]any{"active": false}})
		events(frame.Type, "Manual approval state changed.")
	case "confirmation.starting":
		startedID, err := correlation(frame.Payload, "confirmation_id")
		if err != nil {
			return err
		}
		if *confirmationStarted {
			return errors.New("final confirmation was already armed")
		}
		if input.Hooks.ConfirmationStarting != nil {
			if err := input.Hooks.ConfirmationStarting(); err != nil {
				_ = process.Send("confirmation.error", map[string]any{"confirmation_id": startedID})
				return err
			}
		}
		// Once this flag is set, both the durable store and in-memory crash
		// classification conservatively treat any later failure as ambiguous.
		// Python cannot click until it receives the acknowledgement below.
		*confirmationStarted = true
		*confirmationID = startedID
		events("confirmation.starting", "Final confirmation is starting; a crash from this point may leave an unknown outcome.")
		return process.Send("confirmation.ready", map[string]any{"confirmation_id": startedID})
	case "confirmation.completed":
		if !*confirmationStarted {
			return errors.New("final confirmation completed without a durable start marker")
		}
		completedID, err := correlation(frame.Payload, "confirmation_id")
		if err != nil {
			return err
		}
		if completedID != *confirmationID {
			return errors.New("final confirmation completion identifier did not match its durable start marker")
		}
		events("confirmation.completed", "Yodel reported that final confirmation completed.")
	case "run.complete":
		status, err := terminalStatus(stringValue(frame.Payload, "status"))
		if err != nil {
			return err
		}
		message := sanitizeMessage(stringValue(frame.Payload, "message"), input.Credentials)
		*terminal = &RunResult{Status: status, Message: message}
	default:
		return fmt.Errorf("unsupported action event %q", frame.Type)
	}
	return nil
}

func correlation(payload map[string]any, key string) (string, error) {
	value := stringValue(payload, key)
	if value == "" || len(value) > 128 {
		return "", fmt.Errorf("action frame requires a bounded %s", key)
	}
	return value, nil
}

func stringValue(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func safeToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var result strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			result.WriteRune(r)
		}
	}
	if result.Len() == 0 {
		return "status"
	}
	if result.Len() > 48 {
		return result.String()[:48]
	}
	return result.String()
}

func sanitizeMessage(message string, credentials model.ProfileCredentials) string {
	message = strings.TrimSpace(message)
	for _, secret := range []string{credentials.Phone} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = standaloneCode.ReplaceAllString(message, "$1[redacted-code]$2")
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, message)
	if len(message) > 1000 {
		message = message[:1000] + "..."
	}
	return message
}

func terminalStatus(value string) (model.JobStatus, error) {
	switch value {
	case "succeeded":
		return model.JobSucceeded, nil
	case "failed":
		return model.JobFailed, nil
	case "cancelled":
		return model.JobCancelled, nil
	case "outcome_unknown":
		return model.JobOutcomeUnknown, nil
	default:
		return "", fmt.Errorf("unsupported terminal action status %q", value)
	}
}
