import { computed, signal, type ReadonlySignal, type Signal } from '@preact/signals';
import { decodeSSE } from '../api/client';
import type { Endpoints } from '../api/endpoints';
import type { LivePhase, LiveSnapshot, LiveStartResponse } from '../platform/live';
import { liveCapability } from '../platform/voice';

export interface LiveTurn {
  role: 'user' | 'assistant';
  text: string;
}

export type LiveDelegationState = 'queued' | 'running' | 'done' | 'failed';

export interface LiveDelegation {
  delegationId: string;
  state: LiveDelegationState;
  text?: string;
}

interface LiveCallHandle {
  readonly snapshot: LiveSnapshot;
  subscribe(listener: (snapshot: LiveSnapshot) => void): () => void;
  start(sessionId: string): Promise<LiveStartResponse | null>;
  send(frame: string): void;
  stop(): Promise<void>;
  dispose(): void;
}

interface LiveEventData {
  role?: 'user' | 'assistant';
  text?: string;
  final?: boolean;
  delegation_id?: string;
  state?: LiveDelegationState;
  message?: string;
  payload?: unknown;
}

const RECENT_TURN_LIMIT = 20;

const delay = (milliseconds: number, signal: AbortSignal): Promise<void> =>
  new Promise((resolve) => {
    const finish = () => {
      window.clearTimeout(timer);
      signal.removeEventListener('abort', finish);
      resolve();
    };
    const timer = window.setTimeout(finish, milliseconds);
    signal.addEventListener('abort', finish, { once: true });
  });

function abortError(error: unknown): boolean {
  return error instanceof DOMException
    ? error.name === 'AbortError'
    : (error as { name?: string } | null)?.name === 'AbortError';
}

export class LiveStore {
  readonly enabled = signal(false);
  readonly phase: Signal<LivePhase> = signal('idle');
  readonly capability = signal(this.callCapability());
  readonly partialUser = signal('');
  readonly partialAssistant = signal('');
  readonly recentTurns = signal<LiveTurn[]>([]);
  readonly delegation = signal<LiveDelegation | null>(null);
  readonly lastError = signal('');
  readonly liveId = signal('');
  readonly sessionId = signal('');
  readonly active: ReadonlySignal<boolean> = computed(
    () =>
      Boolean(this.liveId.value) ||
      ['requesting-permission', 'connecting'].includes(this.phase.value),
  );
  readonly working: ReadonlySignal<boolean> = computed(() => {
    const state = this.delegation.value?.state;
    return state === 'queued' || state === 'running';
  });

  private call: LiveCallHandle | null = null;
  private unsubscribeCall: () => void = () => {};
  private generation = 0;
  private eventCursor = 0;
  private streamAbort: AbortController | null = null;
  private disposed = false;
  private relayControlFrames = false;
  private readonly activeDelegations = new Map<string, LiveDelegation>();

  constructor(
    private readonly endpoints: Endpoints,
    private readonly ensureSession: () => string | Promise<string>,
    call?: LiveCallHandle,
  ) {
    if (call) this.attachCall(call);
  }

  private callCapability(): LiveSnapshot['capability'] {
    return liveCapability();
  }

  private attachCall(call: LiveCallHandle): LiveCallHandle {
    this.call = call;
    this.capability.value = call.snapshot.capability;
    this.unsubscribeCall = call.subscribe((snapshot) => this.applyCallSnapshot(snapshot));
    return call;
  }

  // ensureCall defers the WebRTC implementation to the first call so browsers
  // that never use live voice never download it.
  private async ensureCall(): Promise<LiveCallHandle> {
    if (this.call) return this.call;
    const { LiveCall } = await import('../platform/live');
    if (this.call) return this.call;
    return this.attachCall(
      new LiveCall(
        (sdp, sessionId) => this.endpoints.liveStart(sdp, sessionId),
        this.endpoints.liveStop,
        { onSignal: (frame) => this.relaySignal(frame) },
      ),
    );
  }

  /**
   * relaySignal forwards a provider control frame to the server, which owns the
   * conversation state and runs delegated work in the chat session.
   */
  relaySignal(frame: string): void {
    // The direct provider already receives these events over its sideband.
    // Relaying them would duplicate traffic and hit a non-relay session's 409.
    if (!this.relayControlFrames) return;
    const liveId = this.liveId.peek();
    if (!liveId) return;
    void this.endpoints.liveSignal(liveId, frame).catch(() => {
      // A dropped frame is not fatal: the provider resends or the call ends,
      // and the server-side event stream still reports the session state.
    });
  }

  applyCapability(value: unknown): void {
    const capability =
      value && typeof value === 'object' ? (value as Record<string, unknown>) : undefined;
    this.enabled.value = capability?.enabled === true;
    this.relayControlFrames = capability?.provider === 'codex';
    if (!this.enabled.peek() && this.active.peek()) void this.stop();
  }

  async start(): Promise<boolean> {
    if (this.disposed || !this.enabled.peek()) {
      this.lastError.value = 'Live voice is unavailable on this server.';
      return false;
    }
    if (!this.capability.peek().supported) {
      this.lastError.value = this.capability.peek().reason;
      this.phase.value = 'failed';
      return false;
    }
    if (this.active.peek()) return true;

    const generation = this.invalidateStream();
    this.eventCursor = 0;
    this.partialUser.value = '';
    this.partialAssistant.value = '';
    this.recentTurns.value = [];
    this.delegation.value = null;
    this.activeDelegations.clear();
    this.lastError.value = '';
    let call: LiveCallHandle;
    let sessionId: string;
    try {
      // A new-chat draft has no durable session yet. Materialize it with the
      // selected project/model before a voice delegation tries to run there.
      sessionId = await this.ensureSession();
      if (!this.current(generation)) return false;
      if (!sessionId) throw new Error('A chat session is required for live voice.');
      call = await this.ensureCall();
    } catch (error) {
      if (!this.current(generation)) return false;
      this.lastError.value =
        error instanceof Error ? error.message : 'Live voice could not be loaded.';
      this.phase.value = 'failed';
      return false;
    }
    if (!this.current(generation)) return false;
    const started = await call.start(sessionId);
    if (!this.current(generation) || !started) return false;
    this.liveId.value = started.live_id;
    this.sessionId.value = started.session_id;
    const controller = new AbortController();
    this.streamAbort = controller;
    void this.superviseEvents(generation, started.live_id, controller);
    return true;
  }

  async stop(): Promise<void> {
    if (this.disposed) return;
    this.invalidateStream();
    try {
      await this.call?.stop();
    } catch (error) {
      this.lastError.value =
        error instanceof Error ? error.message : 'The live voice session could not be closed.';
    } finally {
      this.liveId.value = '';
      this.sessionId.value = '';
      this.phase.value = 'ended';
      this.delegation.value = null;
      this.activeDelegations.clear();
      this.partialUser.value = '';
      this.partialAssistant.value = '';
    }
  }

  async sendText(text: string): Promise<boolean> {
    const liveId = this.liveId.peek();
    const clean = text.trim();
    if (!liveId || !clean) return false;
    try {
      await this.endpoints.liveText(liveId, clean);
      return true;
    } catch (error) {
      this.lastError.value =
        error instanceof Error ? error.message : 'The live session did not receive the message.';
      return false;
    }
  }

  private applyCallSnapshot(snapshot: LiveSnapshot): void {
    if (this.disposed) return;
    this.capability.value = snapshot.capability;
    this.liveId.value = snapshot.liveId;
    this.sessionId.value = snapshot.sessionId;
    if (
      ['idle', 'requesting-permission', 'connecting', 'listening', 'failed', 'ended'].includes(
        snapshot.phase,
      )
    )
      this.phase.value = snapshot.phase;
    if (snapshot.error) this.lastError.value = snapshot.error;
  }

  private async superviseEvents(
    generation: number,
    liveId: string,
    controller: AbortController,
  ): Promise<void> {
    let retry = 250;
    while (this.current(generation, liveId) && !controller.signal.aborted) {
      try {
        const response = await this.endpoints.liveEvents(
          liveId,
          this.eventCursor,
          controller.signal,
        );
        if (!this.current(generation, liveId)) {
          await response.body?.cancel();
          return;
        }
        if (!response.ok || !response.body) {
          const body = await response.text();
          if (response.status === 404 || response.status === 410) {
            await this.endFromServer(generation);
            return;
          }
          throw new Error(body || `Live event stream returned ${response.status}`);
        }
        if (this.lastError.peek() === 'Live updates disconnected. Reconnecting…')
          this.lastError.value = '';
        retry = 250;
        for await (const message of decodeSSE(response.body, controller.signal)) {
          if (!this.current(generation, liveId)) return;
          const sequence = Math.max(0, Number(message.id) || 0);
          if (sequence && sequence <= this.eventCursor) continue;
          if (sequence) this.eventCursor = sequence;
          let data: LiveEventData;
          try {
            data = JSON.parse(message.data || '{}') as LiveEventData;
          } catch {
            continue;
          }
          if (message.event === 'live.ended') {
            await this.endFromServer(generation);
            return;
          }
          this.applyEvent(message.event, data);
        }
      } catch (error) {
        if (!this.current(generation, liveId) || controller.signal.aborted || abortError(error))
          return;
        this.lastError.value = 'Live updates disconnected. Reconnecting…';
      }
      if (!this.current(generation, liveId) || controller.signal.aborted) return;
      await delay(retry, controller.signal);
      retry = Math.min(5_000, retry * 2);
    }
  }

  private applyEvent(event: string, data: LiveEventData): void {
    if (event === 'live.signal') {
      // The server answers the provider through the peer's data channel.
      if (data.payload !== undefined) this.call?.send(JSON.stringify(data.payload));
      return;
    }
    if (event === 'live.started') {
      this.phase.value = 'listening';
      return;
    }
    if (event === 'live.transcript') {
      if (data.role !== 'user' && data.role !== 'assistant') return;
      const text = String(data.text || '');
      const target = data.role === 'user' ? this.partialUser : this.partialAssistant;
      target.value = data.final ? '' : text;
      if (data.final && text.trim())
        this.recentTurns.value = [...this.recentTurns.peek(), { role: data.role, text }].slice(
          -RECENT_TURN_LIMIT,
        );
      if (!this.working.peek())
        this.phase.value = data.role === 'assistant' && !data.final ? 'speaking' : 'listening';
      return;
    }
    if (event === 'live.delegation') {
      if (!data.delegation_id || !data.state) return;
      const delegationId = data.delegation_id;
      const state = data.state;
      if (state === 'queued' || state === 'running') {
        const entry: LiveDelegation = {
          delegationId,
          state,
          ...(data.text ? { text: data.text } : {}),
        };
        this.activeDelegations.set(delegationId, entry);
        this.delegation.value = entry;
        this.phase.value = 'working';
      } else {
        this.activeDelegations.delete(delegationId);
        const current = this.delegation.peek();
        // A follow-up may finish while the original delegation is still
        // running. Preserve the displayed original instead of clearing working.
        if (current && current.delegationId !== delegationId) {
          if (state === 'failed' && data.text) this.lastError.value = data.text;
          return;
        }
        if (this.activeDelegations.size > 0) {
          const remaining = [...this.activeDelegations.values()].at(-1)!;
          this.delegation.value = remaining;
          this.phase.value = 'working';
          if (state === 'failed' && data.text) this.lastError.value = data.text;
        } else {
          this.delegation.value = {
            delegationId,
            state,
            ...(data.text ? { text: data.text } : {}),
          };
          this.phase.value = 'listening';
          if (state === 'failed' && data.text) this.lastError.value = data.text;
        }
      }
      return;
    }
    if (event === 'live.error') {
      this.lastError.value = String(data.message || 'Live voice reported an error.');
      this.phase.value = 'failed';
    }
  }

  private async endFromServer(generation: number): Promise<void> {
    if (!this.current(generation)) return;
    this.invalidateStream();
    try {
      await this.call?.stop();
    } catch {
      // The server has already declared the session terminal; local cleanup is sufficient.
    }
    this.liveId.value = '';
    this.sessionId.value = '';
    this.phase.value = 'ended';
    this.delegation.value = null;
    this.activeDelegations.clear();
    this.partialUser.value = '';
    this.partialAssistant.value = '';
  }

  private invalidateStream(): number {
    this.generation += 1;
    this.streamAbort?.abort();
    this.streamAbort = null;
    return this.generation;
  }

  private current(generation: number, liveId = this.liveId.peek()): boolean {
    return !this.disposed && generation === this.generation && liveId === this.liveId.peek();
  }

  dispose(): void {
    if (this.disposed) return;
    this.invalidateStream();
    this.disposed = true;
    this.unsubscribeCall();
    this.call?.dispose();
    this.liveId.value = '';
    this.sessionId.value = '';
    this.phase.value = 'ended';
  }
}
