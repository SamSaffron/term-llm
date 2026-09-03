import { batch, computed, signal, type ReadonlySignal } from '@preact/signals';
import { APIError, decodeSSE } from '../api/client';
import {
  initialProjection,
  reduceResponse,
  ResponseProtocolError,
  type ResponseEvent,
  type ResponseProjection,
} from '../domain/response';
import { applyRuntimeToRequest, defaultProvider } from '../domain/runtime';
import { errorMessage } from '../domain/text';
import { mergeDurableProjection, convertServerMessages } from '../domain/transcript';
import type {
  ActiveRun,
  ApprovalPrompt,
  AskUserPrompt,
  Attachment,
  CurrentPlan,
  Message,
  Session,
  ToolCall,
} from '../domain/types';
import {
  persistPendingIntent,
  readPendingIntents,
  removeSessionPendingIntents,
  type PendingIntentRegistry,
} from '../platform/storage';
import { updateSessionRoute } from '../platform/routing';
import { completionEventId } from '../platform/notifications';
import { rebaseHubAssetURL } from '../app/config';
import type { AppStoreServices } from './app-store-services';
import type { ComposerStore } from './composer-store';
import type { SessionStore } from './session-store';
import type { RuntimeStore } from './runtime-store';
import type { InteractionStore } from './interaction-store';
import type { PlanStore } from './plan-store';
import type { ReviewStore } from './review-store';
import { StreamSupervisors, type StreamSupervisor } from './stream-supervisor';
import type { PendingInterjection, SendOptions } from './store-types';
import { approvalPrompt, array, askUserPrompt, listFrom, recordValue, uuid } from './store-utils';
import type { TabEventType } from '../platform/tab-sync';

// Supervisor backoff guarantees seven consecutive failures represent more than
// thirty seconds without a successfully attached response transport.
const STALE_RESPONSE_RECOVERY_FAILURES = 7;
const RESPONSE_STREAM_CONNECT_TIMEOUT_MS = 15_000;

function retainInitiatingMessages(existing: Message[], incoming: Message[]): Message[] {
  const incomingIDs = new Set(incoming.map((message) => message.id));
  const incomingClientIDs = new Set(
    incoming.map((message) => message.clientMessageId).filter(Boolean),
  );
  const initiating = existing.filter(
    (message) =>
      message.role === 'user' &&
      Boolean(message.clientMessageId) &&
      message.id === `pending_${message.clientMessageId}` &&
      !incomingIDs.has(message.id) &&
      !incomingClientIDs.has(message.clientMessageId),
  );
  return [...initiating, ...incoming];
}

export interface RunEngineHost {
  loadSession: (id: string, epoch?: number) => Promise<void>;
  refreshSidebar: (authoritative?: boolean) => Promise<void>;
  publishSessionChange: (
    type?: TabEventType,
    sessionId?: string,
    responseId?: string,
    revision?: number,
    operationId?: string,
  ) => void;
  reconcile: (reason: string, authoritative: boolean) => Promise<void>;
  statusGeneration: () => number;
  refreshStatus: (authoritative?: boolean) => Promise<void>;
  refreshSessionMessages: (
    sessionId: string,
    targetRev?: number,
    responseId?: string,
  ) => Promise<void>;
  resumeResponse: (sessionId: string, responseId: string) => Promise<void>;
  streamResponse: (responseId: string, sessionId: string, sequence: number) => Promise<void>;
  applyResponseEvent: (sessionId: string, event: ResponseEvent, owner?: StreamSupervisor) => void;
  draftMCPEnabled: (draftId: string) => string[];
  rekeyMCP: (oldId: string, newId: string) => void;
}

/** Owns response creation, transport, recovery, intents, interjections, and run projections. */
export class RunEngine {
  readonly runs = signal<Record<string, ResponseProjection>>({});
  readonly pendingIntents = signal<PendingIntentRegistry>({});
  readonly interjections = signal<PendingInterjection[]>([]);
  readonly currentActivityFile = signal('');
  readonly fileChangeRevision = signal(0);
  readonly activeProjection: ReadonlySignal<ResponseProjection | null>;
  readonly visibleMessages: ReadonlySignal<Message[]>;
  readonly responseTransportAttached: ReadonlySignal<boolean>;
  readonly runActive: ReadonlySignal<boolean>;
  readonly streaming: ReadonlySignal<boolean>;
  readonly runLivenessUnknown: ReadonlySignal<boolean>;
  readonly canStop: ReadonlySignal<boolean>;
  readonly canInterject: ReadonlySignal<boolean>;
  readonly sendBlocked: ReadonlySignal<boolean>;

  private readonly supervisors = new StreamSupervisors();
  private readonly activeResponseTransports = signal<
    Record<string, { responseId: string; generation: number }>
  >({});
  private readonly responseRecoveryFailures = signal<
    Record<string, { responseId: string; count: number }>
  >({});
  readonly locallyStoppedResponses = new Set<string>();
  private readonly retiredResponses = new Set<string>();
  private readonly handledCompletionEvents = new Set<string>();
  private readonly titleRefreshTimers = new Map<string, number[]>();
  private interjectionRevision = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly sessionStore: SessionStore,
    private readonly composer: ComposerStore,
    private readonly runtime: RuntimeStore,
    private readonly interactionsStore: InteractionStore,
    private readonly plans: PlanStore,
    private readonly review: ReviewStore,
    private readonly host: RunEngineHost,
  ) {
    this.pendingIntents.value = readPendingIntents(services.storage, services.keys.pendingIntents);
    this.activeProjection = computed(() => {
      const session = sessionStore.activeSession.value;
      return session ? this.runs.value[session.id] || null : null;
    });
    this.visibleMessages = computed(() => {
      const session = sessionStore.activeSession.value;
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
    this.responseTransportAttached = computed(() => {
      const projection = this.activeProjection.value;
      return Boolean(
        projection &&
        ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status) &&
        this.hasActiveResponseTransport(projection.run.sessionId, projection.run.responseId),
      );
    });
    this.runActive = computed(() => {
      const projection = this.activeProjection.value;
      const session = this.sessionStore.activeSession.value;
      if (!projection) return Boolean(session?.activeRun);
      // The projection owns the local response lifecycle. Losing a transport or
      // receiving a contradictory status sample starts recovery; neither is a
      // terminal transition. Only a terminal response event/snapshot may make
      // this projection stop presenting as running.
      return ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status);
    });
    // Compatibility alias for consumers that mean "a response is running", not
    // "this tab currently owns an event-stream body".
    this.streaming = computed(
      () => this.runActive.value && this.services.networkState.value !== 'offline',
    );
    this.canStop = computed(() => {
      const projection = this.activeProjection.value;
      return Boolean(
        this.streaming.value &&
        projection &&
        !projection.run.responseId.startsWith('pending_') &&
        ['connecting', 'streaming', 'cancelling'].includes(projection.run.status),
      );
    });
    this.canInterject = computed(() => {
      const projection = this.activeProjection.value;
      return Boolean(
        this.streaming.value &&
        projection &&
        !projection.run.responseId.startsWith('pending_') &&
        ['connecting', 'streaming'].includes(projection.run.status),
      );
    });
    this.runLivenessUnknown = computed(() => {
      const projection = this.activeProjection.value;
      const failures = projection
        ? this.responseRecoveryFailures.value[projection.run.sessionId]
        : undefined;
      return Boolean(
        this.services.networkState.value !== 'offline' &&
        projection &&
        !projection.run.responseId.startsWith('pending_') &&
        ['connecting', 'streaming'].includes(projection.run.status) &&
        !this.responseTransportAttached.value &&
        failures?.responseId === projection.run.responseId &&
        failures.count >= STALE_RESPONSE_RECOVERY_FAILURES,
      );
    });
    this.sendBlocked = computed(() => {
      const projection = this.activeProjection.value;
      const projectedRunPending = Boolean(
        projection &&
        ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status) &&
        !this.canInterject.value,
      );
      return (
        composer.sendPending.value ||
        projectedRunPending ||
        Boolean(sessionStore.activeSession.value?.activeRun && !this.canInterject.value) ||
        Boolean(
          this.pendingIntents.value[sessionStore.activeSessionId.value]?.some(
            (intent) => intent.state === 'checking',
          ),
        )
      );
    });
  }

  private clearResponseRecoveryFailures(sessionId: string, responseId: string): void {
    const current = this.responseRecoveryFailures.peek()[sessionId];
    if (!current || current.responseId !== responseId) return;
    const next = { ...this.responseRecoveryFailures.peek() };
    delete next[sessionId];
    this.responseRecoveryFailures.value = next;
  }

  private markResponseRecoveryFailure(sessionId: string, responseId: string): void {
    if (!sessionId || !responseId || responseId.startsWith('pending_')) return;
    const current = this.responseRecoveryFailures.peek()[sessionId];
    this.responseRecoveryFailures.value = {
      ...this.responseRecoveryFailures.peek(),
      [sessionId]: {
        responseId,
        count: current?.responseId === responseId ? current.count + 1 : 1,
      },
    };
  }

  markResponseTransportActive(sessionId: string, responseId: string, generation = 0): void {
    this.clearResponseRecoveryFailures(sessionId, responseId);
    const current = this.activeResponseTransports.peek()[sessionId];
    if (
      !sessionId ||
      !responseId ||
      (current?.responseId === responseId && current.generation === generation)
    )
      return;
    this.activeResponseTransports.value = {
      ...this.activeResponseTransports.peek(),
      [sessionId]: { responseId, generation },
    };
  }

  clearResponseTransport(sessionId: string, responseId = '', generation?: number): void {
    const current = this.activeResponseTransports.peek()[sessionId];
    if (
      !current ||
      (responseId && current.responseId !== responseId) ||
      (generation !== undefined && current.generation !== generation)
    )
      return;
    const next = { ...this.activeResponseTransports.peek() };
    delete next[sessionId];
    this.activeResponseTransports.value = next;
  }

  hasActiveResponseTransport(sessionId: string, responseId: string): boolean {
    return Boolean(
      responseId && this.activeResponseTransports.value[sessionId]?.responseId === responseId,
    );
  }

  stoppedResponseCount(): number {
    return this.locallyStoppedResponses.size;
  }

  isLocallyStopped(responseId: string): boolean {
    return this.locallyStoppedResponses.has(responseId);
  }

  clearLocallyStopped(responseId: string): void {
    this.locallyStoppedResponses.delete(responseId);
  }

  get interjectionStateRevision(): number {
    return this.interjectionRevision;
  }

  currentSupervisor(sessionId: string): StreamSupervisor | undefined {
    return this.supervisors.current(sessionId);
  }

  recoverActiveSupervisors(): void {
    const active = this.sessionStore.sessions.value.filter(
      (session) =>
        this.runs.value[session.id] &&
        ['connecting', 'streaming'].includes(this.runs.value[session.id].run.status),
    );
    for (const session of active) {
      const run = this.runs.value[session.id].run;
      if (run.responseId && !run.responseId.startsWith('pending_')) {
        const owner = this.supervisors.current(session.id);
        if (
          owner?.responseId === run.responseId &&
          this.hasActiveResponseTransport(session.id, run.responseId)
        )
          continue;
        if (!owner || owner.responseId !== run.responseId) {
          const adopted = this.supervisors.begin(session.id, run.responseId, run.lastSequence);
          void this.recoverSupervisor(adopted);
        } else void this.recoverSupervisor(owner);
      }
    }
  }

  private setInterjections(entries: PendingInterjection[]): void {
    this.interjectionRevision += 1;
    this.interjections.value = entries;
  }

  reconcilePendingInterjections(
    sessionId: string,
    state: Record<string, unknown>,
    expectedRevision?: number,
  ): void {
    if (expectedRevision !== undefined && expectedRevision !== this.interjectionRevision) return;
    const hasList = Object.hasOwn(state, 'pending_interjections');
    const hasSingle = Object.hasOwn(state, 'pending_interjection');
    if (!hasList && !hasSingle) return;
    const rawEntries = hasList
      ? array(state.pending_interjections)
      : state.pending_interjection
        ? [state.pending_interjection]
        : [];
    const committed = new Set(
      [
        ...(this.sessionStore.sessions.peek().find((session) => session.id === sessionId)
          ?.messages || []),
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

  async send(options: SendOptions = {}): Promise<void> {
    const promptContent = this.composer.prompt.value.trim();
    const composerDraftId = this.composer.storageId();
    const inputText = options.inputText ?? promptContent;
    const content = options.displayContent ?? inputText;
    const attachments = options.contentParts ? [] : [...this.composer.attachments.value];
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
      this.services.toast(
        blockedAttachment.error || `${blockedAttachment.name} is still being prepared.`,
        'error',
      );
      return;
    }
    // This reservation must happen before the first await. The run projection
    // cannot serve as the entry lock because attachment materialization yields.
    this.composer.sendPending.value = true;
    const clientMessageId = uuid();
    const requestId = uuid();
    const notificationState = this.services.notifications.peek();
    const notificationSubscriptionId =
      notificationState.status === 'subscribed' ? notificationState.subscriptionId || '' : '';

    let session = this.sessionStore.activeSession.value;
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
      const project = this.sessionStore.activeProjectId.value;
      session = {
        id: this.composer.runtimeDraftId(),
        name: '',
        title: content.slice(0, 72) || 'New chat',
        mode: 'chat',
        origin: 'web',
        archived: false,
        pinned: false,
        created: Date.now(),
        lastMessageAt: Date.now(),
        projectId: project,
        projectName: this.sessionStore.projects.value.find((entry) => entry.id === project)?.name,
        worktreeDir: this.composer.selectedDraftWorktree.value || undefined,
        mcpEnabled: this.host.draftMCPEnabled(this.composer.runtimeDraftId()),
        messages: [],
      };
      this.sessionStore.prepend(session);
      this.sessionStore.activate(session);
      // Ownership transferred to Session.worktreeDir before draft mode ended.
      this.composer.selectedDraftWorktree.value = '';
    }
    const sessionId = session.id;
    let attachmentParts: Record<string, unknown>[];
    try {
      attachmentParts = await Promise.all(
        attachments.map((entry) => this.composer.attachmentInput(entry)),
      );
    } catch (error) {
      this.composer.sendPending.value = false;
      this.services.toast(error, 'error');
      return;
    }
    this.sessionStore.update(sessionId, (entry) => ({
      ...entry,
      messages: [...entry.messages, optimistic],
      lastMessageAt: Date.now(),
    }));
    this.trackIntent(sessionId, {
      id: optimistic.id,
      clientMessageId,
      content,
      created: optimistic.created,
      attachments: optimistic.attachments,
    });
    if (!options.preserveComposer)
      this.composer.clearSubmitted(sessionId, promptContent, attachments, composerDraftId);
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
    // Keep the initiating row in the live projection as well as the session.
    // A selected-session hydration can briefly return pre-commit transcript
    // bodies; the projection owns this row until the durable handoff catches up.
    this.runs.value = {
      ...this.runs.value,
      [sessionId]: { ...initialProjection(run), messages: [optimistic] },
    };
    // Ownership changes before any previous controller is touched. The POST
    // stream and every later subscription share this same generation.
    const streamOwner = this.supervisors.begin(sessionId, run.responseId);
    this.composer.sendPending.value = false;
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
    else if (this.sessionStore.projectsEnabled.value) {
      if (session.projectId) requestBody.project_id = session.projectId;
      else requestBody.no_project = true;
    } else requestBody.use_default_workspace = true;
    if (!session.lastResponseId && session.worktreeDir)
      requestBody.worktree_dir = session.worktreeDir;
    if (!session.lastResponseId)
      requestBody.agent = session.agent || this.runtime.selectedAgent.value;
    const selectedModel = this.runtime.models.value.find(
      (entry) => entry.id === this.runtime.selectedModel.value,
    );
    const selectedProvider =
      this.runtime.providers.value.find(
        (entry) => entry.id === this.runtime.selectedProvider.value,
      ) || defaultProvider(this.runtime.providers.value);
    applyRuntimeToRequest(
      requestBody,
      {
        provider: session.activeProvider,
        model: session.activeModel,
        effort: session.activeEffort,
        reasoningMode: session.activeReasoningMode,
      },
      {
        provider: this.runtime.selectedProvider.value,
        model: this.runtime.selectedModel.value,
        effort: this.runtime.selectedEffort.value,
        reasoningMode: this.runtime.selectedReasoningMode.value,
        fast: this.runtime.selectedFast.value,
      },
      selectedModel,
      selectedProvider,
    );

    let unknownAttempts = 0;
    const restoreRejectedComposer = (): void => {
      if (
        !options.preserveComposer &&
        this.sessionStore.activeSessionId.peek() === ownerID &&
        !this.composer.prompt.peek()
      ) {
        this.composer.prompt.value = promptContent;
        this.composer.attachments.value = attachments;
        this.composer.persist();
      }
    };
    const submit = async (signal: AbortSignal): Promise<void> => {
      try {
        const response = notificationSubscriptionId
          ? await this.services.endpoints.createResponse(
              requestBody,
              sessionId,
              requestId,
              signal,
              notificationSubscriptionId,
            )
          : await this.services.endpoints.createResponse(requestBody, sessionId, requestId, signal);
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
        if (!this.supervisors.owns(streamOwner)) return;
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
          this.supervisors.retire(streamOwner);
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
        this.supervisors.scheduleRetry(
          streamOwner,
          () => {
            const retryAbort = this.supervisors.replaceAbort(streamOwner);
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
            this.supervisors.retire(streamOwner);
            await this.host.loadSession(sessionId);
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
    const rawSessionNumber = Number(response.headers.get('x-session-number'));
    const sessionNumber =
      Number.isSafeInteger(rawSessionNumber) && rawSessionNumber > 0 ? rawSessionNumber : undefined;
    const responseId = response.headers.get('x-response-id') || '';
    if (!responseId) throw new Error('Server did not return a response id.');
    options.onTransportStarted?.();
    this.composer.releaseResources(attachments, true);
    if (!this.supervisors.owns(streamOwner)) return streamOwner.sessionId;
    if (!this.supervisors.adoptResponse(streamOwner, responseId)) return streamOwner.sessionId;
    let ownerID = sessionId;
    if (durableSessionId !== sessionId) {
      if (!this.supervisors.rekey(streamOwner, durableSessionId)) return streamOwner.sessionId;
      this.rekeySession(sessionId, durableSessionId, undefined, sessionNumber);
      ownerID = durableSessionId;
    } else if (sessionNumber) {
      this.sessionStore.patch(sessionId, { number: sessionNumber });
    }
    const acceptMessage = (message: Message): Message =>
      message.clientMessageId === clientMessageId
        ? { ...message, pending: false, interruptState: undefined }
        : message;
    this.sessionStore.update(ownerID, (entry) => ({
      ...entry,
      activeResponseId: responseId,
      activeRun: true,
      messages: entry.messages.map(acceptMessage),
    }));
    this.retireIntent(ownerID, clientMessageId);
    this.host.publishSessionChange('run-changed', ownerID, responseId, undefined, clientMessageId);
    const projection = this.runs.value[ownerID] || this.runs.value[sessionId];
    this.runs.value = {
      ...this.runs.value,
      [ownerID]: {
        ...projection,
        messages: projection.messages.map(acceptMessage),
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
    const transportGeneration = streamOwner.transportGeneration;
    this.markResponseTransportActive(ownerID, responseId, transportGeneration);
    await this.consumeResponseBody(response.body, streamOwner, transportGeneration);
    if (!this.supervisors.ownsTransport(streamOwner, transportGeneration)) return ownerID;
    const current = this.runs.value[ownerID];
    if (current && ['connecting', 'streaming'].includes(current.run.status))
      await this.recoverSupervisor(streamOwner);
    return ownerID;
  }

  private markIntentChecking(sessionId: string, clientMessageId: string): void {
    const markChecking = (message: Message): Message =>
      message.clientMessageId === clientMessageId
        ? { ...message, interruptState: 'checking_send', pending: true }
        : message;
    batch(() => {
      this.sessionStore.update(sessionId, (session) => ({
        ...session,
        messages: session.messages.map(markChecking),
      }));
      const projection = this.runs.peek()[sessionId];
      if (projection)
        this.runs.value = {
          ...this.runs.peek(),
          [sessionId]: { ...projection, messages: projection.messages.map(markChecking) },
        };
    });
    const intent = this.pendingIntents
      .peek()
      [sessionId]?.find((entry) => entry.clientMessageId === clientMessageId);
    if (intent) this.trackIntent(sessionId, { ...intent, state: 'checking' });
  }

  rekeySession(
    oldID: string,
    id: string,
    source?: Record<string, unknown>,
    sessionNumber?: number,
  ): void {
    const current = this.sessionStore.sessions.value.find((entry) => entry.id === oldID);
    if (!current || !id) return;
    const incoming = source ? this.sessionStore.sessionFrom(source) : null;
    const updated: Session = {
      ...current,
      ...(incoming || {}),
      ...(sessionNumber ? { number: sessionNumber } : {}),
      id,
      messages: current.messages,
    };
    const duplicate = this.sessionStore.sessions.value.find(
      (entry) => entry.id === id && entry.id !== oldID,
    );
    const remaining = this.sessionStore.sessions.value.filter(
      (entry) => entry.id !== oldID && entry.id !== id,
    );
    const merged = this.sessionStore.mergeSession(duplicate, updated);
    this.sessionStore.replace([merged, ...remaining]);
    this.host.rekeyMCP(oldID, id);
    this.sessionStore.rekeyRecent(oldID, merged);
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
    removeSessionPendingIntents(this.services.storage, this.services.keys.pendingIntents, oldID);
    intents.forEach((intent) =>
      persistPendingIntent(this.services.storage, this.services.keys.pendingIntents, id, intent),
    );
    this.setInterjections(
      this.interjections.value.map((entry) =>
        entry.sessionId === oldID ? { ...entry, sessionId: id } : entry,
      ),
    );
    if (this.sessionStore.activeSessionId.peek() === oldID) {
      this.sessionStore.activeSessionId.value = id;
      this.services.storage.setItem(this.services.keys.activeSession, id);
      if (oldID.startsWith('draft_'))
        this.services.storage.removeItem(this.services.keys.draftSessionActive);
      updateSessionRoute(this.services.config.prefix, updated, true);
    }
  }

  private async consumeResponseBody(
    body: ReadableStream<Uint8Array>,
    owner: StreamSupervisor,
    transportGeneration = owner.transportGeneration,
  ): Promise<boolean> {
    let cleanCompletion = false;
    const watchdog = () =>
      this.supervisors.touchWatchdog(
        owner,
        transportGeneration,
        () => {
          this.services.bumpDiagnostic('streamWatchdogTimeouts');
          owner.abort.abort(new DOMException('Response stream became inactive', 'TimeoutError'));
          this.scheduleSupervisorRetry(owner, new Error('Response stream became inactive'));
        },
        35_000,
      );
    const transportActivity = () => {
      if (this.supervisors.ownsTransport(owner, transportGeneration))
        this.markResponseTransportActive(owner.sessionId, owner.responseId, transportGeneration);
      watchdog();
    };
    watchdog();
    try {
      for await (const frame of decodeSSE(body, owner.abort.signal, transportActivity)) {
        if (!this.supervisors.ownsTransport(owner, transportGeneration)) {
          this.services.bumpDiagnostic('staleStreamCallbacks');
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
        this.host.applyResponseEvent(owner.sessionId, event, owner);
        const projection = this.runs.peek()[owner.sessionId];
        if (projection && ['completed', 'cancelled', 'failed'].includes(projection.run.status))
          cleanCompletion = true;
      }
      return cleanCompletion;
    } finally {
      this.clearResponseTransport(owner.sessionId, owner.responseId, transportGeneration);
      this.supervisors.clearWatchdog(owner, transportGeneration);
    }
  }

  async streamResponse(responseId: string, sessionId: string, after: number): Promise<void> {
    if (this.retiredResponses.has(responseId)) return;
    const current = this.supervisors.current(sessionId);
    if (
      current?.responseId === responseId &&
      (current.recoveryInFlight ||
        current.subscriptionInFlight ||
        this.hasActiveResponseTransport(sessionId, responseId))
    )
      return;
    const owner =
      current && current.responseId === responseId
        ? current
        : this.supervisors.begin(sessionId, responseId, after);
    await this.subscribeSupervisor(owner);
  }

  private async subscribeSupervisor(owner: StreamSupervisor): Promise<void> {
    if (!this.supervisors.startSubscription(owner)) return;
    const abort = this.supervisors.replaceAbort(owner);
    const transportGeneration = owner.transportGeneration;
    if (!abort) {
      this.supervisors.finishSubscription(owner, transportGeneration);
      return;
    }
    this.supervisors.touchWatchdog(
      owner,
      transportGeneration,
      () => {
        this.services.bumpDiagnostic('streamWatchdogTimeouts');
        abort.abort(new DOMException('Response stream connection timed out', 'TimeoutError'));
        this.scheduleSupervisorRetry(owner, new Error('Response stream connection timed out'));
      },
      RESPONSE_STREAM_CONNECT_TIMEOUT_MS,
    );
    try {
      const response = await this.services.endpoints.responseEvents(
        owner.responseId,
        owner.lastSequence,
        abort.signal,
      );
      if (!this.supervisors.ownsTransport(owner, transportGeneration) || abort.signal.aborted)
        return;
      if (!response.ok || !response.body)
        throw new Error(`Response stream returned ${response.status}`);
      this.markResponseTransportActive(owner.sessionId, owner.responseId, transportGeneration);
      const clean = await this.consumeResponseBody(response.body, owner, transportGeneration);
      if (!this.supervisors.ownsTransport(owner, transportGeneration) || abort.signal.aborted)
        return;
      const projection = this.runs.peek()[owner.sessionId];
      if (
        clean &&
        projection &&
        ['completed', 'cancelled', 'failed'].includes(projection.run.status)
      )
        this.supervisors.retire(owner);
      else if (clean && projection)
        this.scheduleSupervisorRetry(owner, new Error('Response ended before terminal event'));
      else if (!clean && projection && ['connecting', 'streaming'].includes(projection.run.status))
        this.scheduleSupervisorRetry(owner, new Error('Response stream ended before completion'));
    } catch (error) {
      if (!this.supervisors.ownsTransport(owner, transportGeneration) || abort.signal.aborted)
        return;
      if (error instanceof ResponseProtocolError) {
        this.scheduleSupervisorRetry(owner, error);
        return;
      }
      this.scheduleSupervisorRetry(owner, error);
    } finally {
      this.supervisors.finishSubscription(owner, transportGeneration);
    }
  }

  private scheduleSupervisorRetry(owner: StreamSupervisor, _error?: unknown): void {
    if (!this.supervisors.owns(owner)) return;
    const projection = this.runs.peek()[owner.sessionId];
    if (!projection || !['connecting', 'streaming'].includes(projection.run.status)) {
      // Transport failures are irrelevant once the response has reached a
      // terminal state. A late stream/subscription callback must not overwrite
      // the completed transcript with local "Load failed" messages.
      this.supervisors.retire(owner);
      return;
    }
    const reconnects = projection.run.reconnects + 1;
    this.services.bumpDiagnostic('supervisorRetries');
    this.runs.value = {
      ...this.runs.peek(),
      [owner.sessionId]: {
        ...projection,
        run: { ...projection.run, status: 'connecting', reconnects },
      },
    };
    const scheduled = this.supervisors.scheduleRetry(
      owner,
      () => void this.recoverSupervisor(owner),
      Math.min(60_000, 1_000 * 1.5 ** Math.min(reconnects, 10)),
    );
    if (scheduled) this.markResponseRecoveryFailure(owner.sessionId, owner.responseId);
  }

  applyResponseEvent(sessionId: string, event: ResponseEvent, owner?: StreamSupervisor): void {
    const current = this.runs.value[sessionId];
    if (!current) return;
    if (owner && !this.supervisors.owns(owner)) {
      this.services.bumpDiagnostic('staleStreamCallbacks');
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
        void this.host.resumeResponse(sessionId, current.run.responseId);
      return;
    }
    if (owner)
      this.markResponseTransportActive(
        sessionId,
        current.run.responseId,
        owner.transportGeneration,
      );
    let next: ResponseProjection;
    try {
      next = reduceResponse(current, event);
    } catch (error) {
      if (error instanceof ResponseProtocolError) {
        void this.host.resumeResponse(sessionId, current.run.responseId);
        return;
      }
      throw error;
    }
    if (owner) this.supervisors.advance(owner, Number(event.sequence_number));
    const response = recordValue(event.response) || {};
    const runtimePatch: Partial<Session> = {};
    if (event.type === 'response.created' || event.type === 'response.completed') {
      runtimePatch.activeModel = String(response.model || '') || undefined;
      runtimePatch.activeProvider = String(response.provider || '') || undefined;
      if (Object.hasOwn(response, 'reasoning_effort'))
        runtimePatch.activeEffort = String(response.reasoning_effort || '');
    } else if (event.type === 'response.model_switch') {
      runtimePatch.activeModel = String(event.to_model || event.model || '') || undefined;
      runtimePatch.activeProvider = String(event.to_provider || event.provider || '') || undefined;
      if (Object.hasOwn(event, 'to_reasoning_effort') || Object.hasOwn(event, 'reasoning_effort'))
        runtimePatch.activeEffort = String(
          event.to_reasoning_effort ?? event.reasoning_effort ?? '',
        );
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
      this.sessionStore.patch(
        sessionId,
        Object.fromEntries(Object.entries(runtimePatch).filter(([, value]) => value !== undefined)),
      );
    if (next.plan !== current.plan && sessionId === this.sessionStore.activeSessionId.peek()) {
      this.plans.update(next.plan);
    }
    if (next.askUser && next.askUser !== current.askUser && next.askUser.callId) {
      this.interactionsStore.present(
        'ask-user',
        sessionId,
        next.run.responseId,
        next.askUser.callId,
        next.askUser,
        Number(event.sequence_number) || 0,
      );
    }
    if (next.approval && next.approval !== current.approval && next.approval.id) {
      this.interactionsStore.present(
        'approval',
        sessionId,
        next.run.responseId,
        next.approval.id,
        next.approval,
        Number(event.sequence_number) || 0,
      );
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
      this.interactionsStore.resolve(
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
      if (this.review.diff.value.open && this.review.diff.value.sessionId === sessionId)
        void this.review.loadDiff();
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
          void this.services.notificationController.signalCompletion(
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
      this.clearResponseTransport(sessionId, next.run.responseId);
      this.retireIntent(sessionId);
      this.sessionStore.patch(sessionId, {
        activeResponseId: null,
        activeRun: false,
        lastResponseId: next.run.responseId,
      });
      this.host.publishSessionChange(
        'run-changed',
        sessionId,
        next.run.responseId,
        undefined,
        next.run.requestId || next.run.responseId,
      );
      this.interactionsStore.cancelOutstandingForResponse(sessionId, next.run.responseId);
      if (owner) this.supervisors.retire(owner);
      if (this.review.diff.peek().open && this.review.diff.peek().sessionId === sessionId)
        void this.review.loadDiff();
      this.services.schedule(
        () =>
          void this.host.refreshSessionMessages(
            sessionId,
            next.run.finalRev || 0,
            next.run.responseId,
          ),
        0,
      );
      if (next.run.status === 'completed')
        this.scheduleTitleReconciliation(sessionId, next.run.responseId, owner?.generation || 0);
    }
  }

  scheduleTitleReconciliation(
    sessionId: string,
    responseId = this.sessionStore.sessions.peek().find((entry) => entry.id === sessionId)
      ?.lastResponseId || '',
    streamGeneration = this.supervisors.current(sessionId)?.generation || 0,
  ): void {
    for (const timer of this.titleRefreshTimers.get(sessionId) || []) window.clearTimeout(timer);
    const title =
      this.sessionStore.sessions.peek().find((entry) => entry.id === sessionId)?.title || '';
    const statusGeneration = this.host.statusGeneration();
    const timers = [2_000, 8_000].map((delay, index) =>
      window.setTimeout(() => {
        const session = this.sessionStore.sessions.peek().find((entry) => entry.id === sessionId);
        const currentOwner = this.supervisors.current(sessionId);
        const ownerReplaced = Boolean(currentOwner && currentOwner.generation > streamGeneration);
        if (
          !this.services.isDisposed &&
          (!session ||
            ((!responseId || session.lastResponseId === responseId) && session.title === title)) &&
          !ownerReplaced &&
          this.host.statusGeneration() === statusGeneration &&
          document.visibilityState === 'visible'
        )
          void this.host.reconcile('title', true).catch(() => undefined);
        if (index === 1) this.titleRefreshTimers.delete(sessionId);
      }, delay),
    );
    this.titleRefreshTimers.set(sessionId, timers);
  }

  async refreshSessionMessages(
    sessionId: string,
    targetRev = 0,
    expectedResponseId = '',
    preserveLiveRun = false,
  ): Promise<void> {
    try {
      const interjectionRevision = this.interjectionRevision;
      const stateRequest = this.services.endpoints.sessionState(sessionId).catch(() => null);
      const selected = await this.services.endpoints.selectedSession(sessionId);
      if (expectedResponseId && this.runs.peek()[sessionId]?.run.responseId !== expectedResponseId)
        return;
      const source = recordValue(selected.selected_session);
      const sideload = recordValue(selected.selected_transcript);
      const bodies = recordValue(sideload?.bodies);
      if (!source || !bodies) return;
      const incomingRevision = Number(bodies.rev ?? source.transcript_rev ?? source.rev);
      const incoming = {
        ...this.sessionStore.sessionFrom({
          ...source,
          // The bodies revision is the generation of the messages being installed.
          // Prefer it over summary metadata, which may have advanced independently.
          transcript_rev: bodies.rev ?? source.transcript_rev ?? source.rev,
          messages: listFrom(bodies, 'messages', 'items'),
        }),
        ...(Number.isFinite(incomingRevision) ? { messageBodiesRev: incomingRevision } : {}),
      };
      const incomingRev = incoming.messageBodiesRev || 0;
      const currentSession = this.sessionStore.sessions
        .peek()
        .find((session) => session.id === sessionId);
      const currentRev = currentSession?.messageBodiesRev ?? currentSession?.transcriptRev ?? 0;
      if (incomingRev < currentRev) return;
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
      const preserveCurrentLiveRun = Boolean(
        preserveLiveRun ||
        (projection &&
          ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status)),
      );
      batch(() => {
        this.sessionStore.update(sessionId, (session) =>
          // Selected-session payloads own transcript bodies, not live response
          // ownership. An active projection remains the lifecycle authority
          // until a terminal event/snapshot is installed.
          this.sessionStore.mergeSession(session, incoming, true, preserveCurrentLiveRun),
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
      if (expectedResponseId && this.runs.peek()[sessionId]?.run.responseId !== expectedResponseId)
        return;
      if (state) {
        this.reconcilePendingInterjections(sessionId, state, interjectionRevision);
        const lastResponseId = String(state.lastResponseId || state.last_response_id || '').trim();
        this.sessionStore.patch(sessionId, { lastResponseId: lastResponseId || null });
      }
    } catch {
      /* Status polling will retry durable reconciliation. */
    }
  }

  async resumeResponse(sessionId: string, responseId: string): Promise<void> {
    if (this.retiredResponses.has(responseId)) return;
    const projected = this.runs.peek()[sessionId];
    if (!projected || projected.run.responseId !== responseId) {
      this.runs.value = {
        ...this.runs.peek(),
        [sessionId]: initialProjection({
          responseId,
          sessionId,
          epoch: 1,
          status: 'connecting',
          lastSequence: 0,
          startedRev: 0,
          reconnects: 0,
        }),
      };
    }
    const current = this.supervisors.current(sessionId);
    const owner =
      current && current.responseId === responseId
        ? current
        : this.supervisors.begin(
            sessionId,
            responseId,
            this.runs.peek()[sessionId]?.run.lastSequence || 0,
          );
    await this.recoverSupervisor(owner);
  }

  async reconcileServerIdleResponse(
    sessionId: string,
    responseId: string,
    transcriptRev: number,
  ): Promise<void> {
    if (transcriptRev <= 0 || this.retiredResponses.has(responseId)) return;
    try {
      await this.host.refreshSessionMessages(sessionId, transcriptRev);
    } catch {
      return;
    }
    const session = this.sessionStore.sessions.peek().find((entry) => entry.id === sessionId);
    const projection = this.runs.peek()[sessionId];
    if (
      !session ||
      session.activeRun ||
      session.activeResponseId ||
      (transcriptRev > 0 && (session.messageBodiesRev || 0) < transcriptRev) ||
      projection?.run.responseId !== responseId ||
      !['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status)
    )
      return;

    this.retiredResponses.add(responseId);
    this.clearResponseTransport(sessionId, responseId);
    const owner = this.supervisors.current(sessionId);
    if (owner?.responseId === responseId) this.supervisors.retire(owner);
    const nextRuns = { ...this.runs.peek() };
    delete nextRuns[sessionId];
    batch(() => {
      this.runs.value = nextRuns;
      this.sessionStore.patch(sessionId, {
        activeRun: false,
        activeResponseId: null,
        lastResponseId: responseId,
      });
    });
    this.retireIntent(sessionId);
  }

  private async waitForSubscriptionIdle(owner: StreamSupervisor): Promise<boolean> {
    const deadline = Date.now() + 1_000;
    while (this.supervisors.owns(owner) && owner.subscriptionInFlight) {
      if (Date.now() >= deadline) return false;
      await new Promise<void>((resolve) => window.setTimeout(resolve, 10));
    }
    return this.supervisors.owns(owner);
  }

  private async recoverSupervisor(owner: StreamSupervisor): Promise<void> {
    if (!this.supervisors.startRecovery(owner)) return;
    this.services.bumpDiagnostic('supervisorRecoveries');
    const abort = this.supervisors.replaceAbort(owner, true);
    const transportGeneration = owner.transportGeneration;
    if (!abort) {
      this.supervisors.finishRecovery(owner);
      return;
    }
    const { sessionId, responseId } = owner;
    try {
      const snapshot = await this.services.endpoints.response(responseId, abort.signal);
      if (!this.supervisors.ownsTransport(owner, transportGeneration) || abort.signal.aborted)
        return;
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
              rebaseAssetURL: (url) => rebaseHubAssetURL(this.services.config, url),
            })[0];
          const clientMessageId = String(raw.clientMessageId || raw.client_message_id || '').trim();
          const projectedResponseId = String(
            raw.responseId || raw.response_id || responseId,
          ).trim();
          const segmentOrdinal = Number(
            raw.assistantSegmentOrdinal ?? raw.assistant_segment_ordinal,
          );
          const segmentStartSequence = Number(
            raw.segmentStartSequence ?? raw.segment_start_sequence,
          );
          const segmentEndSequence = Number(raw.segmentEndSequence ?? raw.segment_end_sequence);
          const interruptState = String(raw.interruptState || raw.interrupt_state || '').trim();
          const rawRole = String(raw.role || 'assistant');
          const role = rawRole === 'compaction-ref' ? 'compaction-boundary' : rawRole;
          const eventSequence = Number(
            raw.eventSequence ?? raw.event_sequence ?? raw.compaction_sequence,
          );
          const compactionSeq = Number(raw.compactionSeq ?? raw.compaction_seq);
          const compactionCount = Number(raw.compactionCount ?? raw.compaction_count);
          const recoveredAt = Date.now();
          const tools = Array.isArray(raw.tools)
            ? raw.tools.map((value) => {
                const tool = recordValue(value) || {};
                const status = String(tool.status || 'running') as ToolCall['status'];
                const rawDuration = tool.durationMs ?? tool.duration_ms;
                const durationMs =
                  rawDuration == null ? undefined : Math.max(0, Number(rawDuration));
                const reportedStart = Number(tool.startedAt ?? tool.started_at) || undefined;
                const startedAt =
                  status === 'running' && durationMs !== undefined && Number.isFinite(durationMs)
                    ? recoveredAt - durationMs
                    : reportedStart;
                return {
                  ...tool,
                  id: String(tool.id || ''),
                  name: String(tool.name || 'tool'),
                  status,
                  ...(startedAt ? { startedAt } : {}),
                  ...(Number(tool.endedAt ?? tool.ended_at)
                    ? { endedAt: Number(tool.endedAt ?? tool.ended_at) }
                    : {}),
                  ...(durationMs !== undefined && Number.isFinite(durationMs)
                    ? { durationMs }
                    : {}),
                } satisfies ToolCall;
              })
            : undefined;
          return {
            ...raw,
            id: String(raw.id || `${responseId}:snapshot:${index}`),
            role,
            content:
              role === 'compaction-boundary'
                ? String(raw.content || raw.text || 'Context compacted')
                : String(raw.content || raw.text || ''),
            created: Number(raw.created || raw.created_at) || Date.now(),
            responseId: projectedResponseId || responseId,
            ...(tools ? { tools } : {}),
            ...(clientMessageId ? { clientMessageId } : {}),
            ...(Number.isFinite(segmentOrdinal) ? { assistantSegmentOrdinal: segmentOrdinal } : {}),
            ...(Number.isFinite(segmentStartSequence) ? { segmentStartSequence } : {}),
            ...(Number.isFinite(segmentEndSequence) ? { segmentEndSequence } : {}),
            ...(Number.isFinite(eventSequence) ? { eventSequence } : {}),
            ...(role === 'model-swap'
              ? {
                  boundaryId: String(raw.boundaryId || raw.boundary_id || '').trim(),
                  fromProvider: String(raw.fromProvider || raw.from_provider || '').trim(),
                  fromModel: String(raw.fromModel || raw.from_model || '').trim(),
                  fromEffort: String(raw.fromEffort || raw.from_reasoning_effort || '').trim(),
                  toProvider: String(raw.toProvider || raw.to_provider || '').trim(),
                  toModel: String(raw.toModel || raw.to_model || '').trim(),
                  toEffort: String(raw.toEffort || raw.to_reasoning_effort || '').trim(),
                  swapStatus: String(raw.swapStatus || raw.swap_status || '').trim(),
                  swapStrategy: String(raw.swapStrategy || raw.swap_strategy || '').trim(),
                }
              : {}),
            ...(Number.isFinite(compactionSeq) ? { compactionSeq } : {}),
            ...(Number.isFinite(compactionCount) ? { compactionCount } : {}),
            ...(interruptState ? { interruptState } : {}),
          } as Message;
        })
        .filter(Boolean);
      const status = String(snapshot.status || 'in_progress');
      let recoveredAskUser: AskUserPrompt | null = null;
      let recoveredApproval: ApprovalPrompt | null = null;
      for (const entry of listFrom(recovery, 'events')) {
        const eventType = String(entry.event || entry.type || '');
        const payload = entry.payload ?? entry;
        if (eventType === 'response.ask_user.prompt')
          recoveredAskUser = askUserPrompt(payload, sessionId) || recoveredAskUser;
        if (eventType === 'response.approval.prompt')
          recoveredApproval = approvalPrompt(payload, sessionId) || recoveredApproval;
      }
      const terminal = ['completed', 'cancelled', 'failed'].includes(status);
      if (terminal) {
        recoveredAskUser = null;
        recoveredApproval = null;
      }
      const snapshotSequence = Math.max(
        0,
        Number(snapshot.last_sequence_number ?? recovery.sequence_number) || 0,
      );
      const snapshotError = recordValue(snapshot.error);
      const snapshotUsage = recordValue(snapshot.usage);
      let recoveredPlan = existing.plan;
      for (const tool of projected.flatMap((message) => message.tools || [])) {
        if (tool.name !== 'update_plan' || tool.status !== 'done' || tool.resultStatus === 'error')
          continue;
        try {
          const value = JSON.parse(tool.arguments || '{}') as CurrentPlan;
          if (Array.isArray(value.plan)) recoveredPlan = value;
        } catch {
          /* Preserve the last authoritative plan if recovered arguments are invalid. */
        }
      }
      const next: ResponseProjection = {
        ...existing,
        askUser: recoveredAskUser,
        approval: recoveredApproval,
        plan: recoveredPlan,
        pendingGuardian: {},
        // The snapshot is authoritative for response output. Keep the initiating
        // user row until transcript bodies contain the matching client id; the
        // response recovery payload may legitimately omit that input row.
        messages: retainInitiatingMessages(existing.messages, projected),
        usage: Object.keys(snapshotUsage || {}).length
          ? (snapshotUsage as ResponseProjection['usage'])
          : null,
        phase: undefined,
        retry: undefined,
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
          lastSequence: snapshotSequence,
          startedRev: Number(snapshot.started_rev) || existing.run.startedRev,
          startedAt: Number(snapshot.started_at) || existing.run.startedAt,
          endedAt: Number(snapshot.ended_at) || existing.run.endedAt,
          finalRev: terminal ? Number(snapshot.final_rev) || 0 : undefined,
          durableHandoff: terminal ? snapshot.durable_handoff === true : undefined,
          error:
            status === 'failed'
              ? String(snapshotError?.message || existing.run.error || 'Response failed')
              : undefined,
        },
      };
      if (!this.supervisors.checkpoint(owner, transportGeneration, snapshotSequence)) return;
      if (terminal) this.clearResponseTransport(sessionId, responseId);
      const recoveredClientIDs = new Set(
        projected
          .map((message) => message.clientMessageId)
          .filter((id): id is string => Boolean(id)),
      );
      if (recoveredClientIDs.size)
        this.setInterjections(
          this.interjections
            .peek()
            .filter((entry) => entry.sessionId !== sessionId || !recoveredClientIDs.has(entry.id)),
        );
      batch(() => {
        this.runs.value = { ...this.runs.peek(), [sessionId]: next };
        if (terminal)
          this.sessionStore.patch(sessionId, {
            activeResponseId: null,
            activeRun: false,
            lastResponseId: responseId,
          });
      });
      if (sessionId === this.sessionStore.activeSessionId.peek()) {
        this.plans.current.value = recoveredPlan;
        if (!recoveredPlan) {
          this.plans.openState.value = false;
          this.plans.seen.value = '';
        }
      }
      if (terminal) {
        this.retireIntent(sessionId);
        const interactions = { ...this.interactionsStore.interactions.peek() };
        let changed = false;
        for (const [key, interaction] of Object.entries(interactions)) {
          if (
            interaction.sessionId === sessionId &&
            interaction.responseId === responseId &&
            ['waiting', 'dismissed', 'submitting', 'failed'].includes(interaction.state)
          ) {
            interactions[key] = {
              ...interaction,
              state: 'cancelled-by-agent',
              outcome: 'Decision no longer needed',
              resolvedAt: Date.now(),
            };
            changed = true;
          }
        }
        if (changed) this.interactionsStore.interactions.value = interactions;
      }
      if (recoveredAskUser?.callId) {
        this.interactionsStore.upsert(
          'ask-user',
          sessionId,
          responseId,
          recoveredAskUser.callId,
          recoveredAskUser,
        );
        if (
          sessionId === this.sessionStore.activeSessionId.peek() &&
          this.interactionsStore.shouldOpen('ask-user', sessionId, recoveredAskUser.callId)
        )
          this.interactionsStore.askUser.value = recoveredAskUser;
      } else if (this.interactionsStore.askUser.peek()?.sessionId === sessionId)
        this.interactionsStore.askUser.value = null;
      if (recoveredApproval?.id) {
        this.interactionsStore.upsert(
          'approval',
          sessionId,
          responseId,
          recoveredApproval.id,
          recoveredApproval,
        );
        if (
          sessionId === this.sessionStore.activeSessionId.peek() &&
          this.interactionsStore.shouldOpen('approval', sessionId, recoveredApproval.id)
        )
          this.interactionsStore.approval.value = recoveredApproval;
      } else if (this.interactionsStore.approval.peek()?.sessionId === sessionId)
        this.interactionsStore.approval.value = null;
      for (const resolved of listFrom(recovery, 'resolved_interactions')) {
        const requestId = String(resolved.request_id || '');
        const kind = String(resolved.kind || '') === 'approval' ? 'approval' : 'ask-user';
        if (requestId)
          this.interactionsStore.resolve(
            kind,
            sessionId,
            responseId,
            requestId,
            String(resolved.outcome || 'resolved'),
            Number(resolved.resolved_at) || Date.now(),
          );
      }
      this.supervisors.finishRecovery(owner);
      if (!this.supervisors.ownsTransport(owner, transportGeneration)) return;
      if (next.run.status === 'streaming') {
        if (await this.waitForSubscriptionIdle(owner))
          await this.host.streamResponse(responseId, sessionId, owner.lastSequence);
        else this.scheduleSupervisorRetry(owner, new Error('Previous subscription did not stop'));
      } else {
        this.supervisors.retire(owner);
        await this.host.refreshSessionMessages(
          sessionId,
          Number(snapshot.final_rev) || 0,
          responseId,
        );
        if (next.run.status === 'completed')
          this.scheduleTitleReconciliation(sessionId, responseId, owner.generation);
      }
    } catch (error) {
      this.supervisors.finishRecovery(owner);
      if (!this.supervisors.ownsTransport(owner, transportGeneration) || abort.signal.aborted)
        return;
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
    this.supervisors.cancel(sessionId, responseId);
    try {
      const result = await this.services.endpoints.cancelResponse(responseId);
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
      this.host.publishSessionChange('run-changed', sessionId, responseId, undefined, uuid());
      // Cancellation acknowledgement is intentionally distinct from durable
      // finalization. Reconcile in the background while the UI remains stopped.
      void this.host.reconcile('cancellation', true).catch(() => undefined);
    } catch (error) {
      this.locallyStoppedResponses.delete(responseId);
      this.services.toast(`Couldn’t confirm stop: ${errorMessage(error)}`, 'error');
      const current = this.runs.peek()[sessionId];
      if (current?.run.responseId === responseId)
        void this.host.resumeResponse(sessionId, responseId);
    }
  }

  async interject(content: string, options: SendOptions = {}): Promise<void> {
    const session = this.sessionStore.activeSession.value;
    const value = (options.inputText ?? content).trim();
    const displayContent = (options.displayContent ?? value).trim();
    const attachments = options.contentParts ? [] : [...this.composer.attachments.value];
    if (!session || (!value && !attachments.length && !options.contentParts?.length)) return;
    const blockedAttachment = attachments.find(
      (attachment) => attachment.status && attachment.status !== 'ready',
    );
    if (blockedAttachment) {
      this.services.toast(
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
        attachments.map((attachment) => this.composer.attachmentInput(attachment)),
      );
      const contentParts = options.contentParts?.length
        ? [...options.contentParts, ...(value ? [{ type: 'input_text', text: value }] : [])]
        : [...attachmentParts, ...(value ? [{ type: 'input_text', text: value }] : [])];
      await this.services.endpoints.interrupt(
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
      this.composer.releaseResources(attachments, true);
      this.setInterjections(
        this.interjections.value.map((candidate) =>
          candidate.id === id ? { ...candidate, state: 'pending' } : candidate,
        ),
      );
      this.host.publishSessionChange(
        'run-changed',
        session.id,
        this.runs.peek()[session.id]?.run.responseId || '',
        undefined,
        id,
      );
      if (options.preserveComposer) return;
      this.composer.clearSubmitted(session.id, value, attachments);
    } catch (error) {
      this.setInterjections(
        this.interjections.value.map((candidate) =>
          candidate.id === id ? { ...candidate, state: 'failed' } : candidate,
        ),
      );
      if (options.onTransportFailed) options.onTransportFailed(error);
      else this.services.toast(error, 'error');
    }
  }
  async cancelInterjection(id: string): Promise<void> {
    const entry = this.interjections.value.find((candidate) => candidate.id === id);
    if (!entry) return;
    this.setInterjections(this.interjections.value.filter((candidate) => candidate.id !== id));
    try {
      await this.services.endpoints.deleteInterrupt(entry.sessionId, id);
    } catch (error) {
      this.services.toast(error, 'error');
    }
  }

  private rollbackOptimisticIntent(sessionId: string, clientMessageId: string): void {
    batch(() => {
      this.sessionStore.update(sessionId, (session) => ({
        ...session,
        messages: session.messages.filter((message) => message.clientMessageId !== clientMessageId),
      }));
      const projection = this.runs.peek()[sessionId];
      if (projection)
        this.runs.value = {
          ...this.runs.peek(),
          [sessionId]: {
            ...projection,
            messages: projection.messages.filter(
              (message) => message.clientMessageId !== clientMessageId,
            ),
          },
        };
      this.retireIntent(sessionId, clientMessageId);
    });
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
    this.services.toast(message, 'error');
  }

  retireCommittedIntents(sessionId: string, messages: Message[]): void {
    const committed = new Set(
      messages
        .map((message) => message.clientMessageId)
        .filter((value): value is string => Boolean(value)),
    );
    for (const intent of this.pendingIntents.peek()[sessionId] || [])
      if (committed.has(intent.clientMessageId))
        this.retireIntent(sessionId, intent.clientMessageId);
  }

  reconcileLoadedIntents(sessionId: string, messages: Message[], active: boolean): void {
    this.retireCommittedIntents(sessionId, messages);
    if (active) return;
    // Legacy optimistic intents can be retired by an authoritative idle
    // transcript. Unknown-outcome sends are different: absence is not proof of
    // rejection, so preserve their durable checking state until the same-key
    // replay is rejected or their client_message_id appears in the transcript.
    for (const intent of this.pendingIntents.peek()[sessionId] || [])
      if (intent.state !== 'checking') this.retireIntent(sessionId, intent.clientMessageId);
  }

  trackIntent(sessionId: string, intent: PendingIntentRegistry[string][number]): void {
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
      persistPendingIntent(
        this.services.storage,
        this.services.keys.pendingIntents,
        sessionId,
        persistedIntent,
      );
    } catch (error) {
      this.services.toast(
        `Your message is queued in this tab but could not be saved: ${errorMessage(error)}`,
        'error',
      );
    }
  }
  retireIntent(sessionId: string, clientMessageId = ''): void {
    const registry = { ...this.pendingIntents.value };
    if (clientMessageId)
      registry[sessionId] = (registry[sessionId] || []).filter(
        (entry) => entry.clientMessageId !== clientMessageId,
      );
    else delete registry[sessionId];
    if (!registry[sessionId]?.length) delete registry[sessionId];
    this.pendingIntents.value = registry;
    if (clientMessageId)
      this.services.storage.removeItem(
        `${this.services.keys.pendingIntents}:${encodeURIComponent(sessionId)}:${encodeURIComponent(clientMessageId)}`,
      );
    else
      removeSessionPendingIntents(
        this.services.storage,
        this.services.keys.pendingIntents,
        sessionId,
      );
  }

  dispose(): void {
    for (const timers of this.titleRefreshTimers.values())
      timers.forEach((timer) => window.clearTimeout(timer));
    this.titleRefreshTimers.clear();
    this.supervisors.dispose();
  }
}
