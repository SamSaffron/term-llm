import { signal } from '@preact/signals';
import { describe, expect, it, vi } from 'vitest';
import { initialProjection, type ResponseProjection } from '../domain/response';
import type { Message } from '../domain/types';
import { ClientToolRunner, MAX_CLIENT_TOOL_ROUNDS } from './client-tool-runner';
import type { ClientToolBridge, ClientToolResult } from './webmcp-store';

const toolGroup = (responseId: string, name: string, id = 'call_1'): Message => ({
  id: `${responseId}:tools:${id}`,
  role: 'tool-group',
  content: '',
  created: 1,
  responseId,
  toolGroupClosed: true,
  tools: [{ id, name, status: 'done', arguments: '{}' }],
});

const finished = (
  responseId: string,
  status: ResponseProjection['run']['status'] = 'completed',
  messages = [toolGroup(responseId, 'webmcp__ping')],
): ResponseProjection => ({
  ...initialProjection({
    responseId,
    sessionId: 's1',
    epoch: 1,
    status,
    lastSequence: 0,
    startedRev: 0,
    reconnects: 0,
  }),
  messages,
});

function setup(run: ClientToolBridge['run'] = async () => ({ ok: true, output: 'pong' })) {
  const runs = signal<Record<string, ResponseProjection>>({});
  const host = {
    bridge: { definitions: () => [], provider: () => 'iPhone', run: vi.fn(run) },
    runs,
    prepareContinuation: vi.fn(async (_sessionId: string, _responseId: string) => true),
    continueWith: vi.fn(async () => undefined),
    toast: vi.fn(),
  };
  const runner = new ClientToolRunner(host);
  const complete = (projection: ResponseProjection) => {
    runs.value = { ...runs.value, s1: projection };
    runner.finished('s1', projection);
  };
  return { host, runner, runs, complete, running: runner.runningIn(() => 's1') };
}

describe('ClientToolRunner', () => {
  it('runs the calls of responses it offered tools on, then continues', async () => {
    const { host, runner, complete, runs } = setup();
    runner.recordOffer('r1');
    complete(finished('r1'));

    await vi.waitFor(() => expect(host.continueWith).toHaveBeenCalled());
    expect(host.bridge.run).toHaveBeenCalledWith(
      { callId: 'call_1', name: 'ping', arguments: '{}' },
      expect.any(AbortSignal),
    );
    expect(host.continueWith).toHaveBeenCalledWith('s1', 'r1', [
      { type: 'function_call_output', call_id: 'call_1', output: 'pong' },
    ]);
    expect(runs.value.s1.phase).toBeUndefined();
    expect(runs.value.s1.messages[0].tools?.[0]).toMatchObject({
      status: 'done',
      resultStatus: 'success',
      result: 'pong',
    });
  });

  it('ignores responses it did not offer tools on, and unfinished ones', async () => {
    const { host, runner, complete } = setup();
    complete(finished('r0'));
    runner.recordOffer('r1');
    complete(finished('r1', 'cancelled'));
    runner.recordOffer('r2');
    complete(finished('r2', 'completed', []));
    await Promise.resolve();
    expect(host.bridge.run).not.toHaveBeenCalled();
    expect(host.continueWith).not.toHaveBeenCalled();
  });

  it('shows progress while running and can be stopped', async () => {
    let answer: (result: ClientToolResult) => void = () => undefined;
    const { host, runner, complete, runs, running } = setup(
      () => new Promise((resolve) => (answer = resolve)),
    );
    runner.recordOffer('r1');
    complete(finished('r1'));

    await vi.waitFor(() => expect(host.bridge.run).toHaveBeenCalled());
    expect(running.value).toBe(true);
    expect(runs.value.s1.phase).toBe('Running ping on iPhone…');
    expect(runs.value.s1.messages[0].tools?.[0].status).toBe('running');

    expect(runner.stop('s1')).toBe(true);
    answer({ ok: false, output: 'Error: Stopped by the user' });
    await vi.waitFor(() => expect(running.value).toBe(false));
    expect(runs.value.s1.messages[0].tools?.[0].status).toBe('cancelled');
    expect(host.continueWith).not.toHaveBeenCalled();
    expect(runner.stop('s1')).toBe(false);
  });

  it('stays busy until the continuation takes over, so Send and Stop never miss', async () => {
    const { host, runner, complete, running } = setup();
    let loaded: (current: boolean) => void = () => undefined;
    host.prepareContinuation.mockImplementation(
      () => new Promise<boolean>((resolve) => (loaded = resolve)),
    );
    const busyAtHandoff: boolean[] = [];
    host.continueWith.mockImplementation(async () => void busyAtHandoff.push(running.value));
    runner.recordOffer('r1');
    complete(finished('r1'));

    await vi.waitFor(() => expect(host.prepareContinuation).toHaveBeenCalledWith('s1', 'r1'));
    expect(running.value).toBe(true);
    loaded(true);
    await vi.waitFor(() => expect(host.continueWith).toHaveBeenCalled());
    // Cleared in the same task that hands over, never earlier.
    expect(busyAtHandoff).toEqual([false]);
  });

  it('abandons the results when stopped while the transcript loads', async () => {
    const { host, runner, complete, running } = setup();
    let loaded: (current: boolean) => void = () => undefined;
    host.prepareContinuation.mockImplementation(
      () => new Promise<boolean>((resolve) => (loaded = resolve)),
    );
    runner.recordOffer('r1');
    complete(finished('r1'));

    await vi.waitFor(() => expect(host.prepareContinuation).toHaveBeenCalled());
    expect(runner.stop('s1')).toBe(true);
    await vi.waitFor(() => expect(running.value).toBe(false));
    loaded(true);
    await Promise.resolve();
    expect(host.continueWith).not.toHaveBeenCalled();
    expect(host.toast).not.toHaveBeenCalled();
  });

  it('reports a transcript refresh failure instead of stalling', async () => {
    const { host, runner, complete, running } = setup();
    host.prepareContinuation.mockRejectedValue(new Error('502 Bad Gateway'));
    runner.recordOffer('r1');
    complete(finished('r1'));

    await vi.waitFor(() =>
      expect(host.toast).toHaveBeenCalledWith(
        'Could not send the iPhone tool results: 502 Bad Gateway',
      ),
    );
    expect(running.value).toBe(false);
    expect(host.continueWith).not.toHaveBeenCalled();
  });

  it('skips results that another response made stale', async () => {
    const { host, runner, complete } = setup();
    host.prepareContinuation.mockResolvedValue(false);
    runner.recordOffer('r1');
    complete(finished('r1'));
    await vi.waitFor(() => expect(host.prepareContinuation).toHaveBeenCalled());
    await Promise.resolve();
    expect(host.continueWith).not.toHaveBeenCalled();
  });

  it('abandons running tools and never continues once disposed', async () => {
    let seen: AbortSignal | undefined;
    const { host, runner, complete, running } = setup(
      (_call, signal) =>
        new Promise((resolve) => {
          seen = signal;
          signal.addEventListener('abort', () => resolve({ ok: false, output: 'Error: closed' }));
        }),
    );
    runner.recordOffer('r1');
    complete(finished('r1'));
    await vi.waitFor(() => expect(running.value).toBe(true));

    runner.dispose();
    expect(seen?.aborted).toBe(true);
    expect(running.value).toBe(false);
    runner.recordOffer('r2');
    complete(finished('r2'));
    await Promise.resolve();
    expect(host.bridge.run).toHaveBeenCalledTimes(1);
    expect(host.continueWith).not.toHaveBeenCalled();
  });

  it('does not touch a projection that moved on to another response', async () => {
    const { host, runner, runs } = setup();
    runner.recordOffer('r1');
    const projection = finished('r1');
    runs.value = { s1: finished('r2', 'streaming', []) };
    runner.finished('s1', projection);
    await vi.waitFor(() => expect(host.continueWith).toHaveBeenCalled());
    expect(runs.value.s1.run.responseId).toBe('r2');
    expect(runs.value.s1.phase).toBeUndefined();
  });

  it('stops looping after too many consecutive rounds until the user speaks', async () => {
    const { host, runner, complete } = setup();
    for (let round = 1; round <= MAX_CLIENT_TOOL_ROUNDS + 1; round += 1) {
      runner.recordOffer(`r${round}`);
      complete(finished(`r${round}`));
      await vi.waitFor(() =>
        expect(host.continueWith.mock.calls.length + host.toast.mock.calls.length).toBe(round),
      );
    }
    expect(host.continueWith).toHaveBeenCalledTimes(MAX_CLIENT_TOOL_ROUNDS);
    expect(host.toast).toHaveBeenCalledWith('Stopped after 8 rounds of iPhone tool calls.');

    runner.userTurn('s1');
    runner.recordOffer('next');
    complete(finished('next'));
    await vi.waitFor(() =>
      expect(host.continueWith).toHaveBeenCalledTimes(MAX_CLIENT_TOOL_ROUNDS + 1),
    );
  });
});
