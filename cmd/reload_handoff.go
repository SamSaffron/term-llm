package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

var chatReloadExecutable string

func commandRestartService() string {
	cwd, _ := os.Getwd()
	digest := sha256.Sum256([]byte(strings.Join(os.Args, "\x00") + "\x00" + cwd))
	return "command:" + hex.EncodeToString(digest[:])
}

func saveChatReload(store session.Store, sid string, state chat.ProcessReloadState) (string, string, error) {
	handoffs, ok := session.AsCommandHandoffStore(store)
	if !ok {
		return "", "", fmt.Errorf("reload requires durable session storage")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", "", err
	}
	id, service := uuid.NewString(), commandRestartService()
	err = handoffs.SaveCommandHandoff(context.Background(), session.CommandHandoff{ID: id, Service: service, SourceInstance: process.Instance(), SessionID: sid, Payload: raw})
	return id, service, err
}

func consumeChatReload(store session.Store) (*session.CommandHandoff, *chat.ProcessReloadState, error) {
	if execRestartHint == "" {
		return nil, nil, nil
	}
	handoffs, ok := session.AsCommandHandoffStore(store)
	if !ok {
		return nil, nil, fmt.Errorf("reload handoff requires durable session storage")
	}
	h, err := handoffs.ConsumeCommandHandoff(context.Background(), execRestartHint, commandRestartService(), process.Instance())
	if err != nil {
		return nil, nil, err
	}
	var state chat.ProcessReloadState
	if err = json.Unmarshal(h.Payload, &state); err != nil {
		return nil, nil, err
	}
	execRestartHint = ""
	return &h, &state, nil
}

// Auto-generated MCP credentials belong only to the replacement executable, not
// to tool children. Match the existing Hub credential hand-back convention.
var mcpReloadToken = func() string {
	token := os.Getenv("TERM_LLM_MCP_RELOAD_TOKEN")
	_ = os.Unsetenv("TERM_LLM_MCP_RELOAD_TOKEN")
	return token
}()
