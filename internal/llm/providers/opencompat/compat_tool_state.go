package opencompat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// compatToolState accumulates streamed OpenAI-compatible tool-call deltas
// and produces completed ToolCall values once the stream finishes.
type compatToolState struct {
	byIndex            map[int]*toolCallState
	order              []int
	implicitByPosition map[int]int
	nextImplicitIndex  int
	hasImplicitIndex   bool
}

type toolCallState struct {
	id   string
	name string
	args strings.Builder
}

func newCompatToolState() *compatToolState {
	return &compatToolState{
		byIndex:            make(map[int]*toolCallState),
		implicitByPosition: make(map[int]int),
		nextImplicitIndex:  -1,
	}
}

func (s *compatToolState) Add(calls []oaiToolCall) {
	for pos, call := range calls {
		idx := 0
		if call.Index != nil {
			idx = *call.Index
			if mappedIdx, ok := s.implicitByPosition[pos]; ok && mappedIdx < 0 {
				idx = mappedIdx
			}
		} else {
			s.hasImplicitIndex = true
			var ok bool
			idx, ok = s.implicitByPosition[pos]
			if !ok {
				idx = s.nextImplicitIndex
				s.nextImplicitIndex--
				s.implicitByPosition[pos] = idx
			}
		}
		state, ok := s.byIndex[idx]
		if !ok {
			state = &toolCallState{}
			s.byIndex[idx] = state
			s.order = append(s.order, idx)
		}
		if call.ID != "" {
			state.id = call.ID
		}
		if call.Function.Name != "" {
			state.name = call.Function.Name
		}
		if call.Function.Arguments != "" {
			state.args.WriteString(call.Function.Arguments)
		}
	}
}

func (s *compatToolState) Validate() error {
	if len(s.order) == 0 {
		return nil
	}
	for _, idx := range s.order {
		state := s.byIndex[idx]
		if state == nil {
			continue
		}
		if strings.TrimSpace(state.id) == "" {
			return fmt.Errorf("OpenAI-compatible stream missing tool call id for tool call %d", idx)
		}
		if strings.TrimSpace(state.name) == "" {
			return fmt.Errorf("OpenAI-compatible stream missing tool name for tool call %d", idx)
		}
		args := strings.TrimSpace(state.args.String())
		if args != "" && !json.Valid([]byte(args)) {
			return fmt.Errorf("OpenAI-compatible stream invalid arguments for tool call %d", idx)
		}
	}
	return nil
}

func (s *compatToolState) Calls() []ToolCall {
	if len(s.order) == 0 {
		return nil
	}
	order := append([]int(nil), s.order...)
	if !s.hasImplicitIndex {
		sort.Ints(order)
	}
	calls := make([]ToolCall, 0, len(order))
	for _, idx := range order {
		state := s.byIndex[idx]
		if state == nil {
			continue
		}
		argsString := strings.TrimSpace(state.args.String())
		args := json.RawMessage("{}")
		if argsString != "" {
			args = json.RawMessage(argsString)
		}
		calls = append(calls, ToolCall{
			ID:        state.id,
			Name:      state.name,
			Arguments: args,
		})
	}
	return calls
}
