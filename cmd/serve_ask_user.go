package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

var (
	errServeAskUserNotPending = errors.New("no pending ask_user request")
	errServeAskUserAnswered   = errors.New("ask_user request already answered")
	errServeAskUserCancelled  = errors.New("cancelled by user")
)

type serveAskUserPrompt struct {
	CallID    string                  `json:"call_id"`
	Questions []tools.AskUserQuestion `json:"questions"`
	CreatedAt int64                   `json:"created_at"`
}

type serveAskUserSubmission struct {
	Answers []tools.AskUserAnswer
	Err     error
}

type servePendingAskUser struct {
	CallID    string
	Questions []tools.AskUserQuestion
	CreatedAt time.Time
	responseC chan serveAskUserSubmission
	responded bool
}

func cloneAskUserQuestions(questions []tools.AskUserQuestion) []tools.AskUserQuestion {
	if len(questions) == 0 {
		return nil
	}
	cloned := make([]tools.AskUserQuestion, len(questions))
	for i, q := range questions {
		cloned[i] = q
		if len(q.Options) > 0 {
			cloned[i].Options = append([]tools.AskUserOption(nil), q.Options...)
		}
	}
	return cloned
}

func (p *servePendingAskUser) snapshot() serveAskUserPrompt {
	return serveAskUserPrompt{
		CallID:    p.CallID,
		Questions: cloneAskUserQuestions(p.Questions),
		CreatedAt: p.CreatedAt.UnixMilli(),
	}
}

func (rt *serveRuntime) prepareAskUser(callID string, questions []tools.AskUserQuestion) *servePendingAskUser {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	if rt.pendingAskUsers == nil {
		rt.pendingAskUsers = make(map[string]*servePendingAskUser)
	}
	pending := rt.pendingAskUsers[callID]
	if pending == nil {
		pending = &servePendingAskUser{
			CallID:    callID,
			CreatedAt: time.Now(),
			responseC: make(chan serveAskUserSubmission, 1),
		}
		rt.pendingAskUsers[callID] = pending
	}
	pending.Questions = cloneAskUserQuestions(questions)
	return pending
}

func (rt *serveRuntime) prepareAskUserFromToolArgs(callID string, raw json.RawMessage) (serveAskUserPrompt, error) {
	if callID == "" {
		return serveAskUserPrompt{}, fmt.Errorf("ask_user missing tool call id")
	}
	var args tools.AskUserArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return serveAskUserPrompt{}, fmt.Errorf("parse ask_user args: %w", err)
	}
	if len(args.Questions) == 0 {
		return serveAskUserPrompt{}, fmt.Errorf("ask_user requires at least one question")
	}
	return rt.prepareAskUser(callID, args.Questions).snapshot(), nil
}

func (rt *serveRuntime) awaitAskUser(ctx context.Context, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
	callID := llm.CallIDFromContext(ctx)
	if callID == "" {
		return nil, fmt.Errorf("ask_user missing tool call id")
	}
	pending := rt.prepareAskUser(callID, questions)
	defer rt.removePendingAskUser(callID, pending)

	select {
	case submission := <-pending.responseC:
		return submission.Answers, submission.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (rt *serveRuntime) removePendingAskUser(callID string, pending *servePendingAskUser) {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	if current := rt.pendingAskUsers[callID]; current == pending {
		delete(rt.pendingAskUsers, callID)
	}
}

func (rt *serveRuntime) clearPendingAskUser(callID string) {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	delete(rt.pendingAskUsers, callID)
}

func (rt *serveRuntime) submitAskUser(callID string, answers []tools.AskUserAnswer, cancelled bool) ([]tools.AskUserAnswer, error) {
	rt.askUserMu.Lock()
	pending := rt.pendingAskUsers[callID]
	if pending == nil {
		rt.askUserMu.Unlock()
		return nil, errServeAskUserNotPending
	}
	questions := cloneAskUserQuestions(pending.Questions)
	rt.askUserMu.Unlock()

	submission := serveAskUserSubmission{}
	var normalized []tools.AskUserAnswer
	if cancelled {
		submission.Err = errServeAskUserCancelled
	} else {
		var err error
		normalized, err = tools.NormalizeAskUserAnswers(questions, answers)
		if err != nil {
			return nil, err
		}
		submission.Answers = normalized
	}

	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	pending = rt.pendingAskUsers[callID]
	if pending == nil {
		return nil, errServeAskUserNotPending
	}
	if pending.responded {
		return nil, errServeAskUserAnswered
	}
	pending.responded = true
	select {
	case pending.responseC <- submission:
		return normalized, nil
	default:
		return nil, errServeAskUserAnswered
	}
}

func (rt *serveRuntime) pendingAskUserPrompts() []serveAskUserPrompt {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	if len(rt.pendingAskUsers) == 0 {
		return nil
	}
	prompts := make([]serveAskUserPrompt, 0, len(rt.pendingAskUsers))
	for _, pending := range rt.pendingAskUsers {
		if pending == nil {
			continue
		}
		prompts = append(prompts, pending.snapshot())
	}
	slices.SortFunc(prompts, func(a, b serveAskUserPrompt) int {
		switch {
		case a.CreatedAt < b.CreatedAt:
			return -1
		case a.CreatedAt > b.CreatedAt:
			return 1
		default:
			return 0
		}
	})
	return prompts
}

func (rt *serveRuntime) clearPendingAskUsers() {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	rt.pendingAskUsers = nil
}
