package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	liveclassify "github.com/samsaffron/term-llm/internal/live/classify"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

type recordingDecisionLog struct {
	mu   sync.Mutex
	rows []liveclassify.DecisionRecord
}

func (l *recordingDecisionLog) Enqueue(row liveclassify.DecisionRecord) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, row)
	return true
}

func (l *recordingDecisionLog) last() liveclassify.DecisionRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.rows) == 0 {
		return liveclassify.DecisionRecord{}
	}
	return l.rows[len(l.rows)-1]
}

type liveDecisionClassifierFunc func(context.Context, liveclassify.State) (liveclassify.Decision, error)

func (f liveDecisionClassifierFunc) Classify(ctx context.Context, state liveclassify.State) (liveclassify.Decision, error) {
	return f(ctx, state)
}

type countingLiveStateStore struct {
	session.Store
	mu    sync.Mutex
	gets  int
	lists int
}

func (s *countingLiveStateStore) Get(ctx context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(ctx, id)
}

func (s *countingLiveStateStore) List(ctx context.Context, options session.ListOptions) ([]session.SessionSummary, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.List(ctx, options)
}

func (s *countingLiveStateStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.lists
}

type noOpClassifyClient struct{}

func (noOpClassifyClient) Classify(context.Context, typesafe.Request) (*typesafe.Response, error) {
	return nil, nil
}
func (noOpClassifyClient) ListModels(context.Context) (*typesafe.ModelsResponse, error) {
	return nil, nil
}

func TestPrepareLiveClassifyUsesThreeSecondDefault(t *testing.T) {
	cfg := &config.Config{
		Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{
			"typesafe": {Type: "typesafe", APIKey: "test", Model: "jev-latest"},
		}},
		Live: classifyLiveConfig(),
	}
	cfg.Live.Classify.LogDecisions = false
	old := newLiveClassifyClient
	defer func() { newLiveClassifyClient = old }()
	var captured typesafe.Options
	newLiveClassifyClient = func(options typesafe.Options) (classifyClient, error) {
		captured = options
		return noOpClassifyClient{}, nil
	}
	classifier, store, err := prepareLiveClassify(cfg)
	if err != nil || classifier == nil || store != nil {
		t.Fatalf("prepare = %v, %v, %v", classifier, store, err)
	}
	if captured.Timeout != liveClassifyDefaultTimeout {
		t.Fatalf("timeout = %v, want %v", captured.Timeout, liveClassifyDefaultTimeout)
	}
}

func TestPrepareLiveClassifyClampsConfiguredTimeoutToRoutingBudget(t *testing.T) {
	cfg := &config.Config{
		Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{
			"typesafe": {Type: "typesafe", APIKey: "test", Model: "jev-latest", TimeoutSeconds: 120},
		}},
		Live: classifyLiveConfig(),
	}
	cfg.Live.Classify.LogDecisions = false
	old := newLiveClassifyClient
	defer func() { newLiveClassifyClient = old }()
	var captured typesafe.Options
	newLiveClassifyClient = func(options typesafe.Options) (classifyClient, error) {
		captured = options
		return noOpClassifyClient{}, nil
	}
	if _, _, err := prepareLiveClassify(cfg); err != nil {
		t.Fatal(err)
	}
	if captured.Timeout != liveClassifyMaxTimeout {
		t.Fatalf("timeout = %v, want %v", captured.Timeout, liveClassifyMaxTimeout)
	}
}

func TestPrepareLiveClassifySecuresExistingDiagnosticsDirectory(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	diagnosticsDir := filepath.Join(dataHome, "term-llm", "diagnostics")
	if err := os.MkdirAll(diagnosticsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(diagnosticsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{
			"typesafe": {Type: "typesafe", APIKey: "test", Model: "jev-latest"},
		}},
		Live: classifyLiveConfig(),
	}
	old := newLiveClassifyClient
	defer func() { newLiveClassifyClient = old }()
	newLiveClassifyClient = func(typesafe.Options) (classifyClient, error) { return noOpClassifyClient{}, nil }
	_, store, err := prepareLiveClassify(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("decision store is nil")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		diagnosticsDir:                           0o700,
		filepath.Join(diagnosticsDir, "live.db"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestPrepareLiveClassifyFailsAtStartupAndReloadWithoutResolvableProvider(t *testing.T) {
	for _, phase := range []string{"startup", "reload"} {
		t.Run(phase, func(t *testing.T) {
			cfg := &config.Config{Live: classifyLiveConfig()}
			cfg.Live.Classify.Provider = "missing"
			if _, _, err := prepareLiveClassify(cfg); err == nil || !strings.Contains(err.Error(), `classify provider "missing" is not configured`) {
				t.Fatalf("prepareLiveClassify error = %v", err)
			}
		})
	}
}

func TestLiveControlPlaneCompletionValues(t *testing.T) {
	got := configValueCompletions("live.control_plane", "")
	want := []string{"off", "agent", "classify"}
	if len(got) != len(want) {
		t.Fatalf("completions = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("completions = %v, want %v", got, want)
		}
	}
}

func TestStartLiveControllerInstallsClassifyRouter(t *testing.T) {
	srv := newTestServeServer()
	srv.cfgRef = &config.Config{Live: classifyLiveConfig()}
	srv.store = newSessionDirectoryTestStore(t)
	createDirectorySession(t, srv.store, &session.Session{ID: "controller-classify", GeneratedShortTitle: "Controller classify"})
	called := make(chan struct{}, 1)
	srv.liveClassifier = liveDecisionClassifierFunc(func(context.Context, liveclassify.State) (liveclassify.Decision, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return liveclassify.Decision{Intent: liveclassify.IntentStatus, Probabilities: map[string]float64{liveclassify.IntentStatus: 0.99}}, nil
	})
	provider := &stubLiveSession{events: make(chan live.Event, 8), closed: make(chan struct{})}
	record := newLiveSession("classify-controller", "controller-classify")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	if !srv.startLiveController(record, provider) {
		t.Fatal("controller did not start")
	}
	t.Cleanup(func() { srv.closeLiveSessions(context.Background()) })
	provider.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "classify-item", Text: "what is running"}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("classify router was not called")
	}
}

func TestLiveClassifyRouterPhaseOneActions(t *testing.T) {
	t.Run("steer", func(t *testing.T) {
		router, _, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentSteer, 0.99, 0))
		defer cleanup()
		result, err := router.Route(context.Background(), live.RouteRequest{Input: "fix the parser"})
		if err != nil || result.Handled || result.Input != "fix the parser" {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("status", func(t *testing.T) {
		router, srv, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentStatus, 0.91, 0))
		defer cleanup()
		createDirectorySession(t, srv.store.(*session.SQLiteStore), &session.Session{ID: "recent-status", GeneratedShortTitle: "Recent work"})
		result, err := router.Route(context.Background(), live.RouteRequest{Input: "what is running?"})
		if err != nil || !result.Handled || !strings.Contains(result.Answer, "Nothing is running") || !strings.Contains(result.Answer, "Recent") {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("new session", func(t *testing.T) {
		h := newVoiceNewSessionHarness(t, "classify-new")
		h.conversation(t, &session.Session{ID: "classify-new", GeneratedShortTitle: "Source", Agent: ""})
		h.server.cfgRef.Live = classifyLiveConfig()
		classifier, cleanup := testLiveClassifier(t, liveClassifyResponse(liveclassify.IntentNewSession, 0.95, 0), 0, time.Second)
		defer cleanup()
		router := &serveLiveClassifyRouter{server: h.server, live: h.record, classifier: classifier}
		result, err := router.Route(context.Background(), live.RouteRequest{Input: "start a new conversation"})
		if err != nil || !result.Handled || !strings.Contains(result.Answer, "Started a new conversation") || h.record.boundSession() == "classify-new" {
			t.Fatalf("result = %+v binding=%q err=%v", result, h.record.boundSession(), err)
		}
	})

	t.Run("switch session", func(t *testing.T) {
		router, _, record, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentSwitchSession, 0.93, 0))
		defer cleanup()
		called := 0
		router.resolver = func() func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error) {
			called++
			return func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error) {
				record.sessionID = "target"
				return liveSwitchResolverOutcome{Kind: liveSwitchResolverSucceeded, Answer: "Switched to Target."}, nil
			}
		}
		result, err := router.Route(context.Background(), live.RouteRequest{Input: "switch to target"})
		if err != nil || !result.Handled || result.Answer != "Switched to Target." || called != 1 {
			t.Fatalf("result = %+v called=%d err=%v", result, called, err)
		}
	})
}

func TestLiveClassifyStatusIncludesRunningSessionAndJobs(t *testing.T) {
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: "status-running", GeneratedShortTitle: "Active voice work"})
	srv := newTestServeServer()
	srv.store = store
	runs := srv.ensureResponseRuns()
	runs.setActiveRun("status-running", "response-running")
	t.Cleanup(func() { runs.clearActiveRun("status-running", "response-running") })

	jobs, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	srv.jobsV2 = jobs
	activeJob, err := jobs.CreateJob(jobsV2Job{
		Name: "active backup", Enabled: true, RunnerType: jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"true"}`), TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.TriggerJob(activeJob.ID); err != nil {
		t.Fatal(err)
	}
	recentJob, err := jobs.CreateJob(jobsV2Job{
		Name: "recent report", Enabled: true, RunnerType: jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"true"}`), TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	recentRun, err := jobs.TriggerJob(recentJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.finishRun(recentRun.ID, jobsV2RunSucceeded, jobsV2RunResult{}, nil, recentRun.Attempt); err != nil {
		t.Fatal(err)
	}

	answer, err := srv.liveClassifyStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Currently running:", "Active voice work", "active backup", "Recent:", "recent report succeeded"} {
		if !strings.Contains(answer, want) {
			t.Fatalf("status %q missing %q", answer, want)
		}
	}
}

func TestLiveClassifyStatusDegradesToSessionsWhenJobsUnavailable(t *testing.T) {
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: "status-session-only", GeneratedShortTitle: "Session fallback"})
	jobs, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Close(); err != nil {
		t.Fatal(err)
	}
	srv := newTestServeServer()
	srv.store = store
	srv.jobsV2 = jobs
	answer, err := srv.liveClassifyStatus(context.Background())
	if err != nil {
		t.Fatalf("status failed instead of degrading: %v", err)
	}
	if !strings.Contains(answer, "Session fallback") {
		t.Fatalf("sessions-only status = %q", answer)
	}
}

func TestLiveStatusSessionLabelNeverSpeaksRawID(t *testing.T) {
	if got := liveStatusSessionLabel(sessionDirectoryEntry{ID: "private-raw-id", Number: 42}); got != "session #42" {
		t.Fatalf("numbered title-less label = %q", got)
	}
	if got := liveStatusSessionLabel(sessionDirectoryEntry{ID: "private-raw-id"}); got != "an untitled session" {
		t.Fatalf("un-numbered title-less label = %q", got)
	}
}

func TestLiveClassifyRouterFailuresPreserveOriginalInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		delay   time.Duration
		timeout time.Duration
	}{
		{name: "malformed", body: `{not json`},
		{name: "unknown label", body: liveClassifyResponse("mystery", 0.99, 0)},
		{name: "timeout", body: liveClassifyResponse(liveclassify.IntentStatus, 0.99, 0), delay: 100 * time.Millisecond, timeout: 10 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, _, cleanup := newLiveClassifyRouterHarnessWithDelay(t, tc.body, tc.delay, tc.timeout)
			defer cleanup()
			const original = "keep every original word"
			result, err := router.Route(context.Background(), live.RouteRequest{Input: original})
			if err != nil || result.Handled || result.Input != original {
				t.Fatalf("result = %+v, %v", result, err)
			}
		})
	}
}

func TestLiveClassifyRouterSlowDecisionWriterNeverBlocksRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	decisions, err := liveclassify.OpenDecisionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	router, _, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentSteer, 0.99, 0))
	defer cleanup()
	router.decisions = decisions
	for _, input := range []string{"preserve this exact first request", "and this later request too"} {
		started := time.Now()
		result, routeErr := router.Route(context.Background(), live.RouteRequest{Input: input})
		if routeErr != nil || result.Handled || result.Input != input {
			t.Fatalf("result = %+v, err = %v", result, routeErr)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("Route blocked on decision writer for %v", elapsed)
		}
	}
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := decisions.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveClassifyRouterDecisionInsertFailureLeavesResultUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	decisions, err := liveclassify.OpenDecisionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE route_decisions"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	router, _, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentSteer, 0.99, 0))
	defer cleanup()
	router.decisions = decisions
	const original = "return this unchanged despite the logging failure"
	result, routeErr := router.Route(context.Background(), live.RouteRequest{Input: original})
	if routeErr != nil || result.Handled || result.Input != original {
		t.Fatalf("result = %+v, err = %v", result, routeErr)
	}
	if err := decisions.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveClassifyRouterLogStateFalseStoresOnlyErrorCategories(t *testing.T) {
	const (
		messageMarker    = "PRIVATE_MESSAGE_MARKER"
		transcriptMarker = "PRIVATE_TRANSCRIPT_MARKER"
		titleMarker      = "PRIVATE_TITLE_MARKER"
		errorMarker      = "PRIVATE_PROVIDER_ERROR_MARKER"
	)
	cases := []struct {
		name       string
		classifier liveDecisionClassifier
		resolver   func() func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error)
		want       string
	}{
		{
			name: "provider error",
			classifier: liveDecisionClassifierFunc(func(context.Context, liveclassify.State) (liveclassify.Decision, error) {
				return liveclassify.Decision{}, errors.New(errorMarker)
			}),
			want: liveDecisionErrorProvider,
		},
		{
			name: "invalid answer",
			classifier: liveDecisionClassifierFunc(func(context.Context, liveclassify.State) (liveclassify.Decision, error) {
				return liveclassify.Decision{}, fmt.Errorf("%w: %s", liveclassify.ErrInvalidAnswer, errorMarker)
			}),
			want: liveDecisionErrorClassifyInvalid,
		},
		{
			name: "resolver error",
			classifier: liveDecisionClassifierFunc(func(context.Context, liveclassify.State) (liveclassify.Decision, error) {
				return liveclassify.Decision{Intent: liveclassify.IntentSwitchSession, Probabilities: map[string]float64{liveclassify.IntentSwitchSession: 0.99}}, nil
			}),
			resolver: func() func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error) {
				return func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error) {
					return liveSwitchResolverOutcome{Kind: liveSwitchResolverError}, errors.New(errorMarker)
				}
			},
			want: liveDecisionErrorResolver,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, srv, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentSteer, 0.99, 0))
			defer cleanup()
			srv.cfgRef.Live.Classify.LogState = false
			current, err := srv.store.Get(context.Background(), "source")
			if err != nil {
				t.Fatal(err)
			}
			current.GeneratedShortTitle = titleMarker
			if err := srv.store.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			decisionsPath := filepath.Join(t.TempDir(), "live.db")
			decisions, err := liveclassify.OpenDecisionStore(decisionsPath)
			if err != nil {
				t.Fatal(err)
			}
			router.decisions = decisions
			router.classifier = tc.classifier
			if tc.resolver != nil {
				router.resolver = tc.resolver
			}
			result, err := router.Route(context.Background(), live.RouteRequest{Input: messageMarker, TranscriptDelta: transcriptMarker})
			if err != nil || result.Handled || result.Input != messageMarker {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
			if err := decisions.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := liveclassify.OpenDecisionStoreReadOnly(decisionsPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			stored, err := reader.List(context.Background(), time.Time{}, 10)
			if err != nil || len(stored) != 1 {
				t.Fatalf("stored rows = %+v, err = %v", stored, err)
			}
			row := stored[0]
			if len(row.StateJSON) != 0 || row.Error != tc.want {
				t.Fatalf("row = %+v", row)
			}
			encoded, _ := json.Marshal(row)
			for _, marker := range []string{messageMarker, transcriptMarker, titleMarker, errorMarker} {
				if strings.Contains(string(encoded), marker) {
					t.Fatalf("stored row leaked %q: %s", marker, encoded)
				}
			}
		})
	}
}

func TestLiveClassifyRouterLengthSkipAvoidsSessionStore(t *testing.T) {
	router, srv, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentStatus, 0.99, 0))
	defer cleanup()
	counting := &countingLiveStateStore{Store: srv.store}
	srv.store = counting
	rows := &recordingDecisionLog{}
	router.decisions = rows
	called := false
	router.classifier = liveDecisionClassifierFunc(func(context.Context, liveclassify.State) (liveclassify.Decision, error) {
		called = true
		return liveclassify.Decision{}, nil
	})
	words := make([]string, liveClassifyMaxWords+1)
	for i := range words {
		words[i] = "word"
	}
	input := strings.Join(words, " ")
	result, err := router.Route(context.Background(), live.RouteRequest{Input: input})
	if err != nil || result.Input != input || result.Handled || called {
		t.Fatalf("result = %+v called=%v err=%v", result, called, err)
	}
	if gets, lists := counting.counts(); gets != 0 || lists != 0 {
		t.Fatalf("session store reads = get:%d list:%d", gets, lists)
	}
	if row := rows.last(); row.ResolverOutcome != "skipped_length" {
		t.Fatalf("decision row = %+v", row)
	}
}

func TestLiveClassifyRouterNewSessionFailureFailsOpenOriginalInput(t *testing.T) {
	router, srv, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentNewSession, 0.99, 0))
	defer cleanup()
	if err := srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	const original = "start a fresh conversation"
	result, err := router.Route(context.Background(), live.RouteRequest{Input: original})
	if err != nil || result.Handled || result.Input != original {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestLiveClassifyRouterLogsDecisionsAndHonoursLogState(t *testing.T) {
	router, srv, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentSteer, 0.99, 0))
	defer cleanup()
	logRows := &recordingDecisionLog{}
	router.decisions = logRows
	result, err := router.Route(context.Background(), live.RouteRequest{Input: "fix the parser"})
	if err != nil || result.Handled || result.Input != "fix the parser" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	row := logRows.last()
	if row.GatedLabel != liveclassify.IntentSteer || row.ActedLabel != liveclassify.IntentSteer || len(row.StateJSON) == 0 {
		t.Fatalf("decision row = %+v", row)
	}
	srv.cfgRef.Live.Classify.LogState = false
	if _, err := router.Route(context.Background(), live.RouteRequest{Input: "fix it again"}); err != nil {
		t.Fatal(err)
	}
	if row := logRows.last(); len(row.StateJSON) != 0 || len(row.Probabilities) == 0 {
		t.Fatalf("log_state=false row = %+v", row)
	}
}

func TestLiveClassifyRouterMixedNavigationFailsOpenWholeRequest(t *testing.T) {
	for _, label := range []string{liveclassify.IntentNewSession, liveclassify.IntentSwitchSession} {
		router, _, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(label, 0.99, 0.8))
		const original = "in the album chat make the ornaments smaller"
		result, err := router.Route(context.Background(), live.RouteRequest{Input: original})
		cleanup()
		if err != nil || result.Handled || result.Input != original {
			t.Fatalf("%s result = %+v, %v", label, result, err)
		}
	}
}

func newLiveClassifyRouterHarness(t *testing.T, body string) (*serveLiveClassifyRouter, *serveServer, *liveSession, func()) {
	return newLiveClassifyRouterHarnessWithDelay(t, body, 0, time.Second)
}

func newLiveClassifyRouterHarnessWithDelay(t *testing.T, body string, delay, timeout time.Duration) (*serveLiveClassifyRouter, *serveServer, *liveSession, func()) {
	t.Helper()
	if timeout == 0 {
		timeout = time.Second
	}
	classifier, closeHTTP := testLiveClassifier(t, body, delay, timeout)
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: "source", GeneratedShortTitle: "Source"})
	srv := newTestServeServer()
	srv.store = store
	srv.cfgRef = &config.Config{Live: classifyLiveConfig()}
	record := newLiveSession("live-classify", "source")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	return &serveLiveClassifyRouter{server: srv, live: record, classifier: classifier}, srv, record, closeHTTP
}

func classifyLiveConfig() config.LiveConfig {
	return config.LiveConfig{
		ControlPlane: config.LiveControlPlaneClassify,
		Classify: config.LiveClassifyConfig{
			LogDecisions: true, LogState: true,
			MinConfidence: config.LiveClassifyMinConfidence{Status: 0.6, NewSession: 0.7, SwitchSession: 0.7, SteerNow: 0.8, Side: 0.85},
		},
	}
}

func testLiveClassifier(t *testing.T, body string, delay, timeout time.Duration) (*liveclassify.Classifier, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		_, _ = w.Write([]byte(body))
	}))
	client, err := typesafe.NewClient(typesafe.Options{APIKey: "test", BaseURL: server.URL, Timeout: timeout})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return &liveclassify.Classifier{Client: client, Model: "jev-latest"}, server.Close
}

func liveClassifyResponse(label string, confidence, also float64) string {
	probabilities := map[string]float64{
		liveclassify.IntentSteer: 0.01, liveclassify.IntentStatus: 0.01, liveclassify.IntentNewSession: 0.01,
		liveclassify.IntentSwitchSession: 0.01, liveclassify.IntentSteerNow: 0.01, liveclassify.IntentSide: 0.01,
	}
	if _, ok := probabilities[label]; ok {
		probabilities[label] = confidence
	}
	body, _ := json.Marshal(map[string]any{
		"model": "jev-latest",
		"answers": map[string]any{
			liveclassify.IntentQuestionID:          map[string]any{"type": "choice", "choice": label, "probabilities": probabilities},
			liveclassify.AlsoRequestQuestionID:     map[string]any{"type": "noul", "noul": also},
			liveclassify.UnambiguousStopQuestionID: map[string]any{"type": "noul", "noul": 0.1},
		},
	})
	return string(body)
}
