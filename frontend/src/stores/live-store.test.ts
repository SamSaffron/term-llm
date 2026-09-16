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
  it('replaces registrations only while idle and rejects active/disposed stores', async () => {
    const { store, call, events } = setup();
    await expect(
      store.registerClientTools([
        {
          name: 'delegate_to_controller',
          description: 'bad',
          parameters: { type: 'object', properties: {} },
          execute: () => null,
        },
      ]),
    ).rejects.toThrow();
    expect(call.dispose).not.toHaveBeenCalled();
    await store.start();
    await expect(store.registerClientTools([])).rejects.toThrow('before starting');
    await store.stop();
    await store.registerClientTools([]);
    expect(call.dispose).toHaveBeenCalledTimes(1);
    store.dispose();
    events.close();
    await expect(store.registerClientTools([])).rejects.toThrow('before starting');
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
