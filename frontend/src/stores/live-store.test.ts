import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Endpoints } from '../api/endpoints';
import type { LiveSnapshot, LiveStartResponse } from '../platform/live';
import { LiveStore } from './live-store';

class FakeLiveCall {
  snapshot: LiveSnapshot = {
    phase: 'idle',
    capability: { supported: true, reason: '' },
    generation: 0,
    liveId: '',
    sessionId: '',
  };
  private listener: (snapshot: LiveSnapshot) => void = () => {};
  readonly stop = vi.fn(async () => {
    this.publish({ phase: 'ended', liveId: '' });
  });
  readonly dispose = vi.fn();

  subscribe(listener: (snapshot: LiveSnapshot) => void): () => void {
    this.listener = listener;
    listener(this.snapshot);
    return () => {
      this.listener = () => {};
    };
  }

  async start(sessionId: string): Promise<LiveStartResponse> {
    this.publish({ phase: 'requesting-permission', sessionId });
    const started = { live_id: 'live_one', session_id: sessionId, sdp: 'answer' };
    this.publish({ phase: 'listening', liveId: started.live_id, sessionId });
    return started;
  }

  private publish(patch: Partial<LiveSnapshot>): void {
    this.snapshot = { ...this.snapshot, ...patch, generation: this.snapshot.generation + 1 };
    this.listener(this.snapshot);
  }
}

function eventStream() {
  const encoder = new TextEncoder();
  let streamController!: ReadableStreamDefaultController<Uint8Array>;
  const response = new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        streamController = controller;
      },
    }),
    { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
  );
  return {
    response,
    push(id: number, event: string, data: unknown) {
      streamController.enqueue(
        encoder.encode(`id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`),
      );
    },
    close() {
      try {
        streamController.close();
      } catch {
        // The store may already have cancelled the stream.
      }
    },
  };
}

function closedSSE(...events: Array<{ id: number; event: string; data: unknown }>): Response {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        for (const event of events)
          controller.enqueue(
            encoder.encode(
              `id: ${event.id}\nevent: ${event.event}\ndata: ${JSON.stringify(event.data)}\n\n`,
            ),
          );
        controller.close();
      },
    }),
    { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
  );
}

function setup() {
  const events = eventStream();
  const endpoints = {
    liveEvents: vi.fn(async () => events.response),
    liveText: vi.fn(async () => ({ ok: true as const })),
  } as unknown as Endpoints;
  const call = new FakeLiveCall();
  const store = new LiveStore(endpoints, () => 'session-one', call);
  store.applyCapability({ enabled: true });
  return { store, call, endpoints, events };
}

afterEach(() => vi.useRealTimers());

describe('LiveStore', () => {
  it('selects browser PCM prerequisites from the server transport capability', () => {
    Object.defineProperty(globalThis, 'isSecureContext', { configurable: true, value: true });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn() },
    });
    vi.stubGlobal('WebSocket', class {});
    vi.stubGlobal('AudioContext', class {});
    vi.stubGlobal('AudioWorkletNode', class {});
    try {
      const store = new LiveStore({} as Endpoints, () => 'session-one');
      store.applyCapability({ enabled: true, provider: 'gemini', transport: 'websocket_pcm' });
      expect(store.enabled.value).toBe(true);
      expect(store.capability.value).toEqual({ supported: true, reason: '' });
      store.dispose();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('waits for a durable chat session before opening the microphone call', async () => {
    const events = eventStream();
    const endpoints = { liveEvents: vi.fn(async () => events.response) } as unknown as Endpoints;
    const call = new FakeLiveCall();
    const startCall = vi.spyOn(call, 'start');
    let finish!: (id: string) => void;
    const store = new LiveStore(
      endpoints,
      () =>
        new Promise<string>((resolve) => {
          finish = resolve;
        }),
      call,
    );
    store.applyCapability({ enabled: true });
    const started = store.start();
    expect(startCall).not.toHaveBeenCalled();
    finish('durable-chat');
    await expect(started).resolves.toBe(true);
    expect(startCall).toHaveBeenCalledWith('durable-chat');
    store.dispose();
    events.close();
  });

  it.each(['', 'rejected'])(
    'does not open a call when session creation fails: %s',
    async (failure) => {
      const call = new FakeLiveCall();
      const startCall = vi.spyOn(call, 'start');
      const store = new LiveStore(
        {} as Endpoints,
        async () => {
          if (failure) throw new Error('Could not create the session.');
          return '';
        },
        call,
      );
      store.applyCapability({ enabled: true });
      await expect(store.start()).resolves.toBe(false);
      expect(startCall).not.toHaveBeenCalled();
      expect(store.phase.value).toBe('failed');
      expect(store.lastError.value).toContain(
        failure ? 'Could not create' : 'chat session is required',
      );
      store.dispose();
    },
  );

  it('does not start a call if stopped while materializing the session', async () => {
    const call = new FakeLiveCall();
    const startCall = vi.spyOn(call, 'start');
    let finish!: (id: string) => void;
    const store = new LiveStore(
      {} as Endpoints,
      () =>
        new Promise<string>((resolve) => {
          finish = resolve;
        }),
      call,
    );
    store.applyCapability({ enabled: true });
    const started = store.start();
    await store.stop();
    finish('durable-chat');
    await expect(started).resolves.toBe(false);
    expect(startCall).not.toHaveBeenCalled();
    store.dispose();
  });

  it('keeps the original task working when its steering delegation finishes', async () => {
    const { store, events } = setup();
    await store.start();
    events.push(1, 'live.delegation', { delegation_id: 'task', state: 'running', text: 'review' });
    events.push(2, 'live.delegation', {
      delegation_id: 'correction',
      state: 'queued',
      text: 'changeset',
    });
    events.push(3, 'live.delegation', { delegation_id: 'correction', state: 'done' });
    await vi.waitFor(() => expect(store.delegation.value?.delegationId).toBe('task'));
    expect(store.working.value).toBe(true);
    expect(store.phase.value).toBe('working');
    events.push(4, 'live.delegation', { delegation_id: 'task', state: 'done' });
    await vi.waitFor(() => expect(store.working.value).toBe(false));
    expect(store.phase.value).toBe('listening');
    store.dispose();
    events.close();
  });

  it('replaces interim user previews without recording guesses or duplicating final text', async () => {
    const { store, events } = setup();
    await store.start();
    events.push(1, 'live.transcript', { role: 'assistant', text: 'Previous answer' });
    events.push(2, 'live.transcript', { role: 'user', text: 'open the wrong file', interim: true });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('open the wrong file'));
    events.push(3, 'live.transcript', { role: 'user', text: 'open the right file', interim: true });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('open the right file'));
    expect(store.recentTurns.value).toEqual([]);
    expect(store.partialAssistant.value).toBe('Previous answer');

    events.push(4, 'live.transcript', { role: 'user', text: 'Open the right file.' });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('Open the right file.'));
    events.push(5, 'live.transcript', { role: 'user', text: 'Open the right file.', final: true });
    await vi.waitFor(() =>
      expect(store.recentTurns.value).toEqual([{ role: 'user', text: 'Open the right file.' }]),
    );

    events.push(6, 'live.transcript', { role: 'user', text: 'unconfirmed', interim: true });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('unconfirmed'));
    events.push(7, 'live.transcript', { role: 'user', text: '', interim: true });
    await vi.waitFor(() => expect(store.partialUser.value).toBe(''));
    expect(store.recentTurns.value).toHaveLength(1);
    events.push(8, 'live.transcript', { role: 'user', text: 'another preview', interim: true });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('another preview'));
    await store.stop();
    expect(store.partialUser.value).toBe('');
    store.dispose();
  });

  it('keeps a late-finalized prompt before its interrupted answer and the following stop', async () => {
    const { store, events } = setup();
    await store.start();
    events.push(1, 'live.transcript', { role: 'user', text: 'Tell me a funny story.' });
    events.push(2, 'live.transcript', { role: 'assistant', text: 'A man bought a parrot...' });
    events.push(3, 'live.interrupted', {});
    await vi.waitFor(() => expect(store.recentTurns.value).toHaveLength(1));
    expect(store.transcriptTurns).toEqual([
      { role: 'user', text: 'Tell me a funny story.' },
      { role: 'assistant', text: 'A man bought a parrot...', interrupted: true },
    ]);
    events.push(4, 'live.transcript', {
      role: 'user',
      text: 'Tell me a funny story.',
      final: true,
    });
    events.push(5, 'live.transcript', { role: 'user', text: 'Stop.' });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('Stop.'));
    expect(store.transcriptTurns.map((t) => t.text)).toEqual([
      'Tell me a funny story.',
      'A man bought a parrot...',
      'Stop.',
    ]);
    events.push(6, 'live.transcript', { role: 'user', text: 'Stop.', final: true });
    await vi.waitFor(() => expect(store.recentTurns.value).toHaveLength(3));
    expect(store.recentTurns.value.map((t) => t.text)).toEqual([
      'Tell me a funny story.',
      'A man bought a parrot...',
      'Stop.',
    ]);
    store.dispose();
    events.close();
  });

  it('keeps the first position when interim text is revised and finals arrive in reverse order', async () => {
    const { store, events } = setup();
    await store.start();
    events.push(1, 'live.transcript', { role: 'user', text: 'Tell me', interim: true });
    events.push(2, 'live.transcript', { role: 'assistant', text: 'Sure' });
    events.push(3, 'live.transcript', { role: 'user', text: 'Tell me a story', interim: true });
    events.push(4, 'live.transcript', { role: 'assistant', text: 'Sure!', final: true });
    events.push(5, 'live.transcript', { role: 'user', text: 'Tell me a story.', final: true });
    await vi.waitFor(() => expect(store.recentTurns.value).toHaveLength(2));
    expect(store.transcriptTurns).toEqual([
      { role: 'user', text: 'Tell me a story.' },
      { role: 'assistant', text: 'Sure!' },
    ]);
    store.dispose();
    events.close();
  });

  it('retains interrupted assistant speech even when a user preview arrives first', async () => {
    const { store, events } = setup();
    await store.start();
    events.push(1, 'live.transcript', {
      role: 'assistant',
      text: 'Here is the explanation so far',
    });
    events.push(2, 'live.transcript', { role: 'user', text: 'Wait', interim: true });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('Wait'));
    expect(store.partialAssistant.value).toBe('Here is the explanation so far');
    events.push(3, 'live.interrupted', {});
    events.push(4, 'live.transcript', {
      role: 'assistant',
      text: 'Here is the explanation so far',
      final: true,
    });
    await vi.waitFor(() => expect(store.partialAssistant.value).toBe(''));
    expect(store.recentTurns.value).toEqual([
      { role: 'assistant', text: 'Here is the explanation so far', interrupted: true },
    ]);
    expect(store.phase.value).toBe('listening');
    events.push(5, 'live.transcript', {
      role: 'user',
      text: 'Wait, explain that bit.',
      final: true,
    });
    events.push(6, 'live.transcript', { role: 'assistant', text: 'Let me clarify.' });
    events.push(7, 'live.transcript', { role: 'assistant', text: 'Let me clarify.', final: true });
    await vi.waitFor(() => expect(store.recentTurns.value).toHaveLength(3));
    expect(store.recentTurns.value[0].interrupted).toBe(true);
    expect(store.recentTurns.value[2]).toEqual({ role: 'assistant', text: 'Let me clarify.' });
    store.dispose();
    events.close();
  });

  it('does not duplicate completed turns or clear working state on interruption', async () => {
    const { store, events } = setup();
    await store.start();
    events.push(1, 'live.transcript', { role: 'assistant', text: 'Finished answer', final: true });
    events.push(2, 'live.delegation', { delegation_id: 'job', state: 'running' });
    events.push(3, 'live.interrupted', {});
    await vi.waitFor(() => expect(store.phase.value).toBe('working'));
    expect(store.recentTurns.value).toEqual([{ role: 'assistant', text: 'Finished answer' }]);
    store.dispose();
    events.close();
  });

  it('applies accumulated partial and final transcript events into a bounded turn list', async () => {
    const { store, events } = setup();
    await store.start();

    events.push(1, 'live.transcript', { role: 'user', text: 'list the', final: false });
    await vi.waitFor(() => expect(store.partialUser.value).toBe('list the'));
    expect(store.phase.value).toBe('listening');

    events.push(2, 'live.transcript', {
      role: 'user',
      text: 'list the files',
      final: true,
    });
    events.push(3, 'live.transcript', {
      role: 'assistant',
      text: 'I will check',
      final: false,
    });
    await vi.waitFor(() => expect(store.partialAssistant.value).toBe('I will check'));
    expect(store.partialUser.value).toBe('');
    expect(store.recentTurns.value).toEqual([{ role: 'user', text: 'list the files' }]);
    expect(store.phase.value).toBe('speaking');

    for (let sequence = 4; sequence <= 25; sequence += 1)
      events.push(sequence, 'live.transcript', {
        role: 'assistant',
        text: `turn ${sequence}`,
        final: true,
      });
    await vi.waitFor(() => expect(store.recentTurns.value).toHaveLength(20));
    expect(store.recentTurns.value.at(-1)?.text).toBe('turn 25');
    store.dispose();
    events.close();
  });

  it('tracks delegation lifecycle and returns to listening when work completes', async () => {
    const { store, events } = setup();
    await store.start();

    events.push(1, 'live.delegation', {
      delegation_id: 'delegation-one',
      state: 'queued',
      text: 'inspect files',
    });
    await vi.waitFor(() => expect(store.delegation.value?.state).toBe('queued'));
    expect(store.phase.value).toBe('working');
    expect(store.working.value).toBe(true);

    events.push(2, 'live.delegation', {
      delegation_id: 'delegation-one',
      state: 'running',
    });
    await vi.waitFor(() => expect(store.delegation.value?.state).toBe('running'));
    events.push(3, 'live.delegation', { delegation_id: 'delegation-one', state: 'done' });
    await vi.waitFor(() => expect(store.delegation.value?.state).toBe('done'));
    expect(store.phase.value).toBe('listening');
    expect(store.working.value).toBe(false);
    store.dispose();
    events.close();
  });

  it('reconnects a transiently closed event stream from the last SSE sequence', async () => {
    const endpoints = {
      liveEvents: vi
        .fn()
        .mockResolvedValueOnce(
          closedSSE({
            id: 7,
            event: 'live.transcript',
            data: { role: 'user', text: 'remember this', final: true },
          }),
        )
        .mockResolvedValueOnce(closedSSE({ id: 8, event: 'live.ended', data: {} })),
      liveText: vi.fn(async () => ({ ok: true as const })),
    } as unknown as Endpoints;
    const call = new FakeLiveCall();
    const store = new LiveStore(endpoints, () => 'session-one', call);
    store.applyCapability({ enabled: true });

    await store.start();
    await vi.waitFor(() => expect(endpoints.liveEvents).toHaveBeenCalledTimes(2));

    expect(endpoints.liveEvents).toHaveBeenNthCalledWith(1, 'live_one', 0, expect.any(AbortSignal));
    expect(endpoints.liveEvents).toHaveBeenNthCalledWith(2, 'live_one', 7, expect.any(AbortSignal));
    await vi.waitFor(() => expect(store.phase.value).toBe('ended'));
    expect(store.recentTurns.value).toEqual([{ role: 'user', text: 'remember this' }]);
    store.dispose();
  });

  it('surfaces live errors and ends the peer when the server sends live.ended', async () => {
    const { store, call, events } = setup();
    await store.start();

    events.push(1, 'live.error', { message: 'Provider capacity is unavailable.' });
    await vi.waitFor(() => expect(store.lastError.value).toBe('Provider capacity is unavailable.'));
    expect(store.phase.value).toBe('failed');

    events.push(2, 'live.ended', {});
    await vi.waitFor(() => expect(store.phase.value).toBe('ended'));
    expect(call.stop).toHaveBeenCalledOnce();
    expect(store.liveId.value).toBe('');
    store.dispose();
    events.close();
  });

  it('stops an open call when the server withdraws the capability', async () => {
    const { store, call, events } = setup();
    await store.start();
    expect(store.active.value).toBe(true);

    store.applyCapability({ enabled: false });
    await vi.waitFor(() => expect(call.stop).toHaveBeenCalledOnce());
    expect(store.enabled.value).toBe(false);
    store.dispose();
    events.close();
  });
});
