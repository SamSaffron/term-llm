import { afterEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '../api/client';
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
    shellCollaboration: vi.fn(async (_id: string, shellId: string, enabled: boolean) => ({
      shell_id: shellId,
      supported: true,
      shell_tool_available: true,
      enabled,
      state: enabled ? ('ready' as const) : ('off' as const),
      revision: 2,
      sequence: 2,
      command_id: '',
      tool_call_id: '',
      reason: '',
    })),
    shellInterrupt: vi.fn(async (_id: string, shellId: string) => ({
      shell_id: shellId,
      supported: true,
      shell_tool_available: true,
      enabled: true,
      state: 'ready' as const,
      revision: 3,
      sequence: 3,
      command_id: '',
      tool_call_id: '',
      reason: '',
    })),
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
  it('opens without a session and binds when the active session becomes durable', () => {
    const { store, toast, setActiveSession } = setup();
    setActiveSession('');

    expect(store.show()).toBe(true);
    expect(store.visible.value).toBe(true);
    expect(store.sessionId.value).toBe('');
    expect(toast).not.toHaveBeenCalled();

    setActiveSession('session-created');
    expect(store.bind('session-created')).toBe(true);
    expect(store.sessionId.value).toBe('session-created');
  });

  it('persists terminal docking mode and independent dock sizes', () => {
    localStorage.setItem(
      'term_llm_shell_layout',
      JSON.stringify({ mode: 'right', bottom: 410, right: 610 }),
    );
    const first = setup().store;
    expect(first.layout.value).toBe('right');
    expect(first.dockBottomSize.value).toBe(410);
    expect(first.dockRightSize.value).toBe(610);

    first.setLayout('bottom');
    first.setDockSize('bottom', 455);
    first.setDockSize('right', 675);
    expect(JSON.parse(localStorage.getItem('term_llm_shell_layout') || '{}')).toEqual({
      mode: 'bottom',
      bottom: 455,
      right: 675,
    });
    first.dispose();

    const restored = setup().store;
    expect(restored.layout.value).toBe('bottom');
    expect(restored.dockBottomSize.value).toBe(455);
    expect(restored.dockRightSize.value).toBe(675);
    restored.dispose();
  });

  it('clamps restored terminal dock sizes to supported bounds', () => {
    localStorage.setItem(
      'term_llm_shell_layout',
      JSON.stringify({ mode: 'bottom', bottom: 9999, right: -1 }),
    );
    const { store } = setup();
    expect(store.dockBottomSize.value).toBe(1400);
    expect(store.dockRightSize.value).toBe(320);
    store.dispose();
  });

  it('resets the event cursor when a stale stream reattaches to a replacement shell', async () => {
    const { store, endpoints, sink } = setup();
    const snapshot = (shellId: string, revision: number, sequence: number) => ({
      shell_id: shellId,
      supported: true,
      shell_tool_available: true,
      enabled: true,
      state: 'ready' as const,
      revision,
      sequence,
      command_id: '',
      tool_call_id: '',
      reason: '',
    });
    vi.mocked(endpoints.shellCreate)
      .mockResolvedValueOnce({
        shell_id: 'sh_old',
        cwd: '/old',
        created: false,
        state: 'running',
        collaboration: snapshot('sh_old', 20, 20),
      })
      .mockResolvedValueOnce({
        shell_id: 'sh_new',
        cwd: '/new',
        created: true,
        state: 'running',
        collaboration: snapshot('sh_new', 0, 0),
      });
    vi.mocked(endpoints.shellStream)
      .mockReset()
      .mockResolvedValueOnce(new Response(null, { status: 409 }))
      .mockResolvedValueOnce(
        sse(
          {
            event: 'collaboration_desynchronized',
            data: {
              shell_id: 'sh_new',
              revision: 1,
              sequence: 1,
              state: 'desynchronized',
              reason: 'new generation event',
            },
          },
          { event: 'exit', data: { shell_id: 'sh_new', offset: 0, exit_code: 0 } },
        ),
      );
    store.show('session-one');
    await store.connect(80, 24, sink);
    expect(store.shellId.value).toBe('sh_new');
    expect(store.collaborationReason.value).toBe('new generation event');
  });

  it('reconciles collaboration snapshots and ordered command events', async () => {
    const { store, endpoints, sink } = setup();
    const snapshot = {
      shell_id: 'sh_one',
      supported: true,
      shell_tool_available: true,
      enabled: true,
      state: 'ready' as const,
      revision: 1,
      sequence: 1,
      command_id: '',
      tool_call_id: '',
      reason: '',
    };
    vi.mocked(endpoints.shellCreate).mockResolvedValue({
      shell_id: 'sh_one',
      cwd: '/workspace',
      created: true,
      state: 'running',
      collaboration: snapshot,
    });
    vi.mocked(endpoints.shellStream)
      .mockReset()
      .mockResolvedValueOnce(
        sse(
          {
            event: 'ready',
            data: { shell_id: 'sh_one', sequence: 1, collaboration: snapshot },
          },
          {
            event: 'collaboration_desynchronized',
            data: {
              shell_id: 'sh_one',
              revision: 0,
              sequence: 99,
              state: 'desynchronized',
              reason: 'stale revision',
            },
          },
          {
            event: 'agent_command_started',
            data: {
              shell_id: 'sh_one',
              revision: 2,
              sequence: 2,
              state: 'agent_running',
              command_id: 'cmd_one',
              tool_call_id: 'call_one',
            },
          },
          {
            event: 'agent_command_finished',
            data: {
              shell_id: 'sh_one',
              revision: 3,
              sequence: 3,
              enabled: true,
              state: 'ready',
            },
          },
          {
            event: 'collaboration_desynchronized',
            data: {
              shell_id: 'sh_one',
              revision: 4,
              sequence: 4,
              state: 'desynchronized',
              reason: 'marker lost',
            },
          },
          {
            event: 'exit',
            data: { shell_id: 'sh_one', offset: 0, exit_code: 0, collaboration: snapshot },
          },
        ),
      );
    store.show('session-one');
    await store.connect(80, 24, sink);
    expect(store.collaborationSupported.value).toBe(true);
    expect(store.collaborationState.value).toBe('off');
    expect(store.activeCommandId.value).toBe('');
    expect(store.status.value).toBe('exited');
  });

  it('converges two attached stores from the same server authority', async () => {
    const first = setup();
    const second = setup();
    const ready = {
      shell_id: 'sh_one',
      supported: true,
      shell_tool_available: true,
      enabled: true,
      state: 'ready' as const,
      revision: 1,
      sequence: 1,
      command_id: '',
      tool_call_id: '',
      reason: '',
    };
    const off = { ...ready, enabled: false, state: 'off' as const, revision: 4, sequence: 4 };
    for (const fixture of [first, second]) {
      vi.mocked(fixture.endpoints.shellCreate).mockResolvedValue({
        shell_id: 'sh_one',
        cwd: '/workspace',
        created: false,
        state: 'running',
        collaboration: ready,
      });
      vi.mocked(fixture.endpoints.shellStream)
        .mockReset()
        .mockResolvedValueOnce(
          sse(
            { event: 'ready', data: { shell_id: 'sh_one', collaboration: ready } },
            {
              event: 'agent_command_started',
              data: {
                shell_id: 'sh_one',
                revision: 2,
                sequence: 2,
                enabled: true,
                state: 'agent_running',
                command_id: 'cmd_one',
              },
            },
            {
              event: 'agent_command_finished',
              data: {
                shell_id: 'sh_one',
                revision: 3,
                sequence: 3,
                enabled: false,
                state: 'off',
              },
            },
            { event: 'exit', data: { shell_id: 'sh_one', exit_code: 0, collaboration: off } },
          ),
        );
      fixture.store.show('session-one');
    }
    await Promise.all([
      first.store.connect(80, 24, first.sink),
      second.store.connect(80, 24, second.sink),
    ]);
    expect({
      state: first.store.collaborationState.value,
      enabled: first.store.collaborationEnabled.value,
      revision: first.store.collaborationRevision.value,
      sequence: first.store.collaborationSequence.value,
    }).toEqual({
      state: second.store.collaborationState.value,
      enabled: second.store.collaborationEnabled.value,
      revision: second.store.collaborationRevision.value,
      sequence: second.store.collaborationSequence.value,
    });
    expect(first.store.collaborationRevision.value).toBe(4);
  });

  it('applies authoritative collaboration state from mutation errors without optimism', async () => {
    const { store, endpoints } = setup();
    store.show('session-one');
    store.shellId.value = 'sh_one';
    store.status.value = 'running';
    const snapshot = {
      shell_id: 'sh_one',
      supported: true,
      shell_tool_available: true,
      enabled: false,
      state: 'off' as const,
      revision: 7,
      sequence: 9,
      command_id: '',
      tool_call_id: '',
      reason: 'response active',
    };
    vi.mocked(endpoints.shellCollaboration).mockRejectedValueOnce(
      new APIError('busy', 409, JSON.stringify({ collaboration: snapshot }), 'session_busy'),
    );
    await store.enableCollaboration();
    expect(store.collaborationEnabled.value).toBe(false);
    expect(store.collaborationRevision.value).toBe(7);
    expect(store.collaborationReason.value).toBe('response active');
  });

  it.each(['enable', 'disable', 'interrupt'] as const)(
    'clears a stale pending %s mutation when detaching and reopening',
    async (action) => {
      const pending = deferred<{
        shell_id: string;
        supported: boolean;
        shell_tool_available: boolean;
        enabled: boolean;
        state: 'off' | 'ready';
        revision: number;
        sequence: number;
        command_id: string;
        tool_call_id: string;
        reason: string;
      }>();
      const { store, endpoints } = setup();
      store.show('session-one');
      store.shellId.value = 'sh_one';
      store.status.value = 'running';
      if (action !== 'enable') {
        store.collaborationEnabled.value = true;
        store.collaborationState.value = action === 'interrupt' ? 'agent_running' : 'ready';
      }
      if (action === 'interrupt') store.activeCommandId.value = 'cmd_one';
      if (action === 'interrupt')
        vi.mocked(endpoints.shellInterrupt).mockReturnValueOnce(pending.promise);
      else vi.mocked(endpoints.shellCollaboration).mockReturnValueOnce(pending.promise);

      const mutation =
        action === 'enable'
          ? store.enableCollaboration()
          : action === 'disable'
            ? store.disableCollaboration()
            : store.interruptCommand();
      await Promise.resolve();
      expect(store.collaborationPending.value).not.toBeNull();
      store.back();
      expect(store.collaborationPending.value).toBeNull();
      expect(store.show('session-one')).toBe(true);
      pending.resolve({
        shell_id: 'sh_one',
        supported: true,
        shell_tool_available: true,
        enabled: false,
        state: 'off',
        revision: 99,
        sequence: 99,
        command_id: '',
        tool_call_id: '',
        reason: '',
      });
      await mutation;
      expect(store.collaborationPending.value).toBeNull();
      expect(store.collaborationRevision.value).not.toBe(99);
    },
  );

  it('sends authoritative enable, interrupt, and disable identifiers', async () => {
    const { store, endpoints } = setup();
    store.show('session-one');
    store.shellId.value = 'sh_one';
    store.status.value = 'running';
    await store.enableCollaboration();
    expect(endpoints.shellCollaboration).toHaveBeenCalledWith('session-one', 'sh_one', true);
    store.collaborationState.value = 'agent_running';
    store.activeCommandId.value = 'cmd_one';
    await store.interruptCommand();
    expect(endpoints.shellInterrupt).toHaveBeenCalledWith('session-one', 'sh_one', 'cmd_one');
    await store.disableCollaboration();
    expect(endpoints.shellCollaboration).toHaveBeenCalledWith('session-one', 'sh_one', false);
  });

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

  it('reports rejected input without hiding live collaboration controls', async () => {
    vi.useFakeTimers();
    const { store, endpoints, toast } = setup();
    vi.mocked(endpoints.shellInput).mockRejectedValue(
      new APIError('browser input queue is full', 409, '', 'shell_input_busy'),
    );
    store.show('session-one');
    store.shellId.value = 'sh_one';
    store.status.value = 'running';
    store.collaborationEnabled.value = true;
    store.collaborationState.value = 'agent_running';
    store.activeCommandId.value = 'cmd_one';
    store.input('reply');
    await vi.advanceTimersByTimeAsync(8);
    await Promise.resolve();

    expect(store.status.value).toBe('running');
    expect(store.collaborationState.value).toBe('agent_running');
    expect(store.activeCommandId.value).toBe('cmd_one');
    expect(store.error.value).toContain('browser input queue is full');
    expect(toast).toHaveBeenCalledWith(expect.any(APIError), 'error');
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
