package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ClientToolAnswerOutcome describes an idempotent result submission. Conflicts
// and unknown calls must never be delivered to another response or replayed.
type ClientToolAnswerOutcome uint8

const (
	ClientToolAnswerAccepted ClientToolAnswerOutcome = iota
	ClientToolAnswerDuplicate
	ClientToolAnswerConflict
	ClientToolAnswerUnknown
)

type clientToolAnswer struct {
	name, arguments, output string
	done                    chan struct{}
	answered                bool
	lease                   *ClientToolLease
	dispatch                *clientToolDispatch
}

type clientToolReceipt struct {
	digest [sha256.Size]byte
	lease  *ClientToolLease
}

// ClientToolMailbox is a bounded, response-owned inline request/result inbox.
// It grants no script capability itself. The host supplies event publication,
// authentication/routing, and a separate revocable foreground lease if supported.
type ClientToolMailbox struct {
	mu       sync.Mutex
	names    map[string]bool
	pending  map[string]*clientToolAnswer
	answered map[string]clientToolReceipt
	seen     map[string]bool
	limit    int
	timeout  time.Duration
	publish  func(context.Context, ToolCall) error
}

// NewClientToolMailbox registers waiters before invoking publish. The host
// must call BeginClientToolDispatch after terminal checks, immediately before
// actual publication; admission here would misclassify publication failures.
func NewClientToolMailbox(names map[string]bool, limit int, timeout time.Duration, publish func(context.Context, ToolCall) error) *ClientToolMailbox {
	owned := make(map[string]bool, len(names))
	for name, allowed := range names {
		owned[name] = allowed
	}
	return &ClientToolMailbox{names: owned, pending: make(map[string]*clientToolAnswer), answered: make(map[string]clientToolReceipt), seen: make(map[string]bool), limit: limit, timeout: timeout, publish: publish}
}

func (r *ClientToolMailbox) OwnsClientTool(name string) bool { return r.names[name] }
func (r *ClientToolMailbox) RunClientTool(ctx context.Context, call ToolCall) (ToolOutput, error) {
	if !r.names[call.Name] {
		return ToolOutput{}, fmt.Errorf("client tool %q was not offered", call.Name)
	}
	if err := ctx.Err(); err != nil {
		return ToolOutput{}, err
	}
	if strings.TrimSpace(string(call.Arguments)) == "" {
		call.Arguments = json.RawMessage(`{}`)
	}
	dispatch, _ := ctx.Value(clientToolDispatchKey{}).(*clientToolDispatch)
	answer := &clientToolAnswer{dispatch: dispatch, lease: ClientToolCallLease(ctx), name: call.Name, arguments: string(call.Arguments), done: make(chan struct{})}
	r.mu.Lock()
	if len(r.seen) >= r.limit {
		r.mu.Unlock()
		return ToolOutput{}, fmt.Errorf("stopped after %d device tool calls", r.limit)
	}
	if r.seen[call.ID] {
		r.mu.Unlock()
		return ToolOutput{}, fmt.Errorf("duplicate client tool call %q", call.ID)
	}
	r.seen[call.ID] = true
	r.pending[call.ID] = answer
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, call.ID); r.mu.Unlock() }()
	if err := r.publish(ctx, call); err != nil {
		return ToolOutput{}, fmt.Errorf("request client tool: %w", err)
	}
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case <-answer.done:
		return TextOutput(answer.output), nil
	case <-timer.C:
		return ToolOutput{}, errors.New("the device did not run its tool in time")
	case <-ctx.Done():
		return ToolOutput{}, ctx.Err()
	}
}

func (r *ClientToolMailbox) Pending() []map[string]string { return r.pendingSnapshot(false) }

// RecoveryPending excludes foreground calls even while their lease is active:
// generic snapshots are not proof of originating transport ownership.
func (r *ClientToolMailbox) RecoveryPending() []map[string]string { return r.pendingSnapshot(true) }

func (r *ClientToolMailbox) pendingSnapshot(recovery bool) []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := make([]map[string]string, 0, len(r.pending))
	for callID, answer := range r.pending {
		if !answer.answered && (!recovery || answer.lease == nil) {
			answer.lease.AcceptResult(func() {
				if answer.dispatch == nil || answer.dispatch.started {
					calls = append(calls, map[string]string{"call_id": callID, "name": answer.name, "arguments": answer.arguments})
				}
			})
		}
	}
	return calls
}

func (r *ClientToolMailbox) Answer(callID, output string) ClientToolAnswerOutcome {
	digest := sha256.Sum256([]byte(output))
	r.mu.Lock()
	defer r.mu.Unlock()
	if previous, answered := r.answered[callID]; answered {
		outcome := ClientToolAnswerConflict
		previous.lease.AcceptResult(func() {
			if previous.digest == digest {
				outcome = ClientToolAnswerDuplicate
			}
		})
		return outcome
	}
	answer := r.pending[callID]
	if answer == nil {
		return ClientToolAnswerUnknown
	}
	outcome := ClientToolAnswerConflict
	answer.lease.AcceptResult(func() {
		if answer.dispatch != nil && !answer.dispatch.started {
			return
		}
		answer.output, answer.answered = output, true
		r.answered[callID] = clientToolReceipt{digest: digest, lease: answer.lease}
		close(answer.done)
		outcome = ClientToolAnswerAccepted
	})
	return outcome
}

// ParseClientFunctionTool decodes flat Responses or wrapped function offers,
// preserving the complete input/output schema, strictness and caller metadata.
func ParseClientFunctionTool(generic map[string]json.RawMessage) (ToolSpec, bool) {
	spec := ToolSpec{
		Name:        strings.TrimSpace(clientToolJSONString(generic["name"])),
		Description: strings.TrimSpace(clientToolJSONString(generic["description"])),
	}
	if rawParams := generic["parameters"]; len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &spec.Schema)
	}
	if rawStrict := generic["strict"]; len(rawStrict) > 0 {
		var strict bool
		if err := json.Unmarshal(rawStrict, &strict); err == nil {
			spec.Strict = strict
		}
	}
	if rawCallers := generic["allowed_callers"]; len(rawCallers) > 0 {
		_ = json.Unmarshal(rawCallers, &spec.AllowedCallers)
	}
	if rawOutput := generic["output_schema"]; len(rawOutput) > 0 {
		_ = json.Unmarshal(rawOutput, &spec.OutputSchema)
	}

	if rawFunc := generic["function"]; len(rawFunc) > 0 {
		var fn struct {
			Name           string                 `json:"name"`
			Description    string                 `json:"description"`
			Parameters     map[string]interface{} `json:"parameters"`
			Strict         *bool                  `json:"strict"`
			AllowedCallers []string               `json:"allowed_callers"`
			OutputSchema   map[string]interface{} `json:"output_schema"`
		}
		if err := json.Unmarshal(rawFunc, &fn); err == nil {
			if spec.Name == "" {
				spec.Name = strings.TrimSpace(fn.Name)
			}
			if spec.Description == "" {
				spec.Description = strings.TrimSpace(fn.Description)
			}
			if spec.Schema == nil {
				spec.Schema = fn.Parameters
			}
			if fn.Strict != nil {
				spec.Strict = *fn.Strict
			}
			if spec.AllowedCallers == nil {
				spec.AllowedCallers = append([]string(nil), fn.AllowedCallers...)
			}
			if spec.OutputSchema == nil {
				spec.OutputSchema = fn.OutputSchema
			}
		}
	}

	if spec.Name == "" {
		return ToolSpec{}, false
	}
	if spec.Schema == nil {
		spec.Schema = map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	return spec, true
}

func clientToolJSONString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// ParseClientFunctionTools snapshots the full schemas in validated host offers.
func ParseClientFunctionTools(tools []json.RawMessage) []ToolSpec {
	specs := make([]ToolSpec, 0, len(tools))
	for _, raw := range tools {
		var generic map[string]json.RawMessage
		if json.Unmarshal(raw, &generic) != nil {
			return nil
		}
		spec, ok := ParseClientFunctionTool(generic)
		if !ok {
			return nil
		}
		specs = append(specs, spec)
	}
	return specs
}

// ClientToolSpecsWithPrefix selects one host-supported client offer family.
func ClientToolSpecsWithPrefix(tools []ToolSpec, prefix string) (map[string]bool, []ToolSpec) {
	names := make(map[string]bool)
	var specs []ToolSpec
	for _, spec := range tools {
		if strings.HasPrefix(spec.Name, prefix) {
			names[spec.Name] = true
			specs = append(specs, spec)
		}
	}
	return names, specs
}
