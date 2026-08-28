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
  loadChildRuns: (sessionId?: string) => Promise<void>;
  resumeResponse: (sessionId: string, responseId: string) => Promise<void>;
  refreshSessionMessages: (sessionId: string, targetRev?: number) => Promise<void>;
  refreshDiffComments: (sessionId: string) => Promise<void>;
  retireIntent: (sessionId: string, clientMessageId?: string) => void;
  stoppedResponseCount: () => number;
  isLocallyStopped: (responseId: string) => boolean;
  clearLocallyStopped: (responseId: string) => void;
}

/** Owns polling generations and reconciliation of authoritative server status. */
export class StatusReconciler {
  private timer = 0;
  private unknownActiveSessionIds = new Set<string>();
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
    const poll = async () => {
      if (document.visibilityState === 'visible') {
        if (Date.now() - this.host.sessionStore.sidebarRefreshedAt >= 30_000)
          await this.host.refreshSidebar(false).catch(() => undefined);
        await this.host.reconcile('poll', false).catch(() => undefined);
        if (this.host.activeSessionId.peek())
          await this.host.loadChildRuns().catch(() => undefined);
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
      this.timer = window.setTimeout(
        poll,
        anyActive || this.host.diff.peek().open ? 2_000 : 30_000,
      );
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
    };
    const request = (async () => {
      // An authoritative request invalidates the old generation immediately,
      // but waits for it to settle to avoid needless parallel work.
      if (authoritative && previous) await previous.catch(() => undefined);
      const data = await this.services.endpoints.sessionStatus(
        metadata.selectedSessionId,
        metadata.showHidden,
        metadata.categories,
        this.coordinator.etag,
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
    return (
      !this.services.isDisposed &&
      metadata.generation === this.coordinator.generation &&
      metadata.selectedSessionId === this.host.activeSessionId.peek() &&
      metadata.selectionEpoch === this.host.selectionEpoch() &&
      metadata.showHidden === this.host.sessionStore.showHidden.peek() &&
      metadata.categories.join(',') === this.services.config.sidebarCategories.join(',')
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
        const committedClientMessageId = String(status.client_message_id || '');
        if (
          serverActiveResponseId &&
          committedClientMessageId &&
          this.host.pendingIntents
            .peek()
            [session.id]?.some((intent) => intent.clientMessageId === committedClientMessageId)
        ) {
          followUps.push(() => this.host.retireIntent(session.id, committedClientMessageId));
        }
        const stoppedResponseId = this.host.runs.peek()[session.id]?.run.responseId || '';
        const stoppedServerResponse = Boolean(
          serverActiveResponseId && this.host.isLocallyStopped(serverActiveResponseId),
        );
        const activeResponseId = stoppedServerResponse ? null : serverActiveResponseId;
        const transcriptRev = Math.max(
          session.transcriptRev || 0,
          Number(status.transcript_rev) || 0,
        );
        if (
          activeResponseId &&
          activeResponseId !== session.activeResponseId &&
          !this.host.isLocallyStopped(activeResponseId)
        )
          followUps.push(() => void this.host.resumeResponse(session.id, activeResponseId));
        if (stoppedResponseId && this.host.isLocallyStopped(stoppedResponseId)) {
          if (!serverActiveResponseId || serverActiveResponseId !== stoppedResponseId)
            this.host.clearLocallyStopped(stoppedResponseId);
        }
        if (
          !serverReportsActive &&
          clientReportsActive &&
          projectedRun &&
          projectedRun.responseId &&
          !projectedRun.responseId.startsWith('pending_')
        ) {
          // The status endpoint is authoritative for run liveness, but the
          // response snapshot owns the terminal projection. Mobile browsers
          // can suspend or lose the terminal stream event, so reconcile that
          // snapshot whenever the two views disagree.
          followUps.push(() => void this.host.resumeResponse(session.id, projectedRun.responseId));
        }
        if (
          !serverActiveResponseId &&
          (session.activeResponseId || this.host.pendingIntents.peek()[session.id]?.length) &&
          transcriptRev >= (projectedRun?.finalRev || 0)
        )
          followUps.push(() => void this.host.refreshSessionMessages(session.id, transcriptRev));
        const titleRefreshAllowed = this.host.renameTarget.peek()?.id !== session.id;
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
    if (activeSessionId) followUps.push(() => void this.host.loadChildRuns(activeSessionId));
    // No follow-up from a stale generation may start after a newer reconcile.
    if (this.statusRequestIsCurrent(metadata)) followUps.forEach((followUp) => followUp());
  }

  dispose(): void {
    window.clearTimeout(this.timer);
    this.coordinator.generation += 1;
  }
}
