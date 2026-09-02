import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Endpoints } from '../api/endpoints';
import { ShellStore, type ShellSink } from './shell-store';

const sse = (...events: Array<{ event: string; data: unknown }>): Response => {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream({
      start(controller) {
        for (const event of events)
          controller.enqueue(
            encoder.encode(`event: ${event.event}\ndata: ${JSON.stringify(event.data)}\n\n`),
          );
        controller.close();
      },
    }),
    { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
  );
};

const deferred = <T>() => {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
};

function setup() {
  const endpoints = {
    shellCreate: vi.fn(async () => ({
      shell_id: 'sh_one',
      cwd: '/workspace',
      created: true,
      state: 'running' as const,
    })),
    shellStream: vi
      .fn()
      .mockResolvedValueOnce(
        sse(
          { event: 'ready', data: { shell_id: 'sh_one', next_offset: 0 } },
          { event: 'output', data: { offset: 0, next_offset: 2, data: btoa('hi') } },
        ),
      )
      .mockResolvedValueOnce(sse({ event: 'exit', data: { offset: 2, exit_code: 0 } })),
    shellInput: vi.fn(async () => ({ accepted: 2 })),
    shellResize: vi.fn(async () => undefined),
    shellClose: vi.fn(async () => undefined),
  } as unknown as Endpoints;
  const toast = vi.fn();
  let activeSessionId = 'session-one';
  const store = new ShellStore(endpoints, toast, () => activeSessionId);
  store.enabled.value = true;
  const sink: ShellSink = { write: vi.fn(), reset: vi.fn() };
  return {
    store,
    endpoints,
    toast,
    sink,
    setActiveSession: (sessionId: string) => {
      activeSessionId = sessionId;
    },
  };
}

afterEach(() => vi.useRealTimers());

describe('ShellStore', () => {
  it('attaches, reconnects from the consumed offset, decodes output, and reports exit', async () => {
    const { store, endpoints, sink } = setup();
    expect(store.show('session-one')).toBe(true);

    await store.connect(100, 30, sink);

    expect(endpoints.shellCreate).toHaveBeenCalledWith('session-one', 100, 30);
    expect(endpoints.shellStream).toHaveBeenNthCalledWith(
      1,
      'session-one',
      'sh_one',
      0,
      expect.any(AbortSignal),
    );
    expect(endpoints.shellStream).toHaveBeenNthCalledWith(
      2,
      'session-one',
      'sh_one',
      2,
      expect.any(AbortSignal),
    );
    expect(sink.write).toHaveBeenCalledWith(new Uint8Array([104, 105]));
    expect(store.offset.value).toBe(2);
    expect(store.status.value).toBe('exited');
    expect(store.exitCode.value).toBe(0);
  });

  it('replays retained output into a fresh terminal when returning from chat', async () => {
    const { store, endpoints } = setup();
    vi.mocked(endpoints.shellStream)
      .mockReset()
      .mockResolvedValueOnce(
        sse(
          { event: 'output', data: { offset: 0, next_offset: 2, data: btoa('hi') } },
          { event: 'exit', data: { offset: 2, exit_code: 0 } },
        ),
      );
    store.show('session-one');
    store.shellId.value = 'sh_one';
    store.offset.value = 42;
    store.status.value = 'running';
    store.back();
    expect(store.show('session-one')).toBe(true);

    const freshSink: ShellSink = { write: vi.fn(), reset: vi.fn() };
    await store.connect(100, 30, freshSink);

    expect(freshSink.reset).toHaveBeenCalled();
    expect(endpoints.shellStream).toHaveBeenCalledWith(
      'session-one',
      'sh_one',
      0,
      expect.any(AbortSignal),
    );
    expect(freshSink.write).toHaveBeenCalledWith(new Uint8Array([104, 105]));
  });

  it('serializes and batches input without closing the shell when returning to chat', async () => {
    vi.useFakeTimers();
    const { store, endpoints } = setup();
    store.show('session-one');
    store.shellId.value = 'sh_one';
    store.status.value = 'running';

    store.input('a');
    store.input('b');
    await vi.runAllTimersAsync();
    await Promise.resolve();

    expect(endpoints.shellInput).toHaveBeenCalledTimes(1);
    expect(endpoints.shellInput).toHaveBeenCalledWith('session-one', 'sh_one', btoa('ab'));
    store.back();
    expect(store.visible.value).toBe(false);
    expect(endpoints.shellClose).not.toHaveBeenCalled();
  });

  it('explicitly closes the bound generation and hides unsupported servers', async () => {
    const { store, endpoints, toast } = setup();
    store.enabled.value = false;
    expect(store.show('session-one')).toBe(false);
    expect(toast).toHaveBeenCalledWith('Interactive shell is unavailable on this server.', 'error');

    store.enabled.value = true;
    store.show('session-one');
    store.shellId.value = 'sh_one';
    await store.close();
    expect(endpoints.shellClose).toHaveBeenCalledWith('session-one', 'sh_one');
    expect(store.visible.value).toBe(false);
  });

  it('applies replay resets before writing replacement output', async () => {
    const { store, endpoints, sink } = setup();
    vi.mocked(endpoints.shellStream)
      .mockReset()
      .mockResolvedValueOnce(
        sse(
          { event: 'reset', data: { offset: 40, reason: 'replay_window' } },
          { event: 'output', data: { offset: 40, next_offset: 42, data: btoa('ok') } },
          { event: 'exit', data: { offset: 42, exit_code: 0 } },
        ),
      );
    store.show('session-one');
    await store.connect(80, 24, sink);
    expect(sink.reset).toHaveBeenCalled();
    expect(sink.write).toHaveBeenCalledWith(new Uint8Array([111, 107]));
    expect(store.offset.value).toBe(42);
  });

  it('discards pending input batches when detaching, rebinding, restarting, or closing', async () => {
    vi.useFakeTimers();
    for (const action of ['detach', 'rebind', 'restart', 'close'] as const) {
      const { store, endpoints, sink, setActiveSession } = setup();
      store.show('session-one');
      store.shellId.value = 'sh_one';
      store.status.value = 'running';
      store.input(`stale-${action}`);

      if (action === 'detach') store.detach();
      if (action === 'rebind') {
        setActiveSession('session-two');
        store.show('session-two');
      }
      if (action === 'restart') {
        const restarting = store.restart(80, 24, sink);
        store.dispose();
        await restarting;
      }
      if (action === 'close') await store.close();
      await vi.advanceTimersByTimeAsync(8);

      expect(endpoints.shellInput, action).not.toHaveBeenCalled();
      store.dispose();
    }
  });

  it('does not let stale queued input or its error mutate a replacement binding', async () => {
    vi.useFakeTimers();
    const pending = deferred<{ accepted: number }>();
    const { store, endpoints, setActiveSession } = setup();
    vi.mocked(endpoints.shellInput).mockImplementation(() => pending.promise);
    store.show('session-one');
    store.shellId.value = 'sh_one';
    store.status.value = 'running';
    store.input('x'.repeat(60 * 1024));
    vi.advanceTimersByTime(8);
    await Promise.resolve();
    await Promise.resolve();
    expect(endpoints.shellInput).toHaveBeenCalledTimes(1);

    setActiveSession('session-two');
    store.show('session-two');
    store.shellId.value = 'sh_two';
    store.status.value = 'running';
    store.error.value = '';
    pending.reject(new Error('old shell failed'));
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    expect(endpoints.shellInput).toHaveBeenCalledTimes(1);
    expect(store.status.value).toBe('running');
    expect(store.error.value).toBe('');
    store.dispose();
  });

  it.each(['detach', 'restart', 'close'] as const)(
    'ignores an old input error after %s invalidates its generation',
    async (action) => {
      vi.useFakeTimers();
      const pendingInput = deferred<{ accepted: number }>();
      const pendingCreate = deferred<{
        shell_id: string;
        cwd: string;
        created: boolean;
        state: 'running';
      }>();
      const { store, endpoints, sink } = setup();
      vi.mocked(endpoints.shellInput).mockImplementation(() => pendingInput.promise);
      store.show('session-one');
      store.shellId.value = 'sh_one';
      store.status.value = 'running';
      store.input('old input');
      vi.advanceTimersByTime(8);
      await Promise.resolve();
      await Promise.resolve();
      expect(endpoints.shellInput).toHaveBeenCalledOnce();

      let restarting: Promise<void> | undefined;
      if (action === 'detach') store.detach();
      if (action === 'restart') {
        vi.mocked(endpoints.shellCreate).mockImplementation(() => pendingCreate.promise);
        restarting = store.restart(80, 24, sink);
      }
      if (action === 'close') await store.close();
      const replacementStatus = store.status.value;
      pendingInput.reject(new Error('stale input failure'));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();

      expect(store.status.value).toBe(replacementStatus);
      expect(store.error.value).toBe('');
      store.dispose();
      if (restarting) {
        pendingCreate.resolve({
          shell_id: 'sh_two',
          cwd: '/workspace',
          created: true,
          state: 'running',
        });
        await restarting;
      }
    },
  );

  it('guards transport controls after the active conversation no longer matches', async () => {
    vi.useFakeTimers();
    const { store, endpoints, sink, setActiveSession } = setup();
    store.show('session-one');
    store.shellId.value = 'sh_one';
    store.status.value = 'running';
    setActiveSession('session-two');

    store.input('stale');
    store.resize(100, 30);
    await store.restart(100, 30, sink);
    await store.close();
    await vi.runAllTimersAsync();

    expect(endpoints.shellInput).not.toHaveBeenCalled();
    expect(endpoints.shellResize).not.toHaveBeenCalled();
    expect(endpoints.shellCreate).not.toHaveBeenCalled();
    expect(endpoints.shellClose).not.toHaveBeenCalled();
    expect(store.visible.value).toBe(false);
    expect(store.sessionId.value).toBe('');
    store.dispose();
  });
});
