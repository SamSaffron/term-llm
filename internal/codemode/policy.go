package codemode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// machinePolicy is the exact resource envelope for one fresh QuickJS instance.
// Legacy Run and the async session path use different policies; a cell never
// shares a guest heap or widens its policy after start.
type machinePolicy struct {
	code, text, items, images, imageBytes int
	stateTotal, stateValue                int
	heap, linear, stack                   int
	cpu, wall, maxWall                    time.Duration
	rejectImageState                      bool
}

func legacyMachinePolicy() machinePolicy {
	return machinePolicy{
		code:       Limits.Code,
		text:       Limits.Text,
		items:      Limits.Items,
		images:     Limits.Images,
		imageBytes: Limits.ImageBytes,
		stateTotal: Limits.State,
		heap:       Limits.Heap,
		linear:     Limits.Linear,
		stack:      Limits.Stack,
		cpu:        Limits.CPU,
		wall:       Limits.Wall,
		maxWall:    Limits.MaxWall,
	}
}

func sessionMachinePolicy() machinePolicy {
	return machinePolicy{
		code:             SessionLimits.Code,
		text:             SessionLimits.Text,
		items:            SessionLimits.Items,
		images:           SessionLimits.Images,
		imageBytes:       SessionLimits.ImageBytes,
		stateTotal:       SessionLimits.StateTotal,
		stateValue:       SessionLimits.StateValue,
		heap:             SessionLimits.Heap,
		linear:           SessionLimits.Linear,
		stack:            SessionLimits.Stack,
		cpu:              SessionLimits.CPU,
		rejectImageState: true,
	}
}

// validateStateObject rejects a state that cannot be committed before any
// caller can observe it as successful. Key names are metadata; values are
// never included in the returned error.
func validateStateObject(raw json.RawMessage, total, perValue int, rejectImage bool) error {
	if len(raw) > total {
		return errors.New("state_total_limit")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return errors.New("invalid_state")
	}
	for key, value := range values {
		if perValue > 0 && len(value) > perValue {
			return fmt.Errorf("state_value_limit: %s", key)
		}
		if !json.Valid(value) {
			return errors.New("invalid_state")
		}
	}
	if rejectImage && strings.Contains(string(raw), "cell-image:") {
		return errors.New("state_image_handle")
	}
	return nil
}
