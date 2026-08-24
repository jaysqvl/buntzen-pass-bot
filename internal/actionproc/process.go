// Package actionproc supervises one isolated Python action process per job.
//
// The boundary is deliberately small: bounded, versioned JSON-lines on
// stdin/stdout, while stderr is treated only as a sanitized diagnostic stream.
package actionproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ProtocolVersion = 1
	MaxFrameBytes   = 64 * 1024
)

var (
	ErrFrameTooLarge       = errors.New("action protocol frame exceeds 64 KiB")
	ErrMalformedFrame      = errors.New("malformed action protocol frame")
	ErrUnsupportedVersion  = errors.New("unsupported action protocol version")
	ErrProcessAlreadyEnded = errors.New("action process has already ended")
)

// Only process-launch settings needed by Python/Playwright cross the action
// boundary. In particular, BUNTZEN_* and provider credentials from the Go
// control-plane environment are intentionally not inherited by workers.
var inheritedEnvironmentKeys = []string{
	"HOME",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"PATH",
	"PLAYWRIGHT_BROWSERS_PATH",
	"SSL_CERT_DIR",
	"SSL_CERT_FILE",
	"TMPDIR",
	"TZ",
	"XDG_CACHE_HOME",
}

var allowedEnvironmentOverrides = map[string]struct{}{
	"BUNTZEN_ACTIONPROC_HELPER": {}, // Test helper only; never contains a secret.
	"PYTHONDONTWRITEBYTECODE":   {},
	"PYTHONUNBUFFERED":          {},
}

// Frame is a decoded protocol frame. Payload never gets logged by this
// package; it can contain just-in-time credentials or an active OTP on the
// control-plane-to-child direction.
type Frame struct {
	Version int
	Type    string
	Payload map[string]any
}

func decodeFrame(raw []byte) (Frame, error) {
	if len(raw) > MaxFrameBytes {
		return Frame{}, ErrFrameTooLarge
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Frame{}, fmt.Errorf("%w: invalid JSON", ErrMalformedFrame)
	}
	version, ok := payload["v"].(float64)
	if !ok || version != ProtocolVersion {
		return Frame{}, ErrUnsupportedVersion
	}
	kind, ok := payload["type"].(string)
	if !ok || strings.TrimSpace(kind) == "" {
		return Frame{}, fmt.Errorf("%w: missing type", ErrMalformedFrame)
	}
	return Frame{Version: ProtocolVersion, Type: kind, Payload: payload}, nil
}

// Config describes an executable without involving a shell. Production uses
// Python with Args set to {"-m", "buntzen_actions"}; tests can provide a
// purpose-built helper executable.
type Config struct {
	Executable  string
	Args        []string
	Environment []string
	CancelGrace time.Duration
	OnStderr    func(string)
}

type Result struct {
	ExitCode int
	Err      error
}

type Session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	events    chan Frame
	done      chan Result
	writeMu   sync.Mutex
	cancelOne sync.Once
	ended     chan struct{}
}

func Start(ctx context.Context, config Config) (*Session, error) {
	if strings.TrimSpace(config.Executable) == "" {
		return nil, errors.New("action executable is required")
	}
	if config.CancelGrace <= 0 {
		config.CancelGrace = 5 * time.Second
	}

	cmd := exec.Command(config.Executable, config.Args...)
	configureProcessGroup(cmd)
	environment, err := childEnvironment(config.Environment)
	if err != nil {
		return nil, err
	}
	cmd.Env = environment
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open action stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open action stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open action stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start action process: %w", err)
	}

	session := &Session{
		cmd:    cmd,
		stdin:  stdin,
		events: make(chan Frame, 128),
		done:   make(chan Result, 1),
		ended:  make(chan struct{}),
	}
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan struct{})
	go session.readFrames(stdout, stdoutDone)
	go drainStderr(stderr, config.OnStderr, stderrDone)
	go session.wait(stdoutDone, stderrDone)
	go func() {
		select {
		case <-ctx.Done():
			session.Cancel(config.CancelGrace)
		case <-session.ended:
		}
	}()
	return session, nil
}

func childEnvironment(overrides []string) ([]string, error) {
	values := make(map[string]string, len(inheritedEnvironmentKeys)+len(overrides))
	for _, key := range inheritedEnvironmentKeys {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for _, entry := range overrides {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("invalid action environment override")
		}
		if _, ok := allowedEnvironmentOverrides[key]; !ok {
			return nil, fmt.Errorf("action environment override %q is not allowed", key)
		}
		values[key] = value
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment, nil
}

func (s *Session) Events() <-chan Frame { return s.events }
func (s *Session) Done() <-chan Result  { return s.done }

// Send writes one bounded protocol frame. It never formats the payload into an
// error or diagnostic, which keeps injected credentials and OTPs out of logs.
func (s *Session) Send(kind string, payload map[string]any) error {
	if strings.TrimSpace(kind) == "" {
		return errors.New("action frame type is required")
	}
	select {
	case <-s.ended:
		return ErrProcessAlreadyEnded
	default:
	}
	frame := make(map[string]any, len(payload)+2)
	for key, value := range payload {
		if key == "v" || key == "type" {
			continue
		}
		frame[key] = value
	}
	frame["v"] = ProtocolVersion
	frame["type"] = kind
	raw, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode action frame: %w", err)
	}
	if len(raw)+1 > MaxFrameBytes {
		return ErrFrameTooLarge
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stdin.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write action frame: %w", err)
	}
	return nil
}

// Cancel first asks the worker to clean up its browser. If it does not exit
// during grace, the child is killed. Repeated cancellation is idempotent.
func (s *Session) Cancel(grace time.Duration) {
	s.cancelOne.Do(func() {
		_ = s.Send("control.cancel", nil)
		go func() {
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case <-s.ended:
			case <-timer.C:
				s.forceKill()
			}
		}()
	})
}

func (s *Session) readFrames(reader io.Reader, finished chan<- error) {
	defer close(s.events)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	for scanner.Scan() {
		frame, err := decodeFrame(scanner.Bytes())
		if err != nil {
			s.forceKill()
			finished <- err
			return
		}
		s.events <- frame
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			s.forceKill()
			finished <- ErrFrameTooLarge
			return
		}
		finished <- fmt.Errorf("read action protocol: %w", err)
		return
	}
	finished <- nil
}

func (s *Session) forceKill() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = killProcessGroup(s.cmd)
}

func (s *Session) wait(stdoutDone <-chan error, stderrDone <-chan struct{}) {
	processErr := s.cmd.Wait()
	protocolErr := <-stdoutDone
	<-stderrDone
	_ = s.stdin.Close()
	exitCode := 0
	if s.cmd.ProcessState != nil {
		exitCode = s.cmd.ProcessState.ExitCode()
	}
	resultErr := protocolErr
	if resultErr == nil && processErr != nil {
		resultErr = processErr
	}
	close(s.ended)
	s.done <- Result{ExitCode: exitCode, Err: resultErr}
	close(s.done)
}

func drainStderr(reader io.Reader, callback func(string), finished chan<- struct{}) {
	defer close(finished)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	for scanner.Scan() {
		if callback != nil {
			line := strings.TrimSpace(scanner.Text())
			if len(line) > 4096 {
				line = line[:4096] + "..."
			}
			callback(line)
		}
	}
}
