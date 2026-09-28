import {
  computed,
  effect,
  signal,
  untracked,
  type ReadonlySignal,
  type Signal,
} from '@preact/signals';
import { APIError, decodeSSE } from '../api/client';
import type { Endpoints, LiveToolCallResult } from '../api/endpoints';
import {
  CLIENT_TOOL_PREFIX,
  isClientToolName,
  parsePendingToolCalls,
  type ClientToolDefinition,
} from '../domain/client-tools';
import { errorMessage } from '../domain/text';
import type { PendingToolCall } from '../domain/types';
import type { LivePhase, LiveSnapshot, LiveStartResponse, LiveTransport } from '../platform/live';
import { liveCapability } from '../platform/voice';
import type { ClientToolBridge, ClientToolResult } from './webmcp-store';

export interface LiveTurn {
  interrupted?: boolean;
  role: 'user' | 'assistant';
  text: string;
}

// The lifecycle a delegation reaches. `refused` is terminal like `done`: the host
// declined the request's form or routing and corrected the voice model, which is
// the only audience for that guidance. Nothing failed and nothing ran, so a
// refusal must never become a user-facing error.
export type LiveDelegationState = 'queued' | 'running' | 'done' | 'refused' | 'failed';

export interface LiveDelegation {
  delegationId: string;
  state: LiveDelegationState;
  text?: string;
}

interface LiveCallHandle {
  readonly snapshot: LiveSnapshot;
  subscribe(listener: (snapshot: LiveSnapshot) => void): () => void;
  start(sessionId: string): Promise<LiveStartResponse | null>;
  stop(): Promise<void>;
  dispose(): void;
}

interface LiveEventData {
  role?: 'user' | 'assistant';
  text?: string;
  final?: boolean;
  interim?: boolean;
  delegation_id?: string;
  state?: LiveDelegationState;
  message?: string;
  session_id?: string;
  session_number?: number;
  title?: string;
  request_id?: string;
  deadline_ms?: number;
  calls?: unknown;
}

/**
 * The page's own tools (WebMCP), which a call offers its delegated turns and
 * runs for them. `definitions` must be reactive: the call re-declares the tools
 * whenever what it returns changes.
 */
export type LiveClientTools = Pick<ClientToolBridge, 'definitions' | 'run'>;

/** One round of page tool calls a delegated turn asked this page to run. */
interface LiveToolRound {
  abort: AbortController;
  /** Set once every call ran; a republished round is answered from it, never re-run. */
  result?: LiveToolCallResult;
  posting: boolean;
  /** The server took the answer, or no longer wants one. */
  settled: boolean;
  deadlineTimer?: ReturnType<typeof setTimeout>;
}

// Quick retries for a round's answer; after these the server's republication retries.
const TOOL_RESULT_RETRY_MS = [500, 2_000];

/** The server cannot take these outputs: too large, or not the calls it asked for. */
function unusableAnswer(error: unknown): boolean {
  return error instanceof APIError && (error.status === 400 || error.status === 413);
}

/** A rejection retrying cannot fix: the server no longer wants this answer. */
function definitiveRejection(error: unknown): boolean {
  return (
    error instanceof APIError &&
    error.status >= 400 &&
    error.status < 500 &&
    ![408, 425, 429].includes(error.status)
  );
}

/** The chat session a live call drives after the server moved its binding. */
export interface LiveSessionChange {
  sessionId: string;
  sessionNumber: number;
  title: string;
}

/** How the app names a session the call was found bound to. */
export interface LiveSessionLabel {
  sessionNumber: number;
  title: string;
}

const RECENT_TURN_LIMIT = 20;

/**
 * The text a terminal delegation contributes to the panel, which is the error line
 * and nothing else. A refusal is not an error: the host declined the request's form
 * or routing and told the voice model how to retry, so the user is owed no message
 * for it and the model explains in its own words. Only a failure is reported.
 */
function delegationFailureText(state: LiveDelegationState, text?: string): string {
  return state === 'failed' && text ? text : '';
}

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
  // The binding is owned by the store, not by the media layer: `start` seeds it
  // and `live.session_changed` moves it. Number and title stay empty until the
  // server reports them, which is why they are separate signals.
  readonly sessionId = signal('');
  readonly sessionNumber = signal(0);
  readonly sessionTitle = signal('');
  readonly active: ReadonlySignal<boolean> = computed(
    () =>
      Boolean(this.liveId.value) ||
      ['requesting-permission', 'connecting'].includes(this.phase.value),
  );
  readonly working: ReadonlySignal<boolean> = computed(() => {
    const state = this.delegation.value?.state;
    return state === 'queued' || state === 'running';
  });

  private turnSequence = 0;
  private readonly turnOrder = new WeakMap<LiveTurn, number>();
  private partialOrder: Partial<Record<LiveTurn['role'], number>> = {};

  // Finalization is not chronological: Gemini may finalize the user's prompt
  // after an interrupted assistant answer. Keep each turn's first-seen slot.
  get transcriptTurns(): LiveTurn[] {
    const turns = this.recentTurns.value.map((turn, index) => ({
      turn,
      order: this.turnOrder.get(turn) ?? index - RECENT_TURN_LIMIT,
    }));
    for (const role of ['user', 'assistant'] as const) {
      const text = (role === 'user' ? this.partialUser : this.partialAssistant).value;
      if (text)
        turns.push({
          turn: { role, text },
          order: this.partialOrder[role] ?? Number.MAX_SAFE_INTEGER,
        });
    }
    return turns.sort((a, b) => a.order - b.order).map(({ turn }) => turn);
  }

  private retainTurn(turn: LiveTurn): void {
    this.turnOrder.set(turn, this.partialOrder[turn.role] ?? ++this.turnSequence);
    this.recentTurns.value = [...this.recentTurns.peek(), turn]
      .sort((a, b) => this.turnOrder.get(a)! - this.turnOrder.get(b)!)
      .slice(-RECENT_TURN_LIMIT);
  }

  private call: LiveCallHandle | null = null;
  private transport: LiveTransport = 'webrtc';
  private unsubscribeCall: () => void = () => {};
  private generation = 0;
  private eventCursor = 0;
  private streamAbort: AbortController | null = null;
  private disposed = false;
  private readonly activeDelegations = new Map<string, LiveDelegation>();
  /** JSON of the page tools the server holds for this call; '' when unknown. */
  private toolDeclaration = '[]';
  private toolSync: Promise<void> = Promise.resolve();
  private toolRetryTimer: ReturnType<typeof setTimeout> | null = null;
  private toolRetryDelay = 1_000;
  private readonly toolRounds = new Map<string, LiveToolRound>();
  private readonly stopToolSync: () => void;

  constructor(
    private readonly endpoints: Endpoints,
    private readonly ensureSession: () => string | Promise<string>,
    call?: LiveCallHandle,
    // LiveStore owns live state; navigation is not its job. The app decides
    // where a moved call should take the UI — and only `live.session_changed`,
    // the authoritative binding statement, is allowed to ask it to move.
    private readonly onSessionChanged: (change: LiveSessionChange) => void = () => {},
    // Binding hints carry no title, so the app names them from what it knows.
    private readonly resolveSessionLabel: (sessionId: string) => LiveSessionLabel | null = () =>
      null,
    // Voice delegations are host turns, so the page's tools reach them only
    // through the call: declared with it, and run here when a turn stops on one.
    private readonly clientTools?: LiveClientTools,
  ) {
    if (call) this.attachCall(call);
    // The server's copy follows the tools the bound conversation may use, as
    // the page's tools, the binding, or that conversation's choice change.
    this.stopToolSync = clientTools
      ? effect(() => {
          const liveId = this.liveId.value;
          const sessionId = this.sessionId.value;
          if (!liveId || !sessionId) return;
          const tools = clientTools.definitions(sessionId);
          untracked(() => this.declareTools(liveId, tools));
        })
      : () => {};
  }

  private callCapability(): LiveSnapshot['capability'] {
    return liveCapability(this.transport);
  }

  private attachCall(call: LiveCallHandle): LiveCallHandle {
    this.call = call;
    this.capability.value = call.snapshot.capability;
    this.unsubscribeCall = call.subscribe((snapshot) => this.applyCallSnapshot(snapshot));
    return call;
  }

  // ensureCall defers the provider-specific media implementation to the first call.
  private async ensureCall(): Promise<LiveCallHandle> {
    if (this.call) return this.call;
    const { LiveCall } = await import('../platform/live');
    if (this.call) return this.call;
    return this.attachCall(
      new LiveCall(
        (sdp, sessionId, audioTransport) =>
          this.endpoints.liveStart(sdp, sessionId, audioTransport, this.startTools(sessionId)),
        this.endpoints.liveStop,
        {
          transport: this.transport,
          openPCMOutput: this.endpoints.liveAudioOutput,
          sendPCMInput: this.endpoints.liveAudioInput,
        },
      ),
    );
  }

  applyCapability(value: unknown): void {
    const capability =
      value && typeof value === 'object' ? (value as Record<string, unknown>) : undefined;
    const nextTransport: LiveTransport =
      capability?.transport === 'http_pcm'
        ? 'http_pcm'
        : capability?.transport === 'websocket_pcm'
          ? 'websocket_pcm'
          : 'webrtc';
    if (nextTransport !== this.transport && this.call && !this.active.peek()) {
      this.unsubscribeCall();
      this.call.dispose();
      this.call = null;
      this.unsubscribeCall = () => {};
    }
    this.transport = nextTransport;
    if (!this.call) this.capability.value = this.callCapability();
    this.enabled.value = capability?.enabled === true;
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
    this.turnSequence = 0;
    this.partialOrder = {};
    this.delegation.value = null;
    this.activeDelegations.clear();
    this.abandonToolRounds();
    this.clearToolRetry();
    // A new call starts with no page tools unless its start request declares them.
    this.toolDeclaration = '[]';
    this.lastError.value = '';
    this.sessionNumber.value = 0;
    this.sessionTitle.value = '';
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
    this.abandonToolRounds();
    this.clearToolRetry();
    try {
      await this.call?.stop();
    } catch (error) {
      this.lastError.value =
        error instanceof Error ? error.message : 'The live voice session could not be closed.';
    } finally {
      this.liveId.value = '';
      this.sessionId.value = '';
      this.sessionNumber.value = 0;
      this.sessionTitle.value = '';
      this.phase.value = 'ended';
      this.delegation.value = null;
      this.activeDelegations.clear();
      this.abandonToolRounds();
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
    // The media layer only knows the session it was started with, so mirroring
    // it here would revert a server-side switch on the next phase change.
    // `start` seeds the binding and `live.session_changed` moves it from then on.
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
    if (event === 'live.started') {
      this.adoptBindingHint(data.session_id);
      this.phase.value = 'listening';
      return;
    }
    // The authoritative binding statement, and the only event that carries the
    // label the panel shows. The app follows it; nothing else moves the call.
    if (event === 'live.session_changed') {
      const sessionId = String(data.session_id || '');
      if (!sessionId) return;
      const sessionNumber = Number(data.session_number) || 0;
      const title = String(data.title || '');
      this.sessionId.value = sessionId;
      this.sessionNumber.value = sessionNumber;
      this.sessionTitle.value = title;
      this.onSessionChanged({ sessionId, sessionNumber, title });
      return;
    }
    if (event === 'live.transcript') {
      if (data.role !== 'user' && data.role !== 'assistant') return;
      const text = String(data.text || '');
      // User previews must not erase assistant text: an interruption archives
      // that text separately, and its event can arrive after the user preview.
      const target = data.role === 'user' ? this.partialUser : this.partialAssistant;
      const previous = this.recentTurns.peek().at(-1);
      if (text && this.partialOrder[data.role] === undefined)
        this.partialOrder[data.role] = ++this.turnSequence;
      // Some providers finalize the same partial after the interruption event.
      // Keep its interrupted marker instead of appending an identical turn.
      const repeatedInterrupted =
        data.role === 'assistant' &&
        !target.peek() &&
        previous?.interrupted &&
        previous.text === text;
      target.value = data.final ? '' : text;
      if (data.final && text.trim() && !repeatedInterrupted)
        this.retainTurn({ role: data.role, text });
      if (data.final || !text) delete this.partialOrder[data.role];
      if (!this.working.peek())
        this.phase.value = data.role === 'assistant' && !data.final ? 'speaking' : 'listening';
      return;
    }
    if (event === 'live.interrupted') {
      const text = this.partialAssistant.peek();
      if (text.trim()) this.retainTurn({ role: 'assistant', text, interrupted: true });
      delete this.partialOrder.assistant;
      this.partialAssistant.value = '';
      if (!this.working.peek()) this.phase.value = 'listening';
      return;
    }
    if (event === 'live.delegation') {
      this.adoptBindingHint(data.session_id);
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
        // The only delegation text the panel may show is a genuine failure's. A
        // refusal's text is protocol guidance addressed to the voice model — which
        // speaks an explanation in its own words — so it never surfaces here, in
        // an error or anywhere else.
        const failure = delegationFailureText(state, data.text);
        this.activeDelegations.delete(delegationId);
        const current = this.delegation.peek();
        // A follow-up may finish while the original delegation is still
        // running. Preserve the displayed original instead of clearing working.
        if (current && current.delegationId !== delegationId) {
          if (failure) this.lastError.value = failure;
          return;
        }
        if (this.activeDelegations.size > 0) {
          const remaining = [...this.activeDelegations.values()].at(-1)!;
          this.delegation.value = remaining;
          this.phase.value = 'working';
          if (failure) this.lastError.value = failure;
        } else {
          this.delegation.value = {
            delegationId,
            state,
            ...(failure ? { text: failure } : {}),
          };
          // A refusal returns to listening exactly as `done` does: the in-flight
          // delegation is over and there is nothing left for the user to fix.
          this.phase.value = 'listening';
          if (failure) this.lastError.value = failure;
        }
      }
      return;
    }
    if (event === 'live.tool_calls_cancelled') {
      const round = this.toolRounds.get(String(data.request_id || ''));
      if (round) {
        round.abort.abort();
        if (round.deadlineTimer) clearTimeout(round.deadlineTimer);
        this.toolRounds.delete(String(data.request_id));
      }
      return;
    }
    if (event === 'live.tool_calls_requested') {
      this.runToolRound(data);
      return;
    }
    if (event === 'live.error') {
      this.lastError.value = String(data.message || 'Live voice reported an error.');
      this.phase.value = 'failed';
    }
  }

  /** The page tools the start request declares, recorded as the server's copy. */
  private startTools(sessionId: string): ClientToolDefinition[] {
    const tools = this.clientTools?.definitions(sessionId) ?? [];
    this.toolDeclaration = JSON.stringify(tools);
    return tools;
  }

  /**
   * Keeps the server's copy of the call's page tools equal to the page's.
   * Declarations go out one at a time, so an older one can never land last.
   */
  private declareTools(liveId: string, tools: ClientToolDefinition[]): void {
    const declaration = JSON.stringify(tools);
    if (declaration === this.toolDeclaration) return;
    if (this.toolRetryTimer) {
      this.clearToolRetry();
    }
    this.toolDeclaration = declaration;
    this.toolSync = this.toolSync.then(async () => {
      // Superseded while queued by a newer declaration or another call.
      if (declaration !== this.toolDeclaration || liveId !== this.liveId.peek()) return;
      try {
        await this.endpoints.liveClientTools(liveId, tools);
        this.toolRetryDelay = 1_000;
      } catch {
        if (declaration === this.toolDeclaration && liveId === this.liveId.peek()) {
          this.toolDeclaration = '';
          const delay = this.toolRetryDelay;
          this.toolRetryDelay = Math.min(delay * 2, 30_000);
          this.toolRetryTimer = setTimeout(() => {
            this.toolRetryTimer = null;
            if (liveId === this.liveId.peek()) this.declareTools(liveId, tools);
          }, delay);
        }
      }
    });
  }

  private clearToolRetry(): void {
    if (this.toolRetryTimer) clearTimeout(this.toolRetryTimer);
    this.toolRetryTimer = null;
    this.toolRetryDelay = 1_000;
  }

  /**
   * Runs one round of page tool calls a delegated turn stopped on and posts
   * their outputs, with which the server continues the turn. The server
   * republishes a round until it is answered, so a repeat is answered from the
   * first run's outputs and never runs a tool twice.
   */
  private runToolRound(data: LiveEventData): void {
    const requestId = String(data.request_id || '');
    const liveId = this.liveId.peek();
    if (!requestId || !liveId || !this.clientTools) return;
    const existing = this.toolRounds.get(requestId);
    if (existing) {
      void this.postToolResult(liveId, requestId, existing);
      return;
    }
    const round: LiveToolRound = { abort: new AbortController(), posting: false, settled: false };
    // Deadline comes from the host, not receipt time (reconnects can be late).
    // Leave a margin so a slow batch never starts further side effects after
    // the host has already given up on its answer.
    const deadline = Number(data.deadline_ms);
    if (Number.isFinite(deadline) && deadline > 0) {
      const remaining = deadline - Date.now() - 1_000;
      if (remaining <= 0) return;
      round.deadlineTimer = setTimeout(() => round.abort.abort(), remaining);
    }
    this.toolRounds.set(requestId, round);
    // Keep a bounded replay history; never evict a still-running/posting round.
    const settled = [...this.toolRounds].filter(([, entry]) => entry.settled);
    while (settled.length > 64) {
      const [oldId, oldRound] = settled.shift()!;
      if (oldRound.deadlineTimer) clearTimeout(oldRound.deadlineTimer);
      this.toolRounds.delete(oldId);
    }
    void this.executeToolRound(liveId, requestId, String(data.session_id || ''), data.calls, round);
  }

  private async executeToolRound(
    liveId: string,
    requestId: string,
    sessionId: string,
    rawCalls: unknown,
    round: LiveToolRound,
  ): Promise<void> {
    const calls = parsePendingToolCalls(rawCalls) ?? [];
    if (!calls.length || calls.length !== (rawCalls as unknown[]).length) {
      round.result = { error: 'The device could not read the requested tool calls.' };
    } else {
      const outputs: Array<{ call_id: string; output: string }> = [];
      for (const call of calls) {
        const result = await this.runPageTool(sessionId, call, round.abort.signal);
        // The call ended: nobody is waiting for these outputs any more.
        if (round.abort.signal.aborted) return;
        outputs.push({ call_id: call.callId, output: result.output });
      }
      round.result = { outputs };
    }
    await this.postToolResult(liveId, requestId, round);
  }

  /** Runs one call, but only a page tool the conversation still offers. */
  private async runPageTool(
    sessionId: string,
    call: PendingToolCall,
    signal: AbortSignal,
  ): Promise<ClientToolResult> {
    const tools = this.clientTools!;
    // The name comes from the model; what this conversation allows decides.
    if (
      !isClientToolName(call.name) ||
      !tools.definitions(sessionId).some((tool) => tool.name === call.name)
    )
      return { ok: false, output: `Error: ${call.name} is not available in this conversation` };
    try {
      return await tools.run(
        {
          callId: call.callId,
          name: call.name.slice(CLIENT_TOOL_PREFIX.length),
          arguments: call.arguments,
        },
        signal,
      );
    } catch (error) {
      return { ok: false, output: `Error: ${errorMessage(error)}` };
    }
  }

  /** Posts a round's answer with brief retries; a republished round retries later. */
  private async postToolResult(
    liveId: string,
    requestId: string,
    round: LiveToolRound,
  ): Promise<void> {
    if (!round.result || round.posting || round.settled) return;
    round.posting = true;
    try {
      for (let attempt = 0; ;) {
        const result = round.result;
        if (!result || round.abort.signal.aborted || liveId !== this.liveId.peek()) return;
        try {
          await this.endpoints.liveToolResult(liveId, requestId, result);
          round.settled = true;
          if (round.deadlineTimer) clearTimeout(round.deadlineTimer);
          return;
        } catch (error) {
          if (unusableAnswer(error) && 'outputs' in result) {
            // Say so instead, or the delegation waits out its timeout in silence.
            round.result = { error: 'The device could not send its tool results.' };
            continue;
          }
          if (definitiveRejection(error)) {
            round.settled = true;
            if (round.deadlineTimer) clearTimeout(round.deadlineTimer);
            return;
          }
          const wait = TOOL_RESULT_RETRY_MS[attempt++];
          if (wait === undefined) return;
          await delay(wait, round.abort.signal);
        }
      }
    } finally {
      round.posting = false;
    }
  }

  /** The call is over: stop its page tools and forget its rounds. */
  private abandonToolRounds(): void {
    for (const round of this.toolRounds.values()) {
      round.abort.abort();
      if (round.deadlineTimer) clearTimeout(round.deadlineTimer);
    }
    this.toolRounds.clear();
  }

  /**
   * Binding recovery only, and strictly local: it may move `sessionId` and
   * re-label it, but it must never call `onSessionChanged` and so never
   * navigate. `live.started` and `live.delegation` report a session only as a
   * hint — the ring buffer is bounded, so a reconnecting tab can miss
   * `live.session_changed` entirely and never see the real switch; and a hint
   * snapshotted before a switch was committed can legitimately arrive after one
   * that reports the new binding. Turning either into a navigation would move
   * the UI to a session the call is not on. `live.session_changed` is the only
   * event that moves the UI.
   *
   * The hint carries no title, so the label belongs to whoever can name the
   * session; when nobody can, it is cleared rather than left describing the
   * session the call just left.
   */
  private adoptBindingHint(sessionId: unknown): void {
    const next = String(sessionId || '');
    if (!next || next === this.sessionId.peek()) return;
    this.sessionId.value = next;
    const label = this.resolveSessionLabel(next);
    this.sessionNumber.value = label?.sessionNumber || 0;
    this.sessionTitle.value = label?.title || '';
  }

  private async endFromServer(generation: number): Promise<void> {
    if (!this.current(generation)) return;
    this.invalidateStream();
    this.abandonToolRounds();
    this.clearToolRetry();
    try {
      await this.call?.stop();
    } catch {
      // The server has already declared the session terminal; local cleanup is sufficient.
    }
    this.liveId.value = '';
    this.sessionId.value = '';
    this.sessionNumber.value = 0;
    this.sessionTitle.value = '';
    this.phase.value = 'ended';
    this.delegation.value = null;
    this.activeDelegations.clear();
    this.abandonToolRounds();
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
    this.stopToolSync();
    this.clearToolRetry();
    this.abandonToolRounds();
    this.unsubscribeCall();
    this.call?.dispose();
    this.liveId.value = '';
    this.sessionId.value = '';
    this.sessionNumber.value = 0;
    this.sessionTitle.value = '';
    this.phase.value = 'ended';
  }
}
