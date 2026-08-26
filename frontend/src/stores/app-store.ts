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
  Goal,
  MCPServer,
  Message,
  Project,
  Session,
} from '../domain/types';
import {
  clearDraft,
  migrateScopedStorage,
  persistPendingIntent,
  readDiffCommentQueue,
  readDrafts,
  readPendingIntents,
  removeSessionPendingIntents,
  saveDraft,
  writeJSON,
  type PendingIntentRegistry,
  type StorageKeys,
} from '../platform/storage';
import { sessionIDFromLocation, updateSessionRoute } from '../platform/routing';
import { enableNotifications, hardRefreshAssets, syncTokenCookie } from '../platform/browser';
import { rebaseHubAssetURL } from '../app/config';

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
  | 'skills';
export interface Toast {
  id: string;
  message: string;
  kind: 'info' | 'success' | 'error';
}
export interface RuntimeOption extends ModelOption {
  [key: string]: unknown;
}
export interface SideQuestionState {
  visible: boolean;
  running: boolean;
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
  filter: string;
  comments: DiffComment[];
  error: string;
  maximized: boolean;
  width: number;
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
const retryBackoff = (attempt: number): number =>
  Math.round(Math.min(60_000, 1_000 * 1.5 ** Math.max(0, attempt)));
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

export class AppStore {
  readonly keys: StorageKeys;
  readonly api: APIClient;
  readonly endpoints: Endpoints;

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
  readonly notificationsEnabled: Signal<boolean>;
  readonly widgets = signal<
    Array<{ id: string; name: string; url: string; description?: string; state?: string }>
  >([]);
  readonly hubAgents = signal<HubAgent[]>([]);
  readonly modal = signal<Modal>('');
  readonly toasts = signal<Toast[]>([]);
  readonly currentPlan = signal<CurrentPlan | null>(null);
  readonly planOpen = signal(false);
  readonly askUser = signal<AskUserPrompt | null>(null);
  readonly approval = signal<ApprovalPrompt | null>(null);
  readonly sideQuestion = signal<SideQuestionState>({
    visible: false,
    running: false,
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
    filter: '',
    comments: [],
    error: '',
    maximized: false,
    width: 420,
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
  readonly lightbox = signal<{ src: string; type: 'image' | 'video' } | null>(null);
  readonly renameTarget = signal<Session | null>(null);
  readonly projectTarget = signal<Session | null>(null);
  readonly networkState = signal<'unknown' | 'online' | 'offline' | 'retrying'>('unknown');
  readonly fileChangeRevision = signal(0);
  readonly pendingIntents: Signal<PendingIntentRegistry>;

  readonly activeSession: ReadonlySignal<Session | null>;
  readonly activeProjection: ReadonlySignal<ResponseProjection | null>;
  readonly visibleMessages: ReadonlySignal<Message[]>;
  readonly streaming: ReadonlySignal<boolean>;

  private readonly streamAborts = new Map<string, AbortController>();
  private readonly postAborts = new Map<string, AbortController>();
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
  private modelAbort: AbortController | null = null;
  private statusTimer = 0;
  private lifecycleInstalled = false;
  private selectionEpoch = 0;
  private modelEpoch = 0;
  private skillEpoch = 0;
  private recoverPromise: Promise<void> | null = null;
  private hubAgentLastFetch = 0;
  private hubAgentFetch: Promise<void> | null = null;

  constructor(
    readonly config: AppConfig,
    readonly storage: Storage = localStorage,
  ) {
    this.keys = migrateScopedStorage(storage, config.hub);
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
    this.notificationsEnabled = signal(storage.getItem(this.keys.notificationsEnabled) === '1');
    this.pendingIntents = signal(readPendingIntents(storage, this.keys.pendingIntents));
    this.diff.value = {
      ...this.diff.value,
      width: Math.max(320, Number(storage.getItem(this.keys.diffSidebarWidth)) || 420),
      comments: readDiffCommentQueue(storage, this.keys.diffCommentQueue),
    };
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
        }));
      return [...messages, ...pending];
    });
    this.streaming = computed(() =>
      ['connecting', 'streaming', 'cancelling'].includes(
        this.activeProjection.value?.run.status || '',
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
      const restoreDraft = !routed && this.storage.getItem(this.keys.draftSessionActive) === '1';
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
      else this.newChat(true, this.storage.getItem(this.keys.lastProject) || '');
      this.connected.value = true;
      this.networkState.value = 'online';
      this.startupDone.value = true;
      this.startStatusPoll();
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
    addEventListener('popstate', () => {
      const slug = sessionIDFromLocation(this.config.prefix);
      const session = this.sessions.value.find(
        (entry) => entry.id === slug || String(entry.number || '') === slug,
      );
      if (session) void this.selectSession(session, true);
      else if (!slug) this.newChat(true);
      else void this.resolveAndSelectSession(slug, true);
    });
    addEventListener('online', () => void this.recover());
    addEventListener('offline', () => {
      this.networkState.value = 'offline';
      this.connected.value = false;
    });
    addEventListener('term-llm:transport-fallback', () => void this.recover());
    addEventListener('pageshow', (event) => {
      if (this.startupDone.value || (event as PageTransitionEvent).persisted) void this.recover();
    });
    addEventListener('focus', () => void this.refreshHubAgents());
    addEventListener('beforeunload', () => this.persistCurrentDraft());
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') {
        void this.recover();
        void this.refreshHubAgents();
      }
    });
    addEventListener('storage', (rawEvent) => {
      const event = rawEvent as StorageEvent;
      if (
        event.key === this.keys.pendingIntents ||
        event.key?.startsWith(`${this.keys.pendingIntents}:`)
      ) {
        this.pendingIntents.value = readPendingIntents(this.storage, this.keys.pendingIntents);
      }
    });
  }

  private recover(): Promise<void> {
    if (this.recoverPromise) return this.recoverPromise;
    const request = this.performRecovery();
    const tracked = request.finally(() => {
      if (this.recoverPromise === tracked) this.recoverPromise = null;
    });
    this.recoverPromise = tracked;
    return tracked;
  }

  private async performRecovery(): Promise<void> {
    if (navigator.onLine === false) {
      this.networkState.value = 'offline';
      return;
    }
    await this.refreshStatus().catch(() => undefined);
    const active = this.sessions.value.filter(
      (session) =>
        this.runs.value[session.id] &&
        ['connecting', 'streaming'].includes(this.runs.value[session.id].run.status),
    );
    for (const session of active) {
      const run = this.runs.value[session.id].run;
      if (
        run.responseId &&
        !run.responseId.startsWith('pending_') &&
        !this.streamAborts.has(session.id)
      )
        void this.streamResponse(run.responseId, run.sessionId, run.lastSequence);
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
    this.worktreesEnabled.value = this.config.worktrees && worktrees.enabled !== false;
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
  ): Session {
    if (!existing) return incoming;
    return {
      ...existing,
      ...incoming,
      messages: replaceMessages || incoming.messages.length ? incoming.messages : existing.messages,
      lastResponseId: incoming.lastResponseId || existing.lastResponseId,
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
      incoming.map((session) => [session.id, this.mergeSession(existing.get(session.id), session)]),
    );
    for (const [id, session] of existing)
      if (!merged.has(id) && (this.runs.peek()[id] || id.startsWith('draft_')))
        merged.set(id, session);
    this.sessions.value = [...merged.values()].sort(
      (a, b) => Number(b.pinned) - Number(a.pinned) || b.lastMessageAt - a.lastMessageAt,
    );
    this.projects.value = projects.map((project) => ({
      ...project,
      sessions: project.sessions?.map((summary) => merged.get(summary.id) || summary),
    }));
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

  async refreshSidebar(): Promise<void> {
    const data = this.projectsEnabled.value
      ? await this.endpoints.sidebar(this.showHidden.value)
      : await this.endpoints.sessions(
          `limit=30&include_archived=${this.showHidden.value ? '1' : '0'}`,
        );
    this.applySidebar(data);
  }

  async selectSession(session: Session, replace = false): Promise<void> {
    this.persistCurrentDraft();
    const epoch = ++this.selectionEpoch;
    batch(() => {
      this.activeSessionId.value = session.id;
      this.activeProjectId.value = session.projectId || '';
      this.draftActive.value = false;
      this.currentPlan.value = null;
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
    if (current.activeResponseId) void this.resumeResponse(current.id, current.activeResponseId);
    this.sidebarOpen.value = false;
    void this.recoverSideQuestion();
  }

  newChat(replace = false, projectId = ''): void {
    this.persistCurrentDraft();
    ++this.selectionEpoch;
    const selectedProject =
      projectId &&
      this.projects.value.some(
        (project) => project.id === projectId && !project.archived && project.available !== false,
      )
        ? projectId
        : '';
    batch(() => {
      this.activeSessionId.value = '';
      this.activeProjectId.value = selectedProject;
      this.draftActive.value = true;
      this.currentPlan.value = null;
      this.askUser.value = null;
      this.approval.value = null;
      this.skills.value = [];
      this.branchTree.value = null;
      this.branchPathCount.value = 0;
    });
    this.storage.removeItem(this.keys.activeSession);
    this.storage.setItem(this.keys.draftSessionActive, '1');
    if (selectedProject) this.storage.setItem(this.keys.lastProject, selectedProject);
    updateSessionRoute(this.config.prefix, null, replace);
    this.restoreDraftFor(this.draftStorageID());
  }

  private draftStorageID(): string {
    return this.activeSessionId.peek() || `draft:${this.activeProjectId.peek() || 'none'}`;
  }
  private persistCurrentDraft(): void {
    const id = this.draftStorageID();
    saveDraft(this.storage, this.keys.draftMessages, {
      sessionId: id,
      content: this.prompt.peek(),
      projectId: this.activeProjectId.peek(),
      updated: Date.now(),
      provider: this.selectedProvider.peek(),
      model: this.selectedModel.peek(),
      effort: this.selectedEffort.peek(),
      reasoningMode: this.selectedReasoningMode.peek(),
      agent: this.selectedAgent.peek(),
      worktreeDir: this.activeSession.peek()?.worktreeDir || this.selectedDraftWorktree.peek(),
      attachments: this.attachments.peek().filter((attachment) => !attachment.file),
    });
  }
  private restoreDraftFor(id: string): void {
    const draft = readDrafts(this.storage, this.keys.draftMessages).find(
      (entry) => entry.sessionId === id,
    );
    batch(() => {
      this.prompt.value = draft?.content || '';
      this.attachments.value = draft?.attachments || [];
      this.selectedDraftWorktree.value = draft?.worktreeDir || '';
    });
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

  private async resolveAndSelectSession(id: string, replace: boolean): Promise<void> {
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

  async loadSession(id: string, epoch = this.selectionEpoch): Promise<void> {
    const sampledAskUser = this.askUser.peek();
    const sampledApproval = this.approval.peek();
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
      if (planSource && typeof planSource === 'object') {
        const raw = planSource as Record<string, unknown>;
        const plan = raw.plan || raw.steps;
        this.currentPlan.value = Array.isArray(plan)
          ? { explanation: String(raw.explanation || ''), plan: plan as CurrentPlan['plan'] }
          : null;
      } else this.currentPlan.value = null;
      this.goal.value =
        state.goal && typeof state.goal === 'object' ? (state.goal as Goal) : updated.goal || null;
      this.applyWidgetStatus(selected.widget_status);
      const asks = Array.isArray(state.pending_ask_users)
        ? state.pending_ask_users
        : [state.pending_ask_user];
      const approvals = Array.isArray(state.pending_approvals)
        ? state.pending_approvals
        : [state.pending_approval];
      if (this.askUser.peek() === sampledAskUser)
        this.askUser.value = askUserPrompt(asks.find(Boolean), updated.id);
      if (this.approval.peek() === sampledApproval)
        this.approval.value = approvalPrompt(approvals.find(Boolean), updated.id);
      const activeResponse = String(state.active_response_id || updated.activeResponseId || '');
      if (activeResponse) this.retireCommittedIntents(updated.id, incoming.messages);
      else this.retireIntent(updated.id);
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
      this.streaming.value
    )
      return;
    const clientMessageId = uuid();
    const requestId = uuid();
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
      reconnects: 0,
      requestId,
    };
    this.runs.value = { ...this.runs.value, [sessionId]: initialProjection(run) };
    const postAbort = new AbortController();
    this.postAborts.set(sessionId, postAbort);
    let ownerID = sessionId;
    try {
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
      const selectedModel = this.models.value.find(
        (entry) => entry.id === this.selectedModel.value,
      );
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
      const response = await this.endpoints.createResponse(
        requestBody,
        sessionId.startsWith('draft_') ? '' : sessionId,
        requestId,
        postAbort.signal,
      );
      if (!response.ok || !response.body) {
        if (response.status === 409) {
          const body = await response.text();
          try {
            const parsed = JSON.parse(body) as { error?: { type?: string } };
            if (parsed.error?.type === 'client_message_already_committed') {
              this.retireIntent(sessionId, clientMessageId);
              await this.loadSession(sessionId);
              return;
            }
          } catch {
            /* Normalized below. */
          }
          throw new APIError(body || 'Response conflict', response.status, body);
        }
        throw new APIError(
          (await response.text()) || `Response request returned ${response.status}`,
          response.status,
        );
      }
      const durableSessionId = response.headers.get('x-session-id') || sessionId;
      const responseId = response.headers.get('x-response-id') || '';
      if (!responseId) throw new Error('Server did not return a response id.');
      options.onTransportStarted?.();
      if (durableSessionId !== sessionId) {
        this.rekeySession(sessionId, durableSessionId);
        ownerID = durableSessionId;
      }
      const projection = this.runs.value[ownerID] || this.runs.value[sessionId];
      this.runs.value = {
        ...this.runs.value,
        [ownerID]: {
          ...projection,
          run: { ...projection.run, responseId, sessionId: ownerID, status: 'streaming' },
        },
      };
      clearDraft(this.storage, this.keys.draftMessages, sessionId);
      await this.consumeResponseBody(response.body, ownerID, postAbort.signal);
      const current = this.runs.value[ownerID];
      if (current && ['connecting', 'streaming'].includes(current.run.status))
        await this.resumeResponse(ownerID, responseId);
    } catch (error) {
      if (error instanceof APIError || postAbort.signal.aborted)
        this.rollbackOptimisticIntent(ownerID, clientMessageId);
      if (!postAbort.signal.aborted) {
        options.onTransportFailed?.(error);
        this.failRun(ownerID, error);
        if (
          !options.preserveComposer &&
          this.activeSessionId.peek() === ownerID &&
          !this.prompt.peek()
        ) {
          this.prompt.value = promptContent;
          this.attachments.value = attachments;
        }
      }
    } finally {
      if (this.postAborts.get(sessionId) === postAbort) this.postAborts.delete(sessionId);
    }
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
    this.interjections.value = this.interjections.value.map((entry) =>
      entry.sessionId === oldID ? { ...entry, sessionId: id } : entry,
    );
    if (this.activeSessionId.peek() === oldID) {
      this.activeSessionId.value = id;
      this.storage.setItem(this.keys.activeSession, id);
      updateSessionRoute(this.config.prefix, updated, true);
    }
  }

  private async consumeResponseBody(
    body: ReadableStream<Uint8Array>,
    sessionId: string,
    signal?: AbortSignal,
  ): Promise<void> {
    for await (const frame of decodeSSE(body, signal)) {
      let event: ResponseEvent;
      try {
        const payload = JSON.parse(frame.data) as ResponseEvent;
        event = { ...payload, type: frame.event === 'message' ? payload.type : frame.event };
      } catch {
        continue;
      }
      this.applyResponseEvent(sessionId, event);
    }
  }

  async streamResponse(responseId: string, sessionId: string, after: number): Promise<void> {
    if (this.retiredResponses.has(responseId)) return;
    this.streamAborts.get(sessionId)?.abort();
    const abort = new AbortController();
    this.streamAborts.set(sessionId, abort);
    try {
      const response = await this.endpoints.responseEvents(responseId, after, abort.signal);
      if (!response.ok || !response.body)
        throw new Error(`Response stream returned ${response.status}`);
      await this.consumeResponseBody(response.body, sessionId, abort.signal);
      const projection = this.runs.value[sessionId];
      if (
        projection &&
        ['connecting', 'streaming'].includes(projection.run.status) &&
        !abort.signal.aborted
      )
        await this.resumeResponse(sessionId, responseId);
    } catch (error) {
      if (!abort.signal.aborted) {
        if (error instanceof ResponseProtocolError) {
          await this.resumeResponse(sessionId, responseId);
          return;
        }
        const projection = this.runs.value[sessionId];
        if (projection && ['connecting', 'streaming'].includes(projection.run.status)) {
          const reconnects = projection.run.reconnects + 1;
          this.runs.value = {
            ...this.runs.value,
            [sessionId]: { ...projection, run: { ...projection.run, reconnects } },
          };
          window.setTimeout(
            () => void this.streamResponse(responseId, sessionId, projection.run.lastSequence),
            Math.min(60_000, 1_000 * 1.5 ** Math.min(reconnects, 10)),
          );
        } else this.failRun(sessionId, error);
      }
    } finally {
      if (this.streamAborts.get(sessionId) === abort) this.streamAborts.delete(sessionId);
    }
  }

  applyResponseEvent(sessionId: string, event: ResponseEvent): void {
    const current = this.runs.value[sessionId];
    if (!current) return;
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
    if (next.plan !== current.plan && sessionId === this.activeSessionId.peek())
      this.currentPlan.value = next.plan;
    if (next.askUser && next.askUser !== current.askUser) this.askUser.value = next.askUser;
    if (next.approval && next.approval !== current.approval) this.approval.value = next.approval;
    if (event.type === 'response.interjection') {
      const clientID = String(event.client_message_id || event.interjection_id || '');
      this.interjections.value = this.interjections.value.filter((entry) => entry.id !== clientID);
    }
    if (next.fileChangeRevision !== current.fileChangeRevision) {
      this.fileChangeRevision.value += 1;
      if (this.diff.value.open && this.diff.value.sessionId === sessionId) void this.loadDiff();
    }
    if (
      ['completed', 'cancelled', 'failed'].includes(next.run.status) &&
      next.run.status !== current.run.status
    ) {
      this.retireIntent(sessionId);
      this.sessions.value = this.sessions.value.map((session) =>
        session.id === sessionId
          ? { ...session, activeResponseId: null, lastResponseId: next.run.responseId }
          : session,
      );
      window.setTimeout(
        () => void this.refreshSessionMessages(sessionId, next.run.finalRev || 0),
        0,
      );
    }
  }

  private async refreshSessionMessages(sessionId: string, targetRev = 0): Promise<void> {
    try {
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
          const runs = { ...this.runs.peek() };
          if (runs[sessionId]?.run.responseId === projection.run.responseId) delete runs[sessionId];
          this.runs.value = runs;
        }
      });
      this.retireIntent(sessionId);
      const state = await stateRequest;
      if (state) {
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
    try {
      const snapshot = await this.endpoints.response(responseId);
      // A transcript refresh can complete while the snapshot request is in
      // flight. Never let that late snapshot resurrect a retired projection.
      if (this.retiredResponses.has(responseId)) return;
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
          return {
            ...raw,
            id: String(raw.id || `${responseId}:snapshot:${index}`),
            role: String(raw.role || 'assistant'),
            content: String(raw.content || raw.text || ''),
            created: Number(raw.created || raw.created_at) || Date.now(),
            responseId,
          } as Message;
        })
        .filter(Boolean);
      const status = String(snapshot.status || 'in_progress');
      const next: ResponseProjection = {
        ...existing,
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
        },
      };
      this.runs.value = { ...this.runs.value, [sessionId]: next };
      if (next.run.status === 'streaming')
        await this.streamResponse(responseId, sessionId, next.run.lastSequence);
      else await this.refreshSessionMessages(sessionId, Number(snapshot.final_rev) || 0);
    } catch (error) {
      const projection = this.runs.value[sessionId];
      if (projection && ['connecting', 'streaming'].includes(projection.run.status)) {
        window.setTimeout(
          () => void this.streamResponse(responseId, sessionId, projection.run.lastSequence),
          Math.min(60_000, retryBackoff(projection.run.reconnects)),
        );
      } else this.failRun(sessionId, error);
    }
  }

  async cancel(): Promise<void> {
    const projection = this.activeProjection.value;
    if (!projection || projection.run.status === 'cancelling') return;
    this.runs.value = {
      ...this.runs.value,
      [projection.run.sessionId]: {
        ...projection,
        run: { ...projection.run, status: 'cancelling' },
      },
    };
    this.postAborts.get(projection.run.sessionId)?.abort();
    this.streamAborts.get(projection.run.sessionId)?.abort();
    try {
      await this.endpoints.cancelResponse(projection.run.responseId);
    } catch (error) {
      this.toast(error, 'error');
    }
  }

  async interject(content: string): Promise<void> {
    const session = this.activeSession.value;
    const value = content.trim();
    const attachments = [...this.attachments.value];
    if (!session || (!value && !attachments.length)) return;
    const id = uuid();
    const entry: PendingInterjection = {
      id,
      sessionId: session.id,
      content: value || attachments.map((attachment) => attachment.name).join(', '),
      state: 'sending',
    };
    this.interjections.value = [...this.interjections.value, entry];
    try {
      const attachmentParts = await Promise.all(
        attachments.map((attachment) => this.attachmentInput(attachment)),
      );
      const contentParts = [
        ...attachmentParts,
        ...(value ? [{ type: 'input_text', text: value }] : []),
      ];
      await this.endpoints.interrupt(
        session.id,
        {
          message: value,
          ...(attachmentParts.length ? { content: contentParts } : {}),
          interjection_id: id,
          client_message_id: id,
          delivery: 'steer',
        },
        id,
      );
      this.interjections.value = this.interjections.value.map((candidate) =>
        candidate.id === id ? { ...candidate, state: 'pending' } : candidate,
      );
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
      this.interjections.value = this.interjections.value.map((candidate) =>
        candidate.id === id ? { ...candidate, state: 'failed' } : candidate,
      );
      this.toast(error, 'error');
    }
  }
  async cancelInterjection(id: string): Promise<void> {
    const entry = this.interjections.value.find((candidate) => candidate.id === id);
    if (!entry) return;
    this.interjections.value = this.interjections.value.filter((candidate) => candidate.id !== id);
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
    const message = error instanceof Error ? error.message : String(error);
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

  private trackIntent(sessionId: string, intent: PendingIntentRegistry[string][number]): void {
    const registry = {
      ...this.pendingIntents.value,
      [sessionId]: [
        ...(this.pendingIntents.value[sessionId] || []).filter(
          (entry) => entry.clientMessageId !== intent.clientMessageId,
        ),
        intent,
      ],
    };
    this.pendingIntents.value = registry;
    persistPendingIntent(this.storage, this.keys.pendingIntents, sessionId, intent);
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
    let data = attachment.dataURL || attachment.url || '';
    if (!data && attachment.file)
      data = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result || ''));
        reader.onerror = () =>
          reject(reader.error || new Error(`Could not read ${attachment.name}`));
        reader.readAsDataURL(attachment.file!);
      });
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
    for (const file of Array.from(files)) {
      const attachment: Attachment = {
        id: uuid(),
        name: file.name,
        type: file.type || 'application/octet-stream',
        size: file.size,
        file,
      };
      if (file.type.startsWith('image/')) {
        const preview = URL.createObjectURL(file);
        attachment.previewURL = preview;
        const image = new Image();
        image.onload = () => {
          attachment.width = image.naturalWidth;
          attachment.height = image.naturalHeight;
          this.attachments.value = [...this.attachments.value, attachment];
        };
        image.onerror = () => {
          this.attachments.value = [...this.attachments.value, attachment];
        };
        image.src = preview;
      } else this.attachments.value = [...this.attachments.value, attachment];
    }
  }
  removeAttachment(id: string | undefined): void {
    const attachment = this.attachments.value.find((entry) => entry.id === id);
    if (attachment?.previewURL?.startsWith('blob:')) URL.revokeObjectURL(attachment.previewURL);
    this.attachments.value = this.attachments.value.filter((entry) => entry.id !== id);
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
  }
  async removeSession(session: Session): Promise<void> {
    await this.endpoints.deleteSession(session.id);
    this.sessions.value = this.sessions.value.filter((entry) => entry.id !== session.id);
    if (session.id === this.activeSessionId.value) this.newChat();
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
  async assignProject(projectId: string): Promise<Record<string, unknown> | null> {
    const session = this.projectTarget.value;
    if (!session) return null;
    const response = await this.endpoints.setProject(session.id, { project_id: projectId });
    await this.refreshSidebar();
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
    const enabled = await enableNotifications(this.config, this.endpoints);
    this.notificationsEnabled.value = enabled;
    if (enabled) this.storage.setItem(this.keys.notificationsEnabled, '1');
    else this.storage.removeItem(this.keys.notificationsEnabled);
  }

  async answerAskUser(answers: unknown = [], cancelled = false): Promise<void> {
    const prompt = this.askUser.value;
    if (!prompt) return;
    await this.endpoints.askUser(
      prompt.sessionId,
      cancelled ? { call_id: prompt.callId, cancelled: true } : { call_id: prompt.callId, answers },
    );
    this.askUser.value = null;
  }
  async decideApproval(choice: number, resumeAuto = false): Promise<void> {
    const prompt = this.approval.value;
    if (!prompt) return;
    await this.endpoints.approval(prompt.sessionId, {
      approval_id: prompt.id,
      choice,
      resume_auto: resumeAuto,
    });
    this.approval.value = null;
  }

  async recoverSideQuestion(): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const value = await this.endpoints.sideQuestionState(session.id);
    this.sideQuestion.value = {
      ...this.sideQuestion.value,
      running: Boolean(value.running),
      question: String(value.question || ''),
      response: String(value.response || ''),
      error: String(value.error || ''),
      history: Array.isArray(value.history) ? (value.history as SideQuestionState['history']) : [],
    };
  }

  async askSideQuestion(question: string): Promise<void> {
    const session = this.activeSession.value;
    const value = question.trim();
    if (!session || !value || this.sideQuestion.value.running) return;
    this.sideQuestionAbort?.abort();
    const controller = new AbortController();
    this.sideQuestionAbort = controller;
    this.sideQuestion.value = {
      ...this.sideQuestion.value,
      visible: true,
      running: true,
      question: value,
      response: '',
      error: '',
    };
    try {
      const response = await this.endpoints.startSideQuestion(session.id, value);
      if (!response.ok || !response.body)
        throw new Error((await response.text()) || `Side question failed (${response.status})`);
      let answer = '';
      for await (const frame of decodeSSE(response.body, controller.signal)) {
        let event: Record<string, unknown>;
        try {
          event = JSON.parse(frame.data) as Record<string, unknown>;
        } catch {
          continue;
        }
        if (event.type === 'text_delta') answer += String(event.text || '');
        else if (event.type === 'attempt_discard') answer = '';
        else if (event.type === 'done' && recordValue(event.result))
          answer = String(recordValue(event.result)?.response || answer);
        this.sideQuestion.value = { ...this.sideQuestion.value, response: answer };
      }
      if (!controller.signal.aborted) {
        await this.recoverSideQuestion();
        this.sideQuestion.value = { ...this.sideQuestion.value, visible: true, running: false };
      }
    } catch (error) {
      if (!controller.signal.aborted)
        this.sideQuestion.value = {
          ...this.sideQuestion.value,
          running: false,
          error: error instanceof Error ? error.message : String(error),
        };
    }
  }
  async closeSideQuestion(): Promise<void> {
    const session = this.activeSession.value;
    this.sideQuestionAbort?.abort();
    if (this.sideQuestion.value.running && session)
      await this.endpoints.cancelSideQuestion(session.id).catch(() => undefined);
    this.sideQuestion.value = { ...this.sideQuestion.value, visible: false, running: false };
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
        error: error instanceof Error ? error.message : String(error),
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
        error: error instanceof Error ? error.message : String(error),
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
    if (kind === 'fork' && !anchor) {
      this.toast(
        'The conversation does not yet have a durable completed boundary to fork from.',
        'error',
      );
      return;
    }
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

  async toggleDiff(): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    this.planOpen.value = false;
    this.diff.value = { ...this.diff.value, sessionId: session.id, open: !this.diff.value.open };
    if (this.diff.value.open) await this.loadDiff();
  }
  async loadDiff(): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const owner = session.id;
    const scope = normalizeDiffScope(this.diff.value.scope);
    this.diff.value = { ...this.diff.value, sessionId: owner, scope, loading: true, error: '' };
    try {
      const data = await this.endpoints.fileChanges(owner, scope);
      if (this.activeSessionId.peek() !== owner) return;
      const existing = new Map(this.diff.value.files.map((file) => [file.path, file]));
      const files = sortDiffFiles(
        listFrom(data, 'file_changes', 'files', 'changes')
          .map((entry) => {
            const path = String(entry.path || '');
            const previous = existing.get(path);
            return {
              path,
              old_path: String(entry.old_path || ''),
              status: String(entry.status || entry.kind || ''),
              additions: Number(entry.adds ?? entry.additions) || 0,
              deletions: Number(entry.dels ?? entry.deletions) || 0,
              binary: Boolean(entry.binary || entry.is_binary),
              image: Boolean(entry.image || entry.is_image),
              beforeURL: String(entry.before_url || entry.old_url || previous?.beforeURL || ''),
              afterURL: String(entry.after_url || entry.new_url || previous?.afterURL || ''),
              lastChangedAt: Number(entry.last_changed_at || entry.updated_at) || 0,
              sequence: Number(entry.seq ?? entry.sequence) || 0,
              snapshotSeq: Number(entry.snapshot_seq) || 0,
              expanded: previous?.expanded,
              lines: previous?.lines,
              patch: previous?.patch,
            };
          })
          .filter((entry) => entry.path),
      );
      this.diff.value = { ...this.diff.value, files, git: Boolean(data.git), loading: false };
    } catch (error) {
      if (this.activeSessionId.peek() === owner)
        this.diff.value = {
          ...this.diff.value,
          loading: false,
          error: error instanceof Error ? error.message : String(error),
        };
    }
  }
  async expandDiff(file: DiffFile, context = 0): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    if (file.lines && !context) {
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          entry.path === file.path ? { ...entry, expanded: !entry.expanded } : entry,
        ),
      };
      return;
    }
    this.diff.value = {
      ...this.diff.value,
      files: this.diff.value.files.map((entry) =>
        entry.path === file.path ? { ...entry, loading: true, error: '' } : entry,
      ),
    };
    try {
      const data = await this.endpoints.fileDiff(
        session.id,
        file.path,
        this.diff.value.scope,
        context,
        file.snapshotSeq || 0,
      );
      const lines = Array.isArray(data.hunks)
        ? linesFromHunks(data.hunks)
        : Array.isArray(data.lines)
          ? (data.lines as DiffFile['lines'])
          : parseUnifiedPatch(String(data.diff || data.patch || ''));
      const image = Boolean(data.image || file.image);
      const status = String(data.kind || file.status || '').toLowerCase();
      const beforeURL =
        image && !['add', 'added', 'create', 'created'].includes(status)
          ? this.endpoints.fileContentURL(
              session.id,
              file.path,
              this.diff.value.scope,
              'before',
              file.snapshotSeq || 0,
            )
          : String(data.before_url || file.beforeURL || '');
      const afterURL =
        image && !['delete', 'deleted', 'remove', 'removed'].includes(status)
          ? this.endpoints.fileContentURL(
              session.id,
              file.path,
              this.diff.value.scope,
              'after',
              file.snapshotSeq || 0,
            )
          : String(data.after_url || file.afterURL || '');
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          entry.path === file.path
            ? {
                ...entry,
                status: String(data.kind || entry.status || ''),
                expanded: true,
                loading: false,
                lines,
                image,
                truncated: Boolean(data.truncated),
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
      this.diff.value = {
        ...this.diff.value,
        files: this.diff.value.files.map((entry) =>
          entry.path === file.path
            ? {
                ...entry,
                loading: false,
                error: error instanceof Error ? error.message : String(error),
              }
            : entry,
        ),
      };
    }
  }
  private prepareDiffComments(comments: DiffComment[]): {
    payloads: Array<Record<string, unknown>>;
    inputText: string;
  } | null {
    const payloads = comments.map((comment) => {
      const scope = normalizeDiffScope(comment.scope || this.diff.value.scope);
      const turnScope = ['last_turn', 'last_3_turns'].includes(scope);
      return {
        id: comment.id || uuid(),
        path: comment.path,
        scope,
        side: comment.side,
        line: comment.line,
        file_change_seq: turnScope ? Number(comment.fileChangeSeq) || 0 : 0,
        line_text: comment.context || '',
        instruction: comment.body,
      };
    });
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
    if (this.streaming.value) {
      this.toast('Wait for the current response before sending an inline comment.', 'info');
      return;
    }
    const prepared = this.prepareDiffComments([comment]);
    if (!prepared) return;
    await this.send({
      contentParts: prepared.payloads.map((diff_comment) => ({
        type: 'diff_comment',
        diff_comment,
      })),
      inputText: prepared.inputText,
      displayContent: comment.body,
      preserveComposer: true,
      diffComments: [comment],
      onTransportStarted: () => this.toast('Comment sent to the agent.', 'success'),
      onTransportFailed: (error) => this.toast(error, 'error'),
    });
  }
  queueDiffComment(comment: DiffComment): void {
    const sessionId = this.activeSessionId.peek();
    const value = { ...comment, id: comment.id || uuid(), sessionId };
    const comments = [
      ...this.diff.value.comments.filter(
        (entry) =>
          !(
            entry.sessionId === sessionId &&
            entry.path === comment.path &&
            entry.side === comment.side &&
            entry.line === comment.line
          ),
      ),
      value,
    ];
    this.diff.value = { ...this.diff.value, comments };
    writeJSON(this.storage, this.keys.diffCommentQueue, comments);
  }
  async sendDiffComments(): Promise<void> {
    const session = this.activeSession.value;
    const comments = this.diff.value.comments.filter(
      (comment) => !comment.sessionId || comment.sessionId === session?.id,
    );
    if (!session || !comments.length) return;
    if (this.streaming.value) {
      this.toast('Wait for the current response before sending inline comments.', 'info');
      return;
    }
    const prepared = this.prepareDiffComments(comments);
    if (!prepared) return;
    const { payloads, inputText } = prepared;
    await this.send({
      contentParts: payloads.map((diff_comment) => ({ type: 'diff_comment', diff_comment })),
      inputText,
      displayContent:
        comments.length === 1 ? comments[0].body : `${comments.length} inline comments`,
      preserveComposer: true,
      diffComments: comments,
      onTransportStarted: () => {
        const remaining = this.diff
          .peek()
          .comments.filter((comment) => comment.sessionId && comment.sessionId !== session.id);
        this.diff.value = { ...this.diff.peek(), comments: remaining };
        writeJSON(this.storage, this.keys.diffCommentQueue, remaining);
        this.toast('Comments sent to the agent.', 'success');
      },
      onTransportFailed: (error) => this.toast(error, 'error'),
    });
  }
  resizeDiff(width: number): void {
    this.diff.value = { ...this.diff.value, width };
    this.storage.setItem(this.keys.diffSidebarWidth, String(Math.round(width)));
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
      window.setTimeout(() => void this.followSkillRun(runId), 1_000);
  }
  async invokeSkill(name: string, args: string): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
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
      existing.set(entry.id, this.mergeSession(existing.get(entry.id), entry)),
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
      existing.set(entry.id, this.mergeSession(existing.get(entry.id), entry)),
    );
    this.sessions.value = [...existing.values()].sort(
      (a, b) => Number(b.pinned) - Number(a.pinned) || b.lastMessageAt - a.lastMessageAt,
    );
    this.noProjectCursor.value = String(data.next_cursor || '');
  }
  async mutateProject(project: Project, patch: Record<string, unknown>): Promise<void> {
    await this.endpoints.patchProject(project.id, patch);
    await this.refreshSidebar();
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
      if (document.visibilityState === 'visible') await this.refreshStatus().catch(() => undefined);
      const anyActive = Object.values(this.runs.peek()).some((projection) =>
        ['connecting', 'streaming', 'cancelling'].includes(projection.run.status),
      );
      this.statusTimer = window.setTimeout(poll, anyActive ? 2_000 : 30_000);
    };
    this.statusTimer = window.setTimeout(poll, 30_000);
  }
  private async refreshStatus(): Promise<void> {
    const data = await this.endpoints.sessionStatus(this.activeSessionId.peek());
    const statuses = listFrom(data, 'sessions', 'items');
    const byID = new Map(statuses.map((entry) => [String(entry.id || entry.session_id), entry]));
    this.sessions.value = this.sessions.value.map((session) => {
      const status = byID.get(session.id);
      if (!status) return session;
      const activeResponseId = String(status.active_response_id || '') || null;
      const transcriptRev = Number(status.transcript_rev) || session.transcriptRev || 0;
      if (activeResponseId && activeResponseId !== session.activeResponseId)
        window.setTimeout(() => void this.resumeResponse(session.id, activeResponseId), 0);
      if (
        !activeResponseId &&
        (session.activeResponseId || this.pendingIntents.peek()[session.id]?.length) &&
        transcriptRev >= (this.runs.peek()[session.id]?.run.finalRev || 0)
      )
        window.setTimeout(() => void this.refreshSessionMessages(session.id, transcriptRev), 0);
      return {
        ...session,
        activeResponseId,
        lastResponseId: String(status.last_response_id || '') || session.lastResponseId,
        transcriptRev,
        messageCount: Number(status.message_count) || session.messageCount,
        lastMessageAt: Number(status.last_message_at)
          ? Number(status.last_message_at) *
            (Number(status.last_message_at) < 10_000_000_000 ? 1000 : 1)
          : session.lastMessageAt,
      };
    });
  }

  toast(value: unknown, kind: Toast['kind'] = 'info'): void {
    const message = value instanceof Error ? value.message : String(value);
    const toast = { id: uuid(), message, kind };
    this.toasts.value = [...this.toasts.value, toast];
    window.setTimeout(() => {
      this.toasts.value = this.toasts.value.filter((entry) => entry.id !== toast.id);
    }, 4000);
  }
  async hardRefresh(): Promise<void> {
    await hardRefreshAssets(this.config);
  }
}
