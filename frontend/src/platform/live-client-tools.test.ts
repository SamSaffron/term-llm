import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  LiveClientToolSession,
  snapshotClientTools,
  type LiveClientTool,
} from './live-client-tools';

const tool = (
  execute: LiveClientTool['execute'] = () => ({ room: 'kitchen' }),
): LiveClientTool => ({
  name: 'ui_navigate',
  description: 'Navigate the UI',
  parameters: {
    type: 'object',
    properties: { room: { type: 'string', enum: ['kitchen'] } },
    required: ['room'],
    additionalProperties: false,
  },
  execute,
});
const call = (name = 'ui_navigate', id = 'call_1', args = '{"room":"kitchen"}') => ({
  type: 'function_call',
  name,
  call_id: id,
  arguments: args,
});
const response = (output = [call()], id = 'response_1') =>
  JSON.stringify({ type: 'response.done', response: { id, status: 'completed', output } });
const settle = async () => {
  for (let i = 0; i < 12; i++) await Promise.resolve();
};
function harness(tools = [tool()]) {
  const send = vi.fn();
  const fail = vi.fn();
  return { send, fail, session: new LiveClientToolSession(snapshotClientTools(tools), send, fail) };
}
afterEach(() => vi.useRealTimers());

describe('live client tools', () => {
  it('does not execute incomplete or canceled responses', async () => {
    const execute = vi.fn();
    const { session, send } = harness([tool(execute)]);
    for (const status of ['cancelled', 'failed', 'incomplete']) {
      session.receive(
        JSON.stringify({
          type: 'response.done',
          response: { id: status, status, output: [call()] },
        }),
      );
    }
    await settle();
    expect(execute).not.toHaveBeenCalled();
    expect(send).not.toHaveBeenCalled();
    session.close();
  });

  it('snapshots schemas and rejects duplicate/reserved names and invalid definitions', () => {
    const original = tool();
    const [snapshot] = snapshotClientTools([original]);
    original.parameters.type = 'string';
    expect(snapshot.parameters.type).toBe('object');
    for (const tools of [
      [tool(), tool()],
      [{ ...tool(), name: 'delegate_to_controller' }],
      [original],
      [{ ...tool(), description: '' }],
      Array.from({ length: 17 }, (_, i) => ({ ...tool(), name: `ui_${i}` })),
    ])
      expect(() => snapshotClientTools(tools)).toThrow();
  });

  it('executes a registered action once and returns a correlated JSON result then continues once', async () => {
    const execute = vi.fn(() => ({ room: 'kitchen' }));
    const { session, send, fail } = harness([tool(execute)]);
    session.receive(response());
    session.receive(response());
    await settle();
    expect(execute).toHaveBeenCalledTimes(1);
    expect(execute).toHaveBeenCalledWith(
      { room: 'kitchen' },
      { callId: 'call_1', signal: expect.any(AbortSignal) },
    );
    expect(send.mock.calls.map(([event]) => event)).toEqual([
      {
        type: 'conversation.item.create',
        item: {
          type: 'function_call_output',
          call_id: 'call_1',
          output: JSON.stringify({ ok: true, result: { room: 'kitchen' } }),
        },
      },
      { type: 'response.create' },
    ]);
    expect(fail).not.toHaveBeenCalled();
    session.close();
  });

  it.each(['not-json', 'null', '[]', '"javascript:alert(1)"', 'x'.repeat(16385)])(
    'rejects invalid arguments without executing: %.25s',
    async (args) => {
      const execute = vi.fn();
      const { session, send } = harness([tool(execute)]);
      session.receive(response([call('ui_navigate', 'call_1', args)]));
      await settle();
      expect(execute).not.toHaveBeenCalled();
      expect(JSON.parse(send.mock.calls[0][0].item.output).ok).toBe(false);
      session.close();
    },
  );

  it('returns errors for unknown tools, host validation failures, and unserializable or oversized results', async () => {
    for (const [name, execute] of [
      ['ui_missing', vi.fn()],
      [
        'ui_navigate',
        () => {
          throw new Error('Unknown UI room');
        },
      ],
      ['ui_navigate', () => 1n],
      ['ui_navigate', () => 'x'.repeat(16385)],
    ] as const) {
      const { session, send } = harness([tool(execute)]);
      session.receive(response([call(name)]));
      await settle();
      expect(JSON.parse(send.mock.calls[0][0].item.output).ok).toBe(false);
      expect(send.mock.calls[1][0]).toEqual({ type: 'response.create' });
      session.close();
    }
  });

  it.each([true, false])(
    'coordinates mixed built-in/custom results, built-in first=%s',
    async (builtinFirst) => {
      const execute = vi.fn(() => 'done');
      const { session, send } = harness([tool(execute)]);
      const ack = JSON.stringify({
        type: 'conversation.item.added',
        item: { type: 'function_call_output', call_id: 'builtin_1' },
      });
      if (builtinFirst) session.receive(ack);
      session.receive(response([call(), call('delegate_to_controller', 'builtin_1')]));
      await settle();
      if (!builtinFirst) {
        expect(send).toHaveBeenCalledTimes(1);
        session.receive(ack);
      }
      session.receive(ack);
      session.receive(response());
      expect(execute).toHaveBeenCalledTimes(1);
      expect(send.mock.calls.filter(([event]) => event.type === 'response.create')).toHaveLength(1);
      session.close();
    },
  );

  it('times out a pending handler and ignores its late completion', async () => {
    vi.useFakeTimers();
    let resolve!: (value: unknown) => void;
    let signal!: AbortSignal;
    const { session, send } = harness([
      tool((_, ctx) => {
        signal = ctx.signal;
        return new Promise((r) => {
          resolve = r;
        });
      }),
    ]);
    session.receive(response());
    await settle();
    await vi.advanceTimersByTimeAsync(10000);
    expect(signal.aborted).toBe(true);
    expect(JSON.parse(send.mock.calls[0][0].item.output)).toMatchObject({ ok: false });
    resolve('late');
    await settle();
    expect(send).toHaveBeenCalledTimes(2);
    session.close();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('aborts pending work on teardown without stale results or continuations', async () => {
    vi.useFakeTimers();
    let resolve!: (value: unknown) => void;
    let signal!: AbortSignal;
    const { session, send } = harness([
      tool((_, ctx) => {
        signal = ctx.signal;
        return new Promise((r) => {
          resolve = r;
        });
      }),
    ]);
    session.receive(response());
    await settle();
    session.close();
    resolve('late');
    await settle();
    expect(signal.aborted).toBe(true);
    expect(send).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
    session.receive(response());
    expect(send).not.toHaveBeenCalled();
  });

  it('ignores unrelated/malformed frames and reports send failures', async () => {
    const { session, send, fail } = harness();
    for (const frame of [
      'invalid',
      'null',
      '{}',
      JSON.stringify({ ...call(), type: 'response.function_call_arguments.done' }),
    ])
      session.receive(frame);
    expect(send).not.toHaveBeenCalled();
    send.mockImplementation(() => {
      throw new Error('closed');
    });
    session.receive(response());
    await settle();
    expect(fail).toHaveBeenCalledWith(expect.objectContaining({ message: 'closed' }));
    session.close();
  });
});
