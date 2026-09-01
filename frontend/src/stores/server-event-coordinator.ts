import type { ReadonlySignal } from '@preact/signals';
import { APIError, decodeSSE } from '../api/client';
import type { AppStoreServices } from './app-store-services';
import {
  parseServerEvent,
  parseServerEventPollResponse,
  parseServerEventReady,
  type ServerEvent,
} from '../platform/server-events';

export type ServerEventTransport = 'stopped' | 'sse' | 'poll' | 'unsupported';

export interface ServerEventHost {
  startupDone: ReadonlySignal<boolean>;
  activeSessionId: ReadonlySignal<string>;
  reconcileCatalog: () => Promise<void>;
  reconcileStatus: () => Promise<void>;
  reconcileActiveSession: (revision?: number) => Promise<void>;
  reconcileFiles: (sessionId: string) => Promise<void>;
  authoritativeRecovery: (reason: string) => Promise<void>;
  eventFeedHealthChanged: () => void;
}

type CursorConflict = {
  snapshot_required?: boolean;
  instance_id?: string;
  latest_sequence?: number;
};

const sleep = (milliseconds: number, signal: AbortSignal): Promise<void> =>
  new Promise((resolve, reject) => {
    if (signal.aborted) return reject(signal.reason);
    let timer = 0;
    const onAbort = () => {
      window.clearTimeout(timer);
      reject(signal.reason);
    };
    timer = window.setTimeout(() => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    }, milliseconds);
    signal.addEventListener('abort', onAbort, { once: true });
  });

/** Owns the server-authored event cursor and transparently moves between SSE and long poll. */
export class ServerEventCoordinator {
  mode: ServerEventTransport = 'stopped';

  private instanceId = '';
  private cursor: number | null = null;
  private running: Promise<void> | null = null;
  private transport: AbortController | null = null;
  private intentionalRestart = false;
  private readonly lifetime = new AbortController();
  private buffered: ServerEvent[] = [];
  private failures = 0;
  private pollStartedAt = 0;
  private preparedResolve: (() => void) | null = null;
  private prepared = Promise.resolve();
  private flushTimer = 0;
  private catalogPending = false;
  private statusPending = false;
  private activePending = false;
  private recoveryPending = false;
  private recoveryReason = '';
  private activeRevision = 0;
  private readonly fileSessions = new Set<string>();
  private interestChannels: string[] = [];

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: ServerEventHost,
  ) {}

  prepare(): Promise<void> {
    if (this.running || this.mode === 'unsupported' || this.services.isDisposed)
      return this.prepared;
    this.prepared = new Promise<void>((resolve) => {
      this.preparedResolve = resolve;
    });
    this.running = this.run().finally(() => {
      this.running = null;
    });
    // Event transport is an accelerator; a jammed proxy must not hold startup hostage.
    return Promise.race([this.prepared, sleep(2_000, this.lifetime.signal).catch(() => undefined)]);
  }

  flushBuffered(): void {
    if (!this.host.startupDone.peek()) return;
    const events = this.buffered;
    this.buffered = [];
    events.forEach((event) => this.route(event));
  }

  updateInterest(sessionId: string): void {
    const id = sessionId.trim();
    const next = id ? [`session:${id}`, `files:${id}`] : [];
    if (next.join(',') === this.interestChannels.join(',')) return;
    this.interestChannels = next;
    // The server filters detail events by registered channels. Reconnect from
    // the same cursor so interests change without leaking another session's details.
    if (this.transport && !this.transport.signal.aborted) {
      this.intentionalRestart = true;
      this.transport.abort(new DOMException('Server event interest changed', 'AbortError'));
    }
  }

  restart(): void {
    if (this.mode === 'unsupported' || this.services.isDisposed) return;
    if (this.running && this.isHealthy()) return;
    if (this.transport && !this.transport.signal.aborted) {
      this.intentionalRestart = true;
      this.transport.abort(new DOMException('Transport restart', 'AbortError'));
    } else {
      this.intentionalRestart = false;
    }
    if (!this.running) void this.prepare();
  }

  isHealthy(): boolean {
    return this.mode === 'sse' || this.mode === 'poll';
  }

  private setMode(mode: ServerEventTransport): void {
    const wasHealthy = this.isHealthy();
    this.mode = mode;
    this.services.eventFeedHealthy.value = this.isHealthy();
    if (
      wasHealthy &&
      !this.isHealthy() &&
      !this.services.isDisposed &&
      !this.lifetime.signal.aborted
    )
      this.host.eventFeedHealthChanged();
  }

  private markPrepared(): void {
    this.preparedResolve?.();
    this.preparedResolve = null;
  }

  private async run(): Promise<void> {
    while (!this.lifetime.signal.aborted && this.mode !== 'unsupported') {
      try {
        const sseHealthy = await this.consumeSSE();
        if (sseHealthy) {
          this.intentionalRestart = false;
          this.failures = 0;
          continue;
        }
      } catch (error) {
        if (this.lifetime.signal.aborted) break;
        if (this.intentionalRestart) {
          this.intentionalRestart = false;
          this.setMode('stopped');
          continue;
        }
        this.setMode('stopped');
        if (error instanceof APIError && [404, 405, 501].includes(error.status)) {
          this.setMode('unsupported');
          this.markPrepared();
          return;
        }
        this.services.bumpDiagnostic('serverEventPollFallbacks');
        try {
          await this.consumePolls();
          this.failures = 0;
          continue;
        } catch {
          if (this.lifetime.signal.aborted) break;
          if (this.intentionalRestart) {
            this.intentionalRestart = false;
            this.setMode('stopped');
            continue;
          }
          this.setMode('stopped');
        }
      }
      this.failures += 1;
      this.setMode('stopped');
      this.services.bumpDiagnostic('serverEventReconnects');
      const delay = Math.min(60_000, 1_000 * 2 ** Math.min(6, this.failures - 1));
      await sleep(delay * (0.8 + Math.random() * 0.4), this.lifetime.signal).catch(() => undefined);
    }
    if (this.mode !== 'unsupported') this.setMode('stopped');
    this.markPrepared();
  }

  private newTransport(): AbortController {
    this.transport?.abort(new DOMException('Transport replaced', 'AbortError'));
    const controller = new AbortController();
    const stop = () => controller.abort(this.lifetime.signal.reason);
    const detach = () => this.lifetime.signal.removeEventListener('abort', stop);
    this.lifetime.signal.addEventListener('abort', stop, { once: true });
    controller.signal.addEventListener('abort', detach, { once: true });
    this.transport = controller;
    return controller;
  }

  private async responseError(response: Response): Promise<never> {
    const body = await response.text();
    if (response.status === 409) {
      let conflict: CursorConflict = {};
      try {
        conflict = JSON.parse(body) as CursorConflict;
      } catch {
        /* Recovery below is still safe. */
      }
      this.services.bumpDiagnostic('serverEventCursorResets');
      await this.host.authoritativeRecovery('event-cursor');
      this.instanceId = String(conflict.instance_id || '');
      this.cursor = Number.isSafeInteger(conflict.latest_sequence)
        ? Number(conflict.latest_sequence)
        : null;
      throw new Error('Server event cursor reset');
    }
    throw new APIError(body || `Event request returned ${response.status}`, response.status, body);
  }

  private async consumeSSE(): Promise<boolean> {
    const controller = this.newTransport();
    this.services.bumpDiagnostic('serverEventSSEConnections');
    let lastActivity = Date.now();
    let ready = false;
    let heartbeat = 10_000;
    const watchdog = window.setInterval(() => {
      const deadline = ready ? Math.max(25_000, heartbeat * 2.5) : 8_000;
      if (Date.now() - lastActivity > deadline) {
        this.services.bumpDiagnostic('serverEventSSEJams');
        controller.abort(new DOMException('Server event stream stalled', 'TimeoutError'));
      }
    }, 1_000);
    try {
      const response = await this.services.endpoints.serverEventStream(
        this.cursor,
        this.interestChannels,
        controller.signal,
      );
      if (!response.ok) await this.responseError(response);
      if (!response.body) throw new Error('Server event stream has no response body');
      for await (const message of decodeSSE(response.body, controller.signal, () => {
        lastActivity = Date.now();
      })) {
        if (message.event === 'ready' || message.event === 'cursor') {
          const parsed = parseServerEventReady(JSON.parse(message.data));
          if (!parsed) return this.malformed();
          if (this.instanceId && this.instanceId !== parsed.instanceId) {
            this.services.bumpDiagnostic('serverEventCursorResets');
            await this.host.authoritativeRecovery('event-instance');
          }
          this.instanceId = parsed.instanceId;
          if (message.event === 'cursor')
            this.cursor = Math.max(this.cursor || 0, parsed.latestSequence);
          else if (this.cursor === null) this.cursor = parsed.latestSequence;
          heartbeat = parsed.heartbeatMs || heartbeat;
          if (message.event === 'ready') {
            ready = true;
            this.setMode('sse');
            this.markPrepared();
          }
          continue;
        }
        const parsed = parseServerEvent(JSON.parse(message.data));
        if (!parsed) return this.malformed();
        await this.accept(parsed);
      }
      if (!controller.signal.aborted) throw new Error('Server event stream ended');
      const reason = controller.signal.reason as { name?: string } | undefined;
      if (!ready || reason?.name === 'TimeoutError')
        throw reason || new Error('Server event stream stalled');
      return true;
    } finally {
      window.clearInterval(watchdog);
    }
  }

  private async consumePolls(): Promise<void> {
    this.pollStartedAt = Date.now();
    while (!this.lifetime.signal.aborted && Date.now() - this.pollStartedAt < 60_000) {
      const controller = this.newTransport();
      const response = await this.services.endpoints.serverEventPoll(
        this.cursor,
        this.interestChannels,
        controller.signal,
      );
      if (!response.ok) await this.responseError(response);
      const parsed = parseServerEventPollResponse(await response.json());
      if (!parsed) return this.malformed();
      this.setMode('poll');
      if (this.instanceId && this.instanceId !== parsed.instanceId) {
        this.services.bumpDiagnostic('serverEventCursorResets');
        await this.host.authoritativeRecovery('event-instance');
      }
      this.instanceId = parsed.instanceId;
      if (this.cursor === null) this.cursor = parsed.nextAfter;
      this.markPrepared();
      for (const event of parsed.data) await this.accept(event, true);
      this.cursor = Math.max(this.cursor || 0, parsed.nextAfter);
    }
    // A healthy poll period periodically gives SSE another chance.
  }

  private malformed(): never {
    this.services.bumpDiagnostic('serverEventMalformed');
    void this.host.authoritativeRecovery('event-malformed');
    throw new Error('Malformed server event');
  }

  private async accept(event: ServerEvent, filteredGapAllowed = false): Promise<void> {
    if (this.instanceId && event.instanceId !== this.instanceId) {
      this.services.bumpDiagnostic('serverEventCursorResets');
      await this.host.authoritativeRecovery('event-instance');
      this.instanceId = event.instanceId;
      this.cursor = event.sequence - 1;
    }
    const cursor = this.cursor || 0;
    if (event.sequence <= cursor) return;
    if (this.cursor !== null && event.sequence !== cursor + 1 && !filteredGapAllowed) {
      this.services.bumpDiagnostic('serverEventSequenceGaps');
      await this.host.authoritativeRecovery('event-gap');
    }
    this.instanceId = event.instanceId;
    this.cursor = event.sequence;
    if (!this.host.startupDone.peek()) {
      this.buffered.push(event);
      return;
    }
    this.route(event);
  }

  private route(event: ServerEvent): void {
    switch (event.type) {
      case 'session.created':
      case 'session.metadata_changed':
      case 'session.deleted':
      case 'project.created':
      case 'project.updated':
      case 'project.deleted':
      case 'project.membership_changed':
        this.catalogPending = true;
        break;
      case 'run.started':
        this.statusPending = true;
        break;
      case 'run.finished':
        this.catalogPending = true;
        this.statusPending = true;
        if (event.sessionId === this.host.activeSessionId.peek()) {
          this.activePending = true;
          this.activeRevision = Math.max(this.activeRevision, event.transcriptRev || 0);
        }
        break;
      case 'session.transcript_changed':
        this.statusPending = true;
        if (event.sessionId === this.host.activeSessionId.peek()) {
          this.activePending = true;
          this.activeRevision = Math.max(this.activeRevision, event.transcriptRev || 0);
        }
        break;
      case 'session.attention_changed':
      case 'session.lifecycle_changed':
        this.catalogPending = true;
        this.statusPending = true;
        if (event.sessionId === this.host.activeSessionId.peek()) this.activePending = true;
        break;
      case 'session.runtime_changed':
        if (event.sessionId === this.host.activeSessionId.peek()) this.activePending = true;
        break;
      case 'interaction.changed':
        this.statusPending = true;
        if (event.sessionId === this.host.activeSessionId.peek()) this.activePending = true;
        break;
      case 'files.changed':
        if (event.sessionId) this.fileSessions.add(event.sessionId);
        break;
      case 'snapshot.required':
        this.recoveryPending = true;
        this.recoveryReason = event.reason || event.type;
        break;
    }
    if (!this.flushTimer) this.flushTimer = window.setTimeout(() => void this.flush(), 100);
  }

  private async flush(): Promise<void> {
    this.flushTimer = 0;
    const recovery = this.recoveryPending;
    const recoveryReason = this.recoveryReason;
    const catalog = this.catalogPending;
    const status = this.statusPending;
    const active = this.activePending;
    const revision = this.activeRevision;
    const files = [...this.fileSessions];
    this.catalogPending = this.statusPending = this.activePending = false;
    this.recoveryPending = false;
    this.recoveryReason = '';
    this.activeRevision = 0;
    this.fileSessions.clear();
    if (!recovery && !catalog && !status && !active && !files.length) {
      this.services.bumpDiagnostic('serverEventNoopBatches');
      return;
    }
    if (recovery) {
      await this.host.authoritativeRecovery(recoveryReason).catch(() => undefined);
      await Promise.all(files.map((id) => this.host.reconcileFiles(id).catch(() => undefined)));
      return;
    }
    if (catalog) await this.host.reconcileCatalog().catch(() => undefined);
    if (status) await this.host.reconcileStatus().catch(() => undefined);
    if (active)
      await this.host.reconcileActiveSession(revision || undefined).catch(() => undefined);
    await Promise.all(files.map((id) => this.host.reconcileFiles(id).catch(() => undefined)));
  }

  dispose(): void {
    this.lifetime.abort(new DOMException('Server event coordinator disposed', 'AbortError'));
    this.transport?.abort(this.lifetime.signal.reason);
    window.clearTimeout(this.flushTimer);
    this.mode = 'stopped';
    this.services.eventFeedHealthy.value = false;
    this.markPrepared();
  }
}
