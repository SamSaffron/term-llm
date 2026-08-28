import { batch, computed, signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { AppConfig } from '../app/config';
import { APIClient, APIError, decodeSSE } from '../api/client';
import { endpoints, type Endpoints } from '../api/endpoints';
import {
  initialProjection,
  reduceResponse,
  ResponseProtocolError,
  type ResponseEvent,
  type ResponseProjection,
} from '../domain/response';
import { applyRuntimeToRequest, type ModelOption } from '../domain/runtime';
import { errorMessage } from '../domain/text';
import {
  DEFAULT_ATTACHMENT_POLICY,
  attachmentAccept,
  validateAttachmentFile,
  type AttachmentPolicy,
} from '../domain/attachments';
import { planSummary } from '../domain/plan';
import type { ChildRun } from '../domain/child-run';
import {
  reviewAnchorFingerprint,
  reviewCommentPayload,
  validateReviewBatch,
  validateReviewComment,
} from '../domain/review-policy';
import {
  convertServerMessages,
  mergeDurableProjection,
  sanitizeSession,
} from '../domain/transcript';
import {
  linesFromHunks,
  normalizeDiffScope,
  parseUnifiedPatch,
  sortDiffFiles,
} from '../domain/diff';
import type {
  ActiveRun,
  ApprovalPrompt,
  AskUserPrompt,
  Attachment,
  CurrentPlan,
  DiffComment,
  DiffFile,
  FilesystemObservation,
  OutputClaimDiagnostic,
  Goal,
  InteractionRecord,
  MCPServer,
  Message,
  Project,
  Session,
} from '../domain/types';
import {
  clearDraft,
  clearSessionDiffComments,
  migrateScopedStorage,
  persistDiffComment,
  persistAgentReadMarker,
  persistPendingIntent,
  readAgentReadMarkers,
  readDiffCommentQueue,
  readDrafts,
  readPendingIntents,
  removeDiffComment,
  removeSessionPendingIntents,
  saveDraft,
  type PendingIntentRegistry,
  type StorageKeys,
} from '../platform/storage';
import { sessionIDFromLocation, updateSessionRoute } from '../platform/routing';
import { blobChecksum, blobToDataURL, DraftBlobStore } from '../platform/draft-blobs';
import { hardRefreshAssets, syncTokenCookie } from '../platform/browser';
import {
  NotificationController,
  completionEventId,
  type NotificationState,
} from '../platform/notifications';
import { rebaseHubAssetURL } from '../app/config';
import { parseTabEvent, TabSync, type TabEventType } from '../platform/tab-sync';
import { StreamSupervisors, type StreamSupervisor } from './stream-supervisor';

export type Modal =
  | ''
  | 'settings'
  | 'rename'
  | 'ask-user'
  | 'approval'
  | 'mcp'
  | 'goal'
  | 'widgets'
  | 'branch'
  | 'branch-context'
  | 'project'
  | 'worktrees'
  | 'skills'
  | 'side';
export interface Toast {
  id: string;
  message: string;
  kind: 'info' | 'success' | 'error';
  leaving?: boolean;
}
export interface RuntimeOption extends ModelOption {
  [key: string]: unknown;
}
export interface SideQuestionState {
  sessionId: string;
  loading: boolean;
  running: boolean;
  draft: string;
  question: string;
  response: string;
  error: string;
  history: Array<{ question: string; response: string }>;
}
export interface DiffState {
  open: boolean;
  sessionId: string;
  scope: string;
  git: boolean;
  loading: boolean;
  files: DiffFile[];
  materializations: FilesystemObservation[];
  observations: FilesystemObservation[];
  claimDiagnostics: OutputClaimDiagnostic[];
  unavailableLineCountFiles: number;
  filter: string;
  comments: DiffComment[];
  historyComments: DiffComment[];
  error: string;
  maximized: boolean;
  width: number;
  selectedPath: string;
  followCurrentFile: boolean;
}
export interface PendingInterjection {
  id: string;
  sessionId: string;
  content: string;
  state: 'sending' | 'pending' | 'committed' | 'failed';
}
interface SendOptions {
  contentParts?: Record<string, unknown>[];
  inputText?: string;
  displayContent?: string;
  preserveComposer?: boolean;
  diffComments?: DiffComment[];
  onTransportStarted?: () => void;
  onTransportFailed?: (error: unknown) => void;
}
export interface HubAgent {
  id: string;
  name: string;
  target: string;
  active: boolean;
  attention: boolean;
}

const uuid = (): string => globalThis.crypto?.randomUUID?.() || Math.random().toString(36).slice(2);
const worktreeError = (error: unknown): string => {
  const code = error instanceof APIError ? error.type : '';
  const messages: Record<string, string> = {
    project_required: 'Choose a project before selecting a worktree.',
    worktrees_unavailable: 'Worktrees are unavailable for this project.',
    project_not_found: 'This project no longer exists.',
    project_archived: 'Restore this project before creating a worktree.',
    projects_disabled: 'Worktrees are unavailable in no-project mode.',
  };
  return messages[code] || (error instanceof Error ? error.message : 'Worktree action failed.');
};
const array = (value: unknown): Record<string, unknown>[] =>
  Array.isArray(value)
    ? value.filter(
        (entry): entry is Record<string, unknown> => Boolean(entry) && typeof entry === 'object',
      )
    : [];
const recordValue = (value: unknown): Record<string, unknown> | null =>
  Boolean(value) && typeof value === 'object' ? (value as Record<string, unknown>) : null;
const listFrom = (value: Record<string, unknown>, ...keys: string[]): Record<string, unknown>[] => {
  for (const key of keys) if (Array.isArray(value[key])) return array(value[key]);
  return [];
};
const compareSessionsByActivity = (left: Session, right: Session): number =>
  Number(right.pinned) - Number(left.pinned) ||
  (right.lastMessageAt || right.created) - (left.lastMessageAt || left.created) ||
  (right.number || 0) - (left.number || 0);
const normalizeMCPState = (value: unknown): { servers: MCPServer[]; enabled: string[] } => {
  const source = recordValue(value) || {};
  const enabled = Array.isArray(source.enabled)
    ? source.enabled
        .map(String)
        .map((name) => name.trim())
        .filter(Boolean)
    : [];
  const enabledSet = new Set(enabled);
  const servers = array(source.servers)
    .map((server): MCPServer | null => {
      const name = String(server.name || '').trim();
      if (!name) return null;
      const serverEnabled = enabledSet.has(name) || Boolean(server.enabled);
      const count = (field: string): number => {
        const parsed = Number(server[field]);
        return Number.isFinite(parsed) ? Math.max(0, parsed) : 0;
      };
      return {
        name,
        configured: server.configured !== false,
        enabled: serverEnabled,
        status: String(server.status || (serverEnabled ? 'ready' : 'stopped')).trim() || 'stopped',
        error: String(server.error || '').trim(),
        refreshWarning: String(server.refresh_warning || '').trim(),
        tools: count('tools'),
        active: count('active'),
        deferred: count('deferred'),
        loadingMode: String(server.loading_mode || '').trim(),
      };
    })
    .filter((server): server is MCPServer => server !== null);
  return {
    servers,
    enabled: enabled.length
      ? [...new Set(enabled)]
      : servers.filter((server) => server.enabled).map((server) => server.name),
  };
};
const askUserPrompt = (value: unknown, sessionId: string): AskUserPrompt | null => {
  const source = recordValue(value);
  if (!source || !Array.isArray(source.questions) || !source.questions.length) return null;
  const callId = String(source.callId || source.call_id || '');
  if (!callId) return null;
  return {
    sessionId: String(source.sessionId || source.session_id || sessionId),
    callId,
    questions: source.questions as AskUserPrompt['questions'],
  };
};
const approvalPrompt = (value: unknown, sessionId: string): ApprovalPrompt | null => {
  const source = recordValue(value);
  if (!source) return null;
  const id = String(source.id || source.approval_id || '');
  const options = Array.isArray(source.options)
    ? (source.options as ApprovalPrompt['options'])
    : [];
  if (!id || !options?.length) return null;
  return {
    ...source,
    sessionId: String(source.sessionId || source.session_id || sessionId),
    id,
    options,
    resumeAutoAvailable: Boolean(source.resumeAutoAvailable || source.resume_auto_available),
  } as ApprovalPrompt;
};

export interface StatusRequestMetadata {
  generation: number;
  requestedAt: number;
  selectedSessionId: string;
  selectionEpoch: number;
  showHidden: boolean;
  categories: string[];
}
interface StatusCoordinator {
  generation: number;
  refreshPromise: Promise<void> | null;
  lastAppliedGeneration: number;
  lastAppliedRequestedAt: number;
  lastAppliedReceivedAt: number;
  etag: string;
}

export class AppStore {
  readonly keys: StorageKeys;
  readonly api: APIClient;
  readonly endpoints: Endpoints;
  readonly notificationController: NotificationController;

  readonly sessions = signal<Session[]>([]);
  readonly projects = signal<Project[]>([]);
  readonly noProjectCursor = signal('');
  readonly projectsEnabled = signal(false);
  readonly worktreesEnabled = signal(false);
  readonly activeProjectId = signal('');
  readonly activeSessionId = signal('');
  readonly draftActive = signal(true);
  readonly providers = signal<RuntimeOption[]>([]);
  readonly models = signal<RuntimeOption[]>([]);
  readonly selectedProvider: Signal<string>;
  readonly selectedModel: Signal<string>;
  readonly selectedEffort: Signal<string>;
  readonly selectedReasoningMode: Signal<string>;
  readonly selectedAgent: Signal<string>;
  readonly token: Signal<string>;
  readonly prompt = signal('');
  readonly attachments = signal<Attachment[]>([]);
  // Acquired synchronously before send() performs attachment materialization.
  // Once a stream supervisor owns the operation, its run status takes over.
  readonly sendPending = signal(false);
  readonly attachmentPolicy = signal<AttachmentPolicy>(DEFAULT_ATTACHMENT_POLICY);
  readonly attachmentAccept = computed(() => attachmentAccept(this.attachmentPolicy.value));
  readonly runs = signal<Record<string, ResponseProjection>>({});
  readonly connected = signal(false);
  readonly authRequired = signal(false);
  readonly startup = signal('Loading your chat shell…');
  readonly startupDone = signal(false);
  readonly sidebarCollapsed: Signal<boolean>;
  readonly sidebarOpen = signal(false);
  readonly sidebarSearch = signal('');
  readonly searchResults = signal<Session[] | null>(null);
  readonly searchLoading = signal(false);
  readonly searchError = signal('');
  readonly showHidden: Signal<boolean>;
  readonly showWidgets: Signal<boolean>;
  readonly notifications = signal<NotificationState>({
    status: 'unsupported',
    busy: false,
    detail: 'Checking notification support…',
    verified: false,
  });
  readonly widgets = signal<
    Array<{ id: string; name: string; url: string; description?: string; state?: string }>
  >([]);
  readonly hubAgents = signal<HubAgent[]>([]);
  readonly modal = signal<Modal>('');
  readonly toasts = signal<Toast[]>([]);
  readonly currentPlan = signal<CurrentPlan | null>(null);
  readonly planOpen = signal(false);
  readonly planSeen = signal<string | null>(null);
  readonly planVisible = computed(() => this.planOpen.value && Boolean(this.currentPlan.value));
  readonly askUser = signal<AskUserPrompt | null>(null);
  readonly approval = signal<ApprovalPrompt | null>(null);
  readonly interactions = signal<Record<string, InteractionRecord>>({});
  readonly interactionOrder = signal<string[]>([]);
  readonly childRuns = signal<ChildRun[]>([]);
  readonly agentReadMarkers: Signal<Record<string, number>>;
  readonly sideQuestion = signal<SideQuestionState>({
    sessionId: '',
    loading: false,
    running: false,
    draft: '',
    question: '',
    response: '',
    error: '',
    history: [],
  });
  readonly interjections = signal<PendingInterjection[]>([]);
  readonly diff = signal<DiffState>({
    open: false,
    sessionId: '',
    scope: 'last_turn',
    git: false,
    loading: false,
    files: [],
    materializations: [],
    observations: [],
    claimDiagnostics: [],
    unavailableLineCountFiles: 0,
    filter: '',
    comments: [],
    historyComments: [],
    error: '',
    maximized: false,
    width: 420,
    selectedPath: '',
    followCurrentFile: true,
  });
  readonly goal = signal<Goal | null>(null);
  readonly mcp = signal<{
    servers: MCPServer[];
    enabled: string[];
    loading: boolean;
    pending: string;
    error: string;
  }>({ servers: [], enabled: [], loading: false, pending: '', error: '' });
  readonly worktrees = signal<Record<string, unknown>[]>([]);
  readonly worktreeError = signal('');
  readonly selectedDraftWorktree = signal('');
  readonly skills = signal<Record<string, unknown>[]>([]);
  readonly branchTree = signal<Record<string, unknown> | null>(null);
  readonly branchPathCount = signal(0);
  readonly branchTarget = signal('');
  readonly lightbox = signal<{
    src: string;
    type: 'image' | 'video';
    ownsObjectURL?: boolean;
    items?: Array<{
      key: string;
      src: string;
      type: 'image' | 'video';
      name?: string;
      ownsObjectURL?: boolean;
    }>;
    index?: number;
  } | null>(null);
  readonly renameTarget = signal<Session | null>(null);
  readonly projectTarget = signal<Session | null>(null);
  readonly networkState = signal<'unknown' | 'online' | 'offline' | 'retrying'>('unknown');
  readonly diagnostics = signal({
    staleStatusResults: 0,
    staleStreamCallbacks: 0,
    supervisorRetries: 0,
    supervisorRecoveries: 0,
    streamWatchdogTimeouts: 0,
    queueValidationFailures: 0,
    interactionReconciliations: 0,
    storageFailures: 0,
  });
  readonly fileChangeRevision = signal(0);
  readonly currentActivityFile = signal('');
  readonly pendingIntents: Signal<PendingIntentRegistry>;

  readonly activeSession: ReadonlySignal<Session | null>;
  readonly activeProjection: ReadonlySignal<ResponseProjection | null>;
  readonly visibleMessages: ReadonlySignal<Message[]>;
  readonly streaming: ReadonlySignal<boolean>;
  readonly sendBlocked: ReadonlySignal<boolean>;

  private readonly streamSupervisors = new StreamSupervisors();
  private readonly draftBlobs = new DraftBlobStore();
  private readonly interactionSubmissions = new Map<string, Promise<void>>();
  // A stop is user-visible immediately. Keep its response ID here while the
  // server finishes cancellation so status polling cannot resurrect the run.
  private readonly locallyStoppedResponses = new Set<string>();
  // A response with a confirmed durable handoff must never be projected again.
  // Late snapshot/replay requests may still finish after transcript reconciliation.
  private readonly retiredResponses = new Set<string>();
  private readonly skillRunAborts = new Map<string, AbortController>();
  private readonly skillRunCursors = new Map<
    string,
    { sessionId: string; eventsURL: string; sequence: number }
  >();
  private searchAbort: AbortController | null = null;
  private searchTimer = 0;
  private sideQuestionAbort: AbortController | null = null;
  private sideQuestionEpoch = 0;
  private modelAbort: AbortController | null = null;
  private statusTimer = 0;
  private lastSidebarRefreshAt = 0;
  private unknownActiveSessionIDs = new Set<string>();
  private readonly statusCoordinator: StatusCoordinator = {
    generation: 0,
    refreshPromise: null,
    lastAppliedGeneration: 0,
    lastAppliedRequestedAt: 0,
    lastAppliedReceivedAt: 0,
    etag: '',
  };
  private sidebarGeneration = 0;
  private lastAppliedSidebarGeneration = 0;
  private sidebarRefreshPromise: Promise<void> | null = null;
  private sessionSyncChannel: BroadcastChannel | null = null;
  private readonly tabSync = new TabSync();
  private readonly peerRevisions = new Map<string, number>();
  private peerSyncTimer = 0;
  private pendingPeerSync = false;
  private titleRefreshTimers = new Map<string, number[]>();
  private lifecycleInstalled = false;
  private readonly lifecycleAbort = new AbortController();
  private disposed = false;
  private currentDraftRev = 0;
  private readonly childRunETags = new Map<string, string>();
  private readonly handledCompletionEvents = new Set<string>();
  private newDraftID = '';
  private readonly attachmentGenerations = new Map<string, number>();
  private readonly ownedTimers = new Set<number>();
  private selectionEpoch = 0;
  private modelEpoch = 0;
  private skillEpoch = 0;
  private diffLoadEpoch = 0;
  private readonly diffExpandEpoch = new Map<string, number>();
  private interjectionRevision = 0;
  private recoverPromise: Promise<void> | null = null;
  private hubAgentLastFetch = 0;
  private hubAgentFetch: Promise<void> | null = null;

  constructor(
    readonly config: AppConfig,
    readonly storage: Storage = localStorage,
  ) {
    this.keys = migrateScopedStorage(storage, config.hub);
    const storedDraftID = storage.getItem(this.keys.draftSessionActive) || '';
    this.newDraftID = storedDraftID.startsWith('draft:') ? storedDraftID : `draft:${uuid()}`;
    this.token = signal(storage.getItem(this.keys.token) || '');
    this.selectedProvider = signal(storage.getItem(this.keys.selectedProvider) || '');
    this.selectedModel = signal(storage.getItem(this.keys.selectedModel) || '');
    this.selectedEffort = signal(storage.getItem(this.keys.selectedEffort) || '');
    this.selectedReasoningMode = signal(
      storage.getItem(this.keys.selectedReasoningMode) || 'standard',
    );
    const agent = storage.getItem(this.keys.selectedAgent) || '';
    this.selectedAgent = signal(config.agentNames.includes(agent) ? agent : '');
    this.sidebarCollapsed = signal(storage.getItem(this.keys.sidebarCollapsed) === '1');
    this.showHidden = signal(storage.getItem(this.keys.showHiddenSessions) === '1');
    this.showWidgets = signal(storage.getItem(this.keys.showWidgetsSidebar) !== '0');
    // The legacy boolean was optimistic and is never authoritative. Enrollment
    // is reconstructed from browser and server state below.
    storage.removeItem(this.keys.notificationsEnabled);
    this.pendingIntents = signal(readPendingIntents(storage, this.keys.pendingIntents));
    this.agentReadMarkers = signal(readAgentReadMarkers(storage, this.keys.agentReadMarkers));
    this.diff.value = {
      ...this.diff.value,
      width: Math.max(320, Number(storage.getItem(this.keys.diffSidebarWidth)) || 420),
      comments: readDiffCommentQueue(storage, this.keys.diffCommentQueue),
    };
    const referencedBlobIDs = new Set(
      readDrafts(storage, this.keys.draftMessages).flatMap((draft) =>
        (draft.attachments || []).flatMap((attachment) =>
          attachment.blobRef ? [attachment.blobRef] : [],
        ),
      ),
    );
    void this.draftBlobs.deleteOrphans(referencedBlobIDs).catch(() => {
      if (!this.disposed) this.bumpDiagnostic('storageFailures');
    });
    syncTokenCookie(config.prefix, this.token.value);
    this.api = new APIClient(config, {
      getToken: () => this.token.value,
      onAuthRequired: () => {
        this.authRequired.value = true;
        this.modal.value = 'settings';
      },
      onNetworkState: (state) => {
        this.networkState.value = state;
        this.connected.value = state === 'online';
      },
      onVersionMismatch: () => {
        void this.hardRefresh();
      },
    });
    this.endpoints = endpoints(this.api);
    this.notificationController = new NotificationController(
      config,
      this.endpoints,
      storage,
      this.keys.notificationSubscriptionID,
    );
    this.notificationController.setListener((state) => {
      this.notifications.value = state;
    });
    this.activeSession = computed(
      () =>
        this.sessions.value.find((session) => session.id === this.activeSessionId.value) || null,
    );
    this.activeProjection = computed(() => {
      const session = this.activeSession.value;
      return session ? this.runs.value[session.id] || null : null;
    });
    this.visibleMessages = computed(() => {
      const session = this.activeSession.value;
      if (!session) return [];
      const messages = mergeDurableProjection(
        session.messages,
        this.runs.value[session.id]?.messages || [],
      );
      const clientIDs = new Set(messages.map((message) => message.clientMessageId).filter(Boolean));
      const pending = (this.pendingIntents.value[session.id] || [])
        .filter((intent) => !clientIDs.has(intent.clientMessageId))
        .map((intent) => ({
          id: intent.id,
          role: 'user' as const,
          content: intent.content,
          created: intent.created,
          clientMessageId: intent.clientMessageId,
          attachments: intent.attachments,
          pending: true,
          ...(intent.state === 'checking' ? { interruptState: 'checking_send' } : {}),
        }));
      return [...messages, ...pending];
    });
    this.streaming = computed(() =>
      ['connecting', 'checking', 'streaming', 'cancelling'].includes(
        this.activeProjection.value?.run.status || '',
      ),
    );
    this.sendBlocked = computed(
      () =>
        this.sendPending.value ||
        Boolean(
          this.pendingIntents.value[this.activeSessionId.value]?.some(
            (intent) => intent.state === 'checking',
          ),
        ),
    );
  }

  async bootstrap(): Promise<void> {
    this.installLifecycle();
    this.startup.value = 'Connecting to term-llm…';
    try {
      const capabilities = await this.endpoints.capabilities().catch(() => ({}));
      this.applyCapabilities(capabilities);
      const [providers, sidebar] = await Promise.all([
        this.endpoints.providers(),
        this.projectsEnabled.value
          ? this.endpoints.sidebar(this.showHidden.value)
          : this.endpoints.sessions(
              `limit=30&include_archived=${this.showHidden.value ? '1' : '0'}`,
            ),
      ]);
      this.applyProviders(providers);
      this.applySidebar(sidebar);
      await this.loadModels().catch(() => undefined);
      const routed = sessionIDFromLocation(this.config.prefix);
      const forceNew = new URLSearchParams(location.search).get('new') === '1';
      const restoreDraft = !routed && Boolean(this.storage.getItem(this.keys.draftSessionActive));
      const preferred =
        forceNew || restoreDraft
          ? ''
          : routed || this.storage.getItem(this.keys.activeSession) || '';
      const session =
        forceNew || restoreDraft
          ? null
          : this.sessions.value.find(
              (entry) => entry.id === preferred || String(entry.number || '') === preferred,
            ) ||
            this.sessions.value[0] ||
            null;
      if (session) await this.selectSession(session, true);
      else this.newChat(true, this.storage.getItem(this.keys.lastProject) || '', false);
      this.connected.value = true;
      this.networkState.value = 'online';
      this.startupDone.value = true;
      this.startStatusPoll();
      if (this.pendingPeerSync) {
        this.pendingPeerSync = false;
        this.queuePeerSessionChange();
      }
      void this.refreshHubAgents();
      if (!this.widgets.value.length) void this.loadWidgetStatus();
    } catch (error) {
      this.startup.value = error instanceof Error ? error.message : 'Could not load the chat UI.';
      this.connected.value = false;
      if (error instanceof APIError && [401, 403].includes(error.status)) {
        this.authRequired.value = true;
        this.modal.value = 'settings';
      }
    }
  }

  private installLifecycle(): void {
    if (this.lifecycleInstalled) return;
    this.lifecycleInstalled = true;
    this.notificationController.installLifecycle();
    addEventListener(
      'popstate',
      () => {
        const slug = sessionIDFromLocation(this.config.prefix);
        const session = this.sessions.value.find(
          (entry) => entry.id === slug || String(entry.number || '') === slug,
        );
        if (session) void this.selectSession(session, true);
        else if (!slug) this.newChat(true);
        else void this.resolveAndSelectSession(slug, true);
      },
      { signal: this.lifecycleAbort.signal },
    );
    addEventListener('online', () => void this.reconcile('online', { authoritative: true }), {
      signal: this.lifecycleAbort.signal,
    });
    addEventListener(
      'offline',
      () => {
        this.networkState.value = 'offline';
        this.connected.value = false;
      },
      { signal: this.lifecycleAbort.signal },
    );
    addEventListener(
      'term-llm:transport-fallback',
      () => void this.reconcile('transport-fallback', { authoritative: true }),
      { signal: this.lifecycleAbort.signal },
    );
    addEventListener(
      'pageshow',
      (event) => {
        this.ensureSessionSyncChannel();
        if (this.startupDone.value || (event as PageTransitionEvent).persisted)
          void this.reconcile('pageshow', { authoritative: true });
      },
      { signal: this.lifecycleAbort.signal },
    );
    addEventListener(
      'focus',
      () => {
        void this.reconcile('focus', { authoritative: true });
        void this.refreshHubAgents();
      },
      { signal: this.lifecycleAbort.signal },
    );
    addEventListener('beforeunload', () => this.persistCurrentDraft(), {
      signal: this.lifecycleAbort.signal,
    });
    addEventListener(
      'pagehide',
      (event) => {
        if ((event as PageTransitionEvent).persisted) return;
        window.clearTimeout(this.peerSyncTimer);
        this.sessionSyncChannel?.close();
        this.sessionSyncChannel = null;
      },
      { signal: this.lifecycleAbort.signal },
    );
    document.addEventListener(
      'visibilitychange',
      () => {
        if (document.visibilityState === 'visible') {
          void this.reconcile('visibility', { authoritative: true });
          void this.refreshHubAgents();
        }
      },
      { signal: this.lifecycleAbort.signal },
    );
    this.ensureSessionSyncChannel();
    addEventListener(
      'storage',
      (rawEvent) => {
        const event = rawEvent as StorageEvent;
        if (
          event.key === this.keys.pendingIntents ||
          event.key?.startsWith(`${this.keys.pendingIntents}:`)
        )
          this.pendingIntents.value = readPendingIntents(this.storage, this.keys.pendingIntents);
        if (event.key?.startsWith(`${this.keys.draftMessages}:`))
          this.reconcileDraftStorage(this.draftStorageID());
        if (event.key?.startsWith(`${this.keys.diffCommentQueue}:`))
          this.diff.value = {
            ...this.diff.peek(),
            comments: readDiffCommentQueue(this.storage, this.keys.diffCommentQueue),
          };
        if (event.key?.startsWith(`${this.keys.agentReadMarkers}:`)) {
          this.agentReadMarkers.value = readAgentReadMarkers(
            this.storage,
            this.keys.agentReadMarkers,
          );
          void this.loadChildRuns();
        }
      },
      { signal: this.lifecycleAbort.signal },
    );
  }

  private ensureSessionSyncChannel(): void {
    if (this.sessionSyncChannel || typeof BroadcastChannel !== 'function') return;
    const scope = this.config.hub?.nodeId || this.config.prefix;
    try {
      this.sessionSyncChannel = new BroadcastChannel(`term-llm:sessions:${scope}`);
    } catch {
      this.diagnostics.value = {
        ...this.diagnostics.peek(),
        storageFailures: this.diagnostics.peek().storageFailures + 1,
      };
      return; // The always-installed storage listener remains the fallback.
    }
    this.sessionSyncChannel.addEventListener('message', (message) => {
      const parsed = parseTabEvent(message.data);
      const event = this.tabSync.accept(message.data);
      // Unknown protocol versions are a full invalidation. Malformed and
      // duplicate/self events are ignored.
      if (!event) {
        if (
          parsed === null &&
          message.data &&
          typeof message.data === 'object' &&
          'v' in (message.data as object)
        )
          this.queuePeerSessionChange();
        return;
      }
      if (event !== 'legacy') {
        if (event.revision !== undefined && event.sessionId) {
          const previous = this.peerRevisions.get(event.sessionId) || 0;
          if (previous && event.revision > previous + 1) this.pendingPeerSync = true;
          this.peerRevisions.set(event.sessionId, Math.max(previous, event.revision));
        }
        if (event.type === 'draft-changed' && event.sessionId === this.draftStorageID())
          this.reconcileDraftStorage(event.sessionId);
        if (event.type === 'review-comment-changed')
          this.diff.value = {
            ...this.diff.peek(),
            comments: readDiffCommentQueue(this.storage, this.keys.diffCommentQueue),
          };
      }
      if (!this.startupDone.peek()) {
        this.pendingPeerSync = true;
        return;
      }
      this.queuePeerSessionChange();
    });
  }

  private recover(): Promise<void> {
    if (this.recoverPromise) return this.recoverPromise;
    const request = this.reconcile('recovery', { authoritative: true });
    const tracked = request.finally(() => {
      if (this.recoverPromise === tracked) this.recoverPromise = null;
    });
    this.recoverPromise = tracked;
    return tracked;
  }

  private async reconcile(reason: string, options: { authoritative: boolean }): Promise<void> {
    if (navigator.onLine === false) {
      this.networkState.value = 'offline';
      return;
    }
    if (reason === 'focus') {
      await this.refreshStatus(options.authoritative).catch(() => undefined);
      await this.refreshSidebar(false).catch(() => undefined);
    } else {
      if (['peer', 'pageshow', 'visibility'].includes(reason))
        await this.refreshSidebar(false).catch(() => undefined);
      await this.refreshStatus(options.authoritative).catch(() => undefined);
    }
    if (options.authoritative) await this.performRecovery();
  }

  private async performRecovery(): Promise<void> {
    const activeSessionId = this.activeSessionId.peek();
    if (activeSessionId) {
      const interjectionRevision = this.interjectionRevision;
      void this.endpoints
        .sessionState(activeSessionId)
        .then((state) => {
          if (this.activeSessionId.peek() === activeSessionId)
            this.reconcilePendingInterjections(activeSessionId, state, interjectionRevision);
        })
        .catch(() => undefined);
    }
    const active = this.sessions.value.filter(
      (session) =>
        this.runs.value[session.id] &&
        ['connecting', 'streaming'].includes(this.runs.value[session.id].run.status),
    );
    for (const session of active) {
      const run = this.runs.value[session.id].run;
      if (run.responseId && !run.responseId.startsWith('pending_')) {
        const owner = this.streamSupervisors.current(session.id);
        if (!owner || owner.responseId !== run.responseId) {
          const adopted = this.streamSupervisors.begin(
            session.id,
            run.responseId,
            run.lastSequence,
          );
          void this.recoverSupervisor(adopted);
        } else void this.recoverSupervisor(owner);
      }
    }
  }

  async loadChildRuns(sessionId = this.activeSessionId.peek()): Promise<void> {
    if (!sessionId) {
      this.childRuns.value = [];
      return;
    }
    try {
      const data = await this.endpoints.sessionChildren(
        sessionId,
        this.childRunETags.get(sessionId) || '',
      );
      if (this.activeSessionId.peek() !== sessionId) return;
      const etag = String(data.__etag || '');
      if (etag) this.childRunETags.set(sessionId, etag);
      if (data.__notModified) return;
      this.childRuns.value = listFrom(data, 'children', 'items').map((entry) => {
        const childSessionId = String(entry.session_id || entry.id || '');
        const revision = Number(entry.revision) || 0;
        const state = String(entry.state || 'active');
        const unreadTerminal =
          ['complete', 'error', 'failed'].includes(state.toLowerCase()) &&
          revision > (this.agentReadMarkers.peek()[childSessionId] || 0);
        return {
          sessionId: childSessionId,
          parentSessionId: String(entry.parent_session_id || sessionId),
          parentSpawnItemId: Number(entry.parent_spawn_item_id) || undefined,
          parentSpawnCallId: String(entry.parent_spawn_call_id || '') || undefined,
          title: String(entry.title || entry.agent || 'Agent run'),
          agent: String(entry.agent || '') || undefined,
          taskSummary: String(entry.task_summary || '') || undefined,
          state,
          attention: Boolean(entry.attention) || unreadTerminal,
          responseId: String(entry.response_id || '') || undefined,
          runEpoch: Number(entry.run_epoch) || undefined,
          revision,
          startedAt: Number(entry.started_at) || undefined,
          endedAt: Number(entry.ended_at) || undefined,
          approximateTimes: Boolean(entry.approximate_times),
        };
      });
    } catch {
      // Child runs are additive; status reconciliation will retry.
    }
  }

  private applyCapabilities(data: Record<string, unknown>): void {
    const projects =
      data.projects && typeof data.projects === 'object'
        ? (data.projects as Record<string, unknown>)
        : {};
    const worktrees =
      data.worktrees && typeof data.worktrees === 'object'
        ? (data.worktrees as Record<string, unknown>)
        : {};
    this.projectsEnabled.value = projects.enabled === true;
    this.worktreesEnabled.value =
      worktrees.enabled === true || (worktrees.enabled === undefined && this.config.worktrees);
    const attachments = recordValue(data.attachments);
    if (attachments) {
      const maxCount = Number(attachments.max_count);
      const maxBytes = Number(attachments.max_bytes);
      this.attachmentPolicy.value = {
        maxCount: maxCount > 0 ? maxCount : DEFAULT_ATTACHMENT_POLICY.maxCount,
        maxBytes: maxBytes > 0 ? maxBytes : DEFAULT_ATTACHMENT_POLICY.maxBytes,
        mimeTypes: Array.isArray(attachments.mime_types)
          ? attachments.mime_types.map(String)
          : DEFAULT_ATTACHMENT_POLICY.mimeTypes,
        extensions: Array.isArray(attachments.extensions)
          ? attachments.extensions.map((entry) => String(entry).toLowerCase())
          : DEFAULT_ATTACHMENT_POLICY.extensions,
      };
    }
    this.applyWidgetStatus(data.widget_status || data.widgets);
  }

  private applyWidgetStatus(value: unknown): void {
    const source =
      value && typeof value === 'object' && !Array.isArray(value)
        ? (value as Record<string, unknown>).widgets
        : value;
    this.widgets.value = array(source)
      .map((entry) => {
        const mount = String(entry.mount || entry.id || '').replace(/^\/+|\/+$/g, '');
        return {
          id: String(entry.id || mount),
          name: String(entry.title || entry.name || mount || 'Widget'),
          description: String(entry.description || ''),
          state: String(entry.state || 'stopped'),
          url: mount
            ? `${this.config.prefix}/widgets/${encodeURIComponent(mount)}/`
            : String(entry.url || ''),
        };
      })
      .filter((entry) => entry.url);
  }

  async loadWidgetStatus(): Promise<void> {
    try {
      this.applyWidgetStatus(await this.endpoints.widgetStatus());
    } catch {
      /* Widgets are optional. */
    }
  }

  private applyProviders(data: Record<string, unknown>): void {
    const values = listFrom(data, 'data', 'providers', 'items')
      .map((entry) => ({
        ...entry,
        id: String(entry.id || entry.name || ''),
        name: String(entry.display_name || entry.name || entry.id || ''),
        models: Array.isArray(entry.models) ? entry.models : [],
      }))
      .filter((entry) => entry.id);
    this.providers.value = values;
    if (
      this.selectedProvider.value &&
      !values.some((provider) => provider.id === this.selectedProvider.value)
    ) {
      this.selectedProvider.value = '';
      this.storage.removeItem(this.keys.selectedProvider);
    }
  }

  private sessionFrom(value: Record<string, unknown>): Session {
    return sanitizeSession(value, { rebaseAssetURL: (url) => rebaseHubAssetURL(this.config, url) });
  }

  private mergeSession(
    existing: Session | undefined,
    incoming: Session,
    replaceMessages = false,
    preserveLiveState = false,
  ): Session {
    if (!existing) return incoming;
    return {
      ...existing,
      ...incoming,
      messages: replaceMessages || incoming.messages.length ? incoming.messages : existing.messages,
      lastResponseId: incoming.lastResponseId || existing.lastResponseId,
      activeResponseId: preserveLiveState
        ? incoming.activeResponseId || existing.activeResponseId
        : incoming.activeResponseId,
      activeRun: preserveLiveState
        ? (incoming.activeRun ?? existing.activeRun)
        : incoming.activeRun,
      usage: incoming.usage || existing.usage,
      goal: incoming.goal ?? existing.goal,
      fileChangeSummary: incoming.fileChangeSummary || existing.fileChangeSummary,
    };
  }

  private applySidebar(data: Record<string, unknown>): void {
    const direct = listFrom(data, 'data', 'sessions', 'items').map((entry) =>
      this.sessionFrom(entry),
    );
    const groups = listFrom(data, 'groups');
    const projects: Project[] = [];
    const ungrouped: Session[] = [...direct];
    // Flat listings (projects disabled) carry the cursor at the top level;
    // project sidebars carry it on their "no project" group below.
    this.noProjectCursor.value = String(data.next_cursor || '');
    for (const group of groups) {
      const sessions = listFrom(group, 'sessions', 'items');
      const projectSource =
        group.project && typeof group.project === 'object'
          ? (group.project as Record<string, unknown>)
          : null;
      if (!projectSource || group.no_project) {
        ungrouped.push(...sessions.map((entry) => this.sessionFrom(entry)));
        this.noProjectCursor.value = String(group.next_cursor || '');
        continue;
      }
      const project: Project = {
        id: String(projectSource.id || ''),
        name: String(projectSource.name || projectSource.title || 'Project'),
        path: String(projectSource.canonical_dir || projectSource.path || ''),
        archived: Boolean(projectSource.archived_at || projectSource.archived),
        available: projectSource.available !== false,
        unavailableReason: String(projectSource.unavailable_reason || ''),
        git: projectSource.git === true,
        sessions: sessions.map((entry) =>
          this.sessionFrom({
            ...entry,
            project_id: projectSource.id,
            project_name: projectSource.name,
            project_unavailable: projectSource.available === false,
            project_unavailable_reason: projectSource.unavailable_reason,
          }),
        ),
        sessionCount: Number(group.session_count) || sessions.length,
        next_cursor: String(group.next_cursor || ''),
        has_more: Boolean(group.next_cursor),
      };
      projects.push(project);
    }
    const incoming = [...ungrouped, ...projects.flatMap((project) => project.sessions || [])];
    const existing = new Map(this.sessions.peek().map((session) => [session.id, session]));
    const merged = new Map(
      incoming.map((session) => [
        session.id,
        this.mergeSession(existing.get(session.id), session, false, true),
      ]),
    );
    for (const [id, session] of existing)
      if (!merged.has(id) && (this.runs.peek()[id] || id.startsWith('draft_')))
        merged.set(id, session);
    this.sessions.value = [...merged.values()].sort(compareSessionsByActivity);
    this.projects.value = projects.map((project) => ({
      ...project,
      sessions: project.sessions?.map((summary) => merged.get(summary.id) || summary),
    }));
    this.lastSidebarRefreshAt = Date.now();
  }

  async loadModels(provider = this.selectedProvider.value): Promise<void> {
    const epoch = ++this.modelEpoch;
    this.modelAbort?.abort();
    const controller = new AbortController();
    this.modelAbort = controller;
    let data: Record<string, unknown>;
    try {
      data = await this.endpoints.models(provider, controller.signal);
    } catch (error) {
      if (controller.signal.aborted || epoch !== this.modelEpoch) return;
      throw error;
    }
    if (controller.signal.aborted || epoch !== this.modelEpoch) return;
    this.models.value = listFrom(data, 'data', 'models', 'items')
      .map((entry) => ({
        ...entry,
        id: String(entry.id || entry.name || ''),
        name: String(entry.display_name || entry.name || entry.id || ''),
        provider: String(entry.provider || provider || ''),
        efforts: Array.isArray(entry.reasoning_efforts)
          ? entry.reasoning_efforts.map(String)
          : Array.isArray(entry.efforts)
            ? entry.efforts.map(String)
            : undefined,
        default_effort: String(entry.default_reasoning_effort || entry.default_effort || ''),
      }))
      .filter((entry) => entry.id) as RuntimeOption[];
    if (provider !== this.selectedProvider.peek()) return;
    if (
      this.selectedModel.value &&
      !this.models.value.some((model) => model.id === this.selectedModel.value)
    ) {
      const matching = this.models.value.find(
        (model) =>
          model.id ===
          this.selectedModel.value.replace(/[-_](?:none|minimal|low|medium|high|xhigh|max)$/i, ''),
      );
      if (matching) this.setPreference('model', matching.id, false);
    }
  }

  async refreshSidebar(authoritative = true): Promise<void> {
    if (!authoritative && this.sidebarRefreshPromise) return this.sidebarRefreshPromise;
    const generation = authoritative ? ++this.sidebarGeneration : this.sidebarGeneration || 1;
    if (!this.sidebarGeneration) this.sidebarGeneration = generation;
    if (authoritative && this.sidebarRefreshPromise)
      await this.sidebarRefreshPromise.catch(() => undefined);
    const showHidden = this.showHidden.peek();
    const request = (async () => {
      const data = this.projectsEnabled.value
        ? await this.endpoints.sidebar(showHidden)
        : await this.endpoints.sessions(`limit=30&include_archived=${showHidden ? '1' : '0'}`);
      if (
        this.disposed ||
        generation !== this.sidebarGeneration ||
        showHidden !== this.showHidden.peek() ||
        generation < this.lastAppliedSidebarGeneration
      )
        return;
      this.applySidebar(data);
      this.lastAppliedSidebarGeneration = generation;
    })();
    const tracked = request.finally(() => {
      if (this.sidebarRefreshPromise === tracked) this.sidebarRefreshPromise = null;
    });
    this.sidebarRefreshPromise = tracked;
    return tracked;
  }

  private publishSessionChange(
    type: TabEventType = 'session-upserted',
    sessionId = this.activeSessionId.peek(),
    responseId = this.runs.peek()[sessionId]?.run.responseId || '',
    revision = this.sessions.peek().find((entry) => entry.id === sessionId)?.transcriptRev,
    operationId?: string,
  ): void {
    this.sessionSyncChannel?.postMessage(
      this.tabSync.create(
        type,
        {
          ...(sessionId ? { sessionId } : {}),
          ...(responseId ? { responseId } : {}),
          ...(revision !== undefined ? { revision } : {}),
        },
        operationId,
      ),
    );
  }

  private queuePeerSessionChange(): void {
    window.clearTimeout(this.peerSyncTimer);
    this.peerSyncTimer = window.setTimeout(() => void this.reconcilePeerSessionChange(), 150);
  }

  private async reconcilePeerSessionChange(): Promise<void> {
    await this.reconcile('peer', { authoritative: true }).catch(() => undefined);
    const sessionId = this.activeSessionId.peek();
    if (!sessionId) return;
    const interjectionRevision = this.interjectionRevision;
    const state = await this.endpoints.sessionState(sessionId).catch(() => null);
    if (state && this.activeSessionId.peek() === sessionId)
      this.reconcilePendingInterjections(sessionId, state, interjectionRevision);
  }

  async selectSession(session: Session, replace = false): Promise<void> {
    this.persistCurrentDraft();
    this.releaseAttachmentResources(this.attachments.peek(), false);
    this.resetSideQuestion();
    const epoch = ++this.selectionEpoch;
    batch(() => {
      this.sidebarOpen.value = false;
      this.activeSessionId.value = session.id;
      this.activeProjectId.value = session.projectId || '';
      this.draftActive.value = false;
      this.currentPlan.value = null;
      this.planOpen.value = false;
      this.planSeen.value = null;
      this.askUser.value = null;
      this.approval.value = null;
      this.branchTree.value = null;
      this.branchPathCount.value = 0;
      if (this.diff.value.sessionId !== session.id)
        this.diff.value = {
          ...this.diff.value,
          sessionId: session.id,
          git: Boolean(session.fileChangeSummary?.git),
          files: [],
          error: '',
          comments: this.diff.value.comments.filter(
            (comment) => !comment.sessionId || comment.sessionId === session.id,
          ),
        };
    });
    this.storage.setItem(this.keys.activeSession, session.id);
    this.storage.removeItem(this.keys.draftSessionActive);
    updateSessionRoute(this.config.prefix, session, replace);
    this.restoreDraftFor(session.id);
    this.syncRuntimeFromSession(session);
    await this.loadSession(session.id, epoch);
    if (epoch !== this.selectionEpoch) return;
    const current =
      this.sessions.value.find((entry) => entry.id === this.activeSessionId.value) || session;
    await this.loadSkills(current.id).catch(() => {
      if (epoch === this.selectionEpoch) this.skills.value = [];
    });
    if (epoch !== this.selectionEpoch) return;
    void this.refreshBranchTree(current.id);
    void this.loadChildRuns(current.id);
    if (current.activeResponseId) void this.resumeResponse(current.id, current.activeResponseId);
  }

  newChat(replace = false, projectId?: string, persistCurrent = true): void {
    // Bootstrap has not hydrated the active persisted draft yet. Saving the
    // empty initial composer here would look like a stale edit after reload.
    if (persistCurrent) this.persistCurrentDraft();
    this.releaseAttachmentResources(this.attachments.peek(), false);
    this.resetSideQuestion();
    ++this.selectionEpoch;
    const currentSession = this.activeSession.peek();
    if (currentSession) {
      this.newDraftID = `draft:${uuid()}`;
      this.currentDraftRev = 0;
    }
    const requestedProject =
      projectId === undefined
        ? currentSession
          ? currentSession.projectId || ''
          : this.activeProjectId.peek()
        : projectId;
    const selectedProject =
      requestedProject &&
      this.projects.value.some(
        (project) =>
          project.id === requestedProject && !project.archived && project.available !== false,
      )
        ? requestedProject
        : '';
    if (!this.storage.getItem(this.keys.draftSessionActive)) {
      const legacyID = `draft:${selectedProject || 'none'}`;
      if (
        readDrafts(this.storage, this.keys.draftMessages).some(
          (entry) => entry.sessionId === legacyID,
        )
      )
        this.newDraftID = legacyID;
    }
    batch(() => {
      this.sidebarOpen.value = false;
      this.activeSessionId.value = '';
      this.activeProjectId.value = selectedProject;
      this.draftActive.value = true;
      this.currentPlan.value = null;
      this.planOpen.value = false;
      this.planSeen.value = null;
      this.askUser.value = null;
      this.approval.value = null;
      this.skills.value = [];
      this.branchTree.value = null;
      this.branchPathCount.value = 0;
    });
    this.storage.removeItem(this.keys.activeSession);
    this.storage.setItem(this.keys.draftSessionActive, this.newDraftID);
    if (selectedProject) this.storage.setItem(this.keys.lastProject, selectedProject);
    updateSessionRoute(this.config.prefix, null, replace);
    this.restoreDraftFor(this.draftStorageID());
  }

  composerOwnerKey(): string {
    return this.draftActive.peek() ? this.draftStorageID() : this.activeSessionId.peek();
  }

  private draftStorageID(): string {
    return this.activeSessionId.peek() || this.newDraftID;
  }
  private persistCurrentDraft(): void {
    const id = this.draftStorageID();
    try {
      const drafts = saveDraft(this.storage, this.keys.draftMessages, {
        sessionId: id,
        content: this.prompt.peek(),
        projectId: this.activeProjectId.peek(),
        updated: Date.now(),
        rev: this.currentDraftRev,
        provider: this.selectedProvider.peek(),
        model: this.selectedModel.peek(),
        effort: this.selectedEffort.peek(),
        reasoningMode: this.selectedReasoningMode.peek(),
        agent: this.selectedAgent.peek(),
        worktreeDir: this.activeSession.peek()?.worktreeDir || this.selectedDraftWorktree.peek(),
        attachments: this.attachments
          .peek()
          .map(
            ({ file: _file, dataURL: _dataURL, previewURL: _previewURL, ...attachment }) =>
              attachment,
          ),
      });
      this.currentDraftRev = drafts.find((draft) => draft.sessionId === id)?.rev || 0;
      if (!this.disposed)
        this.publishSessionChange(
          'draft-changed',
          id,
          '',
          this.currentDraftRev,
          `draft:${id}:${this.currentDraftRev}`,
        );
    } catch (error) {
      if (!this.disposed) this.toast(error, 'error');
    }
  }
  private reconcileDraftStorage(id: string): void {
    const incoming = readDrafts(this.storage, this.keys.draftMessages).find(
      (entry) => entry.sessionId === id,
    );
    if (
      incoming &&
      (incoming.rev || 0) > this.currentDraftRev &&
      Boolean(this.prompt.peek().trim()) &&
      incoming.content !== this.prompt.peek()
    ) {
      this.toast(
        'This draft changed in another tab. Reload the conversation to choose that version.',
        'error',
      );
      return;
    }
    this.restoreDraftFor(id);
  }

  private restoreDraftFor(id: string): void {
    const draft = readDrafts(this.storage, this.keys.draftMessages).find(
      (entry) => entry.sessionId === id,
    );
    this.currentDraftRev = draft?.rev || 0;
    batch(() => {
      this.prompt.value = draft?.content || '';
      this.attachments.value = (draft?.attachments || []).map((attachment, index) => {
        const validation = validateAttachmentFile(
          { name: attachment.name, type: attachment.type, size: Number(attachment.size) || 0 },
          index,
          this.attachmentPolicy.peek(),
        );
        return {
          ...attachment,
          status: validation
            ? ('error' as const)
            : attachment.blobRef
              ? ('preparing' as const)
              : attachment.status || ('error' as const),
          error: validation?.message || attachment.error,
        };
      });
      this.selectedDraftWorktree.value = draft?.worktreeDir || '';
    });
    for (const attachment of this.attachments.peek())
      if (attachment.id && attachment.blobRef && attachment.status !== 'error')
        void this.restoreAttachmentBlob(id, attachment.id, attachment.blobRef);
    if (!draft) return;
    if (draft.provider) this.setPreference('provider', draft.provider, false);
    if (draft.model) this.setPreference('model', draft.model, false);
    if (draft.effort) this.setPreference('effort', draft.effort, false);
    if (draft.reasoningMode) this.setPreference('reasoning', draft.reasoningMode, false);
    if (draft.agent) this.setPreference('agent', draft.agent, false);
  }
  private syncRuntimeFromSession(session: Session): void {
    if (session.activeProvider) this.setPreference('provider', session.activeProvider, false);
    if (session.activeModel) this.setPreference('model', session.activeModel, false);
    if (session.activeEffort) this.setPreference('effort', session.activeEffort, false);
    if (session.activeReasoningMode)
      this.setPreference('reasoning', session.activeReasoningMode, false);
  }

  markChildRunRead(sessionId: string): void {
    const child = this.childRuns.peek().find((entry) => entry.sessionId === sessionId);
    if (!child) return;
    persistAgentReadMarker(this.storage, this.keys.agentReadMarkers, sessionId, child.revision);
    this.agentReadMarkers.value = { ...this.agentReadMarkers.peek(), [sessionId]: child.revision };
  }

  async resolveAndSelectSession(id: string, replace = false): Promise<void> {
    try {
      const data = await this.endpoints.selectedSession(id);
      const source = recordValue(data.selected_session);
      if (!source) return this.newChat(replace);
      const session = this.sessionFrom(source);
      const existing = this.sessions.value.find((entry) => entry.id === session.id);
      if (!existing) this.sessions.value = [session, ...this.sessions.value];
      await this.selectSession(existing || session, replace);
    } catch (error) {
      this.toast(error, 'error');
    }
  }

  async resolveAndSelectSessionAtMessage(id: string, messageId?: number): Promise<void> {
    await this.resolveAndSelectSession(id);
    if (!messageId) return;
    requestAnimationFrame(() => {
      const target = document.querySelector<HTMLElement>(
        `[data-message-id="${String(messageId)}"]`,
      );
      if (!target) return;
      target.scrollIntoView({ block: 'center', behavior: 'smooth' });
      target.tabIndex = -1;
      target.focus({ preventScroll: true });
      target.addEventListener('blur', () => target.removeAttribute('tabindex'), { once: true });
    });
  }

  private setInterjections(entries: PendingInterjection[]): void {
    this.interjectionRevision += 1;
    this.interjections.value = entries;
  }

  private reconcilePendingInterjections(
    sessionId: string,
    state: Record<string, unknown>,
    expectedRevision?: number,
  ): void {
    if (expectedRevision !== undefined && expectedRevision !== this.interjectionRevision) return;
    const hasList = Object.hasOwn(state, 'pending_interjections');
    const hasSingle = Object.hasOwn(state, 'pending_interjection');
    // Missing fields mean the server has no authoritative durable/runtime view;
    // retain local state until a commit, cancellation, or explicit empty list.
    if (!hasList && !hasSingle) return;
    const rawEntries = hasList
      ? array(state.pending_interjections)
      : state.pending_interjection
        ? [state.pending_interjection]
        : [];
    const committed = new Set(
      [
        ...(this.sessions.peek().find((session) => session.id === sessionId)?.messages || []),
        ...(this.runs.peek()[sessionId]?.messages || []),
      ]
        .map((message) => message.clientMessageId)
        .filter((id): id is string => Boolean(id)),
    );
    const remote = rawEntries
      .map(recordValue)
      .filter((entry): entry is Record<string, unknown> => Boolean(entry))
      .map((entry) => ({
        id: String(entry.id || entry.client_message_id || '').trim(),
        sessionId,
        content: String(entry.text || entry.attachment_summary || '').trim(),
        state: 'pending' as const,
      }))
      .filter((entry) => entry.id && !committed.has(entry.id));
    const remoteIDs = new Set(remote.map((entry) => entry.id));
    const retained = this.interjections
      .peek()
      .filter(
        (entry) =>
          entry.sessionId !== sessionId ||
          entry.state === 'sending' ||
          entry.state === 'failed' ||
          remoteIDs.has(entry.id),
      );
    const retainedIDs = new Set(retained.map((entry) => entry.id));
    this.setInterjections([...retained, ...remote.filter((entry) => !retainedIDs.has(entry.id))]);
  }

  async loadSession(id: string, epoch = this.selectionEpoch): Promise<void> {
    const sampledAskUser = this.askUser.peek();
    const sampledApproval = this.approval.peek();
    const sampledInterjectionRevision = this.interjectionRevision;
    try {
      const [state, selected] = await Promise.all([
        this.endpoints.sessionState(id),
        this.endpoints.selectedSession(id),
      ]);
      if (epoch !== this.selectionEpoch || this.activeSessionId.peek() !== id) return;
      const selectedSource = recordValue(selected.selected_session) || {};
      const sideload = recordValue(selected.selected_transcript) || {};
      const bodies = recordValue(sideload.bodies) || {};
      const serverMessages = listFrom(bodies, 'messages', 'items');
      const lastResponseId = String(state.lastResponseId || state.last_response_id || '').trim();
      const incoming = this.sessionFrom({ id, ...selectedSource, messages: serverMessages });
      const currentIndex = this.sessions.value.findIndex(
        (session) => session.id === id || (incoming.id && session.id === incoming.id),
      );
      const current = currentIndex >= 0 ? this.sessions.value[currentIndex] : undefined;
      // selected_transcript is authoritative here, including an empty transcript.
      // Session state is authoritative for its durable continuation anchor.
      const updated = {
        ...this.mergeSession(current, incoming, true),
        lastResponseId: lastResponseId || null,
      };
      if (currentIndex >= 0)
        this.sessions.value = this.sessions.value.map((session, index) =>
          index === currentIndex ? updated : session,
        );
      else this.sessions.value = [updated, ...this.sessions.value];
      if (updated.id !== id) this.rekeySession(id, updated.id, selectedSource);
      const planSource = state.current_plan || selectedSource.plan_summary;
      let loadedPlan: CurrentPlan | null = null;
      if (planSource && typeof planSource === 'object') {
        const raw = planSource as Record<string, unknown>;
        const plan = raw.plan || raw.steps;
        loadedPlan = Array.isArray(plan)
          ? { explanation: String(raw.explanation || ''), plan: plan as CurrentPlan['plan'] }
          : null;
      }
      this.currentPlan.value = loadedPlan;
      this.planSeen.value = loadedPlan ? planSummary(loadedPlan).signature : '';
      this.goal.value =
        state.goal && typeof state.goal === 'object' ? (state.goal as Goal) : updated.goal || null;
      this.applyWidgetStatus(selected.widget_status);
      const asks = Array.isArray(state.pending_ask_users)
        ? state.pending_ask_users
        : [state.pending_ask_user];
      const approvals = Array.isArray(state.pending_approvals)
        ? state.pending_approvals
        : [state.pending_approval];
      const activeResponse = String(state.active_response_id || updated.activeResponseId || '');
      const recoveredAsks = asks
        .map((value) => askUserPrompt(value, updated.id))
        .filter((value): value is AskUserPrompt => Boolean(value?.callId));
      const recoveredApprovals = approvals
        .map((value) => approvalPrompt(value, updated.id))
        .filter((value): value is ApprovalPrompt => Boolean(value?.id));
      for (const prompt of recoveredAsks)
        this.upsertInteraction('ask-user', updated.id, activeResponse, prompt.callId!, prompt);
      for (const prompt of recoveredApprovals)
        this.upsertInteraction('approval', updated.id, activeResponse, prompt.id!, prompt);
      for (const resolved of listFrom(state, 'resolved_interactions')) {
        const requestId = String(resolved.request_id || '');
        const kind = String(resolved.kind || '') === 'approval' ? 'approval' : 'ask-user';
        if (requestId)
          this.resolveInteractionRecord(
            kind,
            updated.id,
            activeResponse,
            requestId,
            String(resolved.outcome || 'resolved'),
            Number(resolved.resolved_at) || Date.now(),
          );
      }
      const pendingInteractionIDs = new Set([
        ...recoveredAsks.map((prompt) => `ask-user:${prompt.callId}`),
        ...recoveredApprovals.map((prompt) => `approval:${prompt.id}`),
      ]);
      const reconciledInteractions = { ...this.interactions.peek() };
      for (const [key, record] of Object.entries(reconciledInteractions)) {
        if (
          record.sessionId !== updated.id ||
          !['waiting', 'dismissed', 'submitting', 'failed'].includes(record.state) ||
          (activeResponse && record.responseId && record.responseId !== activeResponse) ||
          pendingInteractionIDs.has(`${record.kind}:${record.requestId}`)
        )
          continue;
        reconciledInteractions[key] = {
          ...record,
          state: activeResponse ? 'resolved-elsewhere' : 'cancelled-by-agent',
          outcome: activeResponse ? 'resolved' : 'cancelled-by-agent',
          resolvedAt: Date.now(),
        };
      }
      this.interactions.value = reconciledInteractions;
      if (this.askUser.peek() === sampledAskUser)
        this.askUser.value =
          recoveredAsks.find((prompt) =>
            this.shouldOpenInteraction('ask-user', updated.id, prompt.callId!),
          ) || null;
      if (this.approval.peek() === sampledApproval)
        this.approval.value =
          recoveredApprovals.find((prompt) =>
            this.shouldOpenInteraction('approval', updated.id, prompt.id!),
          ) || null;
      this.reconcilePendingInterjections(updated.id, state, sampledInterjectionRevision);
      this.reconcileLoadedIntents(updated.id, incoming.messages, Boolean(activeResponse));
      if (activeResponse)
        this.sessions.value = this.sessions.value.map((session) =>
          session.id === updated.id ? { ...session, activeResponseId: activeResponse } : session,
        );
    } catch (error) {
      if (epoch === this.selectionEpoch) this.toast(error, 'error');
    }
  }

  async send(options: SendOptions = {}): Promise<void> {
    const promptContent = this.prompt.value.trim();
    const inputText = options.inputText ?? promptContent;
    const content = options.displayContent ?? inputText;
    const attachments = options.contentParts ? [] : [...this.attachments.value];
    if (
      (!inputText && !attachments.length && !options.contentParts?.length) ||
      this.streaming.value ||
      this.sendBlocked.peek()
    )
      return;
    const blockedAttachment = attachments.find(
      (attachment) => attachment.status && attachment.status !== 'ready',
    );
    if (blockedAttachment) {
      this.toast(
        blockedAttachment.error || `${blockedAttachment.name} is still being prepared.`,
        'error',
      );
      return;
    }
    // This reservation must happen before the first await. The run projection
    // cannot serve as the entry lock because attachment materialization yields.
    this.sendPending.value = true;
    const clientMessageId = uuid();
    const requestId = uuid();
    const notificationState = this.notifications.peek();
    const notificationSubscriptionId =
      notificationState.status === 'subscribed' ? notificationState.subscriptionId || '' : '';

    let session = this.activeSession.value;
    const optimistic: Message = {
      id: `pending_${clientMessageId}`,
      role: 'user',
      content,
      created: Date.now(),
      clientMessageId,
      attachments,
      diffComments: options.diffComments,
    };
    if (!session) {
      const project = this.activeProjectId.value;
      session = {
        id: `draft_${uuid()}`,
        name: '',
        title: content.slice(0, 72) || 'New chat',
        mode: 'chat',
        origin: 'web',
        archived: false,
        pinned: false,
        created: Date.now(),
        lastMessageAt: Date.now(),
        projectId: project,
        projectName: this.projects.value.find((entry) => entry.id === project)?.name,
        worktreeDir: this.selectedDraftWorktree.value || undefined,
        messages: [],
      };
      this.sessions.value = [session, ...this.sessions.value];
      this.activeSessionId.value = session.id;
      this.draftActive.value = false;
    }
    const sessionId = session.id;
    let attachmentParts: Record<string, unknown>[];
    try {
      attachmentParts = await Promise.all(attachments.map((entry) => this.attachmentInput(entry)));
    } catch (error) {
      this.sendPending.value = false;
      this.toast(error, 'error');
      return;
    }
    this.sessions.value = this.sessions.value.map((entry) =>
      entry.id === sessionId
        ? { ...entry, messages: [...entry.messages, optimistic], lastMessageAt: Date.now() }
        : entry,
    );
    this.trackIntent(sessionId, {
      id: optimistic.id,
      clientMessageId,
      content,
      created: optimistic.created,
      attachments: optimistic.attachments,
    });
    if (!options.preserveComposer)
      batch(() => {
        this.prompt.value = '';
        this.attachments.value = [];
      });
    const run: ActiveRun = {
      responseId: `pending_${uuid()}`,
      sessionId,
      epoch: 1,
      status: 'connecting',
      lastSequence: 0,
      startedRev: session.transcriptRev || 0,
      startedAt: Date.now(),
      reconnects: 0,
      requestId,
      notificationSubscriptionId: notificationSubscriptionId || undefined,
    };
    this.runs.value = { ...this.runs.value, [sessionId]: initialProjection(run) };
    // Ownership changes before any previous controller is touched. The POST
    // stream and every later subscription share this same generation.
    const streamOwner = this.streamSupervisors.begin(sessionId, run.responseId);
    this.sendPending.value = false;
    let ownerID = sessionId;
    const inputContent: unknown = options.contentParts?.length
      ? [...options.contentParts, ...(inputText ? [{ type: 'input_text', text: inputText }] : [])]
      : attachmentParts.length > 0
        ? [...attachmentParts, ...(inputText ? [{ type: 'input_text', text: inputText }] : [])]
        : inputText;
    const requestBody: Record<string, unknown> = {
      stream: true,
      include_server_tools: true,
      client_message_id: clientMessageId,
      input: [
        {
          type: 'message',
          role: 'user',
          client_message_id: clientMessageId,
          content: inputContent,
        },
      ],
    };
    if (session.lastResponseId) requestBody.previous_response_id = session.lastResponseId;
    else if (this.projectsEnabled.value) {
      if (session.projectId) requestBody.project_id = session.projectId;
      else requestBody.no_project = true;
    } else requestBody.use_default_workspace = true;
    if (!session.lastResponseId && session.worktreeDir)
      requestBody.worktree_dir = session.worktreeDir;
    if (!session.lastResponseId) requestBody.agent = session.agent || this.selectedAgent.value;
    const selectedModel = this.models.value.find((entry) => entry.id === this.selectedModel.value);
    applyRuntimeToRequest(
      requestBody,
      {
        provider: session.activeProvider,
        model: session.activeModel,
        effort: session.activeEffort,
        reasoningMode: session.activeReasoningMode,
      },
      {
        provider: this.selectedProvider.value,
        model: this.selectedModel.value,
        effort: this.selectedEffort.value,
        reasoningMode: this.selectedReasoningMode.value,
      },
      selectedModel,
    );

    let unknownAttempts = 0;
    const restoreRejectedComposer = (): void => {
      if (
        !options.preserveComposer &&
        this.activeSessionId.peek() === ownerID &&
        !this.prompt.peek()
      ) {
        this.prompt.value = promptContent;
        this.attachments.value = attachments;
      }
    };
    const submit = async (signal: AbortSignal): Promise<void> => {
      try {
        const response = notificationSubscriptionId
          ? await this.endpoints.createResponse(
              requestBody,
              sessionId,
              requestId,
              signal,
              notificationSubscriptionId,
            )
          : await this.endpoints.createResponse(requestBody, sessionId, requestId, signal);
        ownerID = await this.acceptCreatedResponse({
          response,
          streamOwner,
          sessionId,
          clientMessageId,
          requestId,
          attachments,
          options,
        });
      } catch (error) {
        if (!this.streamSupervisors.owns(streamOwner)) return;
        // Once x-response-id has been adopted, delivery is known and only the
        // resumable response stream needs recovery.
        if (!streamOwner.responseId.startsWith('pending_')) {
          this.scheduleSupervisorRetry(streamOwner, error);
          return;
        }
        if (this.definitiveSendRejection(error)) {
          options.onTransportFailed?.(error);
          this.rollbackOptimisticIntent(ownerID, clientMessageId);
          this.failRun(ownerID, error);
          restoreRejectedComposer();
          this.streamSupervisors.retire(streamOwner);
          return;
        }

        // A browser transport exception is not evidence that the mutation was
        // rejected. Keep the idempotency key and intent, expose a durable
        // checking state, and replay exactly the same logical operation.
        unknownAttempts += 1;
        this.markIntentChecking(ownerID, clientMessageId);
        const projection = this.runs.peek()[ownerID];
        if (projection)
          this.runs.value = {
            ...this.runs.peek(),
            [ownerID]: {
              ...projection,
              phase: 'Checking whether this was sent…',
              run: { ...projection.run, status: 'checking', error: undefined },
            },
          };
        this.streamSupervisors.scheduleRetry(
          streamOwner,
          () => {
            const retryAbort = this.streamSupervisors.replaceAbort(streamOwner);
            if (retryAbort) void submit(retryAbort.signal);
          },
          Math.min(30_000, 1_000 * 1.5 ** Math.min(unknownAttempts - 1, 8)),
        );
      }
    };
    await submit(streamOwner.abort.signal);
  }

  private definitiveSendRejection(error: unknown): boolean {
    return (
      error instanceof APIError &&
      error.status >= 400 &&
      error.status < 500 &&
      ![408, 425, 429].includes(error.status)
    );
  }

  private async acceptCreatedResponse(input: {
    response: Response;
    streamOwner: StreamSupervisor;
    sessionId: string;
    clientMessageId: string;
    requestId: string;
    attachments: Attachment[];
    options: SendOptions;
  }): Promise<string> {
    const { response, streamOwner, sessionId, clientMessageId, requestId, attachments, options } =
      input;
    if (!response.ok || !response.body) {
      const body = await response.text();
      if (response.status === 409) {
        try {
          const parsed = JSON.parse(body) as { error?: { type?: string } };
          if (parsed.error?.type === 'client_message_already_committed') {
            this.retireIntent(sessionId, clientMessageId);
            this.streamSupervisors.retire(streamOwner);
            await this.loadSession(sessionId);
            return sessionId;
          }
        } catch {
          /* Normalized below. */
        }
      }
      throw new APIError(
        body || `Response request returned ${response.status}`,
        response.status,
        body,
      );
    }
    const durableSessionId = response.headers.get('x-session-id') || sessionId;
    const responseId = response.headers.get('x-response-id') || '';
    if (!responseId) throw new Error('Server did not return a response id.');
    options.onTransportStarted?.();
    this.releaseAttachmentResources(attachments, true);
    if (!this.streamSupervisors.owns(streamOwner)) return streamOwner.sessionId;
    if (!this.streamSupervisors.adoptResponse(streamOwner, responseId))
      return streamOwner.sessionId;
    let ownerID = sessionId;
    if (durableSessionId !== sessionId) {
      if (!this.streamSupervisors.rekey(streamOwner, durableSessionId))
        return streamOwner.sessionId;
      this.rekeySession(sessionId, durableSessionId);
      ownerID = durableSessionId;
    }
    this.sessions.value = this.sessions.value.map((entry) =>
      entry.id === ownerID
        ? {
            ...entry,
            activeResponseId: responseId,
            activeRun: true,
            messages: entry.messages.map((message) =>
              message.clientMessageId === clientMessageId
                ? { ...message, pending: false, interruptState: undefined }
                : message,
            ),
          }
        : entry,
    );
    this.retireIntent(ownerID, clientMessageId);
    this.publishSessionChange('run-changed', ownerID, responseId, undefined, clientMessageId);
    const projection = this.runs.value[ownerID] || this.runs.value[sessionId];
    this.runs.value = {
      ...this.runs.value,
      [ownerID]: {
        ...projection,
        phase: undefined,
        run: {
          ...projection.run,
          responseId,
          sessionId: ownerID,
          status: 'streaming',
          requestId,
        },
      },
    };
    clearDraft(this.storage, this.keys.draftMessages, sessionId);
    await this.consumeResponseBody(response.body, streamOwner);
    if (!this.streamSupervisors.owns(streamOwner)) return ownerID;
    const current = this.runs.value[ownerID];
    if (current && ['connecting', 'streaming'].includes(current.run.status))
      await this.recoverSupervisor(streamOwner);
    return ownerID;
  }

  private markIntentChecking(sessionId: string, clientMessageId: string): void {
    this.sessions.value = this.sessions.value.map((session) =>
      session.id === sessionId
        ? {
            ...session,
            messages: session.messages.map((message) =>
              message.clientMessageId === clientMessageId
                ? { ...message, interruptState: 'checking_send', pending: true }
                : message,
            ),
          }
        : session,
    );
    const intent = this.pendingIntents
      .peek()
      [sessionId]?.find((entry) => entry.clientMessageId === clientMessageId);
    if (intent) this.trackIntent(sessionId, { ...intent, state: 'checking' });
  }

  private rekeySession(oldID: string, id: string, source?: Record<string, unknown>): void {
    const current = this.sessions.value.find((entry) => entry.id === oldID);
    if (!current || !id) return;
    const incoming = source ? this.sessionFrom(source) : null;
    const updated: Session = { ...current, ...(incoming || {}), id, messages: current.messages };
    const duplicate = this.sessions.value.find((entry) => entry.id === id && entry.id !== oldID);
    this.sessions.value = this.sessions.value.filter(
      (entry) => entry.id !== oldID && entry.id !== id,
    );
    this.sessions.value = [this.mergeSession(duplicate, updated), ...this.sessions.value];
    const projection = this.runs.value[oldID];
    const next = { ...this.runs.value };
    delete next[oldID];
    if (projection) next[id] = { ...projection, run: { ...projection.run, sessionId: id } };
    this.runs.value = next;
    const intents = this.pendingIntents.value[oldID] || [];
    const registry = { ...this.pendingIntents.value };
    delete registry[oldID];
    if (intents.length) registry[id] = [...(registry[id] || []), ...intents];
    this.pendingIntents.value = registry;
    removeSessionPendingIntents(this.storage, this.keys.pendingIntents, oldID);
    intents.forEach((intent) =>
      persistPendingIntent(this.storage, this.keys.pendingIntents, id, intent),
    );
    this.setInterjections(
      this.interjections.value.map((entry) =>
        entry.sessionId === oldID ? { ...entry, sessionId: id } : entry,
      ),
    );
    if (this.activeSessionId.peek() === oldID) {
      this.activeSessionId.value = id;
      this.storage.setItem(this.keys.activeSession, id);
      updateSessionRoute(this.config.prefix, updated, true);
    }
  }

  private async consumeResponseBody(
    body: ReadableStream<Uint8Array>,
    owner: StreamSupervisor,
  ): Promise<boolean> {
    let cleanCompletion = false;
    const watchdog = () =>
      this.streamSupervisors.touchWatchdog(
        owner,
        () => {
          this.bumpDiagnostic('streamWatchdogTimeouts');
          owner.abort.abort(new DOMException('Response stream became inactive', 'TimeoutError'));
          this.scheduleSupervisorRetry(owner, new Error('Response stream became inactive'));
        },
        35_000,
      );
    watchdog();
    try {
      for await (const frame of decodeSSE(body, owner.abort.signal, watchdog)) {
        if (!this.streamSupervisors.owns(owner)) {
          this.bumpDiagnostic('staleStreamCallbacks');
          return false;
        }
        if (frame.done) {
          cleanCompletion = true;
          continue;
        }
        let event: ResponseEvent;
        try {
          const payload = JSON.parse(frame.data) as ResponseEvent;
          event = { ...payload, type: frame.event === 'message' ? payload.type : frame.event };
        } catch {
          continue;
        }
        this.applyResponseEvent(owner.sessionId, event, owner);
        const projection = this.runs.peek()[owner.sessionId];
        if (projection && ['completed', 'cancelled', 'failed'].includes(projection.run.status))
          cleanCompletion = true;
      }
      return cleanCompletion;
    } finally {
      this.streamSupervisors.clearWatchdog(owner);
    }
  }

  async streamResponse(responseId: string, sessionId: string, after: number): Promise<void> {
    if (this.retiredResponses.has(responseId)) return;
    const current = this.streamSupervisors.current(sessionId);
    const owner =
      current && current.responseId === responseId
        ? current
        : this.streamSupervisors.begin(sessionId, responseId, after);
    await this.subscribeSupervisor(owner);
  }

  private async subscribeSupervisor(owner: StreamSupervisor): Promise<void> {
    if (!this.streamSupervisors.startSubscription(owner)) return;
    const abort = this.streamSupervisors.replaceAbort(owner);
    if (!abort) {
      this.streamSupervisors.finishSubscription(owner);
      return;
    }
    try {
      const response = await this.endpoints.responseEvents(
        owner.responseId,
        owner.lastSequence,
        abort.signal,
      );
      if (!this.streamSupervisors.owns(owner) || abort.signal.aborted) return;
      if (!response.ok || !response.body)
        throw new Error(`Response stream returned ${response.status}`);
      const clean = await this.consumeResponseBody(response.body, owner);
      if (!this.streamSupervisors.owns(owner) || abort.signal.aborted) return;
      const projection = this.runs.peek()[owner.sessionId];
      if (!clean && projection && ['connecting', 'streaming'].includes(projection.run.status))
        this.scheduleSupervisorRetry(owner, new Error('Response stream ended before completion'));
    } catch (error) {
      if (!this.streamSupervisors.owns(owner) || abort.signal.aborted) return;
      if (error instanceof ResponseProtocolError) {
        this.scheduleSupervisorRetry(owner, error);
        return;
      }
      this.scheduleSupervisorRetry(owner, error);
    } finally {
      this.streamSupervisors.finishSubscription(owner);
    }
  }

  private scheduleSupervisorRetry(owner: StreamSupervisor, error?: unknown): void {
    if (!this.streamSupervisors.owns(owner)) return;
    const projection = this.runs.peek()[owner.sessionId];
    if (!projection || !['connecting', 'streaming'].includes(projection.run.status)) {
      if (error) this.failRun(owner.sessionId, error);
      return;
    }
    const reconnects = projection.run.reconnects + 1;
    this.bumpDiagnostic('supervisorRetries');
    this.runs.value = {
      ...this.runs.peek(),
      [owner.sessionId]: {
        ...projection,
        run: { ...projection.run, status: 'connecting', reconnects },
      },
    };
    this.streamSupervisors.scheduleRetry(
      owner,
      () => void this.recoverSupervisor(owner),
      Math.min(60_000, 1_000 * 1.5 ** Math.min(reconnects, 10)),
    );
  }

  applyResponseEvent(sessionId: string, event: ResponseEvent, owner?: StreamSupervisor): void {
    const current = this.runs.value[sessionId];
    if (!current) return;
    if (owner && !this.streamSupervisors.owns(owner)) {
      this.bumpDiagnostic('staleStreamCallbacks');
      return;
    }
    const eventResponse = recordValue(event.response);
    const explicitResponseId = String(
      event.response_id || eventResponse?.id || eventResponse?.response_id || '',
    ).trim();
    // A frame explicitly owned by a different response is stale. It must not
    // trigger protocol recovery for the current response.
    if (explicitResponseId && explicitResponseId !== (owner?.responseId || current.run.responseId))
      return;
    // The stop action owns the visible state immediately. Frames already queued
    // when the stream was aborted must not restart the spinner or running tools.
    const stopped = this.locallyStoppedResponses.has(current.run.responseId);
    if (
      stopped &&
      !['response.completed', 'response.cancelled', 'response.failed'].includes(event.type)
    )
      return;
    if (event.type === 'response.stream_error') {
      const error = recordValue(event.error);
      if (String(error?.type || '') === 'stream_buffer_overflow')
        void this.resumeResponse(sessionId, current.run.responseId);
      return;
    }
    let next: ResponseProjection;
    try {
      next = reduceResponse(current, event);
    } catch (error) {
      if (error instanceof ResponseProtocolError) {
        void this.resumeResponse(sessionId, current.run.responseId);
        return;
      }
      throw error;
    }
    if (owner) this.streamSupervisors.advance(owner, Number(event.sequence_number));
    const response = recordValue(event.response) || {};
    const runtimePatch: Partial<Session> = {};
    if (event.type === 'response.created' || event.type === 'response.completed') {
      runtimePatch.activeModel = String(response.model || '') || undefined;
      runtimePatch.activeProvider = String(response.provider || '') || undefined;
      if (Object.hasOwn(response, 'reasoning_effort'))
        runtimePatch.activeEffort = String(response.reasoning_effort || '');
    } else if (event.type === 'response.model_switch') {
      runtimePatch.activeModel = String(event.model || '') || undefined;
      runtimePatch.activeProvider = String(event.provider || '') || undefined;
      if (Object.hasOwn(event, 'reasoning_effort'))
        runtimePatch.activeEffort = String(event.reasoning_effort || '');
    } else if (event.type === 'response.model_swap.progress') {
      const prefix =
        event.stage === 'failed' ? 'previous_' : event.stage === 'complete' ? 'target_' : '';
      if (prefix) {
        runtimePatch.activeModel = String(event[`${prefix}model`] || '') || undefined;
        runtimePatch.activeProvider = String(event[`${prefix}provider`] || '') || undefined;
        if (Object.hasOwn(event, `${prefix}effort`))
          runtimePatch.activeEffort = String(event[`${prefix}effort`] || '');
      }
    }
    if (event.type === 'response.completed' && next.usage) {
      const lastAssistant = [...next.messages]
        .reverse()
        .find((message) => message.role === 'assistant');
      if (lastAssistant)
        next = {
          ...next,
          messages: next.messages.map((message) =>
            message === lastAssistant ? { ...message, usage: next.usage || undefined } : message,
          ),
        };
      runtimePatch.usage = (recordValue(response.session_usage) || next.usage) as Session['usage'];
    }
    this.runs.value = { ...this.runs.value, [sessionId]: next };
    if (Object.keys(runtimePatch).length)
      this.sessions.value = this.sessions.value.map((session) =>
        session.id === sessionId
          ? {
              ...session,
              ...Object.fromEntries(
                Object.entries(runtimePatch).filter(([, value]) => value !== undefined),
              ),
            }
          : session,
      );
    if (next.plan !== current.plan && sessionId === this.activeSessionId.peek()) {
      this.currentPlan.value = next.plan;
      if (!next.plan) {
        this.planOpen.value = false;
        this.planSeen.value = '';
      }
    }
    if (next.askUser && next.askUser !== current.askUser && next.askUser.callId) {
      this.upsertInteraction(
        'ask-user',
        sessionId,
        next.run.responseId,
        next.askUser.callId,
        next.askUser,
      );
      if (this.shouldOpenInteraction('ask-user', sessionId, next.askUser.callId))
        this.askUser.value = next.askUser;
    }
    if (next.approval && next.approval !== current.approval && next.approval.id) {
      this.upsertInteraction(
        'approval',
        sessionId,
        next.run.responseId,
        next.approval.id,
        next.approval,
      );
      if (this.shouldOpenInteraction('approval', sessionId, next.approval.id))
        this.approval.value = next.approval;
    }
    if (event.type === 'response.interjection') {
      const clientID = String(event.client_message_id || event.interjection_id || '');
      this.setInterjections(this.interjections.value.filter((entry) => entry.id !== clientID));
    }
    if (
      event.type === 'response.approval.resolved' ||
      event.type === 'response.ask_user.resolved'
    ) {
      const kind = event.type === 'response.approval.resolved' ? 'approval' : 'ask-user';
      const requestId = String(event.approval_id || event.call_id || '');
      this.resolveInteractionRecord(
        kind,
        sessionId,
        current.run.responseId,
        requestId,
        String(event.outcome || 'resolved'),
        Number(event.resolved_at) || Date.now(),
      );
    }
    if (next.fileChangeRevision !== current.fileChangeRevision) {
      this.currentActivityFile.value = String(event.path || event.file || event.file_path || '');
      this.fileChangeRevision.value += 1;
      if (this.diff.value.open && this.diff.value.sessionId === sessionId) void this.loadDiff();
    }
    if (
      ['completed', 'cancelled', 'failed'].includes(next.run.status) &&
      next.run.status !== current.run.status
    ) {
      const originatingSubscriptionId = next.run.notificationSubscriptionId || '';
      if (
        (next.run.status === 'completed' || next.run.status === 'failed') &&
        originatingSubscriptionId
      ) {
        const eventId = completionEventId(next.run.responseId, originatingSubscriptionId);
        if (!this.handledCompletionEvents.has(eventId)) {
          this.handledCompletionEvents.add(eventId);
          void this.notificationController.signalCompletion(
            {
              responseId: next.run.responseId,
              sessionId,
              outcome: next.run.status,
              createdAt: new Date(next.run.endedAt || Date.now()).toISOString(),
            },
            originatingSubscriptionId,
          );
        }
      }
      this.locallyStoppedResponses.delete(next.run.responseId);
      this.retireIntent(sessionId);
      this.sessions.value = this.sessions.value.map((session) =>
        session.id === sessionId
          ? {
              ...session,
              activeResponseId: null,
              activeRun: false,
              lastResponseId: next.run.responseId,
            }
          : session,
      );
      this.publishSessionChange(
        'run-changed',
        sessionId,
        next.run.responseId,
        undefined,
        next.run.requestId || next.run.responseId,
      );
      const interactions = { ...this.interactions.peek() };
      let interactionsChanged = false;
      for (const [key, interaction] of Object.entries(interactions)) {
        if (
          interaction.sessionId === sessionId &&
          interaction.responseId === next.run.responseId &&
          ['waiting', 'dismissed', 'submitting', 'failed'].includes(interaction.state)
        ) {
          interactions[key] = {
            ...interaction,
            state: 'cancelled-by-agent',
            outcome: 'Decision no longer needed',
            resolvedAt: Date.now(),
          };
          interactionsChanged = true;
        }
      }
      if (interactionsChanged) this.interactions.value = interactions;
      if (owner) this.streamSupervisors.retire(owner);
      if (this.diff.peek().open && this.diff.peek().sessionId === sessionId) void this.loadDiff();
      this.schedule(() => void this.refreshSessionMessages(sessionId, next.run.finalRev || 0), 0);
      if (next.run.status === 'completed')
        this.scheduleTitleReconciliation(sessionId, next.run.responseId, owner?.generation || 0);
    }
  }

  private scheduleTitleReconciliation(
    sessionId: string,
    responseId = this.sessions.peek().find((entry) => entry.id === sessionId)?.lastResponseId || '',
    streamGeneration = this.streamSupervisors.current(sessionId)?.generation || 0,
  ): void {
    for (const timer of this.titleRefreshTimers.get(sessionId) || []) window.clearTimeout(timer);
    const title = this.sessions.peek().find((entry) => entry.id === sessionId)?.title || '';
    const statusGeneration = this.statusCoordinator.generation;
    const timers = [2_000, 8_000].map((delay, index) =>
      window.setTimeout(() => {
        const session = this.sessions.peek().find((entry) => entry.id === sessionId);
        const currentOwner = this.streamSupervisors.current(sessionId);
        const ownerReplaced = Boolean(currentOwner && currentOwner.generation > streamGeneration);
        if (
          !this.disposed &&
          (!session ||
            ((!responseId || session.lastResponseId === responseId) && session.title === title)) &&
          !ownerReplaced &&
          this.statusCoordinator.generation === statusGeneration &&
          document.visibilityState === 'visible'
        )
          void this.reconcile('title', { authoritative: true }).catch(() => undefined);
        if (index === 1) this.titleRefreshTimers.delete(sessionId);
      }, delay),
    );
    this.titleRefreshTimers.set(sessionId, timers);
  }

  private async refreshSessionMessages(sessionId: string, targetRev = 0): Promise<void> {
    try {
      const interjectionRevision = this.interjectionRevision;
      const stateRequest = this.endpoints.sessionState(sessionId).catch(() => null);
      const selected = await this.endpoints.selectedSession(sessionId);
      const source = recordValue(selected.selected_session);
      const sideload = recordValue(selected.selected_transcript);
      const bodies = recordValue(sideload?.bodies);
      if (!source || !bodies) return;
      const incoming = this.sessionFrom({
        ...source,
        // The bodies revision is the generation of the messages being installed.
        // Prefer it over summary metadata, which may have advanced independently.
        transcript_rev: bodies.rev ?? source.transcript_rev ?? source.rev,
        messages: listFrom(bodies, 'messages', 'items'),
      });
      const incomingRev = incoming.transcriptRev || 0;
      // Never combine a terminal projection with transcript bodies whose
      // generation cannot prove that they contain the durable handoff.
      if (targetRev && incomingRev < targetRev) return;
      const projection = this.runs.peek()[sessionId];
      const finalRev = projection?.run.finalRev || 0;
      const retireProjection = Boolean(
        projection &&
        ['completed', 'cancelled', 'failed'].includes(projection.run.status) &&
        projection.run.durableHandoff === true &&
        finalRev > 0 &&
        incomingRev >= finalRev,
      );
      batch(() => {
        this.sessions.value = this.sessions.value.map((session) =>
          session.id === sessionId ? this.mergeSession(session, incoming, true) : session,
        );
        if (retireProjection && projection) {
          this.retiredResponses.add(projection.run.responseId);
          // Keep only compact terminal run-center history. Durable transcript
          // bodies are now authoritative, so retaining projected messages can
          // duplicate old rows that predate response identity metadata.
          const summary = [...projection.messages]
            .reverse()
            .find((message) => message.role === 'assistant' && message.content.trim())
            ?.content.trim()
            .slice(0, 160);
          this.runs.value = {
            ...this.runs.peek(),
            [sessionId]: {
              ...projection,
              messages: [],
              run: { ...projection.run, summary: summary || projection.run.summary },
            },
          };
        }
      });
      this.reconcileLoadedIntents(
        sessionId,
        incoming.messages,
        Boolean(projection && ['connecting', 'streaming'].includes(projection.run.status)),
      );
      const state = await stateRequest;
      if (state) {
        this.reconcilePendingInterjections(sessionId, state, interjectionRevision);
        const lastResponseId = String(state.lastResponseId || state.last_response_id || '').trim();
        this.sessions.value = this.sessions.value.map((session) =>
          session.id === sessionId
            ? { ...session, lastResponseId: lastResponseId || null }
            : session,
        );
      }
    } catch {
      /* Status polling will retry durable reconciliation. */
    }
  }

  private async resumeResponse(sessionId: string, responseId: string): Promise<void> {
    if (this.retiredResponses.has(responseId)) return;
    const current = this.streamSupervisors.current(sessionId);
    const owner =
      current && current.responseId === responseId
        ? current
        : this.streamSupervisors.begin(
            sessionId,
            responseId,
            this.runs.peek()[sessionId]?.run.lastSequence || 0,
          );
    await this.recoverSupervisor(owner);
  }

  private async waitForSubscriptionIdle(owner: StreamSupervisor): Promise<boolean> {
    const deadline = Date.now() + 1_000;
    while (this.streamSupervisors.owns(owner) && owner.subscriptionInFlight) {
      if (Date.now() >= deadline) return false;
      await new Promise<void>((resolve) => window.setTimeout(resolve, 10));
    }
    return this.streamSupervisors.owns(owner);
  }

  private async recoverSupervisor(owner: StreamSupervisor): Promise<void> {
    if (!this.streamSupervisors.startRecovery(owner)) return;
    this.bumpDiagnostic('supervisorRecoveries');
    const abort = this.streamSupervisors.replaceAbort(owner);
    if (!abort) {
      this.streamSupervisors.finishRecovery(owner);
      return;
    }
    const { sessionId, responseId } = owner;
    try {
      const snapshot = await this.endpoints.response(responseId, abort.signal);
      if (!this.streamSupervisors.owns(owner) || abort.signal.aborted) return;
      const recovery = recordValue(snapshot.recovery) || {};
      const existing =
        this.runs.value[sessionId] ||
        initialProjection({
          responseId,
          sessionId,
          epoch: Number(snapshot.run_epoch) || 1,
          status: 'streaming',
          lastSequence: 0,
          startedRev: Number(snapshot.started_rev) || 0,
          startedAt: Number(snapshot.started_at) || undefined,
          endedAt: Number(snapshot.ended_at) || undefined,
          reconnects: 0,
        });
      const rawMessages = listFrom(recovery, 'messages').length
        ? listFrom(recovery, 'messages')
        : listFrom(snapshot, 'messages');
      const projected: Message[] = rawMessages
        .map((raw, index) => {
          if (Array.isArray(raw.parts))
            return convertServerMessages([raw], {
              rebaseAssetURL: (url) => rebaseHubAssetURL(this.config, url),
            })[0];
          const clientMessageId = String(raw.clientMessageId || raw.client_message_id || '').trim();
          const projectedResponseId = String(
            raw.responseId || raw.response_id || responseId,
          ).trim();
          const segmentOrdinal = Number(
            raw.assistantSegmentOrdinal ?? raw.assistant_segment_ordinal,
          );
          const interruptState = String(raw.interruptState || raw.interrupt_state || '').trim();
          return {
            ...raw,
            id: String(raw.id || `${responseId}:snapshot:${index}`),
            role: String(raw.role || 'assistant'),
            content: String(raw.content || raw.text || ''),
            created: Number(raw.created || raw.created_at) || Date.now(),
            responseId: projectedResponseId || responseId,
            ...(clientMessageId ? { clientMessageId } : {}),
            ...(Number.isFinite(segmentOrdinal) ? { assistantSegmentOrdinal: segmentOrdinal } : {}),
            ...(interruptState ? { interruptState } : {}),
          } as Message;
        })
        .filter(Boolean);
      const status = String(snapshot.status || 'in_progress');
      let recoveredAskUser = existing.askUser;
      let recoveredApproval = existing.approval;
      for (const entry of listFrom(recovery, 'events')) {
        const eventType = String(entry.event || entry.type || '');
        const payload = entry.payload ?? entry;
        if (eventType === 'response.ask_user.prompt')
          recoveredAskUser = askUserPrompt(payload, sessionId) || recoveredAskUser;
        if (eventType === 'response.approval.prompt')
          recoveredApproval = approvalPrompt(payload, sessionId) || recoveredApproval;
      }
      const next: ResponseProjection = {
        ...existing,
        askUser: recoveredAskUser,
        approval: recoveredApproval,
        messages: projected.length ? projected : existing.messages,
        run: {
          ...existing.run,
          responseId,
          epoch: Number(snapshot.run_epoch) || existing.run.epoch,
          status:
            status === 'failed'
              ? 'failed'
              : status === 'completed'
                ? 'completed'
                : status === 'cancelled'
                  ? 'cancelled'
                  : 'streaming',
          lastSequence:
            Number(snapshot.last_sequence_number ?? recovery.sequence_number) ||
            existing.run.lastSequence,
          startedRev: Number(snapshot.started_rev) || existing.run.startedRev,
          startedAt: Number(snapshot.started_at) || existing.run.startedAt,
          endedAt: Number(snapshot.ended_at) || existing.run.endedAt,
        },
      };
      this.runs.value = { ...this.runs.value, [sessionId]: next };
      if (recoveredAskUser?.callId) {
        this.upsertInteraction(
          'ask-user',
          sessionId,
          responseId,
          recoveredAskUser.callId,
          recoveredAskUser,
        );
        if (
          sessionId === this.activeSessionId.peek() &&
          this.shouldOpenInteraction('ask-user', sessionId, recoveredAskUser.callId)
        )
          this.askUser.value = recoveredAskUser;
      }
      if (recoveredApproval?.id) {
        this.upsertInteraction(
          'approval',
          sessionId,
          responseId,
          recoveredApproval.id,
          recoveredApproval,
        );
        if (
          sessionId === this.activeSessionId.peek() &&
          this.shouldOpenInteraction('approval', sessionId, recoveredApproval.id)
        )
          this.approval.value = recoveredApproval;
      }
      for (const resolved of listFrom(recovery, 'resolved_interactions')) {
        const requestId = String(resolved.request_id || '');
        const kind = String(resolved.kind || '') === 'approval' ? 'approval' : 'ask-user';
        if (requestId)
          this.resolveInteractionRecord(
            kind,
            sessionId,
            responseId,
            requestId,
            String(resolved.outcome || 'resolved'),
            Number(resolved.resolved_at) || Date.now(),
          );
      }
      if (next.run.lastSequence > owner.lastSequence) owner.lastSequence = next.run.lastSequence;
      this.streamSupervisors.finishRecovery(owner);
      if (!this.streamSupervisors.owns(owner)) return;
      if (next.run.status === 'streaming') {
        if (await this.waitForSubscriptionIdle(owner))
          await this.streamResponse(responseId, sessionId, owner.lastSequence);
        else this.scheduleSupervisorRetry(owner, new Error('Previous subscription did not stop'));
      } else {
        this.streamSupervisors.retire(owner);
        await this.refreshSessionMessages(sessionId, Number(snapshot.final_rev) || 0);
      }
    } catch (error) {
      this.streamSupervisors.finishRecovery(owner);
      if (!this.streamSupervisors.owns(owner) || abort.signal.aborted) return;
      this.scheduleSupervisorRetry(owner, error);
    }
  }

  async cancel(): Promise<void> {
    const projection = this.activeProjection.value;
    if (!projection || !['connecting', 'streaming', 'cancelling'].includes(projection.run.status))
      return;
    const { sessionId, responseId } = projection.run;
    this.locallyStoppedResponses.add(responseId);
    this.runs.value = {
      ...this.runs.value,
      [sessionId]: {
        ...projection,
        messages: projection.messages.map((message) =>
          message.role !== 'tool-group' || message.toolGroupClosed === true
            ? message
            : {
                ...message,
                status: 'done',
                toolGroupClosed: true,
                tools: message.tools?.map((tool) =>
                  tool.status === 'running' ? { ...tool, status: 'cancelled' as const } : tool,
                ),
              },
        ),
        phase: undefined,
        retry: undefined,
        run: { ...projection.run, status: 'cancelled' },
      },
    };
    this.streamSupervisors.cancel(sessionId, responseId);
    try {
      const result = await this.endpoints.cancelResponse(responseId);
      const authoritativeStatus = String(result.status || '');
      const current = this.runs.peek()[sessionId];
      if (
        current &&
        current.run.responseId === responseId &&
        ['completed', 'cancelled', 'failed'].includes(authoritativeStatus)
      ) {
        this.runs.value = {
          ...this.runs.peek(),
          [sessionId]: {
            ...current,
            run: {
              ...current.run,
              status: authoritativeStatus as ResponseProjection['run']['status'],
            },
          },
        };
      }
      this.publishSessionChange('run-changed', sessionId, responseId, undefined, uuid());
      // Cancellation acknowledgement is intentionally distinct from durable
      // finalization. Reconcile in the background while the UI remains stopped.
      void this.reconcile('cancellation', { authoritative: true }).catch(() => undefined);
    } catch (error) {
      this.locallyStoppedResponses.delete(responseId);
      this.toast(`Couldn’t confirm stop: ${errorMessage(error)}`, 'error');
      const current = this.runs.peek()[sessionId];
      if (current?.run.responseId === responseId) void this.resumeResponse(sessionId, responseId);
    }
  }

  async interject(content: string, options: SendOptions = {}): Promise<void> {
    const session = this.activeSession.value;
    const value = (options.inputText ?? content).trim();
    const displayContent = (options.displayContent ?? value).trim();
    const attachments = options.contentParts ? [] : [...this.attachments.value];
    if (!session || (!value && !attachments.length && !options.contentParts?.length)) return;
    const blockedAttachment = attachments.find(
      (attachment) => attachment.status && attachment.status !== 'ready',
    );
    if (blockedAttachment) {
      this.toast(
        blockedAttachment.error || `${blockedAttachment.name} is still being prepared.`,
        'error',
      );
      return;
    }
    const id = uuid();
    const entry: PendingInterjection = {
      id,
      sessionId: session.id,
      content: displayContent || attachments.map((attachment) => attachment.name).join(', '),
      state: 'sending',
    };
    this.setInterjections([...this.interjections.value, entry]);
    try {
      const attachmentParts = await Promise.all(
        attachments.map((attachment) => this.attachmentInput(attachment)),
      );
      const contentParts = options.contentParts?.length
        ? [...options.contentParts, ...(value ? [{ type: 'input_text', text: value }] : [])]
        : [...attachmentParts, ...(value ? [{ type: 'input_text', text: value }] : [])];
      await this.endpoints.interrupt(
        session.id,
        {
          message: displayContent,
          ...(options.contentParts?.length || attachmentParts.length
            ? { content: contentParts }
            : {}),
          interjection_id: id,
          client_message_id: id,
          ...(this.runs.peek()[session.id]?.run.responseId
            ? {
                expected_response_id: this.runs.peek()[session.id].run.responseId,
                expected_run_epoch: this.runs.peek()[session.id].run.epoch,
              }
            : {}),
          delivery: 'steer',
        },
        id,
      );
      options.onTransportStarted?.();
      this.releaseAttachmentResources(attachments, true);
      this.setInterjections(
        this.interjections.value.map((candidate) =>
          candidate.id === id ? { ...candidate, state: 'pending' } : candidate,
        ),
      );
      this.publishSessionChange(
        'run-changed',
        session.id,
        this.runs.peek()[session.id]?.run.responseId || '',
        undefined,
        id,
      );
      if (options.preserveComposer) return;
      const draft = readDrafts(this.storage, this.keys.draftMessages).find(
        (candidate) => candidate.sessionId === session.id,
      );
      const submittedIDs = new Set(
        attachments.map((attachment) => attachment.id).filter((id): id is string => Boolean(id)),
      );
      if (draft?.content.trim() === value)
        saveDraft(this.storage, this.keys.draftMessages, {
          ...draft,
          content: '',
          attachments: (draft.attachments || []).filter(
            (attachment) => !attachment.id || !submittedIDs.has(attachment.id),
          ),
          updated: Date.now(),
        });
      if (this.activeSessionId.peek() === session.id)
        batch(() => {
          if (this.prompt.peek().trim() === value) this.prompt.value = '';
          const submitted = new Set(attachments);
          this.attachments.value = this.attachments
            .peek()
            .filter(
              (attachment) =>
                !submitted.has(attachment) && (!attachment.id || !submittedIDs.has(attachment.id)),
            );
        });
    } catch (error) {
      this.setInterjections(
        this.interjections.value.map((candidate) =>
          candidate.id === id ? { ...candidate, state: 'failed' } : candidate,
        ),
      );
      if (options.onTransportFailed) options.onTransportFailed(error);
      else this.toast(error, 'error');
    }
  }
  async cancelInterjection(id: string): Promise<void> {
    const entry = this.interjections.value.find((candidate) => candidate.id === id);
    if (!entry) return;
    this.setInterjections(this.interjections.value.filter((candidate) => candidate.id !== id));
    try {
      await this.endpoints.deleteInterrupt(entry.sessionId, id);
    } catch (error) {
      this.toast(error, 'error');
    }
  }

  private rollbackOptimisticIntent(sessionId: string, clientMessageId: string): void {
    this.sessions.value = this.sessions.value.map((session) =>
      session.id === sessionId
        ? {
            ...session,
            messages: session.messages.filter(
              (message) => message.clientMessageId !== clientMessageId,
            ),
          }
        : session,
    );
    this.retireIntent(sessionId, clientMessageId);
  }

  private failRun(sessionId: string, error: unknown): void {
    const projection = this.runs.value[sessionId];
    if (!projection) return;
    const message = errorMessage(error);
    this.runs.value = {
      ...this.runs.value,
      [sessionId]: {
        ...projection,
        run: { ...projection.run, status: 'failed', error: message },
        messages: [
          ...projection.messages,
          { id: `error_${uuid()}`, role: 'error', content: message, created: Date.now() },
        ],
      },
    };
    this.toast(message, 'error');
  }

  private retireCommittedIntents(sessionId: string, messages: Message[]): void {
    const committed = new Set(
      messages
        .map((message) => message.clientMessageId)
        .filter((value): value is string => Boolean(value)),
    );
    for (const intent of this.pendingIntents.peek()[sessionId] || [])
      if (committed.has(intent.clientMessageId))
        this.retireIntent(sessionId, intent.clientMessageId);
  }

  private reconcileLoadedIntents(sessionId: string, messages: Message[], active: boolean): void {
    this.retireCommittedIntents(sessionId, messages);
    if (active) return;
    // Legacy optimistic intents can be retired by an authoritative idle
    // transcript. Unknown-outcome sends are different: absence is not proof of
    // rejection, so preserve their durable checking state until the same-key
    // replay is rejected or their client_message_id appears in the transcript.
    for (const intent of this.pendingIntents.peek()[sessionId] || [])
      if (intent.state !== 'checking') this.retireIntent(sessionId, intent.clientMessageId);
  }

  private trackIntent(sessionId: string, intent: PendingIntentRegistry[string][number]): void {
    const persistedIntent = {
      ...intent,
      attachments: intent.attachments?.map(
        ({ file: _file, dataURL: _dataURL, previewURL: _previewURL, ...attachment }) => attachment,
      ),
    };
    const registry = {
      ...this.pendingIntents.value,
      [sessionId]: [
        ...(this.pendingIntents.value[sessionId] || []).filter(
          (entry) => entry.clientMessageId !== intent.clientMessageId,
        ),
        persistedIntent,
      ],
    };
    this.pendingIntents.value = registry;
    try {
      persistPendingIntent(this.storage, this.keys.pendingIntents, sessionId, persistedIntent);
    } catch (error) {
      this.toast(
        `Your message is queued in this tab but could not be saved: ${errorMessage(error)}`,
        'error',
      );
    }
  }
  private retireIntent(sessionId: string, clientMessageId = ''): void {
    const registry = { ...this.pendingIntents.value };
    if (clientMessageId)
      registry[sessionId] = (registry[sessionId] || []).filter(
        (entry) => entry.clientMessageId !== clientMessageId,
      );
    else delete registry[sessionId];
    if (!registry[sessionId]?.length) delete registry[sessionId];
    this.pendingIntents.value = registry;
    if (clientMessageId)
      this.storage.removeItem(
        `${this.keys.pendingIntents}:${encodeURIComponent(sessionId)}:${encodeURIComponent(clientMessageId)}`,
      );
    else removeSessionPendingIntents(this.storage, this.keys.pendingIntents, sessionId);
  }

  async attachmentInput(attachment: Attachment): Promise<Record<string, unknown>> {
    if (attachment.status && attachment.status !== 'ready')
      throw new Error(attachment.error || `${attachment.name} is not ready to send.`);
    const data = attachment.dataURL || attachment.url || '';
    if (!data) throw new Error(`Could not materialize ${attachment.name}`);
    if (attachment.type.startsWith('image/'))
      return {
        type: 'input_image',
        image_url: data,
        filename: attachment.name,
        ...(attachment.width && attachment.height
          ? { width: attachment.width, height: attachment.height }
          : {}),
      };
    return { type: 'input_file', file_data: data, filename: attachment.name };
  }

  addAttachments(files: FileList | File[]): void {
    let count = this.attachments.peek().length;
    for (const file of Array.from(files)) {
      const validation = validateAttachmentFile(file, count, this.attachmentPolicy.peek());
      if (validation) {
        this.toast(validation.message, 'error');
        continue;
      }
      count += 1;
      const attachment: Attachment = {
        id: uuid(),
        name: file.name,
        type: file.type || 'application/octet-stream',
        size: file.size,
        file,
        status: 'preparing',
        progress: 0,
        draftId: this.draftStorageID(),
      };
      this.attachments.value = [...this.attachments.peek(), attachment];
      void this.prepareAttachment(attachment);
    }
  }

  private async prepareAttachment(attachment: Attachment): Promise<void> {
    if (!attachment.id || !attachment.file) return;
    const attachmentId = attachment.id;
    const draftId = attachment.draftId || this.draftStorageID();
    const generation = (this.attachmentGenerations.get(attachmentId) || 0) + 1;
    this.attachmentGenerations.set(attachmentId, generation);
    const owns = (): boolean =>
      !this.disposed &&
      this.draftStorageID() === draftId &&
      this.attachmentGenerations.get(attachmentId) === generation &&
      this.attachments.peek().some((entry) => entry.id === attachmentId);
    const source = attachment.file;
    let previewURL = '';
    try {
      const dataURL = await blobToDataURL(source);
      if (!owns()) return;
      this.updateAttachment(attachmentId, { progress: 0.5 });
      let width: number | undefined;
      let height: number | undefined;
      if (source.type.startsWith('image/')) {
        previewURL = URL.createObjectURL(source);
        const dimensions = await new Promise<{ width: number; height: number }>(
          (resolve, reject) => {
            const image = new Image();
            image.onload = () =>
              resolve({ width: image.naturalWidth, height: image.naturalHeight });
            image.onerror = () => reject(new Error(`Could not decode ${source.name} as an image.`));
            image.src = previewURL;
          },
        );
        width = dimensions.width;
        height = dimensions.height;
      }
      const checksum = await blobChecksum(source);
      if (!owns()) {
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      await this.draftBlobs.put({
        id: attachmentId,
        draftId,
        blob: source,
        mime: source.type,
        size: source.size,
        checksum,
        updated: Date.now(),
      });
      if (!owns()) {
        await this.draftBlobs.delete(attachmentId);
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      this.updateAttachment(attachmentId, {
        dataURL,
        previewURL: previewURL || undefined,
        width,
        height,
        checksum,
        blobRef: attachment.id,
        status: 'ready',
        progress: 1,
        error: '',
      });
      this.persistCurrentDraft();
    } catch (error) {
      if (previewURL) URL.revokeObjectURL(previewURL);
      if (!owns()) return;
      this.updateAttachment(attachmentId, {
        status: 'error',
        error: errorMessage(error),
        progress: 0,
      });
      this.toast(error, 'error');
      this.persistCurrentDraft();
    }
  }

  private updateAttachment(id: string, patch: Partial<Attachment>): void {
    this.attachments.value = this.attachments
      .peek()
      .map((entry) => (entry.id === id ? { ...entry, ...patch } : entry));
  }

  private async restoreAttachmentBlob(draftId: string, id: string, blobRef: string): Promise<void> {
    const generation = (this.attachmentGenerations.get(id) || 0) + 1;
    this.attachmentGenerations.set(id, generation);
    const owns = (): boolean =>
      !this.disposed &&
      this.draftStorageID() === draftId &&
      this.attachmentGenerations.get(id) === generation &&
      this.attachments.peek().some((entry) => entry.id === id);
    try {
      const record = await this.draftBlobs.get(blobRef);
      if (!owns()) return;
      if (!record || record.draftId !== draftId) {
        // The draft metadata can outlive IndexedDB data after browser storage
        // eviction or a partial clear. Drop that stale attachment instead of
        // leaving an unusable error chip in the composer on every reload.
        this.attachmentGenerations.set(id, generation + 1);
        this.attachments.value = this.attachments.peek().filter((entry) => entry.id !== id);
        this.persistCurrentDraft();
        return;
      }
      const dataURL = await blobToDataURL(record.blob);
      const previewURL = record.mime.startsWith('image/')
        ? URL.createObjectURL(record.blob)
        : undefined;
      if (!owns()) {
        if (previewURL) URL.revokeObjectURL(previewURL);
        return;
      }
      this.updateAttachment(id, { dataURL, previewURL, status: 'ready', progress: 1, error: '' });
    } catch (error) {
      if (!owns()) return;
      this.updateAttachment(id, { status: 'error', error: errorMessage(error), progress: 0 });
      this.toast(error, 'error');
    }
  }

  retryAttachment(id: string | undefined): void {
    const attachment = this.attachments.peek().find((entry) => entry.id === id);
    if (!attachment?.file || !attachment.id) return;
    const validation = validateAttachmentFile(
      attachment.file,
      this.attachments.peek().filter((entry) => entry.id !== attachment.id).length,
      this.attachmentPolicy.peek(),
    );
    if (validation) {
      this.updateAttachment(attachment.id, {
        status: 'error',
        error: validation.message,
        progress: 0,
      });
      this.toast(validation.message, 'error');
      return;
    }
    this.updateAttachment(attachment.id, { status: 'preparing', error: '', progress: 0 });
    void this.prepareAttachment(attachment);
  }

  private releaseAttachmentResources(attachments: Attachment[], deleteBlobs: boolean): void {
    for (const attachment of attachments) {
      if (attachment.id)
        this.attachmentGenerations.set(
          attachment.id,
          (this.attachmentGenerations.get(attachment.id) || 0) + 1,
        );
      if (attachment.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
      if (deleteBlobs && attachment.blobRef)
        void this.draftBlobs
          .delete(attachment.blobRef)
          .catch((error) => this.toast(error, 'error'));
    }
  }

  removeAttachment(id: string | undefined): void {
    const attachment = this.attachments.value.find((entry) => entry.id === id);
    if (id) this.attachmentGenerations.set(id, (this.attachmentGenerations.get(id) || 0) + 1);
    if (attachment?.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
    this.attachments.value = this.attachments.value.filter((entry) => entry.id !== id);
    if (attachment?.blobRef)
      void this.draftBlobs.delete(attachment.blobRef).catch((error) => this.toast(error, 'error'));
    this.persistCurrentDraft();
  }

  async search(query: string): Promise<void> {
    this.sidebarSearch.value = query;
    this.searchAbort?.abort();
    clearTimeout(this.searchTimer);
    if (!query.trim()) {
      this.searchResults.value = null;
      this.searchLoading.value = false;
      this.searchError.value = '';
      return;
    }
    this.searchLoading.value = true;
    this.searchResults.value = [];
    this.searchError.value = '';
    await new Promise<void>((resolve) => {
      this.searchTimer = window.setTimeout(resolve, 180);
    });
    if (this.sidebarSearch.peek() !== query) return;
    const abort = new AbortController();
    this.searchAbort = abort;
    try {
      const data = await this.endpoints.searchSessions(
        query,
        this.showHidden.value,
        this.config.sidebarCategories,
        abort.signal,
      );
      if (!abort.signal.aborted && this.sidebarSearch.peek() === query)
        this.searchResults.value = listFrom(data, 'sessions', 'items').map((entry) =>
          this.sessionFrom(entry),
        );
    } catch (error) {
      if (!abort.signal.aborted) {
        this.searchResults.value = null;
        this.searchError.value =
          error instanceof Error ? error.message : 'Could not search conversations';
      }
    } finally {
      if (!abort.signal.aborted) this.searchLoading.value = false;
    }
  }

  async mutateSession(session: Session, patch: Record<string, unknown>): Promise<void> {
    await this.endpoints.patchSession(session.id, patch);
    this.sessions.value = this.sessions.value.map((entry) =>
      entry.id === session.id ? ({ ...entry, ...patch } as Session) : entry,
    );
    await this.refreshSidebar();
    this.publishSessionChange();
  }
  async archiveSession(session: Session): Promise<void> {
    const archived = !session.archived;
    await this.endpoints.patchSession(session.id, { archived });
    const keepVisible = this.showHidden.peek() || !archived;
    const reconcile = (entries: Session[]): Session[] =>
      entries.flatMap((entry) => {
        if (entry.id !== session.id) return [entry];
        return keepVisible ? [{ ...entry, archived }] : [];
      });
    this.sessions.value = reconcile(this.sessions.peek());
    this.projects.value = this.projects.peek().map((project) => {
      const contained = Boolean(project.sessions?.some((entry) => entry.id === session.id));
      if (!contained) return project;
      return {
        ...project,
        sessions: reconcile(project.sessions || []),
        sessionCount:
          !keepVisible && project.sessionCount != null
            ? Math.max(0, project.sessionCount - 1)
            : project.sessionCount,
      };
    });
    if (this.searchResults.peek()) this.searchResults.value = reconcile(this.searchResults.peek()!);
    if (session.id === this.activeSessionId.value && archived) this.newChat();
    this.publishSessionChange();
  }
  async pinSession(session: Session): Promise<void> {
    await this.mutateSession(session, { pinned: !session.pinned });
  }
  openRename(session: Session): void {
    this.renameTarget.value = session;
    this.modal.value = 'rename';
  }
  openProjectPicker(session: Session): void {
    this.projectTarget.value = session;
    this.modal.value = 'project';
  }
  openAddProject(): void {
    this.projectTarget.value = null;
    this.modal.value = 'project';
  }
  async assignProject(projectId: string): Promise<Record<string, unknown> | null> {
    const session = this.projectTarget.value;
    if (!session) return null;
    const response = await this.endpoints.setProject(session.id, { project_id: projectId });
    await this.refreshSidebar();
    this.publishSessionChange();
    this.modal.value = '';
    return response;
  }
  async createProjectFromWorkspace(name: string): Promise<Record<string, unknown> | null> {
    const session = this.projectTarget.value;
    if (!session) return null;
    const response = await this.endpoints.setProject(session.id, {
      create_from_workspace: true,
      name: name.trim(),
    });
    await this.refreshSidebar();
    this.publishSessionChange();
    this.modal.value = '';
    return response;
  }
  async renameSession(
    change: { name: string } | { generatedShortTitle: string; generatedLongTitle: string },
  ): Promise<void> {
    const session = this.renameTarget.value;
    if (!session) return;
    const patch =
      'name' in change
        ? { name: change.name.trim() }
        : {
            name: '',
            generated_short_title: change.generatedShortTitle.trim(),
            generated_long_title: change.generatedLongTitle.trim(),
          };
    await this.endpoints.patchSession(session.id, patch);
    await this.refreshSidebar();
    this.publishSessionChange();
    this.renameTarget.value = null;
    this.modal.value = '';
  }
  async improveTitle(): Promise<{ title: string; detail: string }> {
    const session = this.renameTarget.value;
    if (!session) return { title: '', detail: '' };
    const data = await this.endpoints.refineTitle(session.id);
    return {
      title: String(data.generated_short_title || data.short_title || session.title || ''),
      detail: String(data.generated_long_title || data.long_title || session.longTitle || ''),
    };
  }

  setPreference(
    name: 'provider' | 'model' | 'effort' | 'reasoning' | 'agent',
    value: string,
    commit = true,
  ): void {
    const map = {
      provider: [this.selectedProvider, this.keys.selectedProvider],
      model: [this.selectedModel, this.keys.selectedModel],
      effort: [this.selectedEffort, this.keys.selectedEffort],
      reasoning: [this.selectedReasoningMode, this.keys.selectedReasoningMode],
      agent: [this.selectedAgent, this.keys.selectedAgent],
    } as const;
    const [target, key] = map[name];
    const changed = target.peek() !== value;
    target.value = value;
    if (value) this.storage.setItem(key, value);
    else this.storage.removeItem(key);
    if (name === 'provider' && changed) {
      this.selectedModel.value = '';
      this.storage.removeItem(this.keys.selectedModel);
      const provider = this.providers.peek().find((entry) => entry.id === value);
      const fallback = Array.isArray(provider?.models) ? provider.models : [];
      this.models.value = fallback
        .map((entry) => {
          const source: Record<string, unknown> =
            entry && typeof entry === 'object' ? (entry as Record<string, unknown>) : { id: entry };
          const id = String(source.id || source.name || '');
          return {
            ...source,
            id,
            name: String(source.display_name || source.name || id),
            provider: value,
          } as RuntimeOption;
        })
        .filter((entry) => entry.id);
      void this.loadModels().catch((error) => this.toast(error, 'error'));
    }
    if (commit && name === 'effort' && this.streaming.value && this.activeSession.value) {
      void this.endpoints
        .runtime(this.activeSession.value.id, 'effort', {
          model: this.selectedModel.value || this.activeSession.value.activeModel,
          provider: this.selectedProvider.value || this.activeSession.value.activeProvider,
          reasoning_effort: value,
        })
        .catch((error) => this.toast(error, 'error'));
    }
  }
  saveSettings(token: string): void {
    this.token.value = token.trim();
    if (this.token.value) this.storage.setItem(this.keys.token, this.token.value);
    else this.storage.removeItem(this.keys.token);
    syncTokenCookie(this.config.prefix, this.token.value);
    this.authRequired.value = false;
    this.modal.value = '';
    void this.bootstrap();
  }
  async enableNotifications(): Promise<void> {
    await this.notificationController.enable();
  }
  async retryNotifications(): Promise<void> {
    await this.notificationController.reconcile({ repair: true });
  }
  async disableNotifications(): Promise<void> {
    await this.notificationController.disable();
  }

  private upsertInteraction(
    kind: InteractionRecord['kind'],
    sessionId: string,
    responseId: string,
    requestId: string,
    prompt: ApprovalPrompt | AskUserPrompt,
  ): string {
    const key = `${sessionId}:${responseId}:${requestId}`;
    const existing = this.interactions.peek()[key];
    const record: InteractionRecord = existing || {
      key,
      sessionId,
      responseId,
      requestId,
      kind,
      state: 'waiting',
      order: this.interactionOrder.peek().length,
      createdAt: Date.now(),
      prompt,
    };
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...record, prompt },
    };
    if (!existing) this.interactionOrder.value = [...this.interactionOrder.peek(), key];
    this.publishSessionChange('interaction-changed', sessionId, responseId);
    return key;
  }

  private resolveInteractionRecord(
    kind: InteractionRecord['kind'],
    sessionId: string,
    responseId: string,
    requestId: string,
    outcome: string,
    resolvedAt = Date.now(),
  ): void {
    const existing = this.interactionFor(kind, sessionId, requestId, responseId);
    const key = existing?.key || `${sessionId}:${responseId}:${requestId}`;
    const normalized = outcome.replaceAll('_', '-');
    const state: InteractionRecord['state'] =
      normalized === 'accepted' || normalized === 'answered'
        ? 'accepted'
        : normalized === 'denied'
          ? 'denied'
          : normalized === 'cancelled-by-user' || normalized === 'cancelled'
            ? 'cancelled-by-user'
            : normalized === 'failed'
              ? 'failed'
              : 'cancelled-by-agent';
    const prompt =
      existing?.prompt ||
      (kind === 'approval'
        ? ({ sessionId, id: requestId, title: 'Access request' } satisfies ApprovalPrompt)
        : ({ sessionId, callId: requestId, questions: [] } satisfies AskUserPrompt));
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: {
        key,
        sessionId,
        responseId,
        requestId,
        kind,
        order: existing?.order ?? this.interactionOrder.peek().length,
        createdAt: existing?.createdAt || resolvedAt,
        prompt,
        ...existing,
        state,
        outcome,
        resolvedAt,
      },
    };
    if (!existing) this.interactionOrder.value = [...this.interactionOrder.peek(), key];
    this.bumpDiagnostic('interactionReconciliations');
    if (kind === 'approval' && this.approval.peek()?.id === requestId) this.approval.value = null;
    if (kind === 'ask-user' && this.askUser.peek()?.callId === requestId) this.askUser.value = null;
  }

  private interactionFor(
    kind: InteractionRecord['kind'],
    sessionId: string,
    requestId: string,
    responseId = '',
  ): InteractionRecord | null {
    return (
      Object.values(this.interactions.peek()).find(
        (entry) =>
          entry.kind === kind &&
          entry.sessionId === sessionId &&
          entry.requestId === requestId &&
          (!responseId || entry.responseId === responseId),
      ) || null
    );
  }

  private shouldOpenInteraction(
    kind: InteractionRecord['kind'],
    sessionId: string,
    requestId: string,
  ): boolean {
    const state = this.interactionFor(kind, sessionId, requestId)?.state;
    return state === 'waiting' || state === 'failed';
  }

  dismissInteraction(
    kind: InteractionRecord['kind'],
    promptOverride?: ApprovalPrompt | AskUserPrompt,
  ): void {
    const prompt =
      promptOverride || (kind === 'ask-user' ? this.askUser.peek() : this.approval.peek());
    const requestId =
      kind === 'ask-user'
        ? (prompt as AskUserPrompt | null)?.callId
        : (prompt as ApprovalPrompt | null)?.id;
    if (requestId) {
      const record = this.interactionFor(kind, prompt?.sessionId || '', requestId);
      if (record)
        this.interactions.value = {
          ...this.interactions.peek(),
          [record.key]: { ...record, state: 'dismissed' },
        };
    }
    if (kind === 'ask-user') this.askUser.value = null;
    else this.approval.value = null;
    this.modal.value = '';
  }

  openInteraction(key: string): void {
    const record = this.interactions.peek()[key];
    if (!record || !['waiting', 'dismissed', 'failed'].includes(record.state)) return;
    if (record.kind === 'ask-user') {
      this.askUser.value = record.prompt as AskUserPrompt;
      this.modal.value = 'ask-user';
    } else {
      this.approval.value = record.prompt as ApprovalPrompt;
      this.modal.value = 'approval';
    }
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...record, state: 'waiting' },
    };
  }

  async answerAskUser(
    answers: unknown = [],
    cancelled = false,
    promptOverride?: AskUserPrompt,
  ): Promise<void> {
    const prompt = promptOverride || this.askUser.value;
    const requestId = prompt?.callId;
    if (!prompt || !requestId) return;
    const record = this.interactionFor('ask-user', prompt.sessionId, requestId);
    const key =
      record?.key || this.upsertInteraction('ask-user', prompt.sessionId, '', requestId, prompt);
    const existing = this.interactionSubmissions.get(key);
    if (existing) return existing;
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...this.interactions.peek()[key], state: 'submitting', error: '' },
    };
    const request = (async () => {
      try {
        const result = (await this.endpoints.askUser(
          prompt.sessionId,
          cancelled ? { call_id: requestId, cancelled: true } : { call_id: requestId, answers },
          requestId,
        )) as Record<string, unknown>;
        const authoritative = String(result.status || '') === 'already_resolved';
        this.resolveInteractionRecord(
          'ask-user',
          prompt.sessionId,
          record?.responseId || '',
          requestId,
          authoritative
            ? String(result.outcome || 'resolved')
            : cancelled
              ? 'cancelled-by-user'
              : 'answered',
          authoritative ? Number(result.resolved_at) || Date.now() : Date.now(),
        );
      } catch (error) {
        const current = this.interactions.peek()[key];
        this.interactions.value = {
          ...this.interactions.peek(),
          [key]: { ...current, state: 'failed', error: errorMessage(error) },
        };
        throw error;
      }
    })();
    this.interactionSubmissions.set(key, request);
    try {
      await request;
    } finally {
      if (this.interactionSubmissions.get(key) === request) this.interactionSubmissions.delete(key);
    }
  }

  async decideApproval(
    choice: number,
    resumeAuto = false,
    promptOverride?: ApprovalPrompt,
    cancelled = false,
  ): Promise<void> {
    const prompt = promptOverride || this.approval.value;
    const requestId = prompt?.id;
    if (!prompt || !requestId) return;
    const record = this.interactionFor('approval', prompt.sessionId, requestId);
    const key =
      record?.key || this.upsertInteraction('approval', prompt.sessionId, '', requestId, prompt);
    const existing = this.interactionSubmissions.get(key);
    if (existing) return existing;
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...this.interactions.peek()[key], state: 'submitting', error: '' },
    };
    const denied = prompt.options?.find((option) => option.index === choice)?.choice === 'deny';
    const request = (async () => {
      try {
        const result = (await this.endpoints.approval(
          prompt.sessionId,
          cancelled
            ? { approval_id: requestId, cancelled: true }
            : { approval_id: requestId, choice, resume_auto: resumeAuto },
          requestId,
        )) as Record<string, unknown>;
        const authoritative = String(result.status || '') === 'already_resolved';
        this.resolveInteractionRecord(
          'approval',
          prompt.sessionId,
          record?.responseId || '',
          requestId,
          authoritative
            ? String(result.outcome || 'resolved')
            : cancelled
              ? 'cancelled-by-user'
              : denied
                ? 'denied'
                : 'accepted',
          authoritative ? Number(result.resolved_at) || Date.now() : Date.now(),
        );
      } catch (error) {
        const current = this.interactions.peek()[key];
        this.interactions.value = {
          ...this.interactions.peek(),
          [key]: { ...current, state: 'failed', error: errorMessage(error) },
        };
        throw error;
      }
    })();
    this.interactionSubmissions.set(key, request);
    try {
      await request;
    } finally {
      if (this.interactionSubmissions.get(key) === request) this.interactionSubmissions.delete(key);
    }
  }

  private resetSideQuestion(): void {
    const state = this.sideQuestion.peek();
    const owner = state.sessionId;
    this.sideQuestionEpoch += 1;
    this.sideQuestionAbort?.abort();
    this.sideQuestionAbort = null;
    if (state.running && owner)
      void this.endpoints.cancelSideQuestion(owner).catch(() => undefined);
    if (this.modal.peek() === 'side') this.modal.value = '';
    this.sideQuestion.value = {
      sessionId: '',
      loading: false,
      running: false,
      draft: '',
      question: '',
      response: '',
      error: '',
      history: [],
    };
  }

  openSideQuestion(question = ''): boolean {
    const session = this.activeSession.peek();
    if (!session || this.draftActive.peek()) {
      this.toast('Start the conversation before asking a side question.', 'error');
      return false;
    }
    const value = question.trim();
    if (this.sideQuestion.peek().sessionId !== session.id) this.resetSideQuestion();
    this.modal.value = 'side';
    if (value) {
      void this.askSideQuestion(value);
      return true;
    }
    const epoch = ++this.sideQuestionEpoch;
    this.sideQuestion.value = {
      ...this.sideQuestion.peek(),
      sessionId: session.id,
      loading: true,
      error: '',
    };
    void this.recoverSideQuestion(session.id, epoch);
    return true;
  }

  setSideQuestionDraft(value: string): void {
    this.sideQuestion.value = { ...this.sideQuestion.peek(), draft: value };
  }

  async recoverSideQuestion(
    sessionId = this.activeSession.peek()?.id || '',
    epoch = ++this.sideQuestionEpoch,
  ): Promise<void> {
    if (!sessionId) return;
    try {
      const value = await this.endpoints.sideQuestionState(sessionId);
      if (
        epoch !== this.sideQuestionEpoch ||
        this.activeSessionId.peek() !== sessionId ||
        this.sideQuestion.peek().sessionId !== sessionId
      )
        return;
      const running = Boolean(value.running);
      this.sideQuestion.value = {
        ...this.sideQuestion.peek(),
        sessionId,
        loading: false,
        running,
        question: running ? String(value.question || '') : '',
        response: running ? String(value.response || '') : '',
        error: String(value.error || ''),
        history: Array.isArray(value.history)
          ? (value.history as SideQuestionState['history'])
          : [],
      };
    } catch (error) {
      if (epoch !== this.sideQuestionEpoch) return;
      this.sideQuestion.value = {
        ...this.sideQuestion.peek(),
        loading: false,
        error: errorMessage(error),
      };
    }
  }

  async askSideQuestion(question: string): Promise<void> {
    const session = this.activeSession.peek();
    const value = question.trim();
    if (!session || !value) return;
    if (this.sideQuestion.peek().running) {
      this.sideQuestion.value = {
        ...this.sideQuestion.peek(),
        error: 'A side question is already running.',
      };
      return;
    }
    this.sideQuestionAbort?.abort();
    const controller = new AbortController();
    const epoch = ++this.sideQuestionEpoch;
    this.sideQuestionAbort = controller;
    this.sideQuestion.value = {
      ...this.sideQuestion.peek(),
      sessionId: session.id,
      loading: false,
      running: true,
      draft: '',
      question: value,
      response: '',
      error: '',
    };
    try {
      const response = await this.endpoints.startSideQuestion(session.id, value);
      if (!response.ok || !response.body)
        throw new Error((await response.text()) || `Side question failed (${response.status})`);
      let answer = '';
      let generation = Number(response.headers.get('x-side-generation') || 0);
      for await (const frame of decodeSSE(response.body, controller.signal)) {
        if (epoch !== this.sideQuestionEpoch || this.activeSessionId.peek() !== session.id) return;
        let event: Record<string, unknown>;
        try {
          event = JSON.parse(frame.data) as Record<string, unknown>;
        } catch {
          continue;
        }
        const eventGeneration = Number(event.generation || 0);
        if (!generation && eventGeneration) generation = eventGeneration;
        if (generation && eventGeneration && eventGeneration !== generation) continue;
        if (event.type === 'text_delta') answer += String(event.text || '');
        else if (event.type === 'attempt_discard') answer = '';
        else if (event.type === 'done' && recordValue(event.result))
          answer = String(recordValue(event.result)?.response || answer);
        this.sideQuestion.value = { ...this.sideQuestion.peek(), response: answer };
      }
      if (!controller.signal.aborted && epoch === this.sideQuestionEpoch)
        await this.recoverSideQuestion(session.id, epoch);
    } catch (error) {
      if (!controller.signal.aborted && epoch === this.sideQuestionEpoch) {
        void this.endpoints.cancelSideQuestion(session.id).catch(() => undefined);
        this.sideQuestion.value = {
          ...this.sideQuestion.peek(),
          loading: false,
          running: false,
          error: errorMessage(error),
        };
      }
    } finally {
      if (this.sideQuestionAbort === controller) this.sideQuestionAbort = null;
    }
  }

  cancelSideQuestion(): void {
    const state = this.sideQuestion.peek();
    const owner = state.sessionId;
    const wasRunning = state.running;
    this.sideQuestionEpoch += 1;
    this.sideQuestionAbort?.abort();
    this.sideQuestionAbort = null;
    this.sideQuestion.value = {
      ...state,
      loading: false,
      running: false,
      question: '',
      response: '',
      error: '',
    };
    if (wasRunning && owner) void this.endpoints.cancelSideQuestion(owner).catch(() => undefined);
  }

  closeSideQuestion(): void {
    if (this.modal.peek() === 'side') this.modal.value = '';
    if (this.sideQuestion.peek().running || this.sideQuestion.peek().loading)
      this.cancelSideQuestion();
  }

  async loadMCP(): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    this.mcp.value = { ...this.mcp.value, loading: true, error: '' };
    try {
      const data = await this.endpoints.getMCP(session.id);
      const state = normalizeMCPState(data);
      this.mcp.value = { ...state, loading: false, pending: '', error: '' };
      this.sessions.value = this.sessions.value.map((current) =>
        current.id === session.id ? { ...current, mcpEnabled: state.enabled } : current,
      );
    } catch (error) {
      this.mcp.value = {
        ...this.mcp.value,
        loading: false,
        error: errorMessage(error),
      };
    }
  }
  async toggleMCP(name: string): Promise<void> {
    const session = this.activeSession.value;
    if (!session || this.mcp.value.pending) return;
    const previous = this.mcp.value.enabled;
    const enabled = previous.includes(name)
      ? previous.filter((entry) => entry !== name)
      : [...previous, name];
    this.mcp.value = { ...this.mcp.value, enabled, pending: name, error: '' };
    try {
      const data = await this.endpoints.setMCP(session.id, enabled);
      const state = normalizeMCPState(data);
      this.mcp.value = { ...state, loading: false, pending: '', error: '' };
      this.sessions.value = this.sessions.value.map((current) =>
        current.id === session.id ? { ...current, mcpEnabled: state.enabled } : current,
      );
    } catch (error) {
      this.mcp.value = {
        ...this.mcp.value,
        enabled: previous,
        pending: '',
        error: errorMessage(error),
      };
    }
  }
  async saveGoal(goal: Goal | { action: string }): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const request =
      'objective' in goal
        ? { action: 'set', objective: goal.objective, token_budget: goal.token_budget }
        : goal;
    await this.endpoints.goal(session.id, request);
    if ('objective' in goal) this.goal.value = goal;
    else if (goal.action === 'clear') this.goal.value = null;
    else if (this.goal.value)
      this.goal.value = {
        ...this.goal.value,
        status: goal.action === 'pause' ? 'paused' : 'active',
      };
    this.modal.value = '';
  }

  async shareLocation(): Promise<void> {
    if (!this.config.locationSharing || !navigator.geolocation)
      return this.toast('Location sharing is unavailable.', 'error');
    const position = await new Promise<GeolocationPosition>((resolve, reject) =>
      navigator.geolocation.getCurrentPosition(resolve, reject, {
        enableHighAccuracy: false,
        timeout: 10_000,
      }),
    );
    this.prompt.value += `${this.prompt.value ? '\n' : ''}Current location: ${position.coords.latitude.toFixed(6)}, ${position.coords.longitude.toFixed(6)}`;
  }

  async refreshBranchTree(
    sessionId = this.activeSessionId.peek(),
  ): Promise<Record<string, unknown> | null> {
    if (!sessionId) {
      this.branchTree.value = null;
      this.branchPathCount.value = 0;
      return null;
    }
    try {
      const tree = await this.endpoints.tree(sessionId);
      if (this.activeSessionId.peek() !== sessionId) return null;
      this.branchTree.value = tree;
      this.branchPathCount.value = Math.max(1, Number(tree.path_count) || 1);
      return tree;
    } catch {
      if (this.activeSessionId.peek() === sessionId) {
        this.branchTree.value = null;
        this.branchPathCount.value = 0;
      }
      return null;
    }
  }
  async loadBranchTree(): Promise<void> {
    const tree = await this.refreshBranchTree();
    if (tree && this.branchPathCount.value > 1) this.modal.value = 'branch';
  }
  async branchCommand(kind: 'fork' | 'thread', message = ''): Promise<void> {
    const session = this.activeSession.value;
    if (!session || this.draftActive.value) {
      this.toast('Start the conversation before creating a thread or fork.', 'error');
      return;
    }
    if (this.attachments.value.length) {
      this.toast('Create the thread or fork before attaching files or images.', 'error');
      return;
    }
    const anchor =
      kind === 'thread'
        ? 0
        : [...this.visibleMessages.value].reverse().find((entry) => Number(entry.durableRowId) > 0)
            ?.durableRowId || 0;
    const original = this.prompt.value;
    this.prompt.value = '';
    try {
      await this.branchFrom(String(anchor), 'clean', '', message.trim());
    } catch (error) {
      if (!this.prompt.value) this.prompt.value = original;
      this.toast(error, 'error');
    }
  }
  openBranchContext(messageId: string): void {
    this.branchTarget.value = messageId;
    this.modal.value = 'branch-context';
  }
  async branchFrom(messageId: string, context: string, focus = '', autoSend = ''): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const data = await this.endpoints.branch(session.id, {
      anchor_message_id: Number(messageId) || 0,
      expected_rev: session.transcriptRev || 0,
      idempotency_key: uuid(),
    });
    const child =
      data.session && typeof data.session === 'object'
        ? (data.session as Record<string, unknown>)
        : data;
    const id = String(child.id || data.session_id || '');
    if (id && (context === 'notes' || context === 'focused'))
      await this.endpoints.pathNotes(id, {
        mode: context,
        ...(context === 'focused' ? { focus } : {}),
      });
    await this.refreshSidebar();
    this.publishSessionChange();
    let target = this.sessions.value.find((entry) => entry.id === id);
    if (!target && id) {
      target = this.sessionFrom({ ...child, id });
      this.sessions.value = [target, ...this.sessions.value];
    }
    if (target) {
      await this.selectSession(target);
      if (autoSend) {
        this.prompt.value = autoSend;
        await this.send();
      }
    }
    this.modal.value = '';
  }

  async refreshDiffComments(sessionId = this.activeSessionId.peek()): Promise<void> {
    if (!sessionId) return;
    try {
      const data = await this.endpoints.diffComments(sessionId);
      if (this.diff.peek().sessionId !== sessionId) return;
      const incoming = listFrom(data, 'comments', 'items')
        .map((entry): DiffComment | null => {
          const raw = recordValue(entry.diff_comment) || entry;
          const side = String(raw.side || '').toLowerCase();
          const path = String(raw.path || '');
          const body = String(raw.instruction || raw.body || '').trim();
          const line = Number(raw.line) || 0;
          if (!path || !body || !line || (side !== 'old' && side !== 'new')) return null;
          return {
            id: String(raw.id || '') || undefined,
            parentId: String(raw.parent_id || '') || undefined,
            path,
            side,
            line,
            body,
            sessionId,
            scope: String(raw.scope || '') || undefined,
            context: String(raw.line_text ?? raw.context ?? ''),
            fileChangeSeq: Number(raw.file_change_seq ?? raw.fileChangeSeq) || 0,
            clientMessageId:
              String(entry.client_message_id || raw.client_message_id || '') || undefined,
            createdAt: Number(entry.created_at || raw.created_at) || Date.now(),
          };
        })
        .filter((comment): comment is DiffComment => comment !== null);
      const ids = new Set(incoming.map((comment) => comment.id).filter(Boolean));
      const pending = this.diff
        .peek()
        .historyComments.filter(
          (comment) =>
            comment.sessionId === sessionId && comment.optimistic && !ids.has(comment.id),
        );
      this.diff.value = { ...this.diff.peek(), historyComments: [...incoming, ...pending] };
    } catch {
      /* Keep the optimistic trail visible; the status loop will reconcile it. */
    }
  }

  openPlan(): void {
    const plan = this.currentPlan.peek();
    if (!plan) return;
    this.diff.value = { ...this.diff.peek(), open: false };
    this.planSeen.value = planSummary(plan).signature;
    this.planOpen.value = true;
  }

  closePlan(): void {
    this.planOpen.value = false;
  }

  async toggleDiff(): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    this.planOpen.value = false;
    this.diff.value = { ...this.diff.value, sessionId: session.id, open: !this.diff.value.open };
    this.startStatusPoll();
    if (this.diff.value.open) await this.loadDiff();
  }
  async loadDiff(): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const owner = session.id;
    const scope = normalizeDiffScope(this.diff.value.scope);
    const epoch = ++this.diffLoadEpoch;
    this.diff.value = { ...this.diff.value, sessionId: owner, scope, loading: true, error: '' };
    try {
      void this.refreshDiffComments(owner);
      const data = await this.endpoints.fileChanges(owner, scope);
      const state = this.diff.peek();
      if (
        epoch !== this.diffLoadEpoch ||
        this.activeSessionId.peek() !== owner ||
        state.sessionId !== owner ||
        normalizeDiffScope(state.scope) !== scope
      )
        return;
      const existing = new Map(state.files.map((file) => [file.path, file]));
      const refreshContexts = new Map<string, number>();
      const files = sortDiffFiles(
        listFrom(data, 'file_changes', 'files', 'changes')
          .map((entry): DiffFile => {
            const path = String(entry.path || '');
            const previous = existing.get(path);
            const sequence = Number(entry.seq ?? entry.sequence) || 0;
            const snapshotSeq = Number(entry.snapshot_seq) || 0;
            const sameContent = Boolean(
              previous &&
              (previous.sequence || 0) === sequence &&
              (previous.snapshotSeq || 0) === snapshotSeq,
            );
            if (previous?.expanded && !sameContent)
              refreshContexts.set(path, previous.context || 0);
            return {
              path,
              old_path: String(entry.old_path || ''),
              status: String(entry.status || entry.kind || ''),
              additions: Number(entry.adds ?? entry.additions) || 0,
              deletions: Number(entry.dels ?? entry.deletions) || 0,
              binary: Boolean(entry.binary || entry.is_binary),
              image: Boolean(entry.image || entry.is_image),
              beforeURL: String(
                entry.before_url || entry.old_url || (sameContent ? previous?.beforeURL : '') || '',
              ),
              afterURL: String(
                entry.after_url || entry.new_url || (sameContent ? previous?.afterURL : '') || '',
              ),
              lastChangedAt: Number(entry.last_changed_at || entry.updated_at) || 0,
              sequence,
              snapshotSeq,
              truncated: Boolean(entry.truncated),
              provenance: String(entry.provenance || '') as DiffFile['provenance'],
              provenances: Array.isArray(entry.provenances) ? entry.provenances.map(String) : [],
              baselineState: String(entry.baseline_state || '') as DiffFile['baselineState'],
              contentStatus: String(entry.content_status || ''),
              contentAvailable: Boolean(entry.content_available),
              claimCoverage: String(entry.claim_coverage || '') as DiffFile['claimCoverage'],
              expanded: previous?.expanded,
              loading: sameContent ? previous?.loading : Boolean(previous?.expanded),
              error: sameContent ? previous?.error : '',
              lines: sameContent ? previous?.lines : undefined,
              patch: sameContent ? previous?.patch : undefined,
              context: sameContent ? previous?.context : undefined,
              oldLineCount: sameContent ? previous?.oldLineCount : undefined,
              newLineCount: sameContent ? previous?.newLineCount : undefined,
              lang: sameContent ? previous?.lang : undefined,
            };
          })
          .filter((entry) => entry.path),
      );
      const parseObservation = (entry: Record<string, unknown>): FilesystemObservation => ({
        id: Number(entry.id) || 0,
        classification: String(entry.classification || ''),
        root: String(entry.root || ''),
        createdCount: Number(entry.created_count) || 0,
        modifiedCount: Number(entry.modified_count) || 0,
        deletedCount: Number(entry.deleted_count) || 0,
        sampledPaths: Array.isArray(entry.sampled_paths) ? entry.sampled_paths.map(String) : [],
        samplesTruncated: Boolean(entry.samples_truncated),
        coverageStatus: String(entry.coverage_status || 'complete'),
        eventSeq: Number(entry.event_seq) || 0,
      });
      const materializations = listFrom(data, 'materializations').map(parseObservation);
      const observationContainer = (data.observations || {}) as Record<string, unknown>;
      const observations = listFrom(observationContainer, 'batches').map(parseObservation);
      const claimDiagnostics = listFrom(data, 'claim_diagnostics').map((entry) => ({
        normalizedPattern: String(entry.normalized_pattern || ''),
        claimKind: String(entry.claim_kind || ''),
        reason: String(entry.reason || ''),
        coverageStatus: String(entry.coverage_status || 'complete'),
        matchingPathCount: Number(entry.matching_path_count) || 0,
        message: String(entry.message || ''),
      }));
      const summary = (data.file_change_summary || {}) as Record<string, unknown>;
      let newlyStale = 0;
      const comments = state.comments.map((comment) => {
        if (comment.sessionId !== owner || comment.state === 'stale') return comment;
        const turnScope = ['last_turn', 'last_3_turns'].includes(
          normalizeDiffScope(comment.scope || scope),
        );
        const file = files.find((entry) => entry.path === comment.path);
        let anchorChanged = !file;
        if (file?.lines && comment.anchorFingerprint) {
          const index = file.lines.findIndex((line) =>
            comment.side === 'old' ? line.oldLine === comment.line : line.newLine === comment.line,
          );
          if (index < 0) anchorChanged = true;
          else {
            const beforeCount = comment.contextBefore?.length || 0;
            const afterCount = comment.contextAfter?.length || 0;
            const currentAnchor: DiffComment = {
              ...comment,
              context: file.lines[index].content,
              contextBefore: file.lines
                .slice(Math.max(0, index - beforeCount), index)
                .map((line) => line.content),
              contextAfter: file.lines
                .slice(index + 1, index + 1 + afterCount)
                .map((line) => line.content),
            };
            anchorChanged = reviewAnchorFingerprint(currentAnchor) !== comment.anchorFingerprint;
          }
        }
        const stale =
          anchorChanged ||
          (turnScope &&
            Boolean(comment.fileChangeSeq && (file?.sequence || 0) > comment.fileChangeSeq));
        if (!stale) return comment;
        newlyStale += 1;
        const updated: DiffComment = { ...comment, state: 'stale', updatedAt: Date.now() };
        try {
          persistDiffComment(this.storage, this.keys.diffCommentQueue, updated);
        } catch (error) {
          this.toast(error, 'error');
        }
        return updated;
      });
      this.diff.value = {
        ...state,
        files,
        comments,
        materializations,
        observations,
        claimDiagnostics,
        unavailableLineCountFiles: Number(summary.line_counts_unavailable_files) || 0,
        git: Boolean(data.git),
        loading: false,
      };
      if (newlyStale)
        this.toast(
          `${newlyStale} queued comment${newlyStale === 1 ? '' : 's'} became stale after the source changed.`,
          'info',
        );
      await Promise.all(
        files
          .filter((file) => refreshContexts.has(file.path))
          .map((file) => this.expandDiff(file, refreshContexts.get(file.path) || 0)),
      );
    } catch (error) {
      if (
        epoch === this.diffLoadEpoch &&
        this.activeSessionId.peek() === owner &&
        this.diff.peek().sessionId === owner &&
        normalizeDiffScope(this.diff.peek().scope) === scope
      )
        this.diff.value = {
          ...this.diff.value,
          loading: false,
          error: errorMessage(error),
        };
    }
  }
  async expandDiff(file: DiffFile, context = 0): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const owner = session.id;
    const scope = normalizeDiffScope(this.diff.value.scope);
    const isRequestedVersion = (entry: DiffFile): boolean =>
      entry.path === file.path &&
      (entry.sequence || 0) === (file.sequence || 0) &&
      (entry.snapshotSeq || 0) === (file.snapshotSeq || 0);
    if (file.lines && !context) {
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          isRequestedVersion(entry) ? { ...entry, expanded: !entry.expanded } : entry,
        ),
      };
      return;
    }
    const requestKey = `${owner}\u0000${scope}\u0000${file.path}`;
    const requestEpoch = (this.diffExpandEpoch.get(requestKey) || 0) + 1;
    this.diffExpandEpoch.set(requestKey, requestEpoch);
    this.diff.value = {
      ...this.diff.value,
      files: this.diff.value.files.map((entry) =>
        isRequestedVersion(entry) ? { ...entry, loading: true, error: '' } : entry,
      ),
    };
    try {
      const data = await this.endpoints.fileDiff(
        owner,
        file.path,
        scope,
        context,
        file.snapshotSeq || 0,
      );
      if (
        this.diffExpandEpoch.get(requestKey) !== requestEpoch ||
        this.activeSessionId.peek() !== owner ||
        this.diff.peek().sessionId !== owner ||
        normalizeDiffScope(this.diff.peek().scope) !== scope
      )
        return;
      const lines = Array.isArray(data.hunks)
        ? linesFromHunks(data.hunks, {
            old: Number(data.old_line_count) || 0,
            new: Number(data.new_line_count) || 0,
          })
        : Array.isArray(data.lines)
          ? (data.lines as DiffFile['lines'])
          : parseUnifiedPatch(String(data.diff || data.patch || ''));
      const image = Boolean(data.image || file.image);
      const status = String(data.kind || file.status || '').toLowerCase();
      const beforeURL =
        image && !['add', 'added', 'create', 'created'].includes(status)
          ? this.endpoints.fileContentURL(owner, file.path, scope, 'before', file.snapshotSeq || 0)
          : String(data.before_url || file.beforeURL || '');
      const afterURL =
        image && !['delete', 'deleted', 'remove', 'removed'].includes(status)
          ? this.endpoints.fileContentURL(owner, file.path, scope, 'after', file.snapshotSeq || 0)
          : String(data.after_url || file.afterURL || '');
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          isRequestedVersion(entry)
            ? {
                ...entry,
                status: String(data.kind || entry.status || ''),
                expanded: true,
                loading: false,
                lines,
                image,
                truncated: Boolean(data.truncated),
                contentStatus: String(data.content_status || entry.contentStatus || ''),
                contentAvailable: Boolean(data.content_available),
                provenance: String(
                  data.provenance || entry.provenance || '',
                ) as DiffFile['provenance'],
                baselineState: String(
                  data.baseline_state || entry.baselineState || '',
                ) as DiffFile['baselineState'],
                claimCoverage: String(
                  data.claim_coverage || entry.claimCoverage || '',
                ) as DiffFile['claimCoverage'],
                context: Number(data.context) || context || 3,
                lang: String(data.lang || ''),
                oldLineCount: Number(data.old_line_count) || 0,
                newLineCount: Number(data.new_line_count) || 0,
                patch: String(data.diff || data.patch || entry.patch || ''),
                beforeURL,
                afterURL,
              }
            : entry,
        ),
      };
    } catch (error) {
      if (
        this.diffExpandEpoch.get(requestKey) !== requestEpoch ||
        this.activeSessionId.peek() !== owner ||
        this.diff.peek().sessionId !== owner ||
        normalizeDiffScope(this.diff.peek().scope) !== scope
      )
        return;
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          isRequestedVersion(entry)
            ? {
                ...entry,
                loading: false,
                error: errorMessage(error),
              }
            : entry,
        ),
      };
    } finally {
      if (this.diffExpandEpoch.get(requestKey) === requestEpoch)
        this.diffExpandEpoch.delete(requestKey);
    }
  }
  private prepareDiffComments(comments: DiffComment[]): {
    payloads: Array<Record<string, unknown>>;
    inputText: string;
  } | null {
    const validation = validateReviewBatch(comments);
    if (validation) {
      this.bumpDiagnostic('queueValidationFailures');
      this.toast(validation.message, 'error');
      return null;
    }
    const stale = comments.find((comment) => comment.state === 'stale');
    if (stale) {
      this.toast(
        `${stale.path}:${stale.line} is stale. Re-anchor it or explicitly send it individually.`,
        'error',
      );
      return null;
    }
    const payloads = comments.map((comment) => reviewCommentPayload(comment));
    if (
      payloads.some(
        (comment) =>
          ['last_turn', 'last_3_turns'].includes(String(comment.scope)) &&
          Number(comment.file_change_seq) <= 0,
      )
    ) {
      this.toast(
        'Refresh the diff before sending these comments so their file snapshot can be anchored.',
        'error',
      );
      return null;
    }
    return {
      payloads,
      inputText:
        payloads.length === 1
          ? `[Inline diff instruction]\n${payloads[0].path}:${payloads[0].line} — ${payloads[0].instruction}`
          : `[Inline diff instructions] (${payloads.length} anchored comments)\n\n${payloads.map((comment) => `${comment.path}:${comment.line} — ${comment.instruction}`).join('\n')}`,
    };
  }
  async sendDiffComment(comment: DiffComment): Promise<void> {
    const session = this.activeSession.value;
    if (!session || (comment.sessionId && comment.sessionId !== session.id)) return;
    const value: DiffComment = {
      ...comment,
      id: comment.id || uuid(),
      sessionId: session.id,
      createdAt: comment.createdAt || Date.now(),
      optimistic: true,
    };
    const prepared = this.prepareDiffComments([value]);
    if (!prepared) return;
    this.diff.value = {
      ...this.diff.peek(),
      historyComments: [
        ...this.diff.peek().historyComments.filter((entry) => entry.id !== value.id),
        value,
      ],
    };
    const options: SendOptions = {
      contentParts: prepared.payloads.map((diff_comment) => ({
        type: 'diff_comment',
        diff_comment,
      })),
      inputText: prepared.inputText,
      displayContent: value.body,
      preserveComposer: true,
      diffComments: [value],
      onTransportStarted: () => {
        void this.refreshDiffComments(session.id);
      },
      onTransportFailed: (error) => {
        this.diff.value = {
          ...this.diff.peek(),
          historyComments: this.diff
            .peek()
            .historyComments.filter((entry) => entry.id !== value.id || !entry.optimistic),
        };
        this.toast(error, 'error');
      },
    };
    if (this.streaming.value) await this.interject(prepared.inputText, options);
    else await this.send(options);
  }
  queueDiffComment(comment: DiffComment): void {
    const sessionId = this.activeSessionId.peek();
    const replaced = this.diff.value.comments.find(
      (entry) =>
        entry.sessionId === sessionId &&
        entry.path === comment.path &&
        entry.side === comment.side &&
        entry.line === comment.line,
    );
    const value: DiffComment = {
      ...comment,
      id: comment.id || replaced?.id || uuid(),
      sessionId,
      scope: normalizeDiffScope(comment.scope || this.diff.peek().scope),
      createdAt: comment.createdAt || replaced?.createdAt || Date.now(),
      updatedAt: Date.now(),
      state: 'fresh',
      anchorFingerprint: comment.anchorFingerprint || reviewAnchorFingerprint(comment),
    };
    const validation =
      validateReviewComment(value) ||
      validateReviewBatch([
        ...this.diff.value.comments.filter(
          (entry) => entry.sessionId === sessionId && entry.id !== value.id && entry !== replaced,
        ),
        value,
      ]);
    if (validation) {
      this.bumpDiagnostic('queueValidationFailures');
      this.toast(validation.message, 'error');
      return;
    }
    try {
      persistDiffComment(this.storage, this.keys.diffCommentQueue, value);
      if (replaced?.id && replaced.id !== value.id)
        removeDiffComment(this.storage, this.keys.diffCommentQueue, sessionId, replaced.id);
    } catch (error) {
      this.toast(error, 'error');
      return;
    }
    const comments = [
      ...this.diff.value.comments.filter((entry) => entry.id !== value.id && entry !== replaced),
      value,
    ];
    this.diff.value = { ...this.diff.value, comments };
    this.publishSessionChange('review-comment-changed', sessionId);
  }

  editDiffComment(commentId: string, body: string): void {
    const comment = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (!comment?.id || !comment.sessionId || !body.trim()) return;
    const updated = { ...comment, body: body.trim(), updatedAt: Date.now() };
    const validation =
      validateReviewComment(updated) ||
      validateReviewBatch(
        this.diff
          .peek()
          .comments.map((entry) => (entry.id === commentId ? updated : entry))
          .filter((entry) => entry.sessionId === comment.sessionId),
      );
    if (validation) {
      this.bumpDiagnostic('queueValidationFailures');
      this.toast(validation.message, 'error');
      return;
    }
    try {
      persistDiffComment(this.storage, this.keys.diffCommentQueue, updated);
    } catch (error) {
      this.toast(error, 'error');
      return;
    }
    this.diff.value = {
      ...this.diff.peek(),
      comments: this.diff
        .peek()
        .comments.map((entry) => (entry.id === commentId ? updated : entry)),
    };
    this.publishSessionChange('review-comment-changed', comment.sessionId);
  }

  reanchorDiffComment(
    commentId: string,
    anchor: Pick<DiffComment, 'path' | 'side' | 'line' | 'context' | 'fileChangeSeq' | 'scope'>,
  ): void {
    const comment = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (!comment) return;
    this.queueDiffComment({
      ...comment,
      ...anchor,
      state: 'fresh',
      anchorFingerprint: '',
      updatedAt: Date.now(),
    });
    const updated = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (updated?.state === 'fresh')
      this.toast(`Re-anchored comment to ${anchor.path}:${anchor.line}.`, 'success');
  }

  removeDiffComment(commentId: string): void {
    const comment = this.diff.peek().comments.find((entry) => entry.id === commentId);
    if (!comment?.id || !comment.sessionId) return;
    removeDiffComment(this.storage, this.keys.diffCommentQueue, comment.sessionId, comment.id);
    this.diff.value = {
      ...this.diff.peek(),
      comments: this.diff.peek().comments.filter((entry) => entry.id !== commentId),
    };
    this.publishSessionChange('review-comment-changed', comment.sessionId);
  }

  discardDiffComments(sessionId = this.activeSessionId.peek()): void {
    if (!sessionId) return;
    clearSessionDiffComments(this.storage, this.keys.diffCommentQueue, sessionId);
    this.diff.value = {
      ...this.diff.peek(),
      comments: this.diff.peek().comments.filter((entry) => entry.sessionId !== sessionId),
    };
    this.publishSessionChange('review-comment-changed', sessionId);
  }
  async sendDiffComments(): Promise<void> {
    const session = this.activeSession.value;
    const comments = this.diff.value.comments.filter(
      (comment) => !comment.sessionId || comment.sessionId === session?.id,
    );
    if (!session || !comments.length) return;
    const staleCount = comments.filter((comment) => comment.state === 'stale').length;
    if (
      staleCount &&
      !window.confirm(
        `${staleCount} comment${staleCount === 1 ? '' : 's'} no longer match the current source. Send anyway?`,
      )
    )
      return;
    const prepared = this.prepareDiffComments(
      staleCount ? comments.map((comment) => ({ ...comment, state: 'fresh' as const })) : comments,
    );
    if (!prepared) return;
    const markComments = (state: DiffComment['state'], error = ''): boolean => {
      const updates = new Map(
        comments.map((comment) => [
          comment.id,
          { ...comment, state, error: error || undefined, updatedAt: Date.now() },
        ]),
      );
      try {
        updates.forEach((comment) =>
          persistDiffComment(this.storage, this.keys.diffCommentQueue, comment),
        );
      } catch (persistError) {
        this.toast(persistError, 'error');
        return false;
      }
      this.diff.value = {
        ...this.diff.peek(),
        comments: this.diff.peek().comments.map((comment) => updates.get(comment.id) || comment),
      };
      return true;
    };
    if (!markComments('sending')) return;
    const { payloads, inputText } = prepared;
    const options: SendOptions = {
      contentParts: payloads.map((diff_comment) => ({ type: 'diff_comment', diff_comment })),
      inputText,
      displayContent:
        comments.length === 1 ? comments[0].body : `${comments.length} inline comments`,
      preserveComposer: true,
      diffComments: comments,
      onTransportStarted: () => {
        const sent = comments.map((comment) => ({
          ...comment,
          sessionId: session.id,
          createdAt: comment.createdAt || Date.now(),
          optimistic: true,
        }));
        const sentIDs = new Set(sent.map((comment) => comment.id));
        const current = this.diff.peek();
        const remaining = current.comments.filter(
          (comment) => comment.sessionId && comment.sessionId !== session.id,
        );
        this.diff.value = {
          ...current,
          comments: remaining,
          historyComments: [
            ...current.historyComments.filter((comment) => !sentIDs.has(comment.id)),
            ...sent,
          ],
        };
        comments.forEach((comment) => {
          if (comment.id && (comment.sessionId || session.id))
            removeDiffComment(
              this.storage,
              this.keys.diffCommentQueue,
              comment.sessionId || session.id,
              comment.id,
            );
        });
        this.publishSessionChange('review-comment-changed', session.id);
        void this.refreshDiffComments(session.id);
      },
      onTransportFailed: (error) => {
        markComments('failed', errorMessage(error));
        this.toast(error, 'error');
      },
    };
    if (this.streaming.value) await this.interject(inputText, options);
    else await this.send(options);
  }
  resizeDiff(width: number): void {
    this.diff.value = { ...this.diff.value, width };
    this.storage.setItem(this.keys.diffSidebarWidth, String(Math.round(width)));
  }

  worktreesAvailable(): boolean {
    if (!this.projectsEnabled.value) return this.worktreesEnabled.value;
    const projectId = this.activeSession.value?.projectId || this.activeProjectId.value;
    const project = this.projects.value.find((entry) => entry.id === projectId);
    return Boolean(project?.git && project.available !== false && !project.archived);
  }

  private worktreeProjectID(): string {
    return this.activeSession.value?.projectId || this.activeProjectId.value;
  }
  async loadWorktrees(): Promise<void> {
    const projectId = this.worktreeProjectID();
    this.worktreeError.value = '';
    if (this.projectsEnabled.value && !projectId) {
      this.worktrees.value = [];
      this.worktreeError.value = 'Choose a project before selecting a worktree.';
      return;
    }
    try {
      const data = this.projectsEnabled.value
        ? await this.endpoints.projectWorktrees(projectId)
        : await this.endpoints.legacyWorktrees();
      this.worktrees.value = listFrom(data, 'worktrees', 'items');
    } catch (error) {
      this.worktrees.value = [];
      this.worktreeError.value = worktreeError(error);
    }
  }
  async createWorktree(name: string): Promise<void> {
    const projectId = this.worktreeProjectID();
    try {
      if (this.projectsEnabled.value)
        await this.endpoints.createProjectWorktree(projectId, { name });
      else await this.api.post('/v1/worktrees', { name });
      await this.loadWorktrees();
    } catch (error) {
      this.worktreeError.value = worktreeError(error);
    }
  }
  chooseDraftWorktree(dir: string): void {
    if (!this.draftActive.value) return;
    this.selectedDraftWorktree.value = dir;
    const id = this.draftStorageID();
    const draft = readDrafts(this.storage, this.keys.draftMessages).find(
      (entry) => entry.sessionId === id,
    );
    saveDraft(this.storage, this.keys.draftMessages, {
      ...(draft || { sessionId: id, content: this.prompt.value, updated: Date.now() }),
      worktreeDir: dir,
      projectId: this.activeProjectId.value,
    });
    this.modal.value = '';
  }
  async worktreeDiff(dir: string): Promise<string> {
    const data = await this.endpoints.worktreeDiff(this.worktreeProjectID(), dir);
    return String(data.diff || data.patch || '');
  }
  async mergeWorktree(dir: string): Promise<void> {
    await this.endpoints.mergeWorktree(this.worktreeProjectID(), dir);
    await this.loadWorktrees();
    this.toast('Worktree merged.', 'success');
  }
  async promoteWorktree(dir: string, branch: string): Promise<void> {
    await this.endpoints.promoteWorktree(this.worktreeProjectID(), dir, branch);
    await this.loadWorktrees();
    this.toast('Worktree promoted.', 'success');
  }
  async removeWorktree(dir: string, force = false): Promise<Record<string, unknown>> {
    const result = await this.endpoints.removeWorktree(this.worktreeProjectID(), dir, force);
    await this.loadWorktrees();
    return result || {};
  }

  async loadSkills(sessionId = this.activeSession.value?.id || ''): Promise<void> {
    const epoch = ++this.skillEpoch;
    if (!sessionId) {
      this.skills.value = [];
      return;
    }
    const data = await this.endpoints.skills(sessionId);
    if (epoch !== this.skillEpoch || this.activeSessionId.peek() !== sessionId) return;
    this.skills.value = listFrom(data, 'skills', 'items');
  }
  private skillRunTerminal(status: unknown): boolean {
    return ['complete', 'completed', 'failed', 'cancelled'].includes(
      String(status || '').toLowerCase(),
    );
  }
  private updateSkillRunMessage(
    sessionId: string,
    runId: string,
    patch: Record<string, unknown>,
  ): void {
    this.sessions.value = this.sessions.value.map((session) => {
      if (session.id !== sessionId) return session;
      const messages = session.messages.map((message) => {
        if (message.role !== 'skill-run' || String(message.runId || '') !== runId) return message;
        const status = String(patch.status || message.status || 'running');
        const output = String(patch.output ?? message.output ?? '');
        const error = String(patch.error ?? message.error ?? '');
        const progress = String(patch.progress ?? message.progress ?? '');
        return {
          ...message,
          ...patch,
          status,
          output,
          error,
          progress,
          content: error || output || progress,
        };
      });
      return { ...session, messages };
    });
  }
  private applySkillRunEvent(runId: string, envelope: Record<string, unknown>): void {
    const cursor = this.skillRunCursors.get(runId);
    if (!cursor) return;
    const sequence = Number(envelope.sequence || envelope.sequence_number) || 0;
    if (sequence && sequence <= cursor.sequence) return;
    if (sequence) cursor.sequence = sequence;
    const type = String(envelope.type || '');
    const data = recordValue(envelope.data) || envelope;
    const patch: Record<string, unknown> = {};
    if (type === 'skill_run.created')
      Object.assign(patch, {
        status: 'running',
        skill: String(data.skill || ''),
        agent: String(data.agent || ''),
        childSessionId: String(data.child_session_id || ''),
      });
    else if (type === 'skill_run.progress')
      patch.progress = String(data.message || data.progress || data.stage || 'Working…');
    else if (type === 'skill_run.completed')
      Object.assign(patch, {
        status: String(data.status || 'completed'),
        output: String(data.output || ''),
        error: String(data.error || ''),
        progress: '',
        childSessionId: String(data.child_session_id || ''),
      });
    else return;
    this.updateSkillRunMessage(cursor.sessionId, runId, patch);
    if (this.skillRunTerminal(patch.status)) {
      this.skillRunAborts.get(runId)?.abort();
      this.skillRunCursors.delete(runId);
      void this.refreshSessionMessages(cursor.sessionId);
    }
  }
  private async reconcileSkillRun(runId: string): Promise<void> {
    const cursor = this.skillRunCursors.get(runId);
    if (!cursor) return;
    const snapshot = await this.endpoints.skillRun(cursor.sessionId, runId);
    if (this.disposed || this.skillRunCursors.get(runId) !== cursor) return;
    const events = Array.isArray(snapshot.events) ? snapshot.events : [];
    events.forEach((event) => {
      if (event && typeof event === 'object')
        this.applySkillRunEvent(runId, event as Record<string, unknown>);
    });
    this.updateSkillRunMessage(cursor.sessionId, runId, {
      status: String(snapshot.status || 'running'),
      output: String(snapshot.output || ''),
      error: String(snapshot.error || ''),
      childSessionId: String(snapshot.child_session_id || ''),
    });
    if (this.skillRunTerminal(snapshot.status)) {
      this.skillRunCursors.delete(runId);
      await this.refreshSessionMessages(cursor.sessionId);
    }
  }
  private async followSkillRun(runId: string): Promise<void> {
    const cursor = this.skillRunCursors.get(runId);
    if (!cursor || this.skillRunAborts.has(runId)) return;
    const controller = new AbortController();
    this.skillRunAborts.set(runId, controller);
    try {
      const separator = cursor.eventsURL.includes('?') ? '&' : '?';
      const response = await this.api.request(
        `${cursor.eventsURL}${separator}after=${encodeURIComponent(cursor.sequence)}`,
        {
          signal: controller.signal,
          headers: { Accept: 'text/event-stream', 'X-Term-LLM-Session-ID': cursor.sessionId },
        },
        { policy: 'stream', retries: 0, timeoutMs: 0, auth: 'session' },
      );
      if (!response.ok || !response.body)
        throw new APIError(
          (await response.text()) || `Skill run stream returned ${response.status}`,
          response.status,
        );
      for await (const frame of decodeSSE(response.body, controller.signal)) {
        let envelope: Record<string, unknown>;
        try {
          envelope = JSON.parse(frame.data) as Record<string, unknown>;
        } catch {
          continue;
        }
        if (!envelope.type && frame.event !== 'message') envelope.type = frame.event;
        this.applySkillRunEvent(runId, envelope);
        if (!this.skillRunCursors.has(runId)) return;
      }
    } catch (error) {
      if (!controller.signal.aborted)
        this.updateSkillRunMessage(cursor.sessionId, runId, {
          progress:
            navigator.onLine === false
              ? 'Offline — run is safe; reconnecting when online'
              : 'Reconnecting…',
          streamError: String(error),
        });
    } finally {
      if (this.skillRunAborts.get(runId) === controller) this.skillRunAborts.delete(runId);
    }
    if (!this.skillRunCursors.has(runId)) return;
    await this.reconcileSkillRun(runId).catch(() => undefined);
    if (this.skillRunCursors.has(runId))
      this.schedule(() => void this.followSkillRun(runId), 1_000);
  }
  async invokeSkill(name: string, args: string): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const skill = this.skills.peek().find((entry) => String(entry.name || '') === name);
    if (this.streaming.peek() && skill?.execution !== 'isolated') {
      this.toast('This main-conversation skill cannot run while a response is active.', 'error');
      return;
    }
    const id = uuid();
    const invocation = `/${name}${args.trim() ? ` ${args.trim()}` : ''}`;
    const optimistic: Message = {
      id: `pending_${id}`,
      role: 'user',
      content: invocation,
      clientMessageId: id,
      created: Date.now(),
    };
    this.sessions.value = this.sessions.value.map((entry) =>
      entry.id === session.id
        ? { ...entry, messages: [...entry.messages, optimistic], lastMessageAt: Date.now() }
        : entry,
    );
    this.trackIntent(session.id, {
      id: optimistic.id,
      clientMessageId: id,
      content: invocation,
      created: optimistic.created,
    });
    try {
      const data = await this.endpoints.invokeSkill(
        session.id,
        { name, arguments: args, client_message_id: id },
        id,
      );
      if (String(data.execution) === 'isolated' && data.run_id) {
        const runId = String(data.run_id);
        const eventsURL = String(
          data.events_url ||
            `/v1/sessions/${encodeURIComponent(session.id)}/skill-runs/${encodeURIComponent(runId)}/events`,
        );
        const message: Message = {
          id: `skill-run-${runId}`,
          role: 'skill-run',
          content: '',
          created: Date.now(),
          runId,
          skill: name,
          status: String(data.status || 'running'),
          childSessionId: String(data.child_session_id || ''),
          eventsURL,
        };
        this.sessions.value = this.sessions.value.map((entry) =>
          entry.id === session.id ? { ...entry, messages: [...entry.messages, message] } : entry,
        );
        this.skillRunCursors.set(runId, { sessionId: session.id, eventsURL, sequence: 0 });
        void this.followSkillRun(runId);
      } else {
        const responseId = String(data.response_id || '');
        const epoch = Number(data.run_epoch) || 0;
        if (!responseId || !epoch)
          throw new Error('Skill invocation did not return a response ID and run epoch.');
        const run: ActiveRun = {
          responseId,
          sessionId: session.id,
          epoch,
          status: 'streaming',
          lastSequence: 0,
          startedRev: Number(data.started_rev) || session.transcriptRev || 0,
          startedAt: Number(data.started_at) || Date.now(),
          reconnects: 0,
          requestId: id,
        };
        this.runs.value = { ...this.runs.value, [session.id]: initialProjection(run) };
        this.sessions.value = this.sessions.value.map((entry) =>
          entry.id === session.id ? { ...entry, activeResponseId: responseId } : entry,
        );
        void this.streamResponse(responseId, session.id, 0);
      }
      this.modal.value = '';
      this.toast(`Started ${name}.`, 'success');
    } catch (error) {
      this.retireIntent(session.id, id);
      if (!this.prompt.value) this.prompt.value = invocation;
      this.toast(error, 'error');
    }
  }
  async cancelSkill(runId: string): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    this.updateSkillRunMessage(session.id, runId, {
      status: 'cancelling',
      progress: 'Cancelling…',
    });
    await this.endpoints.cancelSkillRun(session.id, runId);
    if (this.skillRunCursors.has(runId))
      await this.reconcileSkillRun(runId).catch(() => this.refreshSessionMessages(session.id));
    else await this.refreshSessionMessages(session.id);
  }

  async mutateTranscript(operation: 'undo' | 'redo'): Promise<void> {
    const session = this.activeSession.value;
    if (!session || this.streaming.value)
      return this.toast(`Cannot ${operation} while work is active.`, 'error');
    const durable = [...session.messages].reverse().find((message) => message.durableRowId);
    const owner = session.id;
    try {
      const result = await this.endpoints.mutateTranscript(owner, operation, {
        expected_rev: session.transcriptRev || 0,
        expected_head_id: durable?.durableRowId || 0,
      });
      await this.refreshSessionMessages(owner, Number(result.rev) || 0);
      if (this.activeSessionId.peek() === owner)
        this.prompt.value = operation === 'undo' ? String(result.user_text || '') : '';
      this.toast(
        operation === 'undo'
          ? result.attachments_omitted
            ? 'Removed the latest turn. Attachments were not restored.'
            : 'Removed the latest turn. Your prompt is back in the composer.'
          : 'Restored the undone turn.',
        result.attachments_omitted ? 'error' : 'success',
      );
      if (this.diff.value.open) void this.loadDiff();
    } catch (error) {
      if (error instanceof APIError && error.status === 409)
        await this.refreshSessionMessages(owner);
      this.toast(error, 'error');
    }
  }

  async loadMoreProject(projectId: string): Promise<void> {
    const project = this.projects.value.find((entry) => entry.id === projectId);
    if (!project?.next_cursor) return;
    const data = await this.endpoints.projectSessions(
      projectId,
      project.next_cursor,
      this.showHidden.value,
    );
    const incoming = listFrom(data, 'sessions', 'items').map((entry) =>
      this.sessionFrom({ ...entry, project_id: project.id, project_name: project.name }),
    );
    const existing = new Map(this.sessions.value.map((entry) => [entry.id, entry]));
    incoming.forEach((entry) =>
      existing.set(entry.id, this.mergeSession(existing.get(entry.id), entry, false, true)),
    );
    this.sessions.value = [...existing.values()];
    this.projects.value = this.projects.value.map((entry) =>
      entry.id === projectId
        ? {
            ...entry,
            sessions: [
              ...(entry.sessions || []),
              ...incoming.filter(
                (candidate) =>
                  !(entry.sessions || []).some((session) => session.id === candidate.id),
              ),
            ],
            next_cursor: String(data.next_cursor || ''),
            has_more: Boolean(data.next_cursor),
          }
        : entry,
    );
  }

  async loadMoreNoProject(): Promise<void> {
    const cursor = this.noProjectCursor.peek();
    if (!cursor) return;
    const data = await this.endpoints.noProjectSessions(cursor, this.showHidden.value);
    const incoming = listFrom(data, 'sessions', 'items').map((entry) => this.sessionFrom(entry));
    const existing = new Map(this.sessions.peek().map((entry) => [entry.id, entry]));
    incoming.forEach((entry) =>
      existing.set(entry.id, this.mergeSession(existing.get(entry.id), entry, false, true)),
    );
    this.sessions.value = [...existing.values()].sort(compareSessionsByActivity);
    this.noProjectCursor.value = String(data.next_cursor || '');
  }
  async mutateProject(project: Project, patch: Record<string, unknown>): Promise<void> {
    await this.endpoints.patchProject(project.id, patch);
    await this.refreshSidebar();
    this.publishSessionChange();
  }
  async startProjectChat(projectId: string): Promise<void> {
    this.newChat(false, projectId);
    this.sidebarOpen.value = false;
  }

  async refreshHubAgents(force = false): Promise<void> {
    if (this.hubAgentFetch) return this.hubAgentFetch;
    if (document.visibilityState === 'hidden') return;
    const raw = this.config.hub?.url || '';
    if (!raw) {
      this.hubAgents.value = [];
      return;
    }
    let hub: URL;
    try {
      hub = new URL(raw, location.href);
    } catch {
      return;
    }
    if (hub.origin !== location.origin) {
      this.hubAgents.value = [];
      return;
    }
    if (!force && Date.now() - this.hubAgentLastFetch < 60_000) return;
    this.hubAgentLastFetch = Date.now();
    const controller = new AbortController();
    const request = (async () => {
      try {
        const path = `${hub.pathname.replace(/\/+$/, '')}/api/nodes`;
        const data = await this.endpoints.hubNodes(
          new URL(path, location.origin).href,
          controller.signal,
        );
        const safePath = (value: unknown): string =>
          typeof value === 'string' && /^\/(?![\\/])/.test(value) ? value : '';
        const target = (node: Record<string, unknown>): string => {
          const sessions = recordValue(node.sessions) || {};
          const active = array(sessions.active);
          const recent = array(sessions.recent);
          return (
            safePath(sessions.resume_path) ||
            safePath(active[0]?.resume_path) ||
            safePath(recent[0]?.resume_path) ||
            safePath(node.new_session_path) ||
            (safePath(node.proxy_path) ? `${safePath(node.proxy_path)}?new=1` : '')
          );
        };
        const previous = new Map(this.hubAgents.peek().map((entry) => [entry.id, entry]));
        this.hubAgents.value = array(data.nodes)
          .filter((node) => recordValue(node.status)?.reachable === true)
          .map((node) => {
            const id = String(node.id || '');
            const sessions = recordValue(node.sessions) || {};
            const active = Number(sessions.active_count) > 0 || array(sessions.active).length > 0;
            const old = previous.get(id);
            return {
              id,
              name: String(node.name || id),
              target: target(node),
              active,
              attention:
                id !== this.config.hub?.nodeId &&
                Boolean(old?.attention || (old?.active && !active)),
            };
          })
          .filter((entry) => entry.name && entry.target)
          .sort(
            (left, right) =>
              left.name.toLowerCase().localeCompare(right.name.toLowerCase()) ||
              left.id.localeCompare(right.id),
          );
      } catch (error) {
        if (error instanceof APIError && error.status >= 400 && error.status < 500)
          this.hubAgents.value = [];
      }
    })();
    this.hubAgentFetch = request.finally(() => {
      this.hubAgentFetch = null;
    });
    return this.hubAgentFetch;
  }
  clearHubAttention(id: string): void {
    this.hubAgents.value = this.hubAgents.value.map((entry) =>
      entry.id === id ? { ...entry, attention: false } : entry,
    );
  }

  private startStatusPoll(): void {
    clearTimeout(this.statusTimer);
    const poll = async () => {
      if (document.visibilityState === 'visible') {
        if (Date.now() - this.lastSidebarRefreshAt >= 30_000)
          await this.refreshSidebar(false).catch(() => undefined);
        await this.reconcile('poll', { authoritative: false }).catch(() => undefined);
        if (this.activeSessionId.peek()) await this.loadChildRuns().catch(() => undefined);
      }
      const anyActive =
        this.locallyStoppedResponses.size > 0 ||
        Object.values(this.pendingIntents.peek()).some((intents) =>
          intents.some((intent) => intent.state === 'checking'),
        ) ||
        this.sessions.peek().some((session) => session.activeRun) ||
        Object.values(this.runs.peek()).some((projection) =>
          ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status),
        );
      this.statusTimer = window.setTimeout(
        poll,
        anyActive || this.diff.peek().open ? 2_000 : 30_000,
      );
    };
    this.statusTimer = window.setTimeout(poll, 0);
  }
  private async refreshStatus(authoritative = false): Promise<void> {
    if (!authoritative && this.statusCoordinator.refreshPromise)
      return this.statusCoordinator.refreshPromise;

    const generation = ++this.statusCoordinator.generation;
    const previous = this.statusCoordinator.refreshPromise;
    const metadata: StatusRequestMetadata = {
      generation,
      requestedAt: Date.now(),
      selectedSessionId: this.activeSessionId.peek(),
      selectionEpoch: this.selectionEpoch,
      showHidden: this.showHidden.peek(),
      categories: [...this.config.sidebarCategories],
    };
    const request = (async () => {
      // An authoritative request invalidates the old generation immediately,
      // but waits for it to settle to avoid needless parallel work.
      if (authoritative && previous) await previous.catch(() => undefined);
      const data = await this.endpoints.sessionStatus(
        metadata.selectedSessionId,
        metadata.showHidden,
        metadata.categories,
        this.statusCoordinator.etag,
      );
      const receivedAt = Date.now();
      if (!this.statusRequestIsCurrent(metadata)) {
        this.bumpDiagnostic('staleStatusResults');
        return;
      }
      if (data.__notModified === true) {
        this.statusCoordinator.lastAppliedGeneration = generation;
        this.statusCoordinator.lastAppliedRequestedAt = metadata.requestedAt;
        this.statusCoordinator.lastAppliedReceivedAt = receivedAt;
        this.statusCoordinator.etag = String(data.__etag || this.statusCoordinator.etag);
        return;
      }
      await this.applyStatus(data, metadata, receivedAt);
    })();
    const tracked = request.finally(() => {
      if (this.statusCoordinator.refreshPromise === tracked)
        this.statusCoordinator.refreshPromise = null;
    });
    this.statusCoordinator.refreshPromise = tracked;
    return tracked;
  }

  private statusRequestIsCurrent(metadata: StatusRequestMetadata): boolean {
    return (
      !this.disposed &&
      metadata.generation === this.statusCoordinator.generation &&
      metadata.selectedSessionId === this.activeSessionId.peek() &&
      metadata.selectionEpoch === this.selectionEpoch &&
      metadata.showHidden === this.showHidden.peek() &&
      metadata.categories.join(',') === this.config.sidebarCategories.join(',')
    );
  }

  private async applyStatus(
    data: Record<string, unknown>,
    metadata: StatusRequestMetadata,
    receivedAt: number,
  ): Promise<void> {
    if (!this.statusRequestIsCurrent(metadata)) return;
    const activeSessionId = metadata.selectedSessionId;
    const previousActiveRevision =
      this.sessions.peek().find((session) => session.id === activeSessionId)?.transcriptRev || 0;
    const statuses = listFrom(data, 'sessions', 'items');
    const known = new Set(this.sessions.peek().map((session) => session.id));
    const unknownActive = new Set(
      statuses.flatMap((status) => {
        const id = String(status.id || status.session_id || '');
        return id && (status.active_run || status.active_response_id) && !known.has(id) ? [id] : [];
      }),
    );
    const discoveredUnknown = [...unknownActive].some(
      (id) => !this.unknownActiveSessionIDs.has(id),
    );
    if (discoveredUnknown) {
      await this.refreshSidebar(false).catch(() => undefined);
      if (!this.statusRequestIsCurrent(metadata)) return;
    }

    const byID = new Map(statuses.map((entry) => [String(entry.id || entry.session_id), entry]));
    const followUps: Array<() => void> = [];
    this.sessions.value = this.sessions
      .peek()
      .map((session) => {
        const status = byID.get(session.id);
        if (!status) return session.activeRun ? { ...session, activeRun: false } : session;
        const serverActiveResponseId = String(status.active_response_id || '') || null;
        const committedClientMessageId = String(status.client_message_id || '');
        if (
          serverActiveResponseId &&
          committedClientMessageId &&
          this.pendingIntents
            .peek()
            [session.id]?.some((intent) => intent.clientMessageId === committedClientMessageId)
        ) {
          followUps.push(() => this.retireIntent(session.id, committedClientMessageId));
        }
        const stoppedResponseId = this.runs.peek()[session.id]?.run.responseId || '';
        const stoppedServerResponse = Boolean(
          serverActiveResponseId && this.locallyStoppedResponses.has(serverActiveResponseId),
        );
        const activeResponseId = stoppedServerResponse ? null : serverActiveResponseId;
        const transcriptRev = Math.max(
          session.transcriptRev || 0,
          Number(status.transcript_rev) || 0,
        );
        if (
          activeResponseId &&
          activeResponseId !== session.activeResponseId &&
          !this.locallyStoppedResponses.has(activeResponseId)
        )
          followUps.push(() => void this.resumeResponse(session.id, activeResponseId));
        if (stoppedResponseId && this.locallyStoppedResponses.has(stoppedResponseId)) {
          if (!serverActiveResponseId || serverActiveResponseId !== stoppedResponseId)
            this.locallyStoppedResponses.delete(stoppedResponseId);
        }
        if (
          !serverActiveResponseId &&
          (session.activeResponseId || this.pendingIntents.peek()[session.id]?.length) &&
          transcriptRev >= (this.runs.peek()[session.id]?.run.finalRev || 0)
        )
          followUps.push(() => void this.refreshSessionMessages(session.id, transcriptRev));
        const titleRefreshAllowed = this.renameTarget.peek()?.id !== session.id;
        return {
          ...session,
          ...(titleRefreshAllowed && String(status.short_title || '')
            ? {
                title: String(status.short_title),
                longTitle: String(status.long_title || '') || session.longTitle,
              }
            : {}),
          activeResponseId,
          activeRun: Boolean(activeResponseId || (status.active_run && !stoppedServerResponse)),
          ...(committedClientMessageId
            ? {
                messages: session.messages.map((message) =>
                  message.clientMessageId === committedClientMessageId
                    ? { ...message, pending: false, interruptState: undefined }
                    : message,
                ),
              }
            : {}),
          lastResponseId: String(status.last_response_id || '') || session.lastResponseId,
          transcriptRev,
          messageCount: Math.max(session.messageCount || 0, Number(status.message_count) || 0),
          lastMessageAt: Number(status.last_message_at)
            ? Math.max(
                session.lastMessageAt || 0,
                Number(status.last_message_at) *
                  (Number(status.last_message_at) < 10_000_000_000 ? 1000 : 1),
              )
            : session.lastMessageAt,
        };
      })
      .sort(compareSessionsByActivity);
    if (!this.statusRequestIsCurrent(metadata)) return;
    this.unknownActiveSessionIDs = unknownActive;
    this.statusCoordinator.lastAppliedGeneration = metadata.generation;
    this.statusCoordinator.lastAppliedRequestedAt = metadata.requestedAt;
    this.statusCoordinator.lastAppliedReceivedAt = receivedAt;
    this.statusCoordinator.etag = String(data.__etag || '');

    const activeRevision =
      this.sessions.peek().find((session) => session.id === activeSessionId)?.transcriptRev || 0;
    if (
      this.diff.peek().open &&
      this.diff.peek().sessionId === activeSessionId &&
      activeRevision > previousActiveRevision
    )
      followUps.push(() => void this.refreshDiffComments(activeSessionId));
    if (activeSessionId) followUps.push(() => void this.loadChildRuns(activeSessionId));
    // No follow-up from a stale generation may start after a newer reconcile.
    if (this.statusRequestIsCurrent(metadata)) followUps.forEach((followUp) => followUp());
  }

  private bumpDiagnostic(key: keyof ReturnType<typeof this.diagnostics.peek>): void {
    this.diagnostics.value = {
      ...this.diagnostics.peek(),
      [key]: this.diagnostics.peek()[key] + 1,
    };
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.persistCurrentDraft();
    this.lifecycleAbort.abort();
    window.clearTimeout(this.statusTimer);
    window.clearTimeout(this.searchTimer);
    window.clearTimeout(this.peerSyncTimer);
    for (const timers of this.titleRefreshTimers.values())
      timers.forEach((timer) => window.clearTimeout(timer));
    this.titleRefreshTimers.clear();
    this.ownedTimers.forEach((timer) => window.clearTimeout(timer));
    this.ownedTimers.clear();
    this.searchAbort?.abort();
    this.sideQuestionAbort?.abort();
    this.modelAbort?.abort();
    this.skillRunAborts.forEach((abort) => abort.abort());
    this.skillRunAborts.clear();
    this.skillRunCursors.clear();
    this.streamSupervisors.dispose();
    this.notificationController.dispose();
    this.releaseAttachmentResources(this.attachments.peek(), false);
    this.draftBlobs.close();
    this.sessionSyncChannel?.close();
    this.sessionSyncChannel = null;
  }

  private schedule(callback: () => void, delay: number): number {
    const timer = window.setTimeout(() => {
      this.ownedTimers.delete(timer);
      if (!this.disposed) callback();
    }, delay);
    this.ownedTimers.add(timer);
    return timer;
  }

  toast(value: unknown, kind: Toast['kind'] = 'info'): void {
    const message = errorMessage(value);
    const toast = { id: uuid(), message, kind };
    this.toasts.value = [...this.toasts.value, toast];
    this.schedule(() => this.dismissToast(toast.id), 4000);
  }
  dismissToast(id: string): void {
    const toast = this.toasts.peek().find((entry) => entry.id === id);
    if (!toast || toast.leaving) return;
    this.toasts.value = this.toasts.value.map((entry) =>
      entry.id === id ? { ...entry, leaving: true } : entry,
    );
    this.schedule(() => {
      this.toasts.value = this.toasts.value.filter((entry) => entry.id !== id);
    }, 160);
  }
  async hardRefresh(): Promise<void> {
    await hardRefreshAssets(this.config);
  }
}
