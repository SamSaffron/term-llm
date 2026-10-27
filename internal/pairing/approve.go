package pairing

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// PairingPrompt is what the host operator must review before approving.
type PairingPrompt struct {
	DeviceName        string
	DeviceFingerprint string
	HostFingerprint   string
	SAS               string
	ExpiresAt         time.Time
}

// Approver decides whether a verified pairing request becomes a grant.
// There is deliberately no auto-accepting implementation.
type Approver interface {
	ApprovePairing(ctx context.Context, p PairingPrompt) (bool, error)
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(ctx context.Context, p PairingPrompt) (bool, error)

// ApprovePairing implements Approver.
func (f ApproverFunc) ApprovePairing(ctx context.Context, p PairingPrompt) (bool, error) {
	return f(ctx, p)
}

// TerminalApprover prints the request to Out and approves only when the
// operator types "yes" on In. Anything else, EOF, or ctx cancellation denies.
// On cancellation the pending line read is abandoned.
type TerminalApprover struct {
	In  io.Reader
	Out io.Writer
}

// ApprovePairing implements Approver.
func (t TerminalApprover) ApprovePairing(ctx context.Context, p PairingPrompt) (bool, error) {
	if t.In == nil || t.Out == nil {
		return false, fmt.Errorf("terminal approver needs input and output")
	}
	_, err := fmt.Fprintf(t.Out,
		"\nPairing request from device %q\n  device key: %s\n  host key:   %s\n  code:       %s\n  expires:    %s\nApprove this device? Type \"yes\" to approve: ",
		p.DeviceName, p.DeviceFingerprint, p.HostFingerprint, p.SAS, p.ExpiresAt.Local().Format(time.TimeOnly))
	if err != nil {
		return false, err
	}
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(t.In).ReadString('\n')
		ch <- result{line, err}
	}()
	var deadline <-chan time.Time
	if !p.ExpiresAt.IsZero() {
		timer := time.NewTimer(time.Until(p.ExpiresAt))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-deadline:
		_, _ = fmt.Fprintln(t.Out, "\npairing request expired")
		return false, ErrExpired
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return false, nil
		}
		return strings.EqualFold(strings.TrimSpace(r.line), "yes"), nil
	}
}
