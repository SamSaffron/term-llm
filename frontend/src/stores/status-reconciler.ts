import type { ReadonlySignal, Signal } from '@preact/signals';
import type { ResponseProjection } from '../domain/response';
import type { DiffState } from './store-types';
import type { PendingIntentRegistry } from '../platform/storage';
import type { Session } from '../domain/types';
import type { SessionStore } from './session-store';
import type { AppStoreServices } from './app-store-services';
import { compareSessionsByActivity, listFrom } from './store-utils';

export interface StatusRequestMetadata {
  generation: number;
  requestedAt: number;
  selectedSessionId: string;
  selectionEpoch: number;
  showHidden: boolean;
  categories: string[];
  activeResponseIds: Record<string, string>;
}

interface StatusCoordinatorState {
  generation: number;
  refreshPromise: Promise<void> | null;
  lastAppliedGeneration: number;
  lastAppliedRequestedAt: number;
  lastAppliedReceivedAt: number;
  etag: string;
}

export interface StatusReconcilerHost {
  activeSessionId: ReadonlySignal<string>;
  selectionEpoch: () => number;
  sessionStore: SessionStore;
  pendingIntents: Signal<PendingIntentRegistry>;
  runs: ReadonlySignal<Record<string, ResponseProjection>>;
  diff: ReadonlySignal<DiffState>;
  renameTarget: ReadonlySignal<Session | null>;
  reconcile: (reason: string, authoritative: boolean) => Promise<void>;
  refreshSidebar: (authoritative?: boolean) => Promise<void>;
  resumeResponse: (sessionId: string, responseId: string) => Promise<void>;
  reconcileServerIdleResponse: (
    sessionId: string,
    responseId: string,
    transcriptRev: number,
  ) => Promise<void>;
  refreshSessionMessages: (sessionId: string, targetRev?: number) => Promise<void>;
  syncSessionMessagesForAttach: (sessionId: string, targetRev?: number) => Promise<void>;
  refreshDiffComments: (sessionId: string) => Promise<void>;
  retireIntent: (sessionId: string, clientMessageId?: string) => void;
  stoppedResponseCount: () => number;
  isLocallyStopped: (responseId: string) => boolean;
  clearLocallyStopped: (responseId: string) => void;
  eventFeedHealthy: () => boolean;
}

/** Owns polling generations and reconciliation of authoritative server status. */
export class StatusReconciler {
  private timer = 0;
  private pollGeneration = 0;
  private unknownActiveSessionIds = new Set<string>();
  private readonly transcriptAttachSyncs = new Set<string>();
  private readonly coordinator: StatusCoordinatorState = {
    generation: 0,
    refreshPromise: null,
    lastAppliedGeneration: 0,
    lastAppliedRequestedAt: 0,
    lastAppliedReceivedAt: 0,
    etag: '',
  };

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: StatusReconcilerHost,
  ) {}

  get generation(): number {
    return this.coordinator.generation;
  }

  start(): void {
    clearTimeout(this.timer);
    const generation = ++this.pollGeneration;
    const poll = async () => {
      if (this.services.isDisposed || generation !== this.pollGeneration) return;
      const eventFeedHealthy = this.host.eventFeedHealthy();
      if (document.visibilityState === 'visible') {
        const sidebarInterval = eventFeedHealthy ? 60_000 : 30_000;
        if (Date.now() - this.host.sessionStore.sidebarRefreshedAt >= sidebarInterval)
          await this.host.refreshSidebar(false).catch(() => undefined);
        await this.host.reconcile('poll', false).catch(() => undefined);
      }
      const anyActive =
        this.host.stoppedResponseCount() > 0 ||
        Object.values(this.host.pendingIntents.peek()).some((intents) =>
          intents.some((intent) => intent.state === 'checking'),
        ) ||
        this.host.sessionStore.sessions.peek().some((session) => session.activeRun) ||
        Object.values(this.host.runs.peek()).some((projection) =>
          ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status),
        );
      if (this.services.isDisposed || generation !== this.pollGeneration) return;
      const interval = eventFeedHealthy
        ? anyActive || this.host.diff.peek().open
          ? 10_000
          : 60_000
        : anyActive || this.host.diff.peek().open
          ? 2_000
          : 30_000;
      this.timer = window.setTimeout(poll, interval);
    };
    this.timer = window.setTimeout(poll, 0);
  }
  async refresh(authoritative = false): Promise<void> {
    if (!authoritative && this.coordinator.refreshPromise) return this.coordinator.refreshPromise;

    const generation = ++this.coordinator.generation;
    const previous = this.coordinator.refreshPromise;
    const metadata: StatusRequestMetadata = {
      generation,
      requestedAt: Date.now(),
      selectedSessionId: this.host.activeSessionId.peek(),
      selectionEpoch: this.host.selectionEpoch(),
      showHidden: this.host.sessionStore.showHidden.peek(),
      categories: [...this.services.config.sidebarCategories],
      activeResponseIds: Object.fromEntries(
        Object.entries(this.host.runs.peek())
          .filter(([, projection]) =>
            ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status),
          )
          .map(([sessionId, projection]) => [sessionId, projection.run.responseId]),
      ),
    };
    const request = (async () => {
      // An authoritative request invalidates the old generation immediately,
      // but waits for it to settle to avoid needless parallel work.
      if (authoritative && previous) await previous.catch(() => undefined);
      const data = await this.services.endpoints.sessionStatus(
        metadata.selectedSessionId,
        metadata.showHidden,
        metadata.categories,
        authoritative ? '' : this.coordinator.etag,
      );
      const receivedAt = Date.now();
      if (!this.statusRequestIsCurrent(metadata)) {
        this.services.bumpDiagnostic('staleStatusResults');
        return;
      }
      if (data.__notModified === true) {
        this.coordinator.lastAppliedGeneration = generation;
        this.coordinator.lastAppliedRequestedAt = metadata.requestedAt;
        this.coordinator.lastAppliedReceivedAt = receivedAt;
        this.coordinator.etag = String(data.__etag || this.coordinator.etag);
        return;
      }
      await this.applyStatus(data, metadata, receivedAt);
    })();
    const tracked = request.finally(() => {
      if (this.coordinator.refreshPromise === tracked) this.coordinator.refreshPromise = null;
    });
    this.coordinator.refreshPromise = tracked;
    return tracked;
  }

  private statusRequestIsCurrent(metadata: StatusRequestMetadata): boolean {
    const activeRuns = Object.entries(this.host.runs.peek()).filter(([, projection]) =>
      ['connecting', 'checking', 'streaming', 'cancelling'].includes(projection.run.status),
    );
    const sameRunGeneration =
      activeRuns.length === Object.keys(metadata.activeResponseIds).length &&
      activeRuns.every(
        ([sessionId, projection]) =>
          metadata.activeResponseIds[sessionId] === projection.run.responseId,
      );
    return (
      !this.services.isDisposed &&
      metadata.generation === this.coordinator.generation &&
      metadata.selectedSessionId === this.host.activeSessionId.peek() &&
      metadata.selectionEpoch === this.host.selectionEpoch() &&
      metadata.showHidden === this.host.sessionStore.showHidden.peek() &&
      metadata.categories.join(',') === this.services.config.sidebarCategories.join(',') &&
      sameRunGeneration
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
      this.host.sessionStore.sessions.peek().find((session) => session.id === activeSessionId)
        ?.transcriptRev || 0;
    const statuses = listFrom(data, 'sessions', 'items');
    const known = new Set(this.host.sessionStore.sessions.peek().map((session) => session.id));
    const unknownActive = new Set(
      statuses.flatMap((status) => {
        const id = String(status.id || status.session_id || '');
        return id && (status.active_run || status.active_response_id) && !known.has(id) ? [id] : [];
      }),
    );
    const discoveredUnknown = [...unknownActive].some(
      (id) => !this.unknownActiveSessionIds.has(id),
    );
    if (discoveredUnknown) {
      await this.host.refreshSidebar(false).catch(() => undefined);
      if (!this.statusRequestIsCurrent(metadata)) return;
    }

    const byID = new Map(statuses.map((entry) => [String(entry.id || entry.session_id), entry]));
    const followUps: Array<() => void> = [];
    const reconciledSessions = this.host.sessionStore.sessions
      .peek()
      .map((session) => {
        const status = byID.get(session.id);
        if (!status) return session.activeRun ? { ...session, activeRun: false } : session;
        const serverActiveResponseId = String(status.active_response_id || '') || null;
        const serverReportsActive = Boolean(serverActiveResponseId || status.active_run);
        const projectedRun = this.host.runs.peek()[session.id]?.run;
        const clientReportsActive = Boolean(
          projectedRun &&
          ['connecting', 'checking', 'streaming', 'cancelling'].includes(projectedRun.status),
        );
        const projectedRunWasActiveAtRequest = Boolean(
          projectedRun && metadata.activeResponseIds[session.id] === projectedRun.responseId,
        );
        const idleStatusCannotDisproveProjectedRun = Boolean(
          !serverReportsActive && clientReportsActive && !projectedRunWasActiveAtRequest,
        );
        const committedClientMessageId = String(status.client_message_id || '');
        const matchesPendingIntent = Boolean(
          committedClientMessageId &&
          this.host.pendingIntents
            .peek()
            [session.id]?.some((intent) => intent.clientMessageId === committedClientMessageId),
        );
        const locallyAdmittingResponse = Boolean(
          serverActiveResponseId &&
          matchesPendingIntent &&
          projectedRun?.responseId.startsWith('pending_'),
        );
        if (serverActiveResponseId && matchesPendingIntent) {
          followUps.push(() => this.host.retireIntent(session.id, committedClientMessageId));
        }
        const stoppedResponseId = this.host.runs.peek()[session.id]?.run.responseId || '';
        const stoppedServerResponse = Boolean(
          serverActiveResponseId && this.host.isLocallyStopped(serverActiveResponseId),
        );
        const activeResponseId = idleStatusCannotDisproveProjectedRun
          ? session.activeResponseId || projectedRun?.responseId || null
          : stoppedServerResponse
            ? null
            : serverActiveResponseId;
        const serverTranscriptRev = Number(status.transcript_rev) || 0;
        const installedTranscriptRev = session.messageBodiesRev || 0;
        const initiatingMessageInstalled = Boolean(
          committedClientMessageId &&
          session.messages.some(
            (message) =>
              message.role === 'user' && message.clientMessageId === committedClientMessageId,
          ),
        );
        const peerPromptMissing = committedClientMessageId
          ? !initiatingMessageInstalled
          : serverTranscriptRev > installedTranscriptRev;
        const needsTranscriptSyncBeforeAttach = Boolean(
          serverActiveResponseId &&
          session.id === activeSessionId &&
          projectedRun?.responseId !== serverActiveResponseId &&
          peerPromptMissing &&
          !this.host.isLocallyStopped(serverActiveResponseId),
        );
        // Status and sidebar metadata can advance independently from the
        // message bodies installed in this client. Track both generations.
        const transcriptRev = Math.max(session.transcriptRev || 0, serverTranscriptRev);
        if (activeResponseId && !this.host.isLocallyStopped(activeResponseId)) {
          if (needsTranscriptSyncBeforeAttach)
            followUps.push(
              () =>
                void this.syncTranscriptThenResume(
                  session.id,
                  activeResponseId,
                  serverTranscriptRev,
                  committedClientMessageId,
                ),
            );
          else if (activeResponseId !== session.activeResponseId && !locallyAdmittingResponse)
            followUps.push(() => void this.host.resumeResponse(session.id, activeResponseId));
        }
        if (stoppedResponseId && this.host.isLocallyStopped(stoppedResponseId)) {
          if (!serverActiveResponseId || serverActiveResponseId !== stoppedResponseId)
            this.host.clearLocallyStopped(stoppedResponseId);
        }
        if (
          !serverReportsActive &&
          clientReportsActive &&
          projectedRunWasActiveAtRequest &&
          projectedRun &&
          projectedRun.responseId &&
          !projectedRun.responseId.startsWith('pending_')
        ) {
          // The status endpoint is authoritative for run liveness, but the
          // response snapshot owns the terminal projection. Mobile browsers
          // can suspend or lose the terminal stream event, so reconcile that
          // snapshot whenever the two views disagree. A run admitted after
          // this status request started cannot be disproven by its stale body.
          followUps.push(() => {
            // Probe the rich terminal snapshot and durable transcript in parallel:
            // either transport can be the one iOS left permanently wedged.
            void this.host.resumeResponse(session.id, projectedRun.responseId);
            void this.host.reconcileServerIdleResponse(
              session.id,
              projectedRun.responseId,
              serverTranscriptRev,
            );
          });
        }
        if (
          !serverActiveResponseId &&
          !idleStatusCannotDisproveProjectedRun &&
          (session.activeResponseId || this.host.pendingIntents.peek()[session.id]?.length) &&
          transcriptRev >= (projectedRun?.finalRev || 0)
        )
          followUps.push(() => void this.host.refreshSessionMessages(session.id, transcriptRev));
        const titleRefreshAllowed = this.host.renameTarget.peek()?.id !== session.id;
        const messages = committedClientMessageId
          ? session.messages.some(
              (message) =>
                message.clientMessageId === committedClientMessageId &&
                (message.pending || message.interruptState !== undefined),
            )
            ? session.messages.map((message) =>
                message.clientMessageId === committedClientMessageId
                  ? { ...message, pending: false, interruptState: undefined }
                  : message,
              )
            : session.messages
          : session.messages;
        const nextActiveRun = Boolean(
          activeResponseId || (status.active_run && !stoppedServerResponse),
        );
        const nextLastResponseId = String(status.last_response_id || '');
        const nextMessageCount = Math.max(
          session.messageCount || 0,
          Number(status.message_count) || 0,
        );
        const candidate: Session = {
          ...session,
          ...(titleRefreshAllowed && String(status.short_title || '')
            ? {
                title: String(status.short_title),
                longTitle: String(status.long_title || '') || session.longTitle,
              }
            : {}),
          ...(activeResponseId !== null || session.activeResponseId !== undefined
            ? { activeResponseId }
            : {}),
          ...(nextActiveRun || session.activeRun !== undefined ? { activeRun: nextActiveRun } : {}),
          messages,
          ...(nextLastResponseId ? { lastResponseId: nextLastResponseId } : {}),
          ...(transcriptRev || session.transcriptRev !== undefined ? { transcriptRev } : {}),
          ...(nextMessageCount || session.messageCount !== undefined
            ? { messageCount: nextMessageCount }
            : {}),
          lastMessageAt: Number(status.last_message_at)
            ? Math.max(
                session.lastMessageAt || 0,
                Number(status.last_message_at) *
                  (Number(status.last_message_at) < 10_000_000_000 ? 1000 : 1),
              )
            : session.lastMessageAt,
        };
        return candidate.title === session.title &&
          candidate.longTitle === session.longTitle &&
          candidate.activeResponseId === session.activeResponseId &&
          candidate.activeRun === session.activeRun &&
          candidate.messages === session.messages &&
          candidate.lastResponseId === session.lastResponseId &&
          candidate.transcriptRev === session.transcriptRev &&
          candidate.messageCount === session.messageCount &&
          candidate.lastMessageAt === session.lastMessageAt
          ? session
          : candidate;
      })
      .sort(compareSessionsByActivity);
    this.host.sessionStore.replace(reconciledSessions);
    if (!this.statusRequestIsCurrent(metadata)) return;
    this.unknownActiveSessionIds = unknownActive;
    this.coordinator.lastAppliedGeneration = metadata.generation;
    this.coordinator.lastAppliedRequestedAt = metadata.requestedAt;
    this.coordinator.lastAppliedReceivedAt = receivedAt;
    this.coordinator.etag = String(data.__etag || '');

    const activeRevision =
      this.host.sessionStore.sessions.peek().find((session) => session.id === activeSessionId)
        ?.transcriptRev || 0;
    if (
      this.host.diff.peek().open &&
      this.host.diff.peek().sessionId === activeSessionId &&
      activeRevision > previousActiveRevision
    )
      followUps.push(() => void this.host.refreshDiffComments(activeSessionId));
    // No follow-up from a stale generation may start after a newer reconcile.
    if (this.statusRequestIsCurrent(metadata)) followUps.forEach((followUp) => followUp());
  }

  private async syncTranscriptThenResume(
    sessionId: string,
    responseId: string,
    targetRev: number,
    clientMessageId: string,
  ): Promise<void> {
    const key = `${sessionId}:${responseId}`;
    if (this.transcriptAttachSyncs.has(key)) return;
    this.transcriptAttachSyncs.add(key);
    try {
      await this.host.syncSessionMessagesForAttach(sessionId, targetRev).catch(() => undefined);
      if (
        this.services.isDisposed ||
        this.host.activeSessionId.peek() !== sessionId ||
        this.host.isLocallyStopped(responseId)
      )
        return;
      const session = this.host.sessionStore.find(sessionId);
      const installedBodiesRev = session?.messageBodiesRev;
      const initiatingMessageInstalled = Boolean(
        clientMessageId &&
        session?.messages.some(
          (message) => message.role === 'user' && message.clientMessageId === clientMessageId,
        ),
      );
      const durableBaseInstalled = clientMessageId
        ? initiatingMessageInstalled ||
          (installedBodiesRev !== undefined && installedBodiesRev > targetRev)
        : (installedBodiesRev ?? session?.transcriptRev ?? 0) >= targetRev;
      // A newer status result owns a changed response, and a failed/lagging
      // transcript fetch must not expose response output without its durable base.
      if (session?.activeResponseId !== responseId || !durableBaseInstalled) return;
      await this.host.resumeResponse(sessionId, responseId).catch(() => undefined);
    } finally {
      this.transcriptAttachSyncs.delete(key);
    }
  }

  dispose(): void {
    window.clearTimeout(this.timer);
    this.pollGeneration += 1;
    this.coordinator.generation += 1;
    this.transcriptAttachSyncs.clear();
  }
}
