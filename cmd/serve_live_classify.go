package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	liveclassify "github.com/samsaffron/term-llm/internal/live/classify"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

const (
	liveClassifyMaxWords       = 40
	liveClassifyDefaultTimeout = 3 * time.Second
	liveClassifyTimeoutMargin  = 2 * time.Second
	liveClassifyMaxTimeout     = live.RouteCallTimeout - liveControlTurnTimeout - liveClassifyTimeoutMargin
)

const (
	liveDecisionErrorRouterUnavailable = "router_unavailable"
	liveDecisionErrorUnboundSession    = "unbound_session"
	liveDecisionErrorProvider          = "provider_error"
	liveDecisionErrorClassifyTimeout   = "classify_timeout"
	liveDecisionErrorClassifyInvalid   = "classify_invalid_answer"
	liveDecisionErrorStatus            = "status_error"
	liveDecisionErrorNewSession        = "new_session_error"
	liveDecisionErrorResolver          = "resolver_error"
)

var newLiveClassifyClient = func(options typesafe.Options) (classifyClient, error) {
	return typesafe.NewClient(options)
}

func prepareLiveClassify(cfg *config.Config) (*liveclassify.Classifier, *liveclassify.DecisionStore, error) {
	if cfg == nil || cfg.Live.ControlPlane != config.LiveControlPlaneClassify {
		return nil, nil, nil
	}
	provider, err := cfg.Classify.ResolveProvider(cfg.Live.Classify.Provider)
	if err != nil {
		return nil, nil, fmt.Errorf("live classify provider: %w", err)
	}
	options := &classifyOptions{provider: cfg.Live.Classify.Provider}
	if provider.TimeoutSeconds < 0 {
		return nil, nil, errors.New("classify provider timeout_seconds must not be negative")
	}
	const maxTimeoutSeconds = int64(1<<63-1) / int64(time.Second)
	if int64(provider.TimeoutSeconds) > maxTimeoutSeconds {
		return nil, nil, errors.New("classify provider timeout_seconds is too large")
	}
	// The classifier and the optional switch resolver share the controller's
	// routing deadline. Leave enough budget for the resolver and a small handoff
	// margin even when the selected provider has a longer explicit timeout.
	timeout := time.Duration(provider.TimeoutSeconds) * time.Second
	if provider.TimeoutSeconds == config.DefaultTypeSafeTimeoutSeconds {
		timeout = liveClassifyDefaultTimeout
	}
	if timeout > liveClassifyMaxTimeout {
		log.Printf("[serve] live classify timeout %s exceeds routing budget; clamping to %s", timeout, liveClassifyMaxTimeout)
		timeout = liveClassifyMaxTimeout
	}
	options.timeout = timeout
	client, err := newTypeSafeClient(cfg, options, classifyDeps{newClient: newLiveClassifyClient})
	if err != nil {
		return nil, nil, fmt.Errorf("live classify provider: %w", err)
	}
	classifier := &liveclassify.Classifier{Client: client, Model: provider.Model}
	if !cfg.Live.Classify.LogDecisions {
		return classifier, nil, nil
	}
	diagnosticsDir := config.GetDiagnosticsDir()
	if err := os.MkdirAll(diagnosticsDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create live diagnostics directory: %w", err)
	}
	if err := os.Chmod(diagnosticsDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("secure live diagnostics directory: %w", err)
	}
	store, err := liveclassify.OpenDecisionStore(filepath.Join(diagnosticsDir, "live.db"))
	if err != nil {
		return nil, nil, err
	}
	return classifier, store, nil
}

type liveDecisionClassifier interface {
	Classify(context.Context, liveclassify.State) (liveclassify.Decision, error)
}

type liveDecisionLogger interface {
	Enqueue(liveclassify.DecisionRecord) bool
}

// serveLiveClassifyRouter is the low-latency live.Router. Every internal failure
// returns the original request to the session lane; only an evidence-backed
// Phase 1 action consumes it.
type serveLiveClassifyRouter struct {
	server     *serveServer
	live       *liveSession
	classifier liveDecisionClassifier
	decisions  liveDecisionLogger

	// resolver is deliberately a lazy factory. Shadow mode and non-switch labels
	// never construct an engine with mutating bindings.
	resolver func() func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error)
}

func (s *serveServer) newLiveClassifyRouter(record *liveSession) live.Router {
	router := &serveLiveClassifyRouter{server: s, live: record, classifier: s.liveClassifier, decisions: s.liveDecisionStore}
	router.resolver = func() func(context.Context, string, live.RouteRequest) (liveSwitchResolverOutcome, error) {
		return func(ctx context.Context, sessionID string, request live.RouteRequest) (liveSwitchResolverOutcome, error) {
			return s.resolveLiveSwitch(ctx, record, sessionID, request)
		}
	}
	return router
}

func (r *serveLiveClassifyRouter) Route(ctx context.Context, request live.RouteRequest) (result live.RouteResult, err error) {
	started := time.Now()
	var routeFailure string
	record := liveclassify.DecisionRecord{CreatedAt: started.UTC(), GatedLabel: liveclassify.IntentSteer, ActedLabel: liveclassify.IntentSteer}
	cfg := config.LiveClassifyConfig{}
	state := liveclassify.State{Message: request.Input, TranscriptDelta: request.TranscriptDelta}
	defer func() {
		record.Latency = time.Since(started)
		record.Error = routeFailure
		if cfg.LogDecisions && r != nil && r.decisions != nil {
			if !cfg.LogState {
				record.StateJSON = nil
			}
			r.decisions.Enqueue(record)
		}
	}()

	failOpen := func(category string, failure error) (live.RouteResult, error) {
		if failure != nil {
			routeFailure = category
		}
		return live.RouteResult{Input: request.Input}, nil
	}
	if r == nil || r.server == nil || r.live == nil {
		return failOpen(liveDecisionErrorRouterUnavailable, fmt.Errorf("live classify router is unavailable"))
	}
	r.live.touch()
	sessionID := r.live.boundSession()
	record.LiveID, record.BoundSession = r.live.id, sessionID
	cfg = r.server.liveConfig().Classify
	if sessionID == "" {
		return failOpen(liveDecisionErrorUnboundSession, fmt.Errorf("live classify call is not bound to a chat session"))
	}
	if len(strings.Fields(request.Input)) > liveClassifyMaxWords {
		record.ResolverOutcome = "skipped_length"
		if boundedState, marshalErr := liveclassify.MarshalState(state); marshalErr == nil {
			record.StateJSON = boundedState
		}
		return live.RouteResult{Input: request.Input}, nil
	}
	state = r.server.liveClassifyState(ctx, sessionID, request)
	boundedState, err := liveclassify.MarshalState(state)
	if err == nil {
		record.StateJSON = boundedState
	}
	if r.classifier == nil {
		return failOpen(liveDecisionErrorProvider, fmt.Errorf("live classify provider is unavailable"))
	}
	decision, err := r.classifier.Classify(ctx, state)
	if len(decision.BoundedStateJSON) > 0 {
		record.StateJSON = decision.BoundedStateJSON
	}
	record.Probabilities = decision.Probabilities
	if err != nil {
		return failOpen(liveClassifyErrorCategory(err), err)
	}
	gated := liveclassify.Gate(decision, liveclassify.GateConfig{
		Status: cfg.MinConfidence.Status, NewSession: cfg.MinConfidence.NewSession,
		SwitchSession: cfg.MinConfidence.SwitchSession, SteerNow: cfg.MinConfidence.SteerNow, Side: cfg.MinConfidence.Side,
	})
	record.GatedLabel = gated
	if cfg.Shadow || gated == liveclassify.IntentSteer {
		return live.RouteResult{Input: request.Input}, nil
	}

	switch gated {
	case liveclassify.IntentStatus:
		answer, err := r.server.liveClassifyStatus(ctx)
		if err != nil {
			return failOpen(liveDecisionErrorStatus, err)
		}
		record.ActedLabel = gated
		return live.RouteResult{Handled: true, Answer: answer}, nil
	case liveclassify.IntentNewSession:
		created, err := r.server.startLiveNewSession(ctx, r.live, tools.LiveNewSessionRequest{})
		if err != nil {
			return failOpen(liveDecisionErrorNewSession, fmt.Errorf("live classify new session: %w", err))
		}
		record.ActedLabel = gated
		answer := "Started a new conversation and moved the call to it."
		if created.SessionNumber > 0 {
			answer = fmt.Sprintf("Started a new conversation and moved the call to session #%d.", created.SessionNumber)
		}
		return live.RouteResult{Handled: true, Answer: answer}, nil
	case liveclassify.IntentSwitchSession:
		if r.resolver == nil {
			return failOpen(liveDecisionErrorResolver, fmt.Errorf("live switch resolver is unavailable"))
		}
		outcome, err := r.resolver()(ctx, sessionID, request)
		record.ResolverOutcome = string(outcome.Kind)
		if err != nil {
			return failOpen(liveDecisionErrorResolver, err)
		}
		switch outcome.Kind {
		case liveSwitchResolverSucceeded, liveSwitchResolverRefused, liveSwitchResolverAmbiguous:
			record.ActedLabel = gated
			return live.RouteResult{Handled: true, Answer: outcome.Answer}, nil
		default:
			return live.RouteResult{Input: request.Input}, nil
		}
	default:
		return live.RouteResult{Input: request.Input}, nil
	}
}

func liveClassifyErrorCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return liveDecisionErrorClassifyTimeout
	case errors.Is(err, liveclassify.ErrInvalidAnswer):
		return liveDecisionErrorClassifyInvalid
	default:
		return liveDecisionErrorProvider
	}
}

func (s *serveServer) liveClassifyState(ctx context.Context, sessionID string, request live.RouteRequest) liveclassify.State {
	state := liveclassify.State{Message: request.Input, TranscriptDelta: request.TranscriptDelta}
	if runs := s.ensureResponseRuns(); runs != nil {
		if run := runs.activeRun(sessionID); run != nil {
			state.ActiveRun = true
			run.mu.Lock()
			if index := run.currentToolGroup; index >= 0 && index < len(run.recoveryMessages) {
				tools := run.recoveryMessages[index].Tools
				for i := len(tools) - 1; i >= 0; i-- {
					if tools[i].Status == "running" || tools[i].EndedAt == 0 {
						state.ActiveTool = tools[i].Name
						break
					}
				}
			}
			run.mu.Unlock()
		}
	}
	if s.store != nil {
		if current, err := s.store.Get(ctx, sessionID); err == nil && current != nil {
			state.CurrentSessionTitle = current.PreferredShortTitle()
		}
		if entries, err := s.sessionDirectory(ctx, sessionDirectoryQuery{Limit: liveclassify.MaxRecentSessionTitles}); err == nil {
			for _, entry := range entries {
				if title := strings.TrimSpace(entry.Title); title != "" {
					state.RecentSessionTitles = append(state.RecentSessionTitles, title)
				}
			}
		}
	}
	return state.Bounded()
}

func (s *serveServer) liveClassifyStatus(ctx context.Context) (string, error) {
	entries, err := s.sessionDirectory(ctx, sessionDirectoryQuery{Limit: 10})
	if err != nil {
		return "", fmt.Errorf("live status sessions: %w", err)
	}
	var running, recent []string
	for _, entry := range entries {
		label := liveStatusSessionLabel(entry)
		if entry.Running {
			running = append(running, label)
		} else if len(recent) < 3 {
			recent = append(recent, label)
		}
	}
	var activeJobs, recentJobs []string
	if s.jobsV2 != nil {
		jobs, _, listErr := s.jobsV2.ListJobs(100, 0)
		runs, _, runsErr := s.jobsV2.ListRunSummaries("", 10, 0)
		if listErr != nil || runsErr != nil {
			if listErr != nil {
				log.Printf("[serve] live status jobs unavailable: %v", listErr)
			} else {
				log.Printf("[serve] live status job runs unavailable: %v", runsErr)
			}
		} else {
			names := make(map[string]string, len(jobs))
			for _, job := range jobs {
				names[job.ID] = job.Name
			}
			for _, run := range runs {
				name := strings.TrimSpace(names[run.JobID])
				if name == "" {
					name = "a scheduled job"
				}
				switch run.Status {
				case jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning:
					activeJobs = append(activeJobs, name)
				default:
					if len(recentJobs) < 2 {
						recentJobs = append(recentJobs, name+" "+string(run.Status))
					}
				}
			}
		}
	}
	parts := []string{}
	if len(running) > 0 || len(activeJobs) > 0 {
		active := append([]string{}, running...)
		active = append(active, activeJobs...)
		parts = append(parts, "Currently running: "+strings.Join(active, ", ")+".")
	} else {
		parts = append(parts, "Nothing is running right now.")
	}
	completed := append(recent, recentJobs...)
	if len(completed) > 0 {
		parts = append(parts, "Recent: "+strings.Join(completed[:min(3, len(completed))], ", ")+".")
	}
	return strings.Join(parts, " "), nil
}

func liveStatusSessionLabel(entry sessionDirectoryEntry) string {
	label := strings.TrimSpace(entry.Title)
	if label == "" {
		if entry.Number > 0 {
			return fmt.Sprintf("session #%d", entry.Number)
		}
		return "an untitled session"
	}
	if entry.Number > 0 {
		return fmt.Sprintf("session #%d %s", entry.Number, label)
	}
	return label
}
