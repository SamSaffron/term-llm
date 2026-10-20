package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/session"
)

// Clear the complete cached review inbox, not just the dashboard's limited page.
// Captured store identities and sequences leave later completions unseen.
func (s *hubServer) handleClearHubAttention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !hubBrowserRequestAllowed(r, true) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_request_error", "same-origin JSON request required")
		return
	}
	if s.attentionStore == nil {
		writeJSON(w, http.StatusOK, map[string]int64{"cleared": 0, "failed": 0})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	activities, syncs, err := s.attentionStore.List(ctx)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to read Hub attention projection")
		return
	}
	routes := s.hubAttentionRoutes(syncs)
	// Acknowledge the highest sequence already captured across registrations,
	// not a later marker that may have appeared on the node during clearing.
	sequences := make(map[[2]string]int64)
	for _, activity := range activities {
		if activity.Kind == "terminal_unseen" {
			key := [2]string{activity.StoreInstanceID, activity.SessionID}
			sequences[key] = max(sequences[key], activity.AttentionSeq)
		}
	}
	var cleared, failed atomic.Int64
	var unreachable sync.Map
	work := make(chan hub.SessionActivity)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for activity := range work {
				node, ok := routes[activity.StoreInstanceID]
				_, down := unreachable.Load(node.ID)
				if !ok || down || ctx.Err() != nil {
					failed.Add(1)
					continue
				}
				if err := s.clearHubAttentionItem(ctx, node, activity); err != nil {
					var networkError *url.Error
					if errors.As(err, &networkError) || errors.Is(err, context.DeadlineExceeded) {
						unreachable.Store(node.ID, true)
					}
					failed.Add(1)
				} else {
					cleared.Add(1)
				}
			}
		})
	}
	for _, activity := range deduplicateHubActivities(activities, syncs) {
		if activity.Kind == "terminal_unseen" {
			activity.AttentionSeq = sequences[[2]string{activity.StoreInstanceID, activity.SessionID}]
			work <- activity
		}
	}
	close(work)
	workers.Wait()
	writeJSON(w, http.StatusOK, map[string]int64{"cleared": cleared.Load(), "failed": failed.Load()})
}

func (s *hubServer) clearHubAttentionItem(ctx context.Context, node hub.Node, activity hub.SessionActivity) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	path := "/v1/sessions/" + url.PathEscape(activity.SessionID) + "/attention/seen"
	request := markAttentionSeenRequest{StoreInstanceID: activity.StoreInstanceID, ThroughSeq: activity.AttentionSeq}
	var state session.AttentionState
	if err := s.doNodeJSON(ctx, node, http.MethodPost, path, request, &state, http.Header{"X-Term-LLM-Attention-Source": {"hub"}}); err != nil {
		return err
	}
	if state.StoreInstanceID != activity.StoreInstanceID || state.SessionID != activity.SessionID || state.SeenThroughSeq < activity.AttentionSeq {
		return errors.New("node did not acknowledge the requested attention marker")
	}
	// Invalidate in-flight snapshots and remove cached rows under the same short
	// lock used to install collections. Network requests never hold this lock.
	s.attentionMu.Lock()
	defer s.attentionMu.Unlock()
	s.attentionGeneration++
	return s.attentionStore.RemoveSeen(ctx, activity.StoreInstanceID, activity.SessionID, activity.AttentionSeq)
}

func (s *hubServer) hubAttentionRoutes(syncs []hub.AttentionSync) map[string]hub.Node {
	nodes := make(map[string]hub.Node)
	if s.registry != nil {
		registered, _ := s.registry.Nodes()
		for _, node := range registered {
			nodes[node.ID] = node
		}
	}
	// An alias can acknowledge a store even when its cached session list is
	// empty. Choose a healthy registered route from store sync identities.
	routeCandidates := make([]hub.SessionActivity, 0, len(syncs))
	for _, state := range syncs {
		if _, ok := nodes[state.NodeID]; ok {
			routeCandidates = append(routeCandidates, hub.SessionActivity{NodeID: state.NodeID, StoreInstanceID: state.StoreInstanceID})
		}
	}
	routes := make(map[string]hub.Node)
	for _, activity := range deduplicateHubActivities(routeCandidates, syncs) {
		routes[activity.StoreInstanceID] = nodes[activity.NodeID]
	}
	return routes
}

// Acknowledge the exact inbox item captured by the caller, never a newer node
// marker or an uncached session supplied directly by the browser.
func (s *hubServer) handleSeenHubAttention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !hubBrowserRequestAllowed(r, true) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_request_error", "same-origin JSON request required")
		return
	}
	var request struct {
		StoreInstanceID string `json:"store_instance_id"`
		SessionID       string `json:"session_id"`
		AttentionSeq    int64  `json:"attention_seq"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.StoreInstanceID) == "" || strings.TrimSpace(request.SessionID) == "" || request.AttentionSeq <= 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "store_instance_id, session_id and a positive attention_seq are required")
		return
	}
	if s.attentionStore == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "attention item not cached")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	activities, syncs, err := s.attentionStore.List(ctx)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to read Hub attention projection")
		return
	}
	for _, activity := range activities {
		if activity.Kind != "terminal_unseen" || activity.StoreInstanceID != request.StoreInstanceID || activity.SessionID != request.SessionID || activity.AttentionSeq != request.AttentionSeq {
			continue
		}
		node, ok := s.hubAttentionRoutes(syncs)[activity.StoreInstanceID]
		if !ok || s.clearHubAttentionItem(ctx, node, activity) != nil {
			writeOpenAIError(w, http.StatusBadGateway, "server_error", "failed to acknowledge node attention")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"cleared": 1})
		return
	}
	writeOpenAIError(w, http.StatusNotFound, "not_found_error", "attention item not cached")
}
