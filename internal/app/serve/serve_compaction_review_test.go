package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestServeCompactionStatusDoesNotTouchRuntimeTTL(t *testing.T) {
	s, rt, _, _ := newCompactionLifecycleFixture(t, nil)
	rt.beginCompactionActivity()
	rt.finishCompactionActivity(errors.New("retained failure"))
	before := time.Now().Add(-10 * time.Second).Truncate(time.Nanosecond)
	rt.lastUsedUnixNano.Store(before.UnixNano())
	for i := 0; i < 2; i++ {
		out := httptest.NewRecorder()
		s.handleSessionsStatus(out, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
		if out.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
		}
		if got := rt.LastUsed(); !got.Equal(before) {
			t.Fatalf("status poll extended runtime TTL: before=%v after=%v", before, got)
		}
	}
}

func TestServeCompactionCompletionRevisionIsImmutableAndAvoidsStatusReads(t *testing.T) {
	s, rt, _, unblock := newCompactionLifecycleFixture(t, nil)
	acceptTestCompaction(t, s, context.Background())
	unblock()
	completed := waitForServeCompaction(t, s, "lifecycle")
	committed, err := s.transcriptRev(context.Background(), "lifecycle")
	if err != nil || committed <= 0 || completed.TranscriptRev != committed {
		t.Fatalf("completion anchor=%+v committed=%d err=%v", completed, committed, err)
	}
	if err := s.store.AddMessage(context.Background(), "lifecycle", session.NewMessage("lifecycle", core.UserText("later turn"), -1)); err != nil {
		t.Fatal(err)
	}
	later, err := s.transcriptRev(context.Background(), "lifecycle")
	if err != nil || later <= committed {
		t.Fatalf("later revision=%d committed=%d err=%v", later, committed, err)
	}
	indexer, _ := s.transcriptIndexerForWeb()
	counting := &countingStatusTranscriptStore{Store: s.store, indexer: indexer}
	statusServer := &serveServer{store: counting, sessionMgr: s.sessionMgr}
	for i := 0; i < 2; i++ {
		out := httptest.NewRecorder()
		statusServer.handleSessionsStatus(out, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
		var body struct {
			Sessions []struct {
				TranscriptRev int64                 `json:"transcript_rev"`
				Activity      *serveSessionActivity `json:"activity"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if out.Code != http.StatusOK || len(body.Sessions) != 1 || body.Sessions[0].Activity == nil {
			t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
		}
		entry := body.Sessions[0]
		if entry.TranscriptRev != later || entry.Activity.TranscriptRev != committed {
			t.Fatalf("status changed immutable completion anchor: %+v activity=%+v", entry, entry.Activity)
		}
	}
	// Even if the earlier list sample predates compaction, its revision is
	// raised to the committed activity anchor without another indexer read.
	statusServer.store = &staleCompactionSummaryStore{counting}
	out := httptest.NewRecorder()
	statusServer.handleSessionsStatus(out, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
	var staleBody struct {
		Sessions []struct {
			TranscriptRev int64 `json:"transcript_rev"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &staleBody); err != nil {
		t.Fatal(err)
	}
	if out.Code != http.StatusOK || len(staleBody.Sessions) != 1 || staleBody.Sessions[0].TranscriptRev != committed {
		t.Fatalf("stale list was not raised to committed activity revision: %s", out.Body.String())
	}
	if counting.revCalls != 0 {
		t.Fatalf("terminal activity caused %d extra transcript reads", counting.revCalls)
	}
	if rt.activitySnapshot().TranscriptRev != committed {
		t.Fatal("later transcript write changed activity anchor")
	}
}

func TestServeCompactionRevisionReadDoesNotBlockActivitySnapshot(t *testing.T) {
	rt := &serveRuntime{}
	rt.beginCompactionActivity()
	reading, releaseRead, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	rt.activity.mu.Lock()
	rt.activity.transcriptRevision = func(context.Context) (int64, error) {
		close(reading)
		<-releaseRead
		return 42, nil
	}
	rt.activity.mu.Unlock()
	go func() {
		defer close(finished)
		rt.mu.Lock()
		rt.finishCompactionActivityWithRelease(nil, rt.mu.Unlock)
	}()
	defer func() { close(releaseRead); <-finished }()
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("completion revision read did not begin")
	}
	snapshot := make(chan *serveSessionActivity, 1)
	go func() { snapshot <- rt.activitySnapshot() }()
	select {
	case activity := <-snapshot:
		if activity == nil || activity.Status != "running" {
			t.Fatalf("unexpected pending completion snapshot: %+v", activity)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completion held activity lock across revision read")
	}
}

func TestServeCompactionChildRemainsInStatusAfterNavigation(t *testing.T) {
	s, rt, _, unblock := newCompactionLifecycleFixture(t, nil)
	ctx := context.Background()
	if err := s.store.Create(ctx, &session.Session{ID: "parent", Provider: "mock", Model: "mock-model"}); err != nil {
		t.Fatal(err)
	}
	rt.sessionMeta.ParentID, rt.sessionMeta.IsSubagent = "parent", true
	if err := s.store.Update(ctx, rt.sessionMeta); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Create(ctx, &session.Session{ID: "unrelated-child", ParentID: "parent", IsSubagent: true, Provider: "mock", Model: "mock-model"}); err != nil {
		t.Fatal(err)
	}
	s.ensureResponseRuns().setActiveRun("unrelated-child", "unrelated-response")
	accepted := acceptTestCompaction(t, s, ctx)
	for _, selected := range []string{"lifecycle", "parent", ""} {
		out := httptest.NewRecorder()
		s.handleSessionsStatus(out, httptest.NewRequest(http.MethodGet, "/v1/sessions/status?selected_session="+selected, nil))
		var body struct {
			Sessions []struct {
				ID        string                `json:"id"`
				ActiveRun bool                  `json:"active_run"`
				Activity  *serveSessionActivity `json:"activity"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range body.Sessions {
			if entry.ID == "unrelated-child" {
				t.Fatal("compaction exception included unrelated child run")
			}
			if entry.ID == "lifecycle" {
				found = true
				if entry.ActiveRun || entry.Activity == nil || entry.Activity.ID != accepted.ID || entry.Activity.Status != "running" {
					t.Fatalf("child compaction status after selecting %q: %+v", selected, entry)
				}
			}
		}
		if out.Code != http.StatusOK || !found {
			t.Fatalf("compacting child missing after selecting %q: %d %s", selected, out.Code, out.Body.String())
		}
	}
	unblock()
	waitForServeCompaction(t, s, "lifecycle")
}

func TestServeCompactionStructuredCancellationIsNotFailure(t *testing.T) {
	for _, terminalErr := range []error{
		fmt.Errorf("provider stopped: %w", context.Canceled),
		fmt.Errorf("reload: %w", &engine.SuspendedError{}),
	} {
		rt := &serveRuntime{}
		rt.recordRuntimeEvent(core.Event{Type: core.EventCompactionStatus, CompactionActive: true})
		rt.recordRuntimeEvent(core.Event{Type: core.EventCompactionStatus, Err: terminalErr})
		if got := rt.activitySnapshot(); got.Status != "cancelled" || got.Error != "" || got.TranscriptRev != 0 {
			t.Fatalf("structured cancellation=%+v", got)
		}
	}
}

func TestServeCompactionAutomaticCompletionCapturesRevision(t *testing.T) {
	s, rt, _, _ := newCompactionLifecycleFixture(t, nil)
	provider := &statusOnlyCompactionProvider{
		Provider: mockprovider.NewMockProvider("mock"),
		events: []core.Event{
			{Type: core.EventCompactionStatus, CompactionActive: true},
			{Type: core.EventCompactionStatus},
			{Type: core.EventTextDelta, Text: "normal continuation"},
		},
	}
	rt.provider, rt.engine = provider, engine.NewEngine(provider, nil)
	s.bindRuntimeActivity(rt, "lifecycle")
	var atCompletion int64
	_, err := rt.RunWithEvents(context.Background(), true, false, []core.Message{core.UserText("next turn")}, core.Request{SessionID: "lifecycle", Model: "mock-model"}, func(ev core.Event) error {
		if ev.Type == core.EventCompactionStatus && !ev.CompactionActive {
			atCompletion, _ = s.transcriptRev(context.Background(), "lifecycle")
			if got := rt.activitySnapshot(); got.TranscriptRev != atCompletion {
				t.Errorf("automatic completion anchor=%d committed=%d", got.TranscriptRev, atCompletion)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.activitySnapshot(); atCompletion <= 0 || got.Status != "completed" || got.TranscriptRev != atCompletion {
		t.Fatalf("automatic terminal anchor=%+v at completion=%d", got, atCompletion)
	}
}

func TestServeCompactionWorkerRecoversProviderPanic(t *testing.T) {
	s, rt, provider, unblock := newCompactionLifecycleFixture(t, nil)
	provider.panicValue = "private provider panic details"
	acceptTestCompaction(t, s, context.Background())
	unblock()
	terminal := waitForServeCompaction(t, s, "lifecycle")
	if terminal.Status != "failed" || terminal.Error == "" || strings.Contains(terminal.Error, "private provider") {
		t.Fatalf("panic terminalization leaked or lost failure: %+v", terminal)
	}
	if rt.hasActiveActivity() {
		t.Fatal("panicking compaction leaked ownership")
	}
	if !rt.mu.TryLock() {
		t.Fatal("panicking compaction leaked runtime mutex")
	}
	rt.mu.Unlock()
	if got := readTestCompactionState(t, s); got == nil || got.Status != "failed" {
		t.Fatalf("panic result unavailable to state polling: %+v", got)
	}
}

func TestServeCompactionManualCancellationReleasesWithoutFailure(t *testing.T) {
	for _, cause := range []error{context.Canceled, &engine.SuspendedError{}} {
		t.Run(cause.Error(), func(t *testing.T) {
			s, rt, _, unblock := newCompactionLifecycleFixture(t, fmt.Errorf("provider stopped: %w", cause))
			acceptTestCompaction(t, s, context.Background())
			unblock()
			terminal := waitForServeCompaction(t, s, "lifecycle")
			if terminal.Status != "cancelled" || terminal.Error != "" || terminal.TranscriptRev != 0 {
				t.Fatalf("manual cancellation reported as failure: %+v", terminal)
			}
			if rt.hasActiveActivity() {
				t.Fatal("cancelled compaction leaked admission")
			}
		})
	}
}

type staleCompactionSummaryStore struct{ *countingStatusTranscriptStore }

func (s *staleCompactionSummaryStore) List(ctx context.Context, options session.ListOptions) ([]session.SessionSummary, error) {
	rows, err := s.Store.List(ctx, options)
	for i := range rows {
		rows[i].TranscriptRev = 1
	}
	return rows, err
}
