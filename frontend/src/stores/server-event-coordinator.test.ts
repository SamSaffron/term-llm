import { signal } from '@preact/signals';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AppStoreServices } from './app-store-services';
import { ServerEventCoordinator, type ServerEventHost } from './server-event-coordinator';

function harness() {
  const startupDone = signal(false);
  const activeSessionId = signal('s1');
  const calls = {
    catalog: vi.fn(async () => undefined),
    status: vi.fn(async () => undefined),
    active: vi.fn(async () => undefined),
    files: vi.fn(async () => undefined),
    children: vi.fn(async () => undefined),
    recovery: vi.fn(async () => undefined),
    health: vi.fn(),
  };
  const host: ServerEventHost = {
    startupDone,
    activeSessionId,
    reconcileCatalog: calls.catalog,
    reconcileStatus: calls.status,
    reconcileActiveSession: calls.active,
    reconcileFiles: calls.files,
    reconcileChildren: calls.children,
    authoritativeRecovery: calls.recovery,
    eventFeedHealthChanged: calls.health,
  };
  const diagnostics: string[] = [];
  const endpoints = {
    serverEventStream: vi.fn(
      async (_after: number | null, _channels: string[], signal: AbortSignal) =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              signal.addEventListener(
                'abort',
                () => controller.error(signal.reason || new DOMException('Aborted', 'AbortError')),
                { once: true },
              );
            },
          }),
          { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
        ),
    ),
    serverEventPoll: vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            object: 'list',
            instance_id: 'evt_test',
            data: [],
            latest_sequence: 4,
            next_after: 4,
            timed_out: false,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      )
      .mockImplementation(
        async (_after: number | null, _channels: string[], signal: AbortSignal) =>
          new Promise<Response>((_resolve, reject) => {
            signal.addEventListener('abort', () => reject(signal.reason), { once: true });
          }),
      ),
  };
  const eventFeedHealthy = signal(false);
  const services = {
    endpoints,
    isDisposed: false,
    eventFeedHealthy,
    setEventFeedHealthy: (healthy: boolean) => {
      eventFeedHealthy.value = healthy;
    },
    bumpDiagnostic: (key: string) => diagnostics.push(key),
  } as unknown as AppStoreServices;
  return {
    coordinator: new ServerEventCoordinator(services, host),
    services,
    endpoints,
    diagnostics,
    calls,
  };
}

afterEach(() => vi.useRealTimers());

describe('ServerEventCoordinator', () => {
  it('reconnects SSE directly when interests change without reporting a poll fallback', async () => {
    const { coordinator, services, endpoints, diagnostics } = harness();
    const ready = new TextEncoder().encode(
      `event: ready\ndata: ${JSON.stringify({
        v: 1,
        instance_id: 'evt_test',
        latest_sequence: 0,
        heartbeat_ms: 10_000,
        replay_limit: 2_048,
      })}\n\n`,
    );
    endpoints.serverEventStream.mockImplementation(
      async (_after: number | null, _channels: string[], signal: AbortSignal) =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(ready);
              signal.addEventListener(
                'abort',
                () => controller.error(signal.reason || new DOMException('Aborted', 'AbortError')),
                { once: true },
              );
            },
          }),
          { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
        ),
    );

    coordinator.updateInterest('s1');
    await coordinator.prepare();
    expect(coordinator.isHealthy()).toBe(true);
    expect(services.eventFeedHealthy.value).toBe(true);
    coordinator.updateInterest('s2');
    await vi.waitFor(() => expect(endpoints.serverEventStream).toHaveBeenCalledTimes(2));
    expect(endpoints.serverEventPoll).not.toHaveBeenCalled();
    expect(diagnostics).not.toContain('serverEventPollFallbacks');
    coordinator.dispose();
    expect(services.eventFeedHealthy.value).toBe(false);
  });

  it('releases the feed while parked and resumes from its cursor without a failure', async () => {
    const { coordinator, services, endpoints, diagnostics } = harness();
    const encoder = new TextEncoder();
    endpoints.serverEventStream.mockImplementation(
      async (_after: number | null, _channels: string[], signal: AbortSignal) =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(
                encoder.encode(
                  `event: ready\ndata: ${JSON.stringify({
                    v: 1,
                    instance_id: 'evt_test',
                    latest_sequence: 7,
                    heartbeat_ms: 10_000,
                    replay_limit: 2_048,
                  })}\n\n`,
                ),
              );
              signal.addEventListener(
                'abort',
                () => controller.error(signal.reason || new DOMException('Aborted', 'AbortError')),
                { once: true },
              );
            },
          }),
          { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
        ),
    );

    await coordinator.prepare();
    expect(services.eventFeedHealthy.value).toBe(true);
    const firstSignal = endpoints.serverEventStream.mock.calls[0][2] as AbortSignal;

    coordinator.park();
    expect(firstSignal.aborted).toBe(true);
    expect(coordinator.parked).toBe(true);
    expect(services.eventFeedHealthy.value).toBe(false);
    // A parked feed stays closed rather than reconnecting or falling back to poll.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(endpoints.serverEventStream).toHaveBeenCalledOnce();
    expect(endpoints.serverEventPoll).not.toHaveBeenCalled();

    coordinator.resume();
    await vi.waitFor(() => expect(endpoints.serverEventStream).toHaveBeenCalledTimes(2));
    expect(endpoints.serverEventStream.mock.calls[1][0]).toBe(7);
    await vi.waitFor(() => expect(services.eventFeedHealthy.value).toBe(true));
    expect(diagnostics).not.toContain('serverEventReconnects');
    expect(diagnostics).not.toContain('serverEventPollFallbacks');
    coordinator.dispose();
  });

  it('keeps a connection attempt closed when parked mid-connect, and a quick show is not a failure', async () => {
    const { coordinator, endpoints, diagnostics } = harness();
    void coordinator.prepare();
    expect(endpoints.serverEventStream).toHaveBeenCalledOnce();
    coordinator.park();
    expect((endpoints.serverEventStream.mock.calls[0][2] as AbortSignal).aborted).toBe(true);
    // The tab becomes visible again before the abort reaches the loop; the
    // visibility handler restarts right after resuming.
    coordinator.resume();
    coordinator.restart();
    await vi.waitFor(() => expect(endpoints.serverEventStream).toHaveBeenCalledTimes(2));
    expect(endpoints.serverEventPoll).not.toHaveBeenCalled();
    expect(diagnostics).not.toContain('serverEventPollFallbacks');

    coordinator.park();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(endpoints.serverEventStream).toHaveBeenCalledTimes(2);
    coordinator.dispose();
  });

  it('stops long polling when parked while a poll result is being applied', async () => {
    const { coordinator, endpoints, calls } = harness();
    endpoints.serverEventStream.mockRejectedValue(new TypeError('network down'));
    (coordinator as unknown as { instanceId: string }).instanceId = 'evt_previous';
    let finishRecovery!: () => void;
    calls.recovery.mockImplementationOnce(
      () =>
        new Promise<undefined>((resolve) => {
          finishRecovery = () => resolve(undefined);
        }),
    );

    void coordinator.prepare();
    await vi.waitFor(() => expect(calls.recovery).toHaveBeenCalledOnce());
    coordinator.park();
    finishRecovery();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(endpoints.serverEventPoll).toHaveBeenCalledOnce();
    coordinator.dispose();
  });

  it('runs authoritative recovery for a snapshot-required event', async () => {
    vi.useFakeTimers();
    const { coordinator, calls } = harness();
    const internals = coordinator as unknown as {
      route(event: {
        v: 1;
        sequence: number;
        instanceId: string;
        type: 'snapshot.required';
        occurredAt: number;
        reason: string;
      }): void;
    };
    internals.route({
      v: 1,
      sequence: 5,
      instanceId: 'evt_test',
      type: 'snapshot.required',
      occurredAt: 1,
      reason: 'store_cursor_gap',
    });
    await vi.advanceTimersByTimeAsync(100);
    expect(calls.recovery).toHaveBeenCalledWith('store_cursor_gap');
    expect(calls.catalog).not.toHaveBeenCalled();
    coordinator.dispose();
  });

  it('refreshes the active parent projection for children.changed', async () => {
    vi.useFakeTimers();
    const { coordinator, calls } = harness();
    const internals = coordinator as unknown as {
      route(event: {
        v: 1;
        sequence: number;
        instanceId: string;
        type: 'children.changed';
        occurredAt: number;
        parentSessionId: string;
      }): void;
    };
    internals.route({
      v: 1,
      sequence: 6,
      instanceId: 'evt_test',
      type: 'children.changed',
      occurredAt: 1,
      parentSessionId: 'parent-1',
    });
    await vi.advanceTimersByTimeAsync(100);
    expect(calls.children).toHaveBeenCalledWith('parent-1');
    coordinator.dispose();
  });

  it('falls back to long poll when an SSE connection never flushes ready bytes', async () => {
    vi.useFakeTimers();
    const { coordinator, endpoints, diagnostics } = harness();
    coordinator.updateInterest('s1', 's1');
    const prepared = coordinator.prepare();

    await vi.advanceTimersByTimeAsync(2_100);
    await prepared;
    await vi.advanceTimersByTimeAsync(7_000);
    await Promise.resolve();
    await Promise.resolve();
    expect(endpoints.serverEventPoll).toHaveBeenCalled();

    expect(diagnostics).toContain('serverEventSSEJams');
    expect(diagnostics).toContain('serverEventPollFallbacks');
    expect(endpoints.serverEventStream.mock.calls[0]?.[1]).toEqual([
      'session:s1',
      'files:s1',
      'children:s1',
    ]);
    coordinator.dispose();
  });
});
