import { signal } from '@preact/signals';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '../api/client';
import type { Endpoints } from '../api/endpoints';
import {
  clientToolDefinitions,
  type ClientTool,
  type PendingClientCall,
} from '../domain/client-tools';
import { LiveStore, type LiveClientTools } from './live-store';
import type { ClientToolResult } from './webmcp-store';

// The real call is created lazily by the store, with the store's own start
// endpoint; this stand-in drives that endpoint the way the media layer does.
const media = vi.hoisted(() => {
  type Snapshot = {
    phase: string;
    capability: { supported: boolean; reason: string };
    generation: number;
    liveId: string;
    sessionId: string;
  };
  type Started = { live_id: string; session_id: string };
  class MediaCall {
    snapshot: Snapshot = {
      phase: 'idle',
      capability: { supported: true, reason: '' },
      generation: 0,
      liveId: '',
      sessionId: '',
    };
    private listener: (snapshot: Snapshot) => void = () => undefined;
    constructor(
      private readonly startEndpoint: (sdp: string, sessionId: string) => Promise<Started>,
      private readonly stopEndpoint: (liveId: string) => Promise<unknown>,
    ) {}
    subscribe(listener: (snapshot: Snapshot) => void): () => void {
      this.listener = listener;
      listener(this.snapshot);
      return () => {
        this.listener = () => undefined;
      };
    }
    async start(sessionId: string): Promise<Started> {
      const started = await this.startEndpoint('offer-sdp', sessionId);
      this.publish({ phase: 'listening', liveId: started.live_id, sessionId });
      return started;
    }
    async stop(): Promise<void> {
      const liveId = this.snapshot.liveId;
      this.publish({ phase: 'ended', liveId: '' });
      if (liveId) await this.stopEndpoint(liveId);
    }
    dispose(): void {}
    private publish(patch: Partial<Snapshot>): void {
      this.snapshot = { ...this.snapshot, ...patch, generation: this.snapshot.generation + 1 };
      this.listener(this.snapshot);
    }
  }
  return { MediaCall };
});

vi.mock('../platform/live', () => ({ LiveCall: media.MediaCall }));

const ping: ClientTool = {
  name: 'ping',
  title: 'Ping',
  description: 'Replies pong.',
  inputSchema: { type: 'object', properties: {} },
  readOnly: true,
};
const music: ClientTool = { ...ping, name: 'music_search', description: 'Searches music.' };

function eventStream() {
  const encoder = new TextEncoder();
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const response = new Response(
    new ReadableStream<Uint8Array>({
      start(value) {
        controller = value;
      },
    }),
    { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
  );
  return {
    response,
    push(id: number, event: string, data: unknown) {
      controller.enqueue(
        encoder.encode(`id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`),
      );
    },
    close() {
      try {
        controller.close();
      } catch {
        // The store may already have cancelled the stream.
      }
    },
  };
}

/** Page tools whose list and per-conversation choice tests can change. */
function pageTools(initial: ClientTool[] = [ping]) {
  const tools = signal(initial);
  const disabled = signal<string[]>([]);
  const run = vi.fn(
    async (call: PendingClientCall, _signal: AbortSignal): Promise<ClientToolResult> => ({
      ok: true,
      output: `${call.name}:${call.arguments}`,
    }),
  );
  const bridge: LiveClientTools = {
    definitions: (sessionId) =>
      disabled.value.includes(sessionId) ? [] : clientToolDefinitions(tools.value, 'iPhone'),
    run,
  };
  return { tools, disabled, run, bridge };
}

function setup(bridge: LiveClientTools) {
  const events = eventStream();
  const endpoints = {
    liveStart: vi.fn(async (_sdp: string, sessionId: string) => ({
      live_id: 'live_one',
      session_id: sessionId,
      sdp: 'answer',
    })),
    liveStop: vi.fn(async () => ({ live_id: 'live_one', status: 'ended' as const })),
    liveEvents: vi.fn(async () => events.response),
    liveClientTools: vi.fn(async () => ({ ok: true as const, tools: 0 })),
    liveToolResult: vi.fn(async () => ({ ok: true as const })),
  };
  const store = new LiveStore(
    endpoints as unknown as Endpoints,
    () => 'session-one',
    undefined,
    undefined,
    undefined,
    bridge,
  );
  store.applyCapability({ enabled: true });
  return { store, endpoints, events };
}

const round = {
  session_id: 'session-one',
  request_id: 'tools_1',
  response_id: 'resp_1',
  resend: false,
  calls: [
    { call_id: 'call_1', name: 'webmcp__ping', arguments: '{"n":1}' },
    { call_id: 'call_2', name: 'webmcp__ping', arguments: '{"n":2}' },
  ],
};
const answer = {
  outputs: [
    { call_id: 'call_1', output: 'ping:{"n":1}' },
    { call_id: 'call_2', output: 'ping:{"n":2}' },
  ],
};

beforeEach(() => {
  // A secure context with a microphone and WebRTC, so the call may start.
  vi.stubGlobal('isSecureContext', true);
  vi.stubGlobal('RTCPeerConnection', class {});
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    value: { getUserMedia: vi.fn() },
  });
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('LiveStore page tools', () => {
  it('declares the page tools with the start request', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    await expect(store.start()).resolves.toBe(true);
    expect(endpoints.liveStart).toHaveBeenCalledWith('offer-sdp', 'session-one', undefined, [
      expect.objectContaining({ type: 'function', name: 'webmcp__ping' }),
    ]);
    // The server already holds them, so the sync has nothing to send.
    await vi.waitFor(() => expect(endpoints.liveEvents).toHaveBeenCalled());
    expect(endpoints.liveClientTools).not.toHaveBeenCalled();
    store.dispose();
    events.close();
  });

  it('declares nothing for a conversation that turned the page tools off', async () => {
    const page = pageTools();
    page.disabled.value = ['session-one'];
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();
    expect(endpoints.liveStart).toHaveBeenCalledWith('offer-sdp', 'session-one', undefined, []);
    await vi.waitFor(() => expect(endpoints.liveEvents).toHaveBeenCalled());
    expect(endpoints.liveClientTools).not.toHaveBeenCalled();
    store.dispose();
    events.close();
  });

  it('keeps the declaration current as the tools, the choice, or the binding change', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();

    page.tools.value = [ping, music];
    await vi.waitFor(() => expect(endpoints.liveClientTools).toHaveBeenCalledTimes(1));
    expect(endpoints.liveClientTools).toHaveBeenLastCalledWith('live_one', [
      expect.objectContaining({ name: 'webmcp__ping' }),
      expect.objectContaining({ name: 'webmcp__music_search' }),
    ]);

    // Another conversation's choice changes nothing for this call...
    page.disabled.value = ['session-two'];
    // ...until the call moves there.
    events.push(1, 'live.session_changed', {
      session_id: 'session-two',
      session_number: 2,
      title: 'Two',
    });
    await vi.waitFor(() => expect(endpoints.liveClientTools).toHaveBeenCalledTimes(2));
    expect(endpoints.liveClientTools).toHaveBeenLastCalledWith('live_one', []);

    page.disabled.value = [];
    await vi.waitFor(() => expect(endpoints.liveClientTools).toHaveBeenCalledTimes(3));
    await store.stop();
    page.tools.value = [ping];
    expect(endpoints.liveClientTools).toHaveBeenCalledTimes(3);
    store.dispose();
  });

  it('retries a failed tool declaration without requiring another settings change', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();
    endpoints.liveClientTools.mockRejectedValueOnce(new TypeError('offline'));
    page.tools.value = [ping, music];
    await vi.waitFor(() => expect(endpoints.liveClientTools).toHaveBeenCalledTimes(2), {
      timeout: 3_000,
    });
    expect(endpoints.liveClientTools).toHaveBeenLastCalledWith('live_one', [
      expect.objectContaining({ name: 'webmcp__ping' }),
      expect.objectContaining({ name: 'webmcp__music_search' }),
    ]);
    store.dispose();
    events.close();
  });

  it('runs a round in order, answers it once, and never reruns a republished round', async () => {
    const page = pageTools();
    let finishFirst!: () => void;
    page.run.mockImplementationOnce(
      (call) =>
        new Promise((resolve) => {
          finishFirst = () => resolve({ ok: true, output: `${call.name}:${call.arguments}` });
        }),
    );
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();

    events.push(1, 'live.tool_calls_requested', round);
    await vi.waitFor(() => expect(page.run).toHaveBeenCalledTimes(1));
    // The second call waits for the first, as the calls were made in order.
    events.push(2, 'live.tool_calls_requested', { ...round, resend: true });
    events.push(3, 'live.transcript', { role: 'assistant', text: 'still running' });
    await vi.waitFor(() => expect(store.partialAssistant.value).toBe('still running'));
    expect(page.run).toHaveBeenCalledTimes(1);
    finishFirst();

    await vi.waitFor(() => expect(endpoints.liveToolResult).toHaveBeenCalledTimes(1));
    expect(page.run.mock.calls.map(([call]) => call)).toEqual([
      { callId: 'call_1', name: 'ping', arguments: '{"n":1}' },
      { callId: 'call_2', name: 'ping', arguments: '{"n":2}' },
    ]);
    expect(endpoints.liveToolResult).toHaveBeenCalledWith('live_one', 'tools_1', answer);

    // The server took the answer; a late republication is neither run nor answered.
    events.push(4, 'live.tool_calls_requested', { ...round, resend: true });
    events.push(5, 'live.transcript', { role: 'assistant', text: 'answered' });
    await vi.waitFor(() => expect(store.partialAssistant.value).toBe('answered'));
    expect(page.run).toHaveBeenCalledTimes(2);
    expect(endpoints.liveToolResult).toHaveBeenCalledTimes(1);
    store.dispose();
    events.close();
  });

  it('answers a republished round from its first outputs after posting failed', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    endpoints.liveToolResult.mockRejectedValue(new TypeError('Failed to fetch'));
    await store.start();

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    events.push(1, 'live.tool_calls_requested', round);
    await vi.waitFor(() => expect(endpoints.liveToolResult).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(500);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(endpoints.liveToolResult).toHaveBeenCalledTimes(3);
    vi.useRealTimers();

    endpoints.liveToolResult.mockResolvedValue({ ok: true });
    events.push(2, 'live.tool_calls_requested', { ...round, resend: true });
    await vi.waitFor(() => expect(endpoints.liveToolResult).toHaveBeenCalledTimes(4));
    expect(endpoints.liveToolResult).toHaveBeenLastCalledWith('live_one', 'tools_1', answer);
    expect(page.run).toHaveBeenCalledTimes(2);
    store.dispose();
    events.close();
  });

  it('stops answering a round the server no longer wants', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    endpoints.liveToolResult.mockRejectedValue(
      new APIError('this tool call request is no longer accepting that result', 409),
    );
    await store.start();
    events.push(1, 'live.tool_calls_requested', round);
    await vi.waitFor(() => expect(endpoints.liveToolResult).toHaveBeenCalledTimes(1));
    events.push(2, 'live.tool_calls_requested', { ...round, resend: true });
    events.push(3, 'live.transcript', { role: 'assistant', text: 'refused' });
    await vi.waitFor(() => expect(store.partialAssistant.value).toBe('refused'));
    expect(endpoints.liveToolResult).toHaveBeenCalledTimes(1);
    expect(page.run).toHaveBeenCalledTimes(2);
    store.dispose();
    events.close();
  });

  it('answers with an error when the server cannot take the outputs', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    endpoints.liveToolResult.mockRejectedValueOnce(
      new APIError('the tool results are too large', 413),
    );
    await store.start();
    events.push(1, 'live.tool_calls_requested', round);
    await vi.waitFor(() => expect(endpoints.liveToolResult).toHaveBeenCalledTimes(2));
    expect(endpoints.liveToolResult).toHaveBeenNthCalledWith(1, 'live_one', 'tools_1', answer);
    // Failing now beats a delegation that waits out its timeout in silence.
    expect(endpoints.liveToolResult).toHaveBeenLastCalledWith('live_one', 'tools_1', {
      error: 'The device could not send its tool results.',
    });
    expect(page.run).toHaveBeenCalledTimes(2);
    store.dispose();
    events.close();
  });

  it('answers a tool the conversation does not offer without running it', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();
    page.disabled.value = ['session-one'];
    events.push(1, 'live.tool_calls_requested', {
      ...round,
      calls: [
        { call_id: 'call_1', name: 'webmcp__ping', arguments: '{}' },
        { call_id: 'call_2', name: 'shell', arguments: '{}' },
      ],
    });
    await vi.waitFor(() => expect(endpoints.liveToolResult).toHaveBeenCalledTimes(1));
    expect(page.run).not.toHaveBeenCalled();
    expect(endpoints.liveToolResult).toHaveBeenCalledWith('live_one', 'tools_1', {
      outputs: [
        { call_id: 'call_1', output: 'Error: webmcp__ping is not available in this conversation' },
        { call_id: 'call_2', output: 'Error: shell is not available in this conversation' },
      ],
    });
    store.dispose();
    events.close();
  });

  it('reports a round it cannot read instead of guessing', async () => {
    const page = pageTools();
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();
    events.push(1, 'live.tool_calls_requested', {
      ...round,
      calls: [{ call_id: 'call_1', name: 'webmcp__ping' }, { name: 'webmcp__ping' }],
    });
    await vi.waitFor(() => expect(endpoints.liveToolResult).toHaveBeenCalledTimes(1));
    expect(page.run).not.toHaveBeenCalled();
    expect(endpoints.liveToolResult).toHaveBeenCalledWith('live_one', 'tools_1', {
      error: 'The device could not read the requested tool calls.',
    });
    store.dispose();
    events.close();
  });

  it('cancels device work before a stalled stop request finishes', async () => {
    const page = pageTools();
    page.run.mockImplementation(
      (_call, signal) =>
        new Promise((resolve) => {
          signal.addEventListener('abort', () => resolve({ ok: false, output: 'cancelled' }), {
            once: true,
          });
        }),
    );
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();
    events.push(1, 'live.tool_calls_requested', round);
    await vi.waitFor(() => expect(page.run).toHaveBeenCalledTimes(1));
    let finishStop!: () => void;
    endpoints.liveStop.mockImplementation(
      () =>
        new Promise((resolve) => {
          finishStop = () => resolve({ live_id: 'live_one', status: 'ended' });
        }),
    );
    const stopping = store.stop();
    expect(page.run.mock.calls[0][1].aborted).toBe(true);
    finishStop();
    await stopping;
    expect(endpoints.liveToolResult).not.toHaveBeenCalled();
    store.dispose();
    events.close();
  });

  it('does not start later device calls after the host cancels a round', async () => {
    const page = pageTools();
    page.run.mockImplementation(
      (_call, signal) =>
        new Promise((resolve) => {
          signal.addEventListener('abort', () => resolve({ ok: false, output: 'cancelled' }), {
            once: true,
          });
        }),
    );
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();
    events.push(1, 'live.tool_calls_requested', round);
    await vi.waitFor(() => expect(page.run).toHaveBeenCalledTimes(1));
    events.push(2, 'live.tool_calls_cancelled', { request_id: 'tools_1' });
    events.push(3, 'live.transcript', { role: 'assistant', text: 'timed out' });
    await vi.waitFor(() => expect(store.partialAssistant.value).toBe('timed out'));
    expect(page.run.mock.calls[0][1].aborted).toBe(true);
    expect(page.run).toHaveBeenCalledTimes(1);
    expect(endpoints.liveToolResult).not.toHaveBeenCalled();
    store.dispose();
    events.close();
  });

  it('rejects an already expired host deadline without running tools', async () => {
    const page = pageTools();
    const { store, events } = setup(page.bridge);
    await store.start();
    events.push(1, 'live.tool_calls_requested', { ...round, deadline_ms: Date.now() - 100 });
    events.push(2, 'live.transcript', { role: 'assistant', text: 'expired' });
    await vi.waitFor(() => expect(store.partialAssistant.value).toBe('expired'));
    expect(page.run).not.toHaveBeenCalled();
    store.dispose();
    events.close();
  });

  it('abandons a running round when the call stops', async () => {
    const page = pageTools();
    page.run.mockImplementation(
      (_call, signal) =>
        new Promise((resolve) =>
          signal.addEventListener('abort', () => resolve({ ok: false, output: 'Error: stopped' }), {
            once: true,
          }),
        ),
    );
    const { store, endpoints, events } = setup(page.bridge);
    await store.start();
    events.push(1, 'live.tool_calls_requested', round);
    await vi.waitFor(() => expect(page.run).toHaveBeenCalledTimes(1));
    const [, signal] = page.run.mock.calls[0];
    await store.stop();
    expect(signal.aborted).toBe(true);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(page.run).toHaveBeenCalledTimes(1);
    expect(endpoints.liveToolResult).not.toHaveBeenCalled();
    store.dispose();
    events.close();
  });
});
