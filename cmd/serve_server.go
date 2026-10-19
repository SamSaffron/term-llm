package cmd

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/live"
	liveclassify "github.com/samsaffron/term-llm/internal/live/classify"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mentions"
	"github.com/samsaffron/term-llm/internal/restart"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/share"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/widgets"
)

type serveServer struct {
	mcpRegistry              mcpRegistrySearcher
	reloadRunsUnregister     func()
	browserAuth              *browserPasskeyHandler
	reloadHTTPOnce           sync.Once
	reloadHTTP               *restart.HTTPTransport
	cfg                      serveServerConfig
	sessionMgr               *serveSessionManager
	jobsV2                   *jobsV2Manager
	cfgRef                   *config.Config
	providerCreds            llm.ProviderCredentialCache
	store                    session.Store
	sessionStore             runtime.SessionStoreOverrides
	memoryPaths              appmemory.Paths
	mediaPublisher           *serveMediaPublisher
	approvalDefault          tools.ApprovalMode
	sharePublisherFactory    func() (share.Publisher, error)
	shareMu                  sync.Mutex
	shareCacheKey            string
	shareCachedPublisher     share.Publisher
	shareCachedCapabilities  share.Capabilities
	shareCachedErr           error
	shareCacheUntil          time.Time
	shareBuildFlight         chan struct{}
	shareInvocation          chan struct{}
	projectsEnabled          bool
	bootstrapProjectID       string
	startupDir               string
	projectStatusMu          sync.Mutex
	projectStatuses          map[string]projectStatusCacheEntry
	server                   *http.Server
	listener                 net.Listener
	shutdownCh               chan struct{}
	shutdownOnce             sync.Once
	modelsMu                 sync.Mutex
	modelsProviders          map[string]core.Provider // keyed by provider name
	modelsCache              map[string]serveModelsCacheEntry
	responseToSession        sync.Map // response_id (string) → session_id (string)
	sessionToResponse        sync.Map // session_id (string) → latest response_id (string)
	branchNotes              sync.Map // child session_id → in-flight path-note preparation
	branchPathNoteFlights    sync.Map // source/idempotency key → shared path-note helper result
	restoringWebRuns         bool
	agentWakeMu              sync.Mutex
	agentWakeActive          map[string]bool
	agentWakeSignals         map[string]uint64
	agentWakeWG              sync.WaitGroup
	responseRunsOnce         sync.Once
	responseRuns             *responseRunManager
	agentOwner               *runtime.AgentHostOwner
	childRunsOnce            sync.Once
	childRuns                *childRunRegistry
	responseOwnerOnce        sync.Once
	responseLifecycleOnce    sync.Once
	responseLifecycleCancel  context.CancelFunc
	responseLifecycleWG      sync.WaitGroup
	responseOwnerInstanceID  string
	attentionDiagnostics     serveAttentionDiagnostics
	eventBrokerMu            sync.Mutex
	eventBroker              *serveEventBroker
	eventWatcherMu           sync.Mutex
	eventWatcherCancel       context.CancelFunc
	eventWatcherWG           sync.WaitGroup
	completionPushWake       chan struct{}
	completionPushWG         sync.WaitGroup
	transcriptIndexerOnce    sync.Once
	transcriptIndexer        session.TranscriptIndexer
	baseSystemPrompt         string
	skillsSetup              *skills.Setup
	skillsConfig             *config.SkillsConfig
	skillsCacheMu            sync.Mutex
	skillsByDir              map[string]serveSkillsCacheEntry
	mentionsCacheMu          sync.Mutex
	mentionsByRoot           map[string]*serveMentionCacheEntry
	mentionBuildFn           func(context.Context, string, mentions.BuildOptions) (*mentions.Snapshot, error)
	skillRunsMu              sync.Mutex
	skillRuns                map[string]*serveSkillRun
	skillRunsWG              sync.WaitGroup
	skillRunsStopping        bool
	skillRunRetention        time.Duration
	skillChildRunnerFactory  func(sessionID string, runtime *serveRuntime) (runpkg.ChildRunner, error)
	runtimeFactory           func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error)
	agentRuntimeFactory      func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error)
	titleProviderFactory     func(*config.Config) (core.Provider, error)
	autoTitleProviderFactory func(string) (core.Provider, error)

	// liveControlProviderFactory is the test seam for the session assistant's
	// provider; nil resolves it through the shared fast-provider conventions.
	liveControlProviderFactory func(providerKey string) (core.Provider, error)
	liveClassifier             liveDecisionClassifier
	liveDecisionStore          *liveclassify.DecisionStore

	autoTitleMu              sync.Mutex
	autoTitleFlights         map[string]struct{}
	autoTitleAttempts        map[string]serveAutoTitleAttempt
	autoTitleSlots           chan struct{}
	autoTitleCtx             context.Context
	autoTitleCancel          context.CancelFunc
	autoTitleWG              sync.WaitGroup
	autoTitleStopping        bool
	pathNotesProviderFactory func(providerName, model string) (core.Provider, error)
	widgetsMgr               *widgets.Manager
	extensions               *serveExtensions
	indexHTMLMu              sync.Mutex
	cachedIndexHTML          []byte
	worktreeRootOnce         sync.Once
	worktreeRoot             string
	worktreeRootErr          error
	fileChangeRepoCacheMu    sync.Mutex
	fileChangeRepoCache      map[string]sessionGitRepoCacheEntry
	fileTrackStoreFn         func() *filetrack.Store // test seam; nil → process-wide store from config
	worktreeRootFn           func() (string, error)  // test seam; nil → os.Getwd
	rootMutationAdmitted     func()                  // test seam; called while the exclusive root lease is held
	commitMu                 sync.Mutex
	commitRuns               map[string]*serveCommitRun
	commitOperations         map[string]*serveCommitOperation
	commitOperationsWG       sync.WaitGroup
	commitRunsWG             sync.WaitGroup
	commitStopping           bool
	shellsMu                 sync.Mutex
	shells                   *serveShellManager
	shellsClosed             bool
	liveMu                   sync.Mutex
	liveSessions             map[string]*liveSession
	liveByChat               map[string]string
	liveClosed               bool
	liveProviderInstance     live.Provider
	liveProviderFactory      func(config.LiveConfig) (live.Provider, error) // test seam; nil → live.NewProvider
}

// fileTrackStore returns the file-change history store, or nil when file
// tracking is disabled.
func (s *serveServer) fileTrackStore() *filetrack.Store {
	if s.fileTrackStoreFn != nil {
		return s.fileTrackStoreFn()
	}
	return fileTrackingStore(s.cfgRef)
}
