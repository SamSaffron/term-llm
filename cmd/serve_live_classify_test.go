package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func (l *recordingDecisionLog) Insert(_ context.Context, row liveclassify.DecisionRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, row)
	return nil
}

func (l *recordingDecisionLog) last() liveclassify.DecisionRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.rows) == 0 {
		return liveclassify.DecisionRecord{}
	}
	return l.rows[len(l.rows)-1]
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
		Live: classifyLiveConfig(true),
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

func TestPrepareLiveClassifyFailsAtStartupWithoutResolvableProvider(t *testing.T) {
	cfg := &config.Config{Live: classifyLiveConfig(true)}
	cfg.Live.Classify.Provider = "missing"
	if _, _, err := prepareLiveClassify(cfg); err == nil || !strings.Contains(err.Error(), `classify provider "missing" is not configured`) {
		t.Fatalf("prepareLiveClassify error = %v", err)
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
		h.server.cfgRef.Live = classifyLiveConfig(false)
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

func TestLiveClassifyRouterShadowLogsWithoutAuthorityOrResolver(t *testing.T) {
	router, srv, _, cleanup := newLiveClassifyRouterHarness(t, liveClassifyResponse(liveclassify.IntentSwitchSession, 0.99, 0))
	defer cleanup()
	srv.cfgRef.Live.Classify.Shadow = true
	logRows := &recordingDecisionLog{}
	router.decisions = logRows
	constructed := 0
	router.resolver = func() func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error) {
		constructed++
		return nil
	}
	result, err := router.Route(context.Background(), live.RouteRequest{Input: "switch to the other session"})
	if err != nil || result.Handled || result.Input != "switch to the other session" || constructed != 0 {
		t.Fatalf("result=%+v constructed=%d err=%v", result, constructed, err)
	}
	row := logRows.last()
	if row.GatedLabel != liveclassify.IntentSwitchSession || row.ActedLabel != liveclassify.IntentSteer || len(row.StateJSON) == 0 {
		t.Fatalf("decision row = %+v", row)
	}
	if srv.liveConfig().ControlAuthorityAllowed() || strings.Contains(srv.liveSessionOptions(context.Background(), "source", live.Capabilities{}, "").Context, live.ControlPlaneContext) {
		t.Fatal("shadow mode installed or advertised control authority")
	}
	srv.cfgRef.Live.Classify.LogState = false
	if _, err := router.Route(context.Background(), live.RouteRequest{Input: "switch again"}); err != nil {
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
	srv.cfgRef = &config.Config{Live: classifyLiveConfig(false)}
	record := newLiveSession("live-classify", "source")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	return &serveLiveClassifyRouter{server: srv, live: record, classifier: classifier}, srv, record, closeHTTP
}

func classifyLiveConfig(shadow bool) config.LiveConfig {
	return config.LiveConfig{
		ControlPlane: config.LiveControlPlaneClassify,
		Classify: config.LiveClassifyConfig{
			Shadow: shadow, LogDecisions: true, LogState: true,
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
