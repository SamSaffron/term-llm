import { computed, signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { AppConfig } from '../app/config';
import { APIError, type APIClient } from '../api/client';
import type { Endpoints } from '../api/endpoints';
import type { ResponseEvent, ResponseProjection } from '../domain/response';
import { DEFAULT_ATTACHMENT_POLICY, type AttachmentPolicy } from '../domain/attachments';
import type {
  ApprovalPrompt,
  AskUserPrompt,
  Attachment,
  CurrentPlan,
  DiffComment,
  DiffFile,
  Goal,
  InteractionRecord,
  MCPServer,
  Message,
  Project,
  Session,
  Widget,
} from '../domain/types';
import {
  readPendingIntents,
  type PendingIntentRegistry,
  type StorageKeys,
} from '../platform/storage';
import { sessionIDFromLocation } from '../platform/routing';
import { eventFeedCapability } from '../platform/server-events';
import { NotificationController, type NotificationState } from '../platform/notifications';
import { type TabEventType } from '../platform/tab-sync';
import type { StreamSupervisor } from './stream-supervisor';
import { AppStoreServices, type StoreDiagnostics } from './app-store-services';
import { RuntimeStore } from './runtime-store';
import { InteractionStore } from './interaction-store';
import { SideQuestionStore } from './side-question-store';
import { MCPStore } from './mcp-store';
import { WorktreeStore } from './worktree-store';
import { GoalStore } from './goal-store';
import { PlanStore } from './plan-store';
import { BranchStore } from './branch-store';
import { WidgetStore } from './widget-store';
import { ReviewStore } from './review-store';
import { TabSyncCoordinator } from './tab-sync-coordinator';
import { ServerEventCoordinator } from './server-event-coordinator';
import { ComposerStore } from './composer-store';
import { SessionStore, type SidebarView } from './session-store';
import { SkillStore } from './skill-store';
import { StatusReconciler } from './status-reconciler';
import { RunEngine } from './run-engine';
import { SelectionStore } from './selection-store';
import type {
  DiffState,
  HubAgent,
  Modal,
  PendingInterjection,
  RuntimeOption,
  SendOptions,
  SideQuestionState,
  Toast,
} from './store-types';
import { recordValue } from './store-utils';

export type {
  DiffState,
  HubAgent,
  Modal,
  PendingInterjection,
  RuntimeOption,
  SideQuestionState,
  Toast,
} from './store-types';
export type { StatusRequestMetadata } from './status-reconciler';

export class AppStore {
  readonly services: AppStoreServices;
  readonly runtime: RuntimeStore;
  readonly interactionStore: InteractionStore;
  readonly sideQuestions: SideQuestionStore;
  readonly mcpStore: MCPStore;
  readonly worktreeStore: WorktreeStore;
  readonly goalStore: GoalStore;
  readonly planStore: PlanStore;
  readonly branchStore: BranchStore;
  readonly widgetStore: WidgetStore;
  readonly reviewStore: ReviewStore;
  readonly tabSyncCoordinator: TabSyncCoordinator;
  readonly serverEventCoordinator: ServerEventCoordinator;
  readonly composer: ComposerStore;
  readonly sessionStore: SessionStore;
  readonly skillStore: SkillStore;
  readonly statusReconciler: StatusReconciler;
  readonly runEngine: RunEngine;
  readonly selectionStore: SelectionStore;
  readonly keys: StorageKeys;
  readonly api: APIClient;
  readonly endpoints: Endpoints;
  readonly notificationController: NotificationController;

  readonly sessions: Signal<Session[]>;
  readonly recentSessions: Signal<Session[]>;
  readonly recentCursor: Signal<string>;
  readonly sidebarView: Signal<SidebarView>;
  readonly projects: Signal<Project[]>;
  readonly noProjectCursor: Signal<string>;
  readonly projectsEnabled: Signal<boolean>;
  readonly worktreesEnabled: Signal<boolean>;
  readonly activeProjectId: Signal<string>;
  readonly activeSessionId: Signal<string>;
  readonly draftActive: Signal<boolean>;
  readonly providers: Signal<RuntimeOption[]>;
  readonly models: Signal<RuntimeOption[]>;
  readonly selectedProvider: Signal<string>;
  readonly selectedModel: Signal<string>;
  readonly selectedEffort: Signal<string>;
  readonly selectedReasoningMode: Signal<string>;
  readonly selectedAgent: Signal<string>;
  readonly token: Signal<string>;
  readonly prompt: Signal<string>;
  readonly attachments: Signal<Attachment[]>;
  // Acquired synchronously before send() performs attachment materialization.
  // Once a stream supervisor owns the operation, its run status takes over.
  readonly sendPending: Signal<boolean>;
  readonly attachmentPolicy: Signal<AttachmentPolicy>;
  readonly attachmentAccept: ReadonlySignal<string>;
  readonly runs: Signal<Record<string, ResponseProjection>>;
  readonly connected: Signal<boolean>;
  readonly authRequired: Signal<boolean>;
  readonly startup = signal('Loading your chat shell…');
  readonly startupDone = signal(false);
  readonly sidebarCollapsed: Signal<boolean>;
  readonly sidebarOpen: Signal<boolean>;
  readonly sidebarSearch: Signal<string>;
  readonly searchResults: Signal<Session[] | null>;
  readonly searchLoading: Signal<boolean>;
  readonly searchError: Signal<string>;
  readonly showHidden: Signal<boolean>;
  readonly showWidgets: Signal<boolean>;
  readonly notifications: Signal<NotificationState>;
  readonly widgets: Signal<Widget[]>;
  readonly hubAgents: Signal<HubAgent[]>;
  readonly modal = signal<Modal>('');
  readonly toasts: Signal<Toast[]>;
  readonly currentPlan: Signal<CurrentPlan | null>;
  readonly planOpen: Signal<boolean>;
  readonly planSeen: Signal<string | null>;
  readonly planVisible: ReadonlySignal<boolean>;
  readonly askUser: Signal<AskUserPrompt | null>;
  readonly approval: Signal<ApprovalPrompt | null>;
  readonly interactions: Signal<Record<string, InteractionRecord>>;
  readonly interactionOrder: Signal<string[]>;
  readonly sideQuestion: Signal<SideQuestionState>;
  readonly interjections: Signal<PendingInterjection[]>;
  readonly diff: Signal<DiffState>;
  readonly goal: Signal<Goal | null>;
  readonly mcp: Signal<{
    servers: MCPServer[];
    enabled: string[];
    loading: boolean;
    pending: string;
    error: string;
  }>;
  readonly worktrees: Signal<Record<string, unknown>[]>;
  readonly worktreeError: Signal<string>;
  readonly selectedDraftWorktree: Signal<string>;
  readonly currentWorktreeDir: ReadonlySignal<string>;
  readonly skills: Signal<Record<string, unknown>[]>;
  readonly branchTree: Signal<Record<string, unknown> | null>;
  readonly branchPathCount: Signal<number>;
  readonly branchTarget: Signal<string>;
  readonly branchPrefill: Signal<string>;
  readonly branchBusy: Signal<boolean>;
  readonly branchError: Signal<string>;
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
  readonly renameTarget: Signal<Session | null>;
  readonly projectTarget: Signal<Session | null>;
  readonly networkState: Signal<'unknown' | 'online' | 'offline' | 'retrying'>;
  readonly diagnostics: Signal<StoreDiagnostics>;
  readonly fileChangeRevision: Signal<number>;
  readonly currentActivityFile: Signal<string>;
  readonly pendingIntents: Signal<PendingIntentRegistry>;

  readonly activeSession: ReadonlySignal<Session | null>;
  readonly activeProjection: ReadonlySignal<ResponseProjection | null>;
  readonly visibleMessages: ReadonlySignal<Message[]>;
  readonly responseTransportAttached: ReadonlySignal<boolean>;
  readonly runActive: ReadonlySignal<boolean>;
  readonly streaming: ReadonlySignal<boolean>;
  readonly runLivenessUnknown: ReadonlySignal<boolean>;
  readonly canStop: ReadonlySignal<boolean>;
  readonly canInterject: ReadonlySignal<boolean>;
  readonly sendBlocked: ReadonlySignal<boolean>;

  private lifecycleInstalled = false;
  private readonly lifecycleAbort = new AbortController();
  private disposed = false;
  private recoveryPromise: Promise<void> | null = null;
  private serverEventFeedEnabled = false;
  private readonly locallyStoppedResponses: Set<string>;

  private get selectionEpoch(): number {
    return this.selectionStore?.generation || 0;
  }

  constructor(
    readonly config: AppConfig,
    readonly storage: Storage = localStorage,
  ) {
    this.services = new AppStoreServices(config, storage, this.modal);
    this.keys = this.services.keys;
    this.api = this.services.api;
    this.endpoints = this.services.endpoints;
    this.notificationController = this.services.notificationController;
    this.token = this.services.token;
    this.authRequired = this.services.authRequired;
    this.networkState = this.services.networkState;
    this.connected = this.services.connected;
    this.toasts = this.services.toasts;
    this.diagnostics = this.services.diagnostics;
    this.notifications = this.services.notifications;
    this.sessionStore = new SessionStore(this.services, {
      hasRun: (sessionId) => Boolean(this.runEngine?.runs.peek()[sessionId]),
      modal: this.modal,
      publishSessionChange: () => this.publishSessionChange(),
      refreshSidebar: () => this.refreshSidebar(),
      newChat: (replace, projectId) => this.newChat(replace, projectId),
    });
    this.sessions = this.sessionStore.sessions;
    this.recentSessions = this.sessionStore.recentSessions;
    this.recentCursor = this.sessionStore.recentCursor;
    this.sidebarView = this.sessionStore.sidebarView;
    this.projects = this.sessionStore.projects;
    this.noProjectCursor = this.sessionStore.noProjectCursor;
    this.projectsEnabled = this.sessionStore.projectsEnabled;
    this.worktreesEnabled = this.sessionStore.worktreesEnabled;
    this.activeProjectId = this.sessionStore.activeProjectId;
    this.activeSessionId = this.sessionStore.activeSessionId;
    this.draftActive = this.sessionStore.draftActive;
    this.sidebarCollapsed = this.sessionStore.sidebarCollapsed;
    this.sidebarOpen = this.sessionStore.sidebarOpen;
    this.sidebarSearch = this.sessionStore.sidebarSearch;
    this.searchResults = this.sessionStore.searchResults;
    this.searchLoading = this.sessionStore.searchLoading;
    this.searchError = this.sessionStore.searchError;
    this.showHidden = this.sessionStore.showHidden;
    this.hubAgents = this.sessionStore.hubAgents;
    this.renameTarget = this.sessionStore.renameTarget;
    this.projectTarget = this.sessionStore.projectTarget;
    this.activeSession = this.sessionStore.activeSession;
    this.showWidgets = signal(storage.getItem(this.keys.showWidgetsSidebar) !== '0');
    // The legacy boolean was optimistic and is never authoritative. Enrollment
    // is reconstructed from browser and server state below.
    storage.removeItem(this.keys.notificationsEnabled);
    const streamingProxy = computed(() => this.runEngine?.streaming.value ?? false);
    this.runtime = new RuntimeStore(this.services, {
      activeSession: this.activeSession,
      streaming: streamingProxy,
      modal: this.modal,
      bootstrap: () => this.bootstrap(),
    });
    this.providers = this.runtime.providers;
    this.models = this.runtime.models;
    this.selectedProvider = this.runtime.selectedProvider;
    this.selectedModel = this.runtime.selectedModel;
    this.selectedEffort = this.runtime.selectedEffort;
    this.selectedReasoningMode = this.runtime.selectedReasoningMode;
    this.selectedAgent = this.runtime.selectedAgent;
    this.composer = new ComposerStore(this.services, {
      activeSessionId: this.activeSessionId,
      activeProjectId: this.activeProjectId,
      draftActive: this.draftActive,
      selectedProvider: this.selectedProvider,
      selectedModel: this.selectedModel,
      selectedEffort: this.selectedEffort,
      selectedReasoningMode: this.selectedReasoningMode,
      selectedAgent: this.selectedAgent,
      setPreference: (name, value, commit) => this.runtime.setPreference(name, value, commit),
      publishDraftChange: (sessionId, revision, operationId) =>
        this.publishSessionChange('draft-changed', sessionId, '', revision, operationId),
    });
    this.prompt = this.composer.prompt;
    this.attachments = this.composer.attachments;
    this.sendPending = this.composer.sendPending;
    this.attachmentPolicy = this.composer.attachmentPolicy;
    this.attachmentAccept = this.composer.attachmentAccept;
    this.selectedDraftWorktree = this.composer.selectedDraftWorktree;
    this.interactionStore = new InteractionStore(
      this.services,
      this.modal,
      (type, sessionId, responseId) => this.publishSessionChange(type, sessionId, responseId),
    );
    this.askUser = this.interactionStore.askUser;
    this.approval = this.interactionStore.approval;
    this.interactions = this.interactionStore.interactions;
    this.interactionOrder = this.interactionStore.order;
    this.sideQuestions = new SideQuestionStore(this.services, {
      activeSession: this.activeSession,
      activeSessionId: this.activeSessionId,
      draftActive: this.draftActive,
      modal: this.modal,
    });
    this.sideQuestion = this.sideQuestions.state;
    this.mcpStore = new MCPStore(this.services, {
      activeSession: this.activeSession,
      patchSession: (id, patch) => this.sessionStore.patch(id, patch),
    });
    this.mcp = this.mcpStore.state;
    this.worktreeStore = new WorktreeStore(this.services, {
      projectsEnabled: this.projectsEnabled,
      worktreesEnabled: this.worktreesEnabled,
      activeProjectId: this.activeProjectId,
      activeSession: this.activeSession,
      draftActive: this.draftActive,
      prompt: this.prompt,
      projects: this.projects,
      modal: this.modal,
      selectedDraftWorktree: this.selectedDraftWorktree,
      draftStorageId: () => this.draftStorageID(),
      patchSession: (id, patch) => this.sessionStore.patch(id, patch),
      send: (options) => this.send(options),
    });
    this.worktrees = this.worktreeStore.worktrees;
    this.worktreeError = this.worktreeStore.error;
    this.currentWorktreeDir = this.worktreeStore.currentDir;
    this.goalStore = new GoalStore(this.services, this.activeSession, this.modal);
    this.goal = this.goalStore.state;
    this.reviewStore = new ReviewStore(this.services, {
      activeSession: this.activeSession,
      activeSessionId: this.activeSessionId,
      streaming: streamingProxy,
      closePlan: () => this.planStore.close(),
      restartStatusPoll: () => this.startStatusPoll(),
      send: (options) => this.send(options),
      interject: (content, options) => this.interject(content, options),
      publishSessionChange: (type, sessionId) => this.publishSessionChange(type, sessionId),
    });
    this.diff = this.reviewStore.diff;
    this.planStore = new PlanStore(() => {
      this.diff.value = { ...this.diff.peek(), open: false };
    });
    this.currentPlan = this.planStore.current;
    this.planOpen = this.planStore.openState;
    this.planSeen = this.planStore.seen;
    this.planVisible = this.planStore.visible;
    this.runEngine = new RunEngine(
      this.services,
      this.sessionStore,
      this.composer,
      this.runtime,
      this.interactionStore,
      this.planStore,
      this.reviewStore,
      {
        loadSession: (id, epoch) => this.loadSession(id, epoch),
        refreshSidebar: (authoritative) => this.refreshSidebar(authoritative),
        publishSessionChange: (type, sessionId, responseId, revision, operationId) =>
          this.publishSessionChange(type, sessionId, responseId, revision, operationId),
        reconcile: (reason, authoritative) => this.reconcile(reason, { authoritative }),
        statusGeneration: () => this.statusReconciler?.generation || 0,
        refreshStatus: (authoritative) => this.refreshStatus(authoritative),
        refreshSessionMessages: (sessionId, targetRev, responseId) =>
          this.refreshSessionMessages(sessionId, targetRev, responseId),
        resumeResponse: (sessionId, responseId) => this.resumeResponse(sessionId, responseId),
        streamResponse: (responseId, sessionId, sequence) =>
          this.streamResponse(responseId, sessionId, sequence),
        applyResponseEvent: (sessionId, event, owner) =>
          this.applyResponseEvent(sessionId, event, owner),
      },
    );
    this.runs = this.runEngine.runs;
    this.pendingIntents = this.runEngine.pendingIntents;
    this.interjections = this.runEngine.interjections;
    this.currentActivityFile = this.runEngine.currentActivityFile;
    this.fileChangeRevision = this.runEngine.fileChangeRevision;
    this.activeProjection = this.runEngine.activeProjection;
    this.visibleMessages = this.runEngine.visibleMessages;
    this.responseTransportAttached = this.runEngine.responseTransportAttached;
    this.runActive = this.runEngine.runActive;
    this.streaming = this.runEngine.streaming;
    this.runLivenessUnknown = this.runEngine.runLivenessUnknown;
    this.canStop = this.runEngine.canStop;
    this.canInterject = this.runEngine.canInterject;
    this.sendBlocked = this.runEngine.sendBlocked;
    this.locallyStoppedResponses = this.runEngine.locallyStoppedResponses;
    this.widgetStore = new WidgetStore(this.services);
    this.widgets = this.widgetStore.widgets;
    this.branchStore = new BranchStore(this.services, {
      activeSession: this.activeSession,
      activeSessionId: this.activeSessionId,
      draftActive: this.draftActive,
      attachments: this.attachments,
      visibleMessages: this.visibleMessages,
      prompt: this.prompt,
      modal: this.modal,
      refreshSidebar: () => this.refreshSidebar(),
      publishSessionChange: () => this.publishSessionChange(),
      findSession: (id) => this.sessions.peek().find((session) => session.id === id),
      createSession: (value) => this.sessionFrom(value),
      prependSession: (session) => this.sessionStore.prepend(session),
      selectSession: (session) => this.selectSession(session),
      send: () => this.send(),
    });
    this.branchTree = this.branchStore.tree;
    this.branchPathCount = this.branchStore.pathCount;
    this.branchTarget = this.branchStore.target;
    this.branchPrefill = this.branchStore.prefill;
    this.branchBusy = this.branchStore.busy;
    this.branchError = this.branchStore.error;
    this.skillStore = new SkillStore(this.services, {
      activeSession: this.activeSession,
      activeSessionId: this.activeSessionId,
      streaming: this.streaming,
      prompt: this.prompt,
      modal: this.modal,
      updateSession: (id, updater) => this.sessionStore.update(id, updater),
      setRun: (sessionId, projection) => {
        this.runs.value = { ...this.runs.peek(), [sessionId]: projection };
      },
      patchSession: (id, patch) => this.sessionStore.patch(id, patch),
      refreshSessionMessages: (sessionId) => this.refreshSessionMessages(sessionId),
      trackIntent: (sessionId, intent) => this.trackIntent(sessionId, intent),
      retireIntent: (sessionId, clientMessageId) => this.retireIntent(sessionId, clientMessageId),
      streamResponse: (responseId, sessionId, sequence) =>
        this.streamResponse(responseId, sessionId, sequence),
    });
    this.skills = this.skillStore.skills;
    this.selectionStore = new SelectionStore(
      this.services,
      this.sessionStore,
      this.composer,
      this.runEngine,
      this.interactionStore,
      this.sideQuestions,
      this.skillStore,
      this.branchStore,
      this.reviewStore,
      this.planStore,
      this.goalStore,
      this.widgetStore,
      {
        publishSessionChange: (type, sessionId, responseId, revision, operationId) =>
          this.publishSessionChange(type, sessionId, responseId, revision, operationId),
      },
    );
    this.tabSyncCoordinator = new TabSyncCoordinator(this.services, {
      startupDone: this.startupDone,
      activeSessionId: this.activeSessionId,
      currentResponseId: (sessionId) => this.runs.peek()[sessionId]?.run.responseId || '',
      currentRevision: (sessionId) =>
        this.sessions.peek().find((session) => session.id === sessionId)?.transcriptRev,
      draftStorageId: () => this.draftStorageID(),
      reconcileDraftStorage: (sessionId) => this.reconcileDraftStorage(sessionId),
      reloadReviewQueue: () => this.reviewStore.reloadCommentQueue(),
      reconcilePeerChange: () => this.reconcilePeerSessionChange(),
      onPendingIntentStorage: () => {
        this.pendingIntents.value = readPendingIntents(this.storage, this.keys.pendingIntents);
      },
      serverEventsEnabled: () =>
        this.serverEventFeedEnabled && this.serverEventCoordinator?.mode !== 'unsupported',
    });
    this.statusReconciler = new StatusReconciler(this.services, {
      activeSessionId: this.activeSessionId,
      selectionEpoch: () => this.selectionEpoch,
      sessionStore: this.sessionStore,
      pendingIntents: this.pendingIntents,
      runs: this.runs,
      diff: this.diff,
      renameTarget: this.renameTarget,
      reconcile: (reason, authoritative) => this.reconcile(reason, { authoritative }),
      refreshSidebar: (authoritative) => this.refreshSidebar(authoritative),
      resumeResponse: (sessionId, responseId) => this.resumeResponse(sessionId, responseId),
      refreshSessionMessages: (sessionId, targetRev) =>
        this.refreshSessionMessages(sessionId, targetRev),
      syncSessionMessagesForAttach: (sessionId, targetRev) =>
        this.refreshSessionMessages(sessionId, targetRev, '', true),
      refreshDiffComments: (sessionId) => this.refreshDiffComments(sessionId),
      retireIntent: (sessionId, clientMessageId) => this.retireIntent(sessionId, clientMessageId),
      stoppedResponseCount: () => this.locallyStoppedResponses.size,
      isLocallyStopped: (responseId) => this.locallyStoppedResponses.has(responseId),
      clearLocallyStopped: (responseId) => {
        this.locallyStoppedResponses.delete(responseId);
      },
      eventFeedHealthy: () => this.serverEventCoordinator?.isHealthy() || false,
    });
    this.serverEventCoordinator = new ServerEventCoordinator(this.services, {
      startupDone: this.startupDone,
      activeSessionId: this.activeSessionId,
      reconcileCatalog: () => this.refreshSidebar(false),
      reconcileStatus: () => this.refreshStatus(true),
      reconcileActiveSession: (revision) => this.reconcileServerEventActive(revision),
      reconcileFiles: async (sessionId) => {
        if (this.diff.peek().open && this.diff.peek().sessionId === sessionId)
          await this.reviewStore.loadDiff();
      },
      authoritativeRecovery: (reason) => this.authoritativeRecovery(reason),
      eventFeedHealthChanged: () => this.startStatusPoll(),
    });
  }

  async bootstrap(): Promise<void> {
    this.installLifecycle();
    this.startup.value = 'Connecting to term-llm…';
    try {
      const capabilities = await this.endpoints.capabilities().catch(() => ({}));
      this.applyCapabilities(capabilities);
      this.serverEventFeedEnabled = eventFeedCapability(capabilities);
      if (this.serverEventFeedEnabled) await this.serverEventCoordinator.prepare();
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
      this.serverEventCoordinator.updateInterest(this.activeSessionId.peek());
      this.connected.value = true;
      this.networkState.value = 'online';
      this.startupDone.value = true;
      this.serverEventCoordinator.flushBuffered();
      this.startStatusPoll();
      this.tabSyncCoordinator.flushPending();
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
    addEventListener(
      'online',
      () => {
        this.serverEventCoordinator.restart();
        void this.reconcile('online', { authoritative: true });
      },
      { signal: this.lifecycleAbort.signal },
    );
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
        this.serverEventCoordinator.restart();
        if (this.startupDone.value || (event as PageTransitionEvent).persisted)
          void this.reconcile('pageshow', { authoritative: true });
      },
      { signal: this.lifecycleAbort.signal },
    );
    addEventListener(
      'focus',
      () => {
        this.serverEventCoordinator.restart();
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
        this.tabSyncCoordinator.closeChannel();
      },
      { signal: this.lifecycleAbort.signal },
    );
    document.addEventListener(
      'visibilitychange',
      () => {
        if (document.visibilityState === 'visible') {
          this.serverEventCoordinator.restart();
          void this.reconcile('visibility', { authoritative: true });
          void this.refreshHubAgents();
        }
      },
      { signal: this.lifecycleAbort.signal },
    );
    this.ensureSessionSyncChannel();
    this.tabSyncCoordinator.installStorageListener(this.lifecycleAbort.signal);
  }

  private ensureSessionSyncChannel(): void {
    this.tabSyncCoordinator.ensureChannel();
  }

  private authoritativeRecovery(reason: string): Promise<void> {
    if (this.recoveryPromise) return this.recoveryPromise;
    const request = this.reconcile(reason, { authoritative: true });
    const tracked = request.finally(() => {
      if (this.recoveryPromise === tracked) this.recoveryPromise = null;
    });
    this.recoveryPromise = tracked;
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
    this.runEngine.recoverActiveSupervisors();
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
    const projectsEnabled = projects.enabled === true;
    const worktreesEnabled =
      worktrees.enabled === true || (worktrees.enabled === undefined && this.config.worktrees);
    this.sessionStore.applyCapabilities(projectsEnabled, worktreesEnabled);
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
    this.widgetStore.apply(value);
  }

  async loadWidgetStatus(): Promise<void> {
    await this.widgetStore.load();
  }

  private applyProviders(data: Record<string, unknown>): void {
    this.runtime.applyProviders(data);
  }

  private sessionFrom(value: Record<string, unknown>): Session {
    return this.sessionStore.sessionFrom(value);
  }

  private mergeSession(
    existing: Session | undefined,
    incoming: Session,
    replaceMessages = false,
    preserveLiveState = false,
  ): Session {
    return this.sessionStore.mergeSession(existing, incoming, replaceMessages, preserveLiveState);
  }

  private applySidebar(data: Record<string, unknown>): void {
    this.sessionStore.applySidebar(data);
  }

  async loadModels(provider = this.selectedProvider.value): Promise<void> {
    await this.runtime.loadModels(provider);
  }

  async refreshSidebar(authoritative = true): Promise<void> {
    await this.sessionStore.refreshSidebar(authoritative);
  }

  private publishSessionChange(
    type: TabEventType = 'session-upserted',
    sessionId = this.activeSessionId.peek(),
    responseId = this.runs.peek()[sessionId]?.run.responseId || '',
    revision = this.sessions.peek().find((entry) => entry.id === sessionId)?.transcriptRev,
    operationId?: string,
  ): void {
    if (
      this.serverEventFeedEnabled &&
      this.serverEventCoordinator.mode !== 'unsupported' &&
      type !== 'draft-changed' &&
      type !== 'review-comment-changed'
    )
      return;
    this.tabSyncCoordinator.publish(type, sessionId, responseId, revision, operationId);
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

  private async reconcileServerEventActive(revision?: number): Promise<void> {
    const sessionId = this.activeSessionId.peek();
    if (!sessionId) return;
    const currentRevision =
      this.sessions.peek().find((session) => session.id === sessionId)?.transcriptRev || 0;
    if (revision && revision > currentRevision)
      await this.refreshSessionMessages(sessionId, revision).catch(() => undefined);
    if (this.activeSessionId.peek() !== sessionId) return;
    await this.loadSession(sessionId).catch(() => undefined);
  }

  async selectSession(session: Session, replace = false): Promise<void> {
    await this.selectionStore.selectSession(session, replace);
    this.serverEventCoordinator.updateInterest(this.activeSessionId.peek());
  }

  newChat(replace = false, projectId?: string, persistCurrent = true): void {
    this.selectionStore.newChat(replace, projectId, persistCurrent);
    this.serverEventCoordinator.updateInterest('');
  }

  composerOwnerKey(): string {
    return this.composer.ownerKey();
  }

  private draftStorageID(): string {
    return this.composer.storageId();
  }
  private persistCurrentDraft(): void {
    this.composer.persist();
  }
  private reconcileDraftStorage(id: string): void {
    this.composer.reconcileStorage(id);
  }
  private restoreDraftFor(id: string): void {
    this.composer.restore(id, 'draft');
  }
  private syncRuntimeFromSession(session: Session): void {
    this.composer.syncRuntimeFromSession(session);
  }

  async resolveAndSelectSession(id: string, replace = false): Promise<void> {
    await this.selectionStore.resolveAndSelectSession(id, replace);
  }

  private get interjectionRevision(): number {
    return this.runEngine.interjectionStateRevision;
  }

  private reconcilePendingInterjections(
    sessionId: string,
    state: Record<string, unknown>,
    expectedRevision?: number,
  ): void {
    this.runEngine.reconcilePendingInterjections(sessionId, state, expectedRevision);
  }

  async loadSession(id: string, epoch = this.selectionEpoch): Promise<void> {
    await this.selectionStore.loadSession(id, epoch);
  }

  async send(options: SendOptions = {}): Promise<void> {
    await this.runEngine.send(options);
  }

  private rekeySession(oldID: string, id: string, source?: Record<string, unknown>): void {
    this.runEngine.rekeySession(oldID, id, source);
  }

  async streamResponse(responseId: string, sessionId: string, after: number): Promise<void> {
    await this.runEngine.streamResponse(responseId, sessionId, after);
  }

  applyResponseEvent(sessionId: string, event: ResponseEvent, owner?: StreamSupervisor): void {
    this.runEngine.applyResponseEvent(sessionId, event, owner);
  }

  private scheduleTitleReconciliation(
    sessionId: string,
    responseId = this.sessions.peek().find((entry) => entry.id === sessionId)?.lastResponseId || '',
    streamGeneration = this.runEngine.currentSupervisor(sessionId)?.generation || 0,
  ): void {
    this.runEngine.scheduleTitleReconciliation(sessionId, responseId, streamGeneration);
  }

  private async refreshSessionMessages(
    sessionId: string,
    targetRev = 0,
    responseId = '',
    preserveLiveRun = false,
  ): Promise<void> {
    await this.runEngine.refreshSessionMessages(sessionId, targetRev, responseId, preserveLiveRun);
  }

  private async resumeResponse(sessionId: string, responseId: string): Promise<void> {
    await this.runEngine.resumeResponse(sessionId, responseId);
  }

  async cancel(): Promise<void> {
    await this.runEngine.cancel();
  }

  async interject(content: string, options: SendOptions = {}): Promise<void> {
    await this.runEngine.interject(content, options);
  }

  async cancelInterjection(id: string): Promise<void> {
    await this.runEngine.cancelInterjection(id);
  }

  private retireCommittedIntents(sessionId: string, messages: Message[]): void {
    this.runEngine.retireCommittedIntents(sessionId, messages);
  }

  private reconcileLoadedIntents(sessionId: string, messages: Message[], active: boolean): void {
    this.runEngine.reconcileLoadedIntents(sessionId, messages, active);
  }

  private trackIntent(sessionId: string, intent: PendingIntentRegistry[string][number]): void {
    this.runEngine.trackIntent(sessionId, intent);
  }

  private retireIntent(sessionId: string, clientMessageId = ''): void {
    this.runEngine.retireIntent(sessionId, clientMessageId);
  }

  async attachmentInput(attachment: Attachment): Promise<Record<string, unknown>> {
    return this.composer.attachmentInput(attachment);
  }

  addAttachments(files: FileList | File[]): void {
    this.composer.addAttachments(files);
  }

  retryAttachment(id: string | undefined): void {
    this.composer.retryAttachment(id);
  }

  private releaseAttachmentResources(attachments: Attachment[], deleteBlobs: boolean): void {
    this.composer.releaseResources(attachments, deleteBlobs);
  }

  removeAttachment(id: string | undefined): void {
    this.composer.removeAttachment(id);
  }

  async search(query: string): Promise<void> {
    await this.sessionStore.search(query);
  }

  async mutateSession(session: Session, patch: Record<string, unknown>): Promise<void> {
    await this.sessionStore.mutateSession(session, patch);
  }
  async archiveSession(session: Session): Promise<void> {
    await this.sessionStore.archiveSession(session);
  }
  async pinSession(session: Session): Promise<void> {
    await this.sessionStore.pinSession(session);
  }
  openRename(session: Session): void {
    this.sessionStore.openRename(session);
  }
  openProjectPicker(session: Session): void {
    this.sessionStore.openProjectPicker(session);
  }
  openAddProject(): void {
    this.sessionStore.openAddProject();
  }
  async assignProject(projectId: string): Promise<Record<string, unknown> | null> {
    return this.sessionStore.assignProject(projectId);
  }
  async createProjectFromWorkspace(name: string): Promise<Record<string, unknown> | null> {
    return this.sessionStore.createProjectFromWorkspace(name);
  }
  async renameSession(
    change: { name: string } | { generatedShortTitle: string; generatedLongTitle: string },
  ): Promise<void> {
    await this.sessionStore.renameSession(change);
  }
  async improveTitle(): Promise<{
    title: string;
    detail: string;
    abstained?: boolean;
  }> {
    return this.sessionStore.improveTitle();
  }

  setPreference(
    name: 'provider' | 'model' | 'effort' | 'reasoning' | 'agent',
    value: string,
    commit = true,
  ): void {
    this.runtime.setPreference(name, value, commit);
  }
  saveSettings(token: string): void {
    this.runtime.saveSettings(token);
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
    return this.interactionStore.upsert(kind, sessionId, responseId, requestId, prompt);
  }

  private resolveInteractionRecord(
    kind: InteractionRecord['kind'],
    sessionId: string,
    responseId: string,
    requestId: string,
    outcome: string,
    resolvedAt = Date.now(),
  ): void {
    this.interactionStore.resolve(kind, sessionId, responseId, requestId, outcome, resolvedAt);
  }

  private interactionFor(
    kind: InteractionRecord['kind'],
    sessionId: string,
    requestId: string,
    responseId = '',
  ): InteractionRecord | null {
    return this.interactionStore.find(kind, sessionId, requestId, responseId);
  }

  private shouldOpenInteraction(
    kind: InteractionRecord['kind'],
    sessionId: string,
    requestId: string,
  ): boolean {
    return this.interactionStore.shouldOpen(kind, sessionId, requestId);
  }

  dismissInteraction(
    kind: InteractionRecord['kind'],
    promptOverride?: ApprovalPrompt | AskUserPrompt,
  ): void {
    this.interactionStore.dismiss(kind, promptOverride);
  }

  openInteraction(key: string): void {
    this.interactionStore.open(key);
  }

  async answerAskUser(
    answers: unknown = [],
    cancelled = false,
    promptOverride?: AskUserPrompt,
  ): Promise<void> {
    await this.interactionStore.answerAskUser(answers, cancelled, promptOverride);
  }

  async decideApproval(
    choice: number,
    resumeAuto = false,
    promptOverride?: ApprovalPrompt,
    cancelled = false,
  ): Promise<void> {
    await this.interactionStore.decideApproval(choice, resumeAuto, promptOverride, cancelled);
  }

  private resetSideQuestion(): void {
    this.sideQuestions.reset();
  }

  openSideQuestion(question = ''): boolean {
    return this.sideQuestions.open(question);
  }

  setSideQuestionDraft(value: string): void {
    this.sideQuestions.setDraft(value);
  }

  async recoverSideQuestion(sessionId = this.activeSession.peek()?.id || ''): Promise<void> {
    await this.sideQuestions.recover(sessionId);
  }

  async askSideQuestion(question: string): Promise<void> {
    await this.sideQuestions.ask(question);
  }

  cancelSideQuestion(): void {
    this.sideQuestions.cancel();
  }

  closeSideQuestion(): void {
    this.sideQuestions.close();
  }

  async loadMCP(): Promise<void> {
    await this.mcpStore.load();
  }
  async toggleMCP(name: string): Promise<void> {
    await this.mcpStore.toggle(name);
  }
  async saveGoal(goal: Goal | { action: string }): Promise<void> {
    await this.goalStore.save(goal);
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
    includeBranchPoints = false,
  ): Promise<Record<string, unknown> | null> {
    return this.branchStore.refresh(sessionId, includeBranchPoints);
  }
  async loadBranchTree(): Promise<void> {
    await this.branchStore.load();
  }
  async branchCommand(kind: 'fork' | 'thread', message = ''): Promise<void> {
    await this.branchStore.command(kind, message);
  }
  openBranchContext(messageId: string, prefill = ''): void {
    this.branchStore.openContext(messageId, prefill);
  }
  async branchFrom(
    messageId: string,
    context: string,
    focus = '',
    autoSend = '',
    prefill = '',
  ): Promise<boolean> {
    return this.branchStore.branchFrom(messageId, context, focus, autoSend, prefill);
  }

  async refreshDiffComments(sessionId = this.activeSessionId.peek()): Promise<void> {
    await this.reviewStore.refreshDiffComments(sessionId);
  }

  openPlan(): void {
    this.planStore.open();
  }

  closePlan(): void {
    this.planStore.close();
  }

  async toggleDiff(): Promise<void> {
    await this.reviewStore.toggleDiff();
  }
  async openWorktreeDiff(dir: string, title: string): Promise<void> {
    await this.reviewStore.openWorktreeDiff(dir, title);
  }
  async loadDiff(): Promise<void> {
    await this.reviewStore.loadDiff();
  }
  async expandDiff(file: DiffFile, context = 0): Promise<void> {
    await this.reviewStore.expandDiff(file, context);
  }
  async sendDiffComment(comment: DiffComment): Promise<void> {
    await this.reviewStore.sendDiffComment(comment);
  }
  queueDiffComment(comment: DiffComment): void {
    this.reviewStore.queueDiffComment(comment);
  }
  editDiffComment(commentId: string, body: string): void {
    this.reviewStore.editDiffComment(commentId, body);
  }
  reanchorDiffComment(
    commentId: string,
    anchor: Pick<DiffComment, 'path' | 'side' | 'line' | 'context' | 'fileChangeSeq' | 'scope'>,
  ): void {
    this.reviewStore.reanchorDiffComment(commentId, anchor);
  }
  removeDiffComment(commentId: string): void {
    this.reviewStore.removeDiffComment(commentId);
  }
  discardDiffComments(sessionId = this.activeSessionId.peek()): void {
    this.reviewStore.discardDiffComments(sessionId);
  }
  async sendDiffComments(): Promise<void> {
    await this.reviewStore.sendDiffComments();
  }
  resizeDiff(width: number): void {
    this.reviewStore.resizeDiff(width);
  }

  worktreesAvailable(): boolean {
    return this.worktreeStore.available();
  }

  async loadWorktrees(): Promise<void> {
    await this.worktreeStore.load();
  }
  async createWorktree(name: string, clean = false): Promise<void> {
    await this.worktreeStore.create(name, clean);
  }
  chooseDraftWorktree(dir: string): void {
    this.worktreeStore.chooseDraft(dir);
  }
  async switchWorktree(dir: string): Promise<void> {
    await this.worktreeStore.switchTo(dir);
  }
  async mergeWorktree(dir: string, force = false): Promise<Record<string, unknown>> {
    return this.worktreeStore.merge(dir, force);
  }
  async recoverWorktree(dir: string): Promise<Record<string, unknown>> {
    return this.worktreeStore.recover(dir);
  }
  async promoteWorktree(dir: string, branch: string): Promise<Record<string, unknown>> {
    return this.worktreeStore.promote(dir, branch);
  }
  async removeWorktree(dir: string, force = false): Promise<Record<string, unknown>> {
    return this.worktreeStore.remove(dir, force);
  }

  async loadSkills(sessionId = this.activeSession.value?.id || ''): Promise<void> {
    await this.skillStore.loadSkills(sessionId);
  }
  async invokeSkill(name: string, args: string): Promise<void> {
    await this.skillStore.invokeSkill(name, args);
  }
  async cancelSkill(runId: string): Promise<void> {
    await this.skillStore.cancelSkill(runId);
  }

  async mutateTranscript(operation: 'undo' | 'redo'): Promise<void> {
    await this.selectionStore.mutateTranscript(operation);
  }

  setSidebarView(view: SidebarView): void {
    this.sessionStore.setSidebarView(view);
  }

  async loadMoreRecent(): Promise<void> {
    await this.sessionStore.loadMoreRecent();
  }

  async loadMoreProject(projectId: string): Promise<void> {
    await this.sessionStore.loadMoreProject(projectId);
  }

  async loadMoreNoProject(): Promise<void> {
    await this.sessionStore.loadMoreNoProject();
  }
  async mutateProject(project: Project, patch: Record<string, unknown>): Promise<void> {
    await this.sessionStore.mutateProject(project, patch);
  }
  async startProjectChat(projectId: string): Promise<void> {
    await this.sessionStore.startProjectChat(projectId);
  }

  async refreshHubAgents(force = false): Promise<void> {
    await this.sessionStore.refreshHubAgents(force);
  }
  clearHubAttention(id: string): void {
    this.sessionStore.clearHubAttention(id);
  }

  private startStatusPoll(): void {
    this.statusReconciler.start();
  }
  private async refreshStatus(authoritative = false): Promise<void> {
    await this.statusReconciler.refresh(authoritative);
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.persistCurrentDraft();
    this.lifecycleAbort.abort();
    this.sideQuestions.dispose();
    this.runtime.dispose();
    this.sessionStore.dispose();
    this.skillStore.dispose();
    this.statusReconciler.dispose();
    this.runEngine.dispose();
    this.composer.dispose();
    this.tabSyncCoordinator.dispose();
    this.serverEventCoordinator.dispose();
    this.services.dispose();
  }

  toast(value: unknown, kind: Toast['kind'] = 'info'): void {
    this.services.toast(value, kind);
  }
  dismissToast(id: string): void {
    this.services.dismissToast(id);
  }
  async hardRefresh(): Promise<void> {
    await this.services.hardRefresh();
  }
}
