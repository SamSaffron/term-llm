import { computed, signal, type ReadonlySignal, type Signal } from '@preact/signals';
import {
  isClientToolName,
  patchToolCalls,
  pendingClientCalls,
  toolOutputItem,
  type ClientToolOutputItem,
  type PendingClientCall,
} from '../domain/client-tools';
import type { ResponseProjection } from '../domain/response';
import { errorMessage } from '../domain/text';
import type { ClientToolBridge } from './webmcp-store';

/** Resolves false once `signal` aborts. */
function whenAborted(signal: AbortSignal): Promise<false> {
  return new Promise((resolve) => {
    if (signal.aborted) resolve(false);
    else signal.addEventListener('abort', () => resolve(false), { once: true });
  });
}

// Consecutive client-tool continuations allowed before the user speaks again.
export const MAX_CLIENT_TOOL_ROUNDS = 8;

export interface ClientToolRunnerHost {
  bridge: ClientToolBridge;
  runs: Signal<Record<string, ResponseProjection>>;
  /**
   * Loads the finished turn into the durable transcript. Resolves whether
   * `responseId` is still the conversation's latest response.
   */
  prepareContinuation: (sessionId: string, responseId: string) => Promise<boolean>;
  /**
   * Starts the response that carries the tool results back to the model. Its
   * run must be installed synchronously, before the first await, so the
   * conversation stays busy across the handoff.
   */
  continueWith: (
    sessionId: string,
    responseId: string,
    outputs: ClientToolOutputItem[],
  ) => Promise<void>;
  sendInline: (responseId: string, callId: string, output: string) => Promise<void>;
  toast: (message: string) => void;
}

/**
 * Runs the client (page) tools a finished response called, then hands their
 * results to the run engine to continue that response. Transport stays in the
 * engine; this owns ownership, progress, stopping, and loop limits.
 */
export class ClientToolRunner {
  /** Sessions whose finished response is waiting on this page's tools. */
  private readonly running = signal<Record<string, AbortController>>({});
  /** Responses this tab created while offering tools; only this tab runs them. */
  private readonly offered = new Set<string>();
  private readonly rounds = new Map<string, number>();
  private readonly inline = new Map<
    string,
    { sessionId: string; controller: AbortController; output?: string }
  >();
  private disposed = false;

  constructor(private readonly host: ClientToolRunnerHost) {}

  /** Whether `sessionId` is waiting on this page, as a reactive signal. */
  runningIn(sessionId: () => string): ReadonlySignal<boolean> {
    return computed(() => Boolean(this.running.value[sessionId()]));
  }

  /** Records that this tab created `responseId` with client tools offered. */
  recordOffer(responseId: string): void {
    this.offered.add(responseId);
  }

  /** A user turn starts a fresh budget of continuations. */
  userTurn(sessionId: string): void {
    this.rounds.delete(sessionId);
  }

  /** Abandons the tools running for `sessionId`. Returns whether any were. */
  stop(sessionId: string): boolean {
    for (const [key, entry] of this.inline) {
      if (entry.sessionId === sessionId) {
        entry.controller.abort();
        this.inline.delete(key);
      }
    }
    const controller = this.running.peek()[sessionId];
    controller?.abort(new Error('Stopped by the user'));
    return Boolean(controller);
  }

  /** Executes an inline CLI bridge call while the response remains open. Replayed
   * SSE frames retry delivery but must never run a device tool twice. */
  inlineRequested(
    sessionId: string,
    responseId: string,
    call: { callId: string; name: string; arguments: string },
  ): void {
    if (
      this.disposed ||
      !this.offered.has(responseId) ||
      !call.callId ||
      !isClientToolName(call.name)
    )
      return;
    const key = `${responseId}:${call.callId}`;
    const previous = this.inline.get(key);
    if (previous) {
      if (previous.output !== undefined)
        void this.sendInline(responseId, call.callId, previous.output, previous.controller.signal);
      return;
    }
    const controller = new AbortController();
    const entry = { sessionId, controller, output: undefined as string | undefined };
    this.inline.set(key, entry);
    void (async () => {
      let output: string;
      if (!this.host.bridge.definitions(sessionId).some((tool) => tool.name === call.name)) {
        output = `Error: ${call.name} is not available in this conversation`;
      } else {
        try {
          const result = await this.host.bridge.run(
            {
              callId: call.callId,
              name: call.name.slice('webmcp__'.length),
              arguments: call.arguments || '{}',
            },
            controller.signal,
          );
          output = result.output;
        } catch (error) {
          output = `Error: ${errorMessage(error)}`;
        }
      }
      if (controller.signal.aborted || this.disposed) return;
      entry.output = output;
      await this.sendInline(responseId, call.callId, output, controller.signal);
    })();
  }

  private async sendInline(
    responseId: string,
    callId: string,
    output: string,
    signal: AbortSignal,
  ): Promise<void> {
    if (signal.aborted || this.disposed) return;
    try {
      await this.host.sendInline(responseId, callId, output);
    } catch (error) {
      if (!signal.aborted)
        this.host.toast(
          `Could not send the ${this.host.bridge.provider()} tool result: ${errorMessage(error)}`,
        );
    }
  }

  /** A response ended. If it stopped on calls this page offered, run them. */
  finished(sessionId: string, projection: ResponseProjection): void {
    const { responseId, status } = projection.run;
    for (const [key, entry] of this.inline) {
      if (key.startsWith(`${responseId}:`)) {
        entry.controller.abort();
        this.inline.delete(key);
      }
    }
    if (!this.offered.delete(responseId) || status !== 'completed' || this.disposed) return;
    // One continuation per conversation at a time.
    if (this.running.peek()[sessionId]) return;
    const calls = pendingClientCalls(
      projection.messages,
      responseId,
      projection.run.pendingToolCalls,
    );
    if (calls.length) void this.run(sessionId, responseId, calls);
  }

  /** The page is going away: abandon running tools and never continue. */
  dispose(): void {
    this.disposed = true;
    for (const controller of Object.values(this.running.peek()))
      controller.abort(new Error('The chat was closed'));
    for (const entry of this.inline.values()) entry.controller.abort();
    this.inline.clear();
    this.running.value = {};
    this.offered.clear();
  }

  private async run(
    sessionId: string,
    responseId: string,
    calls: PendingClientCall[],
  ): Promise<void> {
    const provider = this.host.bridge.provider();
    const rounds = (this.rounds.get(sessionId) || 0) + 1;
    if (rounds > MAX_CLIENT_TOOL_ROUNDS) {
      this.host.toast(`Stopped after ${MAX_CLIENT_TOOL_ROUNDS} rounds of ${provider} tool calls.`);
      return;
    }
    this.rounds.set(sessionId, rounds);
    const controller = new AbortController();
    this.setRunning(sessionId, controller);
    let outputs: ClientToolOutputItem[] | null;
    try {
      outputs = await this.collect(sessionId, responseId, calls, provider, controller.signal);
    } finally {
      // Stop and Send stay blocked until here; the handoff below is synchronous.
      this.setRunning(sessionId, null, controller);
    }
    // Stopped while the transcript loaded: the server drops unanswered calls.
    if (!outputs || controller.signal.aborted || this.disposed) return;
    try {
      await this.host.continueWith(sessionId, responseId, outputs);
    } catch (error) {
      this.host.toast(`Could not send the ${provider} tool results: ${errorMessage(error)}`);
    }
  }

  /**
   * Runs `calls`, then prepares the transcript for their results. Resolves
   * null when there is nothing to send: stopped, stale, or failed.
   */
  private async collect(
    sessionId: string,
    responseId: string,
    calls: PendingClientCall[],
    provider: string,
    signal: AbortSignal,
  ): Promise<ClientToolOutputItem[] | null> {
    try {
      const outputs = await this.execute(sessionId, responseId, calls, signal);
      if (!outputs) return null;
      // Stop must not wait out a slow refresh; the refresh may finish unobserved.
      const current = await Promise.race([
        this.host.prepareContinuation(sessionId, responseId),
        whenAborted(signal),
      ]);
      return current && !signal.aborted ? outputs : null;
    } catch (error) {
      if (!signal.aborted)
        this.host.toast(`Could not send the ${provider} tool results: ${errorMessage(error)}`);
      return null;
    }
  }

  /** Runs `calls` in order. Resolves null when stopped part-way. */
  private async execute(
    sessionId: string,
    responseId: string,
    calls: PendingClientCall[],
    signal: AbortSignal,
  ): Promise<ClientToolOutputItem[] | null> {
    const outputs: ClientToolOutputItem[] = [];
    for (const call of calls) {
      const ids = new Set([call.callId]);
      const startedAt = Date.now();
      this.update(sessionId, responseId, (projection) => ({
        ...projection,
        messages: patchToolCalls(projection.messages, ids, { status: 'running', startedAt }),
      }));
      const result = await this.host.bridge.run(call, signal);
      const stopped = signal.aborted;
      const endedAt = Date.now();
      this.update(sessionId, responseId, (projection) => ({
        ...projection,
        messages: patchToolCalls(projection.messages, ids, {
          status: stopped ? 'cancelled' : result.ok ? 'done' : 'error',
          resultStatus: result.ok ? 'success' : 'error',
          result: result.output,
          endedAt,
          durationMs: endedAt - startedAt,
        }),
      }));
      // The server drops unanswered calls before the next turn.
      if (stopped) return null;
      outputs.push(toolOutputItem(call.callId, result.output));
    }
    return outputs;
  }

  private setRunning(
    sessionId: string,
    controller: AbortController | null,
    replacing?: AbortController,
  ): void {
    const next = { ...this.running.peek() };
    if (controller) next[sessionId] = controller;
    else if (!replacing || next[sessionId] === replacing) delete next[sessionId];
    this.running.value = next;
  }

  /** Updates the session's projection only while it still shows `responseId`. */
  private update(
    sessionId: string,
    responseId: string,
    apply: (projection: ResponseProjection) => ResponseProjection,
  ): void {
    const projection = this.host.runs.peek()[sessionId];
    if (projection?.run.responseId !== responseId) return;
    this.host.runs.value = { ...this.host.runs.peek(), [sessionId]: apply(projection) };
  }
}
