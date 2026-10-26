package linkeddevices

import (
	"context"
	"fmt"
	"net/http"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

// ServeHost owns the linked-device runtime's integration with a serve process.
// Runtime is embedded so focused integration tests can supply an isolated broker.
type ServeHost struct {
	*Runtime
}

// NewServeHost resolves the session-store identity and constructs the standard
// linked-device runtime for a serve process.
func NewServeHost(enabled bool, ctx context.Context, store session.Store, agentName, basePath string, authenticated bool, title, approvalMode string, cfg *config.Config) (*ServeHost, error) {
	if !enabled {
		return nil, nil
	}
	storeID, storeErr := session.SessionInputStoreIdentity(store)
	if attentionStore, ok := session.AsAttentionStore(store); ok {
		storeID, storeErr = attentionStore.StoreInstanceID(ctx)
	}
	if storeErr != nil {
		storeID = ""
	}
	runtime, err := NewServer(agentName, storeID, basePath, authenticated, title, fmt.Sprintf("can see every conversation this server shows; messages drive tools under approval mode %s", approvalMode), ServerOptionsFromConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("initialize linked devices: %w", err)
	}
	return &ServeHost{Runtime: runtime}, nil
}

// SetPeerHandler connects peer traffic to the complete serve handler after all
// routes have been registered.
func (h *ServeHost) SetPeerHandler(handler http.Handler) {
	if h == nil || h.Runtime == nil {
		return
	}
	h.SetHandler(handler)
}

// StartAndLog starts a configured runtime while preserving serve startup's
// optional-feature behavior.
func (h *ServeHost) StartAndLog(logf func(string, ...any)) {
	if h == nil || h.Runtime == nil {
		return
	}
	if err := h.Start(); err != nil && logf != nil {
		logf("linked devices startup: %v", err)
	}
}

// Close closes a configured runtime. A nil host is already closed.
func (h *ServeHost) Close() error {
	if h == nil || h.Runtime == nil {
		return nil
	}
	return h.Runtime.Close()
}
