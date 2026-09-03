package share

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	DefaultCommandTimeout = 120 * time.Second
	MaxCommandTimeout     = 600 * time.Second
	capabilitiesTimeout   = 10 * time.Second
	commandStdoutLimit    = int64(1 << 20)
	commandStderrLimit    = int64(64 << 10)
	commandWaitDelay      = time.Second
)

type CommandPublisher struct {
	argv    []string
	timeout time.Duration

	capabilitiesMu sync.Mutex
	capabilities   *Capabilities
	runMu          sync.Mutex
}

func NewCommandPublisher(argv []string, timeout time.Duration) (*CommandPublisher, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, fmt.Errorf("share helper executable cannot be empty")
	}
	if len(argv) > 64 {
		return nil, fmt.Errorf("share helper command accepts at most 64 argv entries")
	}
	for i, arg := range argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return nil, fmt.Errorf("share helper argv[%d] contains NUL", i)
		}
		if len(arg) > 4096 {
			return nil, fmt.Errorf("share helper argv[%d] must be at most 4096 bytes", i)
		}
	}
	if timeout == 0 {
		timeout = DefaultCommandTimeout
	}
	if timeout < 0 || timeout > MaxCommandTimeout {
		return nil, fmt.Errorf("share helper timeout must be greater than zero and at most %s", MaxCommandTimeout)
	}
	resolved, err := exec.LookPath(argv[0])
	if err != nil && !(errors.Is(err, exec.ErrDot) && resolved != "") {
		return nil, errorWithDiagnostic(ErrorDependencyMissing, "share helper executable was not found", err.Error(), err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, errorWithDiagnostic(ErrorProvider, "could not resolve share helper executable", err.Error(), err)
	}
	command := append([]string(nil), argv...)
	command[0] = resolved
	return &CommandPublisher{argv: command, timeout: timeout}, nil
}

type helperEnvelope struct {
	Protocol  string `json:"protocol"`
	Version   int    `json:"version"`
	RequestID string `json:"request_id"`
}

type helperRequest struct {
	helperEnvelope
	ID          string     `json:"id,omitempty"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Visibility  Visibility `json:"visibility"`
	Entrypoint  string     `json:"entrypoint"`
	Files       []File     `json:"files"`
}

type helperResponse struct {
	Protocol   string     `json:"protocol"`
	Version    int        `json:"version"`
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	SourceURL  string     `json:"source_url,omitempty"`
	Visibility Visibility `json:"visibility"`
	Error      *Error     `json:"error,omitempty"`
}

func (p *CommandPublisher) Capabilities(ctx context.Context) (Capabilities, error) {
	p.capabilitiesMu.Lock()
	defer p.capabilitiesMu.Unlock()
	if p.capabilities != nil {
		return *p.capabilities, nil
	}

	payload := helperEnvelope{Protocol: Protocol, Version: Version, RequestID: NewRequestID()}
	var capabilities Capabilities
	if err := p.run(ctx, OperationCapabilities, capabilitiesTimeout, "", payload, &capabilities); err != nil {
		return Capabilities{}, err
	}
	if err := ValidateHelperCapabilities(capabilities); err != nil {
		return Capabilities{}, errorWithDiagnostic(ErrorProtocol, "share helper returned invalid capabilities", err.Error(), err)
	}
	copy := capabilities
	p.capabilities = &copy
	return capabilities, nil
}

func (p *CommandPublisher) Create(ctx context.Context, req Request) (Result, error) {
	return p.publish(ctx, OperationCreate, "", req)
}

func (p *CommandPublisher) Update(ctx context.Context, id string, req Request) (Result, error) {
	if !validID(id) {
		return Result{}, NewError(ErrorProtocol, "stored share ID is invalid")
	}
	return p.publish(ctx, OperationUpdate, id, req)
}

func (p *CommandPublisher) publish(ctx context.Context, operation Operation, id string, req Request) (Result, error) {
	if err := ValidateRequest(req); err != nil {
		return Result{}, errorWithDiagnostic(ErrorProtocol, "share request is invalid", err.Error(), err)
	}
	capabilities, err := p.Capabilities(ctx)
	if err != nil {
		return Result{}, err
	}
	if !capabilities.Supports(operation) {
		return Result{}, NewError(ErrorProvider, fmt.Sprintf("sharing provider does not support %s", operation))
	}
	if !capabilities.SupportsVisibility(req.Visibility) {
		return Result{}, NewError(ErrorUnsupportedVisibility, fmt.Sprintf("sharing provider does not support %s visibility", req.Visibility))
	}

	dir, err := os.MkdirTemp("", "term-llm-share-")
	if err != nil {
		return Result{}, errorWithDiagnostic(ErrorProvider, "could not prepare share bundle", err.Error(), err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return Result{}, errorWithDiagnostic(ErrorProvider, "could not prepare share bundle", err.Error(), err)
	}
	for _, file := range req.Files {
		path := filepath.Join(dir, file.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Result{}, errorWithDiagnostic(ErrorProvider, "could not prepare share bundle", err.Error(), err)
		}
		if err := os.WriteFile(path, file.Content, 0o600); err != nil {
			return Result{}, errorWithDiagnostic(ErrorProvider, "could not prepare share bundle", err.Error(), err)
		}
	}

	payload := helperRequest{
		helperEnvelope: helperEnvelope{Protocol: Protocol, Version: Version, RequestID: req.RequestID},
		ID:             id,
		Title:          req.Title,
		Description:    req.Description,
		Visibility:     req.Visibility,
		Entrypoint:     req.Entrypoint,
		Files:          req.Files,
	}
	var response helperResponse
	if err := p.run(ctx, operation, p.timeout, dir, payload, &response); err != nil {
		return Result{}, err
	}
	if response.Protocol != Protocol || response.Version != Version {
		return Result{}, NewError(ErrorProtocol, "share helper returned an incompatible protocol response")
	}
	result := Result{
		Provider: capabilities.Provider.ID, ID: response.ID, URL: response.URL,
		SourceURL: response.SourceURL, Visibility: response.Visibility, Ready: true,
	}
	if err := ValidateResult(result); err != nil {
		return Result{}, errorWithDiagnostic(ErrorProtocol, "share helper returned an invalid result", err.Error(), err)
	}
	if !capabilities.SupportsVisibility(result.Visibility) {
		return Result{}, NewError(ErrorProtocol, "share helper returned a visibility it did not advertise")
	}
	return result, nil
}

func (p *CommandPublisher) run(ctx context.Context, operation Operation, timeout time.Duration, dir string, input, output any) error {
	p.runMu.Lock()
	defer p.runMu.Unlock()

	payload, err := json.Marshal(input)
	if err != nil {
		return errorWithDiagnostic(ErrorProtocol, "could not encode share helper request", err.Error(), err)
	}
	payload = append(payload, '\n')
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append(append([]string(nil), p.argv[1:]...), string(operation))
	cmd := exec.CommandContext(timeoutCtx, p.argv[0], args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(payload)
	cmd.WaitDelay = commandWaitDelay
	stdout := procutil.NewLimitedBuffer(commandStdoutLimit)
	stderr := procutil.NewLimitedBuffer(commandStderrLimit)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cleanup, err := procutil.PrepareCommand(cmd)
	if err != nil {
		return errorWithDiagnostic(ErrorProvider, "could not start share helper", err.Error(), err)
	}
	defer cleanup()
	defer func() {
		// A successful helper may have left descendants holding inherited pipes.
		// Run's WaitDelay closes those pipes, and this group cancellation prevents
		// the detached descendants from surviving the completed invocation.
		if cmd.Cancel != nil {
			_ = cmd.Cancel()
		}
	}()

	runErr := cmd.Run()
	if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
		return errorWithDiagnostic(ErrorTimeout, "share helper timed out", stderr.String(), context.DeadlineExceeded)
	}
	if errors.Is(timeoutCtx.Err(), context.Canceled) {
		return errorWithDiagnostic(ErrorProvider, "sharing was canceled", stderr.String(), context.Canceled)
	}
	if stdout.Truncated() {
		return errorWithDiagnostic(ErrorProtocol, "share helper stdout exceeded 1 MiB", stderr.String(), runErr)
	}
	// A helper may successfully exit after writing its complete response while a
	// background descendant still retains inherited pipe descriptors. os/exec
	// reports ErrWaitDelay after closing those pipes; the helper's zero exit code
	// remains authoritative, so parse the bounded stdout response normally.
	if errors.Is(runErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		runErr = nil
	}
	if runErr != nil {
		var response helperResponse
		if json.Unmarshal([]byte(stdout.String()), &response) == nil && response.Error != nil {
			code := response.Error.Code
			if !slicesContainsErrorCode(code) {
				code = ErrorProvider
			}
			message := strings.TrimSpace(response.Error.Message)
			if validateStructuredErrorMessage(message) != nil {
				message = "share helper failed"
			}
			return errorWithDiagnostic(code, message, stderr.String(), runErr)
		}
		if errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, os.ErrNotExist) {
			return errorWithDiagnostic(ErrorDependencyMissing, "share helper executable was not found", stderr.String(), runErr)
		}
		return errorWithDiagnostic(ErrorProvider, "share helper failed", stderr.String(), runErr)
	}
	if err := json.Unmarshal([]byte(stdout.String()), output); err != nil {
		return errorWithDiagnostic(ErrorProtocol, "share helper returned malformed JSON", stderr.String(), err)
	}
	return nil
}

func slicesContainsErrorCode(code ErrorCode) bool {
	for _, stable := range stableErrorCodes {
		if code == stable {
			return true
		}
	}
	return false
}
