package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

const (
	hubAttentionMaxBytes   = 2 << 20
	hubAttentionStaleAfter = 3 * time.Minute
)

var errHubAttentionSnapshotChanged = errors.New("attention snapshot changed during pagination")

type hubAttentionDiagnostics struct {
	CollectorSuccesses atomic.Uint64
	CollectorFailures  atomic.Uint64
	CollectorLatencyMS atomic.Uint64
	CollectorSamples   atomic.Uint64
	SnapshotRetries    atomic.Uint64
}

func (d *hubAttentionDiagnostics) snapshot(staleNodes int) map[string]uint64 {
	if d == nil {
		return map[string]uint64{"stale_nodes": uint64(max(staleNodes, 0))}
	}
	return map[string]uint64{
		"collector_successes":        d.CollectorSuccesses.Load(),
		"collector_failures":         d.CollectorFailures.Load(),
		"collector_latency_total_ms": d.CollectorLatencyMS.Load(),
		"collector_samples":          d.CollectorSamples.Load(),
		"snapshot_retries":           d.SnapshotRetries.Load(),
		"stale_nodes":                uint64(max(staleNodes, 0)),
	}
}

type hubAttentionPage struct {
	ProtocolVersion int                    `json:"protocol_version"`
	StoreInstanceID string                 `json:"store_instance_id"`
	SnapshotVersion int64                  `json:"snapshot_version"`
	Items           []hubAttentionPageItem `json:"items"`
	NextCursor      string                 `json:"next_cursor"`
	HasMore         bool                   `json:"has_more"`
}

type hubAttentionPageItem struct {
	SessionID      string    `json:"session_id"`
	ResponseID     string    `json:"response_id"`
	Kind           string    `json:"kind"`
	LifecycleState string    `json:"lifecycle_state"`
	AttentionSeq   int64     `json:"attention_seq"`
	FinalRev       int64     `json:"final_rev"`
	ShortTitle     string    `json:"short_title"`
	LongTitle      string    `json:"long_title"`
	ProjectID      string    `json:"project_id"`
	Outcome        string    `json:"outcome"`
	StartedAt      time.Time `json:"started_at"`
	TerminalAt     time.Time `json:"terminal_at"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func (s *hubServer) startAttentionCollector() {
	if s.attentionStore == nil || s.registry == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.attentionCancel = cancel
	s.attentionWG.Add(1)
	go func() {
		defer s.attentionWG.Done()
		failureStreak := 0
		for {
			failures := s.collectAttention(ctx)
			if failures > 0 {
				failureStreak++
			} else {
				failureStreak = 0
			}
			active, _ := s.attentionStore.HasActivity(ctx)
			timer := time.NewTimer(hubAttentionCollectionDelay(active, failureStreak))
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
		}
	}()
}

func hubAttentionCollectionDelay(active bool, failureStreak int) time.Duration {
	base := 45 * time.Second
	if active {
		base = 10 * time.Second
	}
	if failureStreak > 0 {
		maxShift := 2
		maxDelay := 2 * time.Minute
		if active {
			maxShift = 1
			maxDelay = 30 * time.Second
		}
		shift := min(failureStreak, maxShift)
		base *= time.Duration(1 << shift)
		if base > maxDelay {
			base = maxDelay
		}
	}
	// Add bounded jitter so multiple Hubs/nodes do not synchronize their polls.
	jitter := base / 10
	span := int64(2*jitter + 1)
	offset := time.Duration(rand.Int64N(span)) - jitter
	return base + offset
}

func hubAttentionErrorSummary(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "collection timed out"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "node unreachable"
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, errHubAttentionSnapshotChanged):
		return "snapshot changed during collection"
	case strings.Contains(message, "decode"), strings.Contains(message, "invalid identity"), strings.Contains(message, "pagination cursor"):
		return "node returned malformed attention data"
	case strings.Contains(message, "http"):
		return "node attention endpoint returned an error"
	default:
		return "attention collection failed"
	}
}

func (s *hubServer) collectAttention(ctx context.Context) int {
	nodes, _ := s.registry.Nodes()
	current := make(map[string]struct{}, len(nodes))
	semaphore := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var failureMu sync.Mutex
	failures := 0
	for _, node := range nodes {
		current[node.ID] = struct{}{}
		node := node
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			nodeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			started := time.Now()
			err := s.collectNodeAttention(nodeCtx, node)
			s.attentionDiagnostics.CollectorSamples.Add(1)
			s.attentionDiagnostics.CollectorLatencyMS.Add(uint64(max(time.Since(started).Milliseconds(), 0)))
			if err == nil {
				s.attentionDiagnostics.CollectorSuccesses.Add(1)
			} else if !errors.Is(err, context.Canceled) {
				s.attentionDiagnostics.CollectorFailures.Add(1)
				failureMu.Lock()
				failures++
				failureMu.Unlock()
				summary := hubAttentionErrorSummary(err)
				_ = s.attentionStore.MarkError(context.Background(), node.ID, summary)
				log.Printf("[hub] attention sync failed for %.12s: %s", node.ID, summary)
			}
		}()
	}
	wg.Wait()
	_, syncs, err := s.attentionStore.List(ctx)
	if err == nil {
		for _, state := range syncs {
			if _, ok := current[state.NodeID]; !ok {
				_ = s.attentionStore.RemoveNode(ctx, state.NodeID)
			}
		}
	}
	return failures
}

func (s *hubServer) collectNodeAttention(ctx context.Context, node hub.Node) error {
	const maxAttempts = 2
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = s.collectNodeAttentionOnce(ctx, node)
		if !errors.Is(err, errHubAttentionSnapshotChanged) {
			return err
		}
		if attempt+1 < maxAttempts {
			s.attentionDiagnostics.SnapshotRetries.Add(1)
		}
	}
	return err
}

func (s *hubServer) collectNodeAttentionOnce(ctx context.Context, node hub.Node) error {
	previous, previousErr := s.attentionStore.GetSync(ctx, node.ID)
	etag := ""
	if previousErr == nil {
		etag = previous.ETag
	}
	activities := make([]hub.SessionActivity, 0)
	storeID := ""
	version := int64(0)
	responseETag := ""
	for _, kind := range []string{"unseen", "running"} {
		cursor := ""
		for {
			page, pageETag, notModified, status, err := s.fetchNodeAttentionPage(ctx, node, kind, cursor, version, etag)
			if err != nil {
				return err
			}
			if notModified {
				return s.attentionStore.MarkSuccess(ctx, node.ID)
			}
			if status == http.StatusNotFound || status == http.StatusNotImplemented {
				lost := previousErr == nil && previous.Capability == hub.AttentionSupported
				return s.attentionStore.MarkUnavailable(ctx, node.ID, lost)
			}
			if status == http.StatusConflict && version > 0 {
				return errHubAttentionSnapshotChanged
			}
			if status != http.StatusOK {
				return fmt.Errorf("attention endpoint returned HTTP %d", status)
			}
			if page.ProtocolVersion != 1 || page.StoreInstanceID == "" {
				return errors.New("attention endpoint returned invalid identity")
			}
			if storeID == "" {
				storeID, version, responseETag = page.StoreInstanceID, page.SnapshotVersion, pageETag
			} else if storeID != page.StoreInstanceID || version != page.SnapshotVersion {
				return errHubAttentionSnapshotChanged
			}
			// Conditional validation is only for the first request of a node sync.
			// Once any page body is accepted, every remaining kind/page must be read.
			etag = ""
			for _, item := range page.Items {
				activityKind := "running"
				if kind == "unseen" {
					activityKind = "terminal_unseen"
				}
				activities = append(activities, hub.SessionActivity{NodeID: node.ID, StoreInstanceID: storeID,
					SessionID: item.SessionID, Kind: activityKind, ResponseID: item.ResponseID,
					LifecycleState: item.LifecycleState, AttentionSeq: item.AttentionSeq, FinalRev: item.FinalRev,
					ShortTitle: item.ShortTitle, LongTitle: item.LongTitle, ProjectID: item.ProjectID,
					Outcome: item.Outcome, StartedAt: item.StartedAt, TerminalAt: item.TerminalAt,
					LeaseExpiresAt: item.LeaseExpiresAt})
			}
			if !page.HasMore {
				break
			}
			if page.NextCursor == "" {
				return errors.New("attention endpoint omitted pagination cursor")
			}
			cursor = page.NextCursor
			etag = "" // conditional requests only apply to the first page
		}
	}
	if storeID == "" {
		return errors.New("attention endpoint returned no snapshot identity")
	}
	return s.attentionStore.ReplaceNode(ctx, node.ID, storeID, responseETag, activities)
}

func (s *hubServer) fetchNodeAttentionPage(ctx context.Context, node hub.Node, kind, cursor string, version int64, etag string) (hubAttentionPage, string, bool, int, error) {
	var endpoint string
	if node.UsesReverseConnection() {
		u := &url.URL{Scheme: "http", Host: "reverse.local", Path: hubJoinBasePath(node.BasePath, "/v1/attention")}
		endpoint = u.String()
	} else {
		endpoint = node.BaseURL() + "/v1/attention"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return hubAttentionPage{}, "", false, 0, err
	}
	query := req.URL.Query()
	query.Set("kind", kind)
	query.Set("limit", "200")
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if version > 0 {
		query.Set("snapshot_version", strconv.FormatInt(version, 10))
	}
	req.URL.RawQuery = query.Encode()
	if node.Token != "" {
		req.Header.Set("Authorization", "Bearer "+node.Token)
	}
	if cursor == "" && etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := s.doHubNodeRequest(ctx, node, req)
	if err != nil {
		return hubAttentionPage{}, "", false, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return hubAttentionPage{}, resp.Header.Get("ETag"), true, resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return hubAttentionPage{}, resp.Header.Get("ETag"), false, resp.StatusCode, nil
	}
	var page hubAttentionPage
	if err := json.NewDecoder(io.LimitReader(resp.Body, hubAttentionMaxBytes)).Decode(&page); err != nil {
		return hubAttentionPage{}, "", false, resp.StatusCode, fmt.Errorf("decode attention page: %w", err)
	}
	return page, resp.Header.Get("ETag"), false, resp.StatusCode, nil
}

type hubAttentionNodeView struct {
	NodeID         string                  `json:"node_id"`
	NodeName       string                  `json:"node_name"`
	Capability     hub.AttentionCapability `json:"capability_state"`
	Stale          bool                    `json:"stale"`
	LastSuccessAt  time.Time               `json:"last_success_at,omitempty"`
	LastError      string                  `json:"last_error,omitempty"`
	RunningCount   int                     `json:"running_count"`
	UnseenCount    int                     `json:"unseen_count"`
	GreenIndicator bool                    `json:"has_green_indicator"`
}

type hubAttentionInboxItem struct {
	NodeID       string    `json:"node_id"`
	NodeName     string    `json:"node_name"`
	SessionID    string    `json:"session_id"`
	Title        string    `json:"title"`
	Outcome      string    `json:"outcome"`
	TerminalAt   time.Time `json:"terminal_at,omitempty"`
	AttentionSeq int64     `json:"attention_seq"`
	ResumePath   string    `json:"resume_path"`
}

func hubAttentionSyncHealthy(state hub.AttentionSync) bool {
	return state.Capability == hub.AttentionSupported &&
		(state.LastErrorAt.IsZero() || !state.LastErrorAt.After(state.LastSuccessAt))
}

// deduplicateHubActivities selects one stable resume route when multiple node
// registrations expose the same underlying session store. Sync health wins,
// followed by the freshest successful collection and then lexical node ID.
func deduplicateHubActivities(activities []hub.SessionActivity, syncs []hub.AttentionSync) []hub.SessionActivity {
	syncByNode := make(map[string]hub.AttentionSync, len(syncs))
	for _, state := range syncs {
		syncByNode[state.NodeID] = state
	}
	selected := make(map[string]hub.SessionActivity, len(activities))
	prefer := func(candidate, current hub.SessionActivity) bool {
		candidateSync, currentSync := syncByNode[candidate.NodeID], syncByNode[current.NodeID]
		candidateHealthy, currentHealthy := hubAttentionSyncHealthy(candidateSync), hubAttentionSyncHealthy(currentSync)
		if candidateHealthy != currentHealthy {
			return candidateHealthy
		}
		if !candidateSync.LastSuccessAt.Equal(currentSync.LastSuccessAt) {
			return candidateSync.LastSuccessAt.After(currentSync.LastSuccessAt)
		}
		return candidate.NodeID < current.NodeID
	}
	for _, activity := range activities {
		key := activity.StoreInstanceID + "\x00" + activity.SessionID + "\x00" + activity.Kind
		current, exists := selected[key]
		if !exists || prefer(activity, current) {
			selected[key] = activity
		}
	}
	result := make([]hub.SessionActivity, 0, len(selected))
	for _, activity := range selected {
		result = append(result, activity)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if !result[i].TerminalAt.Equal(result[j].TerminalAt) {
			return result[i].TerminalAt.After(result[j].TerminalAt)
		}
		if result[i].StoreInstanceID != result[j].StoreInstanceID {
			return result[i].StoreInstanceID < result[j].StoreInstanceID
		}
		if result[i].SessionID != result[j].SessionID {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].NodeID < result[j].NodeID
	})
	return result
}

func (s *hubServer) handleHubAttention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.attentionStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"total_running": 0, "total_unseen": 0, "nodes": []any{}, "inbox": []any{}, "has_more": false})
		return
	}
	limit := 200
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed <= 0 || parsed > 500 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "limit must be between 1 and 500")
			return
		}
		limit = parsed
	}
	activities, syncs, err := s.attentionStore.List(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to read Hub attention projection")
		return
	}
	nodes := []hub.Node(nil)
	if s.registry != nil {
		nodes, _ = s.registry.Nodes()
	}
	names := make(map[string]string, len(nodes))
	for _, node := range nodes {
		names[node.ID] = node.Name
	}
	states := make(map[string]*hubAttentionNodeView)
	for _, state := range syncs {
		now := time.Now().UTC()
		staleByAge := !state.LastSuccessAt.IsZero() && now.Sub(state.LastSuccessAt) > hubAttentionStaleAfter
		states[state.NodeID] = &hubAttentionNodeView{NodeID: state.NodeID, NodeName: names[state.NodeID], Capability: state.Capability,
			Stale:         state.LastSuccessAt.IsZero() || staleByAge || !state.LastErrorAt.IsZero() && state.LastErrorAt.After(state.LastSuccessAt),
			LastSuccessAt: state.LastSuccessAt, LastError: state.LastError}
	}
	activities = deduplicateHubActivities(activities, syncs)
	inbox := make([]hubAttentionInboxItem, 0)
	totalRunning, totalUnseen := 0, 0
	for _, activity := range activities {
		state := states[activity.NodeID]
		if state == nil {
			continue
		}
		state.GreenIndicator = true
		if activity.Kind == "running" {
			state.RunningCount++
			totalRunning++
			continue
		}
		state.UnseenCount++
		totalUnseen++
		title := strings.TrimSpace(activity.ShortTitle)
		if title == "" {
			title = strings.TrimSpace(activity.LongTitle)
		}
		if title == "" {
			title = "Untitled conversation"
		}
		inbox = append(inbox, hubAttentionInboxItem{NodeID: activity.NodeID, NodeName: names[activity.NodeID], SessionID: activity.SessionID,
			Title: title, Outcome: activity.Outcome, TerminalAt: activity.TerminalAt, AttentionSeq: activity.AttentionSeq,
			ResumePath: s.hubPath("/node/" + url.PathEscape(activity.NodeID) + "/" + url.PathEscape(activity.SessionID))})
	}
	sort.Slice(inbox, func(i, j int) bool {
		if !inbox[i].TerminalAt.Equal(inbox[j].TerminalAt) {
			return inbox[i].TerminalAt.After(inbox[j].TerminalAt)
		}
		if inbox[i].NodeID != inbox[j].NodeID {
			return inbox[i].NodeID < inbox[j].NodeID
		}
		return inbox[i].SessionID < inbox[j].SessionID
	})
	hasMore := len(inbox) > limit
	if hasMore {
		inbox = inbox[:limit]
	}
	nodeViews := make([]hubAttentionNodeView, 0, len(states))
	for _, state := range states {
		nodeViews = append(nodeViews, *state)
	}
	sort.Slice(nodeViews, func(i, j int) bool { return nodeViews[i].NodeName < nodeViews[j].NodeName })
	writeJSON(w, http.StatusOK, map[string]any{"total_running": totalRunning, "total_unseen": totalUnseen, "nodes": nodeViews, "inbox": inbox, "has_more": hasMore})
}

func (s *hubServer) applyHubAttentionViews(ctx context.Context, views []hubNodeView) {
	if s.attentionStore == nil {
		return
	}
	activities, syncs, err := s.attentionStore.List(ctx)
	if err != nil {
		return
	}
	activities = deduplicateHubActivities(activities, syncs)
	counts := make(map[string][2]int)
	for _, activity := range activities {
		value := counts[activity.NodeID]
		if activity.Kind == "running" {
			value[0]++
		} else {
			value[1]++
		}
		counts[activity.NodeID] = value
	}
	syncByNode := make(map[string]hub.AttentionSync)
	for _, state := range syncs {
		syncByNode[state.NodeID] = state
	}
	for i := range views {
		count := counts[views[i].ID]
		if views[i].Sessions == nil {
			views[i].Sessions = &hubNodeSessionsView{}
		}
		views[i].Sessions.ActiveCount = max(views[i].Sessions.ActiveCount, count[0])
		views[i].Sessions.UnseenCount = count[1]
		if state, ok := syncByNode[views[i].ID]; ok {
			views[i].Sessions.AttentionCapability = string(state.Capability)
			if !state.LastSuccessAt.IsZero() {
				views[i].Sessions.AttentionLastSuccessAt = state.LastSuccessAt.UnixMilli()
			}
		}
	}
}
