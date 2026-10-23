import { afterEach, describe, expect, it, vi } from 'vitest';
import { NotificationOwner } from './notification-owner';
import type { PresenceSource } from './presence';
import type { TermLLMNativePush } from './push-platform';

const owners: NotificationOwner[] = [];
afterEach(async () => {
  for (const owner of owners.splice(0)) {
    owner.dispose();
    await owner.publish();
  }
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function fixture(
  statuses: Array<string | { status: string; previous_document_id?: string }> = ['bound'],
) {
  const sent: Array<{ window_id: string; document_id: string }> = [];
  class Channel {
    port1 = { onmessage: null as null | ((event: { data: unknown }) => void), close: vi.fn() };
    port2 = { reply: (data: unknown) => this.port1.onmessage?.({ data }) };
  }
  vi.stubGlobal('MessageChannel', Channel);
  const registration = {
    active: {
      postMessage: vi.fn((message, ports) => {
        sent.push(message);
        const result = statuses.shift() || 'bound';
        ports[0].reply(typeof result === 'string' ? { status: result } : result);
      }),
    },
  };
  Object.defineProperty(navigator, 'serviceWorker', {
    configurable: true,
    value: {
      getRegistration: vi.fn(async () => registration),
    },
  });
  const endpoints = { pushPresence: vi.fn(async (_body: unknown) => ({ ok: true, version: 2 })) };
  const owner = new NotificationOwner('/node/Dev', endpoints as never, () => 'sub');
  owners.push(owner);
  return { owner, endpoints, sent, registration };
}

function presenceFixture() {
  let last = 0;
  let focus = true;
  const listeners = new Set<() => void>();
  const source: PresenceSource = {
    lastPresenceAt: () => last,
    focused: () => focus,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
  return {
    source,
    signal: (at: number, focused = true) => {
      last = at;
      focus = focused;
      for (const listener of listeners) listener();
    },
  };
}

describe('notification document ownership and attendance', () => {
  it('uses a reload-stable window, fresh document and increasing persisted window sequence', async () => {
    const first = fixture();
    const one = await first.owner.origin();
    const two = fixture();
    const reloaded = await two.owner.origin();
    expect(reloaded?.window_id).toBe(one?.window_id);
    expect(reloaded?.document_id).not.toBe(one?.document_id);
    const firstLease = first.endpoints.pushPresence.mock.calls[0][0] as { sequence: number };
    const nextLease = two.endpoints.pushPresence.mock.calls[0][0] as { sequence: number };
    expect(nextLease.sequence).toBeGreaterThan(firstLease.sequence);
    expect(nextLease).toMatchObject({ previous_document_id: one?.document_id });
    await two.owner.publish();
    expect(two.endpoints.pushPresence.mock.lastCall![0]).not.toHaveProperty('previous_document_id');
    expect(nextLease).toMatchObject({
      version: 2,
      subscription_id: 'sub',
      source: '/node/Dev',
      active: false,
    });
  });

  it('retains the acknowledged predecessor across a failed HTTP bind and another reload', async () => {
    const first = fixture();
    const origin = await first.owner.origin();
    const second = fixture([{ status: 'bound', previous_document_id: origin!.document_id }]);
    second.endpoints.pushPresence.mockRejectedValueOnce(new Error('offline'));
    expect(await second.owner.origin()).toBeNull();
    const third = fixture([{ status: 'bound', previous_document_id: second.sent[0].document_id }]);
    await third.owner.origin();
    expect(third.endpoints.pushPresence).toHaveBeenLastCalledWith(
      expect.objectContaining({
        previous_document_id: origin!.document_id,
      }),
    );
  });

  it('resolves cloned sessionStorage before emitting any eligible admission identity', async () => {
    sessionStorage.setItem('term_llm_notification_window:/node/Dev', 'cloned-owner');
    const { owner, endpoints, sent } = fixture(['collision', 'bound']);
    const origin = await owner.origin();
    expect(sent[0].window_id).toBe('cloned-owner');
    expect(origin?.window_id).not.toBe('cloned-owner');
    expect(sent[1].window_id).toBe(origin?.window_id);
    expect(endpoints.pushPresence).toHaveBeenCalledOnce();
    expect(sessionStorage.getItem('term_llm_notification_window:/node/Dev')).toBe(
      origin?.window_id,
    );
  });

  it('visible-only is inactive; real attendance leases expire and never acknowledge reads', async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
    const { owner, endpoints, registration } = fixture();
    const presence = presenceFixture();
    owner.start(presence.source);
    await owner.publish();
    expect(endpoints.pushPresence).toHaveBeenLastCalledWith(
      expect.objectContaining({ active: false }),
    );
    presence.signal(Date.now());
    await owner.publish();
    expect(endpoints.pushPresence).toHaveBeenLastCalledWith(
      expect.objectContaining({ active: true }),
    );
    const active = endpoints.pushPresence.mock.lastCall![0] as { expires_at: string };
    expect(Date.parse(active.expires_at) - Date.now()).toBeLessThanOrEqual(65_000);
    // The owner lease has no selected-session dependency: another conversation is still around.
    await vi.advanceTimersByTimeAsync(60_001);
    await owner.publish();
    expect(endpoints.pushPresence).toHaveBeenLastCalledWith(
      expect.objectContaining({ active: false }),
    );
    expect(
      registration.active.postMessage.mock.calls.every(
        ([message]) => message.type === 'notification-bind',
      ),
    ).toBe(true);
  });

  it('coalesces interaction bursts while HTTP publication is pending and publishes inactive afterward', async () => {
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
    const { owner, endpoints } = fixture();
    const presence = presenceFixture();
    let release!: () => void;
    endpoints.pushPresence.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = () => resolve({ ok: true, version: 2 });
        }),
    );
    presence.signal(Date.now());
    owner.start(presence.source);
    await vi.waitFor(() => expect(endpoints.pushPresence).toHaveBeenCalledOnce());
    for (let i = 0; i < 50; i++) presence.signal(Date.now());
    presence.signal(Date.now(), false);
    release();
    await owner.publish();
    await owner.publish();
    expect(endpoints.pushPresence.mock.calls.length).toBeLessThanOrEqual(3);
    expect(endpoints.pushPresence).toHaveBeenLastCalledWith(
      expect.objectContaining({ active: false }),
    );
  });

  it('fails closed for denied binding or failed authenticated presence, not resampling a destination', async () => {
    const { owner, endpoints } = fixture(['unavailable']);
    expect(await owner.origin()).toBeNull();
    expect(endpoints.pushPresence).not.toHaveBeenCalled();
    endpoints.pushPresence.mockRejectedValueOnce(new Error('401'));
    expect(await owner.origin()).toBeNull();
  });

  it('uses the native-injected document and exact source, and tolerates old capability failure', async () => {
    const endpoints = { pushPresence: vi.fn(async () => ({})) };
    const bridge = {
      version: 1,
      notificationCapabilities: vi.fn(async () => ({
        version: 2,
        window_id: 'scene',
        document_id: 'native-doc',
      })),
      notificationPresence: vi.fn(async () => {}),
      notificationRead: vi.fn(async () => {}),
    } as unknown as TermLLMNativePush;
    const owner = new NotificationOwner(
      '/node/Dev',
      endpoints as never,
      () => 'native-sub',
      bridge,
    );
    owners.push(owner);
    expect(await owner.origin()).toMatchObject({ window_id: 'scene', document_id: 'native-doc' });
    expect(bridge.notificationPresence).toHaveBeenCalledWith(
      expect.objectContaining({ source: '/node/Dev', document_id: 'native-doc' }),
    );
    expect(endpoints.pushPresence).toHaveBeenCalledWith(
      expect.objectContaining({ source: '/node/Dev', document_id: 'native-doc' }),
    );
    const reloadedBridge = {
      ...bridge,
      notificationCapabilities: vi.fn(async () => ({
        version: 2 as const,
        window_id: 'scene',
        document_id: 'next-native-doc',
      })),
    };
    const reloaded = new NotificationOwner(
      '/node/Dev',
      endpoints as never,
      () => 'native-sub',
      reloadedBridge,
    );
    owners.push(reloaded);
    await reloaded.origin();
    expect(endpoints.pushPresence).toHaveBeenLastCalledWith(
      expect.objectContaining({
        document_id: 'next-native-doc',
        previous_document_id: 'native-doc',
      }),
    );
    expect(bridge.notificationPresence).toHaveBeenLastCalledWith(
      expect.not.objectContaining({
        previous_document_id: expect.anything(),
      }),
    );
    const old = new NotificationOwner('/node/Dev', endpoints as never, () => 'native-sub', {
      version: 1,
    } as TermLLMNativePush);
    owners.push(old);
    expect(await old.origin()).toBeNull();
  });
});
