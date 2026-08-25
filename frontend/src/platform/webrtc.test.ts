import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { installWebRTC } from './webrtc';

interface SentFrame {
  id: string;
  type?: string;
  method?: string;
  path?: string;
  body?: string;
}

class FakeDataChannel {
  readyState: RTCDataChannelState = 'open';
  throwOnSend = false;
  sent: SentFrame[] = [];
  onopen: ((event: Event) => unknown) | null = null;
  onclose: ((event: Event) => unknown) | null = null;
  onerror: ((event: Event) => unknown) | null = null;
  onmessage: ((event: MessageEvent<string>) => unknown) | null = null;

  send(data: string): void {
    if (this.throwOnSend) throw new Error('simulated channel send failure');
    this.sent.push(JSON.parse(data) as SentFrame);
  }

  close(): void {
    this.readyState = 'closed';
    this.onclose?.(new Event('close'));
  }

  receive(frame: Record<string, unknown>): void {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(frame) }));
  }
}

interface Harness {
  channels: FakeDataChannel[];
  cleanup(): void;
  fetch: ReturnType<typeof vi.fn>;
  apiCalls(): number;
  recoveries(): number;
  setSignalingOnline(value: boolean): void;
}

const flush = async (): Promise<void> => {
  for (let index = 0; index < 20; index += 1) await Promise.resolve();
};

async function enabledHarness(): Promise<Harness> {
  const channels: FakeDataChannel[] = [];
  let signalingOnline = true;
  let httpsAPICalls = 0;
  let transportRecoveries = 0;

  class FakePeerConnection {
    iceGatheringState: RTCIceGatheringState = 'complete';
    iceConnectionState: RTCIceConnectionState = 'connected';
    localDescription: RTCSessionDescriptionInit | null = null;
    oniceconnectionstatechange: (() => unknown) | null = null;
    onicecandidate: ((event: RTCPeerConnectionIceEvent) => unknown) | null = null;
    onicegatheringstatechange: (() => unknown) | null = null;

    createDataChannel(): RTCDataChannel {
      const channel = new FakeDataChannel();
      channels.push(channel);
      return channel as unknown as RTCDataChannel;
    }
    async createOffer(): Promise<RTCSessionDescriptionInit> { return { type: 'offer', sdp: 'fake-offer' }; }
    async setLocalDescription(offer: RTCSessionDescriptionInit): Promise<void> { this.localDescription = offer; }
    async setRemoteDescription(): Promise<void> { /* no-op */ }
    close(): void { /* no-op */ }
  }

  const fetch = vi.fn(async (input: RequestInfo | URL): Promise<Response> => {
    const url = input instanceof Request ? input.url : String(input);
    if (url.endsWith('/session')) {
      if (!signalingOnline) throw new TypeError('simulated signaling outage');
      return Response.json({ session_id: 'signal-session' });
    }
    if (url.includes('/signal?')) return Response.json({ type: 'answer', sdp: 'fake-answer' });
    if (url.endsWith('/signal')) return new Response(null, { status: 200 });
    if (url.includes('/v1/')) httpsAPICalls += 1;
    return Response.json({ sessions: [] });
  });

  vi.stubGlobal('RTCPeerConnection', FakePeerConnection);
  window.fetch = fetch as unknown as typeof window.fetch;
  window.__WEBRTC_ENABLED__ = true;
  window.__TERM_LLM_WEBRTC_TESTING__ = true;
  window.__WEBRTC_SIGNALING_URL__ = '/webrtc';
  window.TERM_LLM_UI_PREFIX = '/ui';
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: true });
  const recovery = (): void => { transportRecoveries += 1; };
  window.addEventListener('term-llm:transport-fallback', recovery);
  const uninstall = installWebRTC();
  await flush();
  expect(channels).toHaveLength(1);
  expect(channels[0].onclose).toBeTypeOf('function');
  expect(window.fetch).not.toBe(fetch);

  return {
    channels,
    fetch,
    apiCalls: () => httpsAPICalls,
    recoveries: () => transportRecoveries,
    setSignalingOnline: (value) => { signalingOnline = value; },
    cleanup: () => { uninstall(); window.removeEventListener('term-llm:transport-fallback', recovery); },
  };
}

let cleanupRTC: (() => void) | null = null;
let originalFetch: typeof window.fetch;

beforeEach(() => {
  vi.useFakeTimers();
  originalFetch = window.fetch;
});

afterEach(() => {
  cleanupRTC?.();
  cleanupRTC = null;
  window.fetch = originalFetch;
  delete window.__TERM_LLM_WEBRTC_TESTING__;
  delete window.__TERM_LLM_WEBRTC_TEST_HOOKS__;
  delete window.__WEBRTC_ENABLED__;
  delete window.__WEBRTC_SIGNALING_URL__;
  delete window.TERM_LLM_UI_PREFIX;
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('WebRTC platform bridge', () => {
  it('uses a 1 second first-frame timeout for reads and 5 seconds for mutations', () => {
    window.__TERM_LLM_WEBRTC_TESTING__ = true;
    window.__WEBRTC_ENABLED__ = false;
    installWebRTC();
    const hooks = window.__TERM_LLM_WEBRTC_TEST_HOOKS__ as { responseTimeoutForMethod(method: string): number };
    expect(['GET', 'get', 'HEAD', 'OPTIONS'].map(hooks.responseTimeoutForMethod)).toEqual([1_000, 1_000, 1_000, 1_000]);
    expect(['POST', 'PATCH', 'PUT', 'DELETE'].map(hooks.responseTimeoutForMethod)).toEqual([5_000, 5_000, 5_000, 5_000]);
  });

  it('restores HTTPS and emits recovery exactly once when the channel closes', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    const patched = window.fetch; const channel = harness.channels[0];
    channel.close();
    expect(window.fetch).not.toBe(patched);
    expect(harness.recoveries()).toBe(1);
    channel.onclose?.(new Event('close')); channel.onerror?.(new Event('error'));
    expect(harness.recoveries()).toBe(1);
  });

  it('restores HTTPS and emits recovery once on a synchronous request fallback', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    const channel = harness.channels[0]; channel.throwOnSend = true;
    const response = await window.fetch('/ui/v1/sessions/status');
    expect(response.ok).toBe(true);
    expect(harness.apiCalls()).toBe(1);
    expect(harness.recoveries()).toBe(1);
    channel.onclose?.(new Event('close'));
    expect(harness.recoveries()).toBe(1);
  });

  it('safely falls back for a mutation when dataChannel.send proves non-delivery', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    harness.channels[0].throwOnSend = true;
    await expect(window.fetch('/ui/v1/non-idempotent-action', { method: 'POST', body: '{}' })).resolves.toMatchObject({ ok: true });
    expect(harness.apiCalls()).toBe(1);
  });

  it('routes a 100 KiB body over WebRTC and larger bodies over HTTPS without degradation', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    const channel = harness.channels[0]; const limit = 100 * 1024;
    const atLimit = window.fetch('/ui/v1/at-limit', { method: 'POST', body: 'a'.repeat(limit), __termLLMRetrySafe: true } as RequestInit);
    const request = channel.sent.find((frame) => frame.path === '/ui/v1/at-limit');
    expect(request?.id).toBeTruthy(); channel.receive({ id: request?.id, type: 'done', status: 200 });
    await expect(atLimit).resolves.toMatchObject({ ok: true });
    await expect(window.fetch('/ui/v1/oversized', { method: 'POST', body: 'a'.repeat(limit + 1) })).resolves.toMatchObject({ ok: true });
    expect(channel.sent.some((frame) => frame.path === '/ui/v1/oversized')).toBe(false);
    expect(harness.apiCalls()).toBe(1); expect(harness.recoveries()).toBe(0); expect(channel.readyState).toBe('open');
  });

  it('never replays an unsafe mutation whose first-frame outcome is ambiguous', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    const pending = window.fetch('/ui/v1/non-idempotent-action', { method: 'POST', body: '{}' });
    harness.channels[0].close();
    await expect(pending).rejects.toMatchObject({ name: 'UnknownMutationOutcomeError' });
    expect(harness.apiCalls()).toBe(0);
  });

  it('keeps HTTPS alive through an outage and immediately retries signaling when online', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    harness.setSignalingOnline(false); harness.channels[0].close();
    await expect(window.fetch('/ui/v1/sessions/status')).resolves.toMatchObject({ ok: true });
    await vi.advanceTimersByTimeAsync(20_000); await flush();
    expect(harness.channels).toHaveLength(1); expect(harness.apiCalls()).toBe(1);
    harness.setSignalingOnline(true); window.dispatchEvent(new Event('online'));
    await vi.advanceTimersByTimeAsync(0); await flush();
    expect(harness.channels).toHaveLength(2); expect(window.fetch).not.toBe(harness.fetch);
  });

  it('preserves one armed signaling backoff through visibility churn', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    harness.setSignalingOnline(false); harness.channels[0].close();
    for (let index = 0; index < 8; index += 1) document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(1_999); await flush();
    expect(harness.channels).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1); await flush();
    expect(harness.channels).toHaveLength(1);
  });

  it('cancels an abandoned peer response exactly once but not a completed request', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    const pending = window.fetch('/ui/v1/responses/resp-1/events'); const request = channel.sent.find((frame) => frame.path?.includes('/events'))!;
    channel.receive({ id: request.id, type: 'headers', status: 200, headers: {} });
    const response = await pending; await response.body?.cancel();
    expect(channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === request.id)).toHaveLength(1);
    const completed = window.fetch('/ui/v1/sessions/status'); const completeRequest = channel.sent.find((frame) => frame.path === '/ui/v1/sessions/status')!;
    channel.receive({ id: completeRequest.id, type: 'done', status: 200 }); await completed;
    expect(channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === completeRequest.id)).toHaveLength(0);
  });

  it('does not cancel or replay unanswered mutation work after its timeout', async () => {
    const harness = await enabledHarness(); cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    const pending = window.fetch('/ui/v1/worktrees', { method: 'POST', body: '{}' }).then(
      () => null,
      (error: unknown) => error,
    );
    const request = channel.sent.find((frame) => frame.path === '/ui/v1/worktrees')!;
    await vi.advanceTimersByTimeAsync(5_000); await flush();
    expect(await pending).toMatchObject({ name: 'UnknownMutationOutcomeError' });
    expect(channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === request.id)).toHaveLength(0);
    expect(harness.apiCalls()).toBe(0);
  });
});
