import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { installWebRTC, withDeadline } from './webrtc';

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

type SignalingStage = 'session' | 'offer' | 'answer';
type NegotiationFault = SignalingStage | 'connect' | 'rejected';

interface Harness {
  channels: FakeDataChannel[];
  signalingRequests: { stage: SignalingStage; signal: AbortSignal }[];
  setFault(fault?: NegotiationFault): void;
  cleanup(): void;
  fetch: ReturnType<typeof vi.fn>;
  apiCalls(): number;
  recoveries(): number;
  setSignalingOnline(value: boolean): void;
}

const flush = async (): Promise<void> => {
  for (let index = 0; index < 20; index += 1) await Promise.resolve();
};

async function enabledHarness(fault?: NegotiationFault): Promise<Harness> {
  const channels: FakeDataChannel[] = [];
  const signalingRequests: Harness['signalingRequests'] = [];
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

    channel: FakeDataChannel | null = null;

    createDataChannel(): RTCDataChannel {
      const channel = new FakeDataChannel();
      channel.readyState = 'connecting';
      this.channel = channel;
      channels.push(channel);
      return channel as unknown as RTCDataChannel;
    }
    async createOffer(): Promise<RTCSessionDescriptionInit> {
      return { type: 'offer', sdp: 'fake-offer' };
    }
    async setLocalDescription(offer: RTCSessionDescriptionInit): Promise<void> {
      this.localDescription = offer;
    }
    async setRemoteDescription(): Promise<void> {
      if (this.channel && fault !== 'connect') this.channel.readyState = 'open';
    }
    close(): void {
      this.channel?.close();
    }
  }

  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = input instanceof Request ? input.url : String(input);
    const stage = url.endsWith('/session')
      ? 'session'
      : url.includes('/signal?')
        ? 'answer'
        : url.endsWith('/signal')
          ? 'offer'
          : null;
    if (stage) {
      const signal = init?.signal;
      if (!signal) throw new Error('Signaling requests must have an owned deadline');
      signalingRequests.push({ stage, signal });
      if (fault === stage) {
        // Model fetch cancellation, not a promise that ignores its AbortSignal.
        return new Promise<Response>((_resolve, reject) => {
          if (signal.aborted) reject(signal.reason);
          else signal.addEventListener('abort', () => reject(signal.reason), { once: true });
        });
      }
    }
    if (stage === 'session') {
      if (!signalingOnline) throw new TypeError('simulated signaling outage');
      return Response.json({ session_id: 'signal-session' });
    }
    if (stage === 'answer')
      return Response.json(
        fault === 'rejected'
          ? { type: 'rejected', reason: 'capacity' }
          : { type: 'answer', sdp: 'fake-answer' },
      );
    if (stage === 'offer') return new Response(null, { status: 200 });
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
  const recovery = (): void => {
    transportRecoveries += 1;
  };
  window.addEventListener('term-llm:transport-fallback', recovery);
  const uninstall = installWebRTC();
  await flush();
  if (!fault) {
    expect(channels).toHaveLength(1);
    expect(channels[0].onclose).toBeTypeOf('function');
    expect(window.fetch).not.toBe(fetch);
  }

  return {
    channels,
    signalingRequests,
    setFault: (value) => {
      fault = value;
    },
    fetch,
    apiCalls: () => httpsAPICalls,
    recoveries: () => transportRecoveries,
    setSignalingOnline: (value) => {
      signalingOnline = value;
    },
    cleanup: () => {
      uninstall();
      window.removeEventListener('term-llm:transport-fallback', recovery);
    },
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
  // These replace the browser-to-Go ICE smoke assertions: exercise the real
  // negotiation owner, but control signaling, peer state, and time explicitly.
  it.each(['session', 'offer', 'answer', 'connect'] as const)(
    'keeps HTTPS usable through a hung %s and recovers after its deadline',
    async (stage) => {
      const harness = await enabledHarness(stage);
      cleanupRTC = harness.cleanup;
      const request = harness.signalingRequests.find((request) => request.stage === stage);
      if (stage !== 'connect') expect(request?.signal.aborted).toBe(false);
      const failedChannels = [...harness.channels];

      await expect(window.fetch('/ui/v1/sessions/status')).resolves.toMatchObject({ ok: true });
      expect(harness.apiCalls()).toBe(1);
      await vi.advanceTimersByTimeAsync(7_999);
      expect(harness.signalingRequests.filter(({ stage }) => stage === 'session')).toHaveLength(1);
      if (stage !== 'connect') expect(request?.signal.aborted).toBe(false);
      else expect(failedChannels[0].readyState).toBe('connecting');

      await vi.advanceTimersByTimeAsync(1);
      if (stage !== 'connect') {
        expect(request?.signal.aborted).toBe(true);
        expect(request?.signal.reason).toMatchObject({ name: 'TimeoutError' });
      }
      for (const channel of failedChannels) expect(channel.readyState).toBe('closed');
      expect(vi.getTimerCount()).toBe(1); // Only the owned retry remains.
      await expect(window.fetch('/ui/v1/sessions/status')).resolves.toMatchObject({ ok: true });
      expect(harness.apiCalls()).toBe(2);

      harness.setFault();
      await vi.advanceTimersByTimeAsync(4_999);
      expect(harness.signalingRequests.filter(({ stage }) => stage === 'session')).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(harness.signalingRequests.filter(({ stage }) => stage === 'session')).toHaveLength(2);
      const channel = harness.channels.at(-1)!;
      expect(channel.readyState).toBe('open');
      expect(vi.getTimerCount()).toBe(0);

      // Prove fetch actually uses the recovered channel, not merely that a
      // peer was allocated (or fetch was rebound to the original HTTPS fetch).
      const response = window.fetch('/ui/v1/sessions/status');
      const frame = channel.sent.at(-1)!;
      expect(frame.path).toBe('/ui/v1/sessions/status');
      channel.receive({ id: frame.id, type: 'done', status: 200 });
      await expect(response).resolves.toMatchObject({ ok: true });
      expect(harness.apiCalls()).toBe(2);
    },
  );

  it('cleans up an offer timeout and admission rejection before a later generation recovers', async () => {
    const harness = await enabledHarness('offer');
    cleanupRTC = harness.cleanup;
    await vi.advanceTimersByTimeAsync(8_000);
    expect(harness.channels[0].readyState).toBe('closed');

    harness.setFault('rejected');
    await vi.advanceTimersByTimeAsync(5_000);
    expect(harness.channels).toHaveLength(2);
    expect(harness.channels[1].readyState).toBe('closed');
    expect(vi.getTimerCount()).toBe(1);
    await expect(window.fetch('/ui/v1/sessions/status')).resolves.toMatchObject({ ok: true });
    expect(harness.apiCalls()).toBe(1);

    harness.setFault();
    await vi.advanceTimersByTimeAsync(9_999);
    expect(harness.channels).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(harness.channels).toHaveLength(3);
    expect(vi.getTimerCount()).toBe(0);
    const channel = harness.channels[2];
    const response = window.fetch('/ui/v1/sessions/status');
    const frame = channel.sent.at(-1)!;
    expect(frame.path).toBe('/ui/v1/sessions/status');
    channel.receive({ id: frame.id, type: 'done', status: 200 });
    await expect(response).resolves.toMatchObject({ ok: true });
    expect(harness.apiCalls()).toBe(1);
    // Dead peers cannot tear down the recovered generation.
    harness.channels[0].close();
    harness.channels[1].close();
    expect(channel.readyState).toBe('open');
    expect(harness.recoveries()).toBe(0);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('uses a 1 second first-frame timeout for reads and 5 seconds for mutations', () => {
    window.__TERM_LLM_WEBRTC_TESTING__ = true;
    window.__WEBRTC_ENABLED__ = false;
    installWebRTC();
    const hooks = window.__TERM_LLM_WEBRTC_TEST_HOOKS__ as {
      responseTimeoutForMethod(method: string): number;
    };
    expect(['GET', 'get', 'HEAD', 'OPTIONS'].map(hooks.responseTimeoutForMethod)).toEqual([
      1_000, 1_000, 1_000, 1_000,
    ]);
    expect(['POST', 'PATCH', 'PUT', 'DELETE'].map(hooks.responseTimeoutForMethod)).toEqual([
      5_000, 5_000, 5_000, 5_000,
    ]);
  });

  it('restores HTTPS and emits recovery exactly once when the channel closes', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const patched = window.fetch;
    const channel = harness.channels[0];
    channel.close();
    expect(window.fetch).not.toBe(patched);
    expect(harness.recoveries()).toBe(1);
    channel.onclose?.(new Event('close'));
    channel.onerror?.(new Event('error'));
    expect(harness.recoveries()).toBe(1);
  });

  it('restores HTTPS and emits recovery once on a synchronous request fallback', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    channel.throwOnSend = true;
    const response = await window.fetch('/ui/v1/sessions/status');
    expect(response.ok).toBe(true);
    expect(harness.apiCalls()).toBe(1);
    expect(harness.recoveries()).toBe(1);
    channel.onclose?.(new Event('close'));
    expect(harness.recoveries()).toBe(1);
  });

  it('rejects an unsafe mutation when dataChannel.send has no application acknowledgement', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    harness.channels[0].throwOnSend = true;
    await expect(
      window.fetch('/ui/v1/non-idempotent-action', { method: 'POST', body: '{}' }),
    ).rejects.toMatchObject({ name: 'UnknownMutationOutcomeError' });
    expect(harness.apiCalls()).toBe(0);
  });

  it('replays a keyed idempotent mutation after send failure without changing its key', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    harness.channels[0].throwOnSend = true;
    const headers = { 'Idempotency-Key': 'same-operation-key' };
    await expect(
      window.fetch('/ui/v1/responses', {
        method: 'POST',
        body: '{}',
        headers,
        __termLLMRetrySafe: true,
      } as RequestInit),
    ).resolves.toMatchObject({ ok: true });
    expect(harness.apiCalls()).toBe(1);
    expect(harness.fetch).toHaveBeenLastCalledWith(
      '/ui/v1/responses',
      expect.objectContaining({ headers }),
    );
  });

  it('settles throwing header and done callbacks through HTTPS replay', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];

    const headerFailure = window.fetch('/ui/v1/header-callback');
    const headerRequest = channel.sent.find((frame) => frame.path === '/ui/v1/header-callback')!;
    channel.receive({ id: headerRequest.id, type: 'headers', status: 101, headers: {} });
    await expect(headerFailure).resolves.toMatchObject({ ok: true });

    // The failed callback degrades the channel, so reinstall a fresh harness to
    // exercise an independently throwing terminal callback.
    harness.cleanup();
    cleanupRTC = null;
    const second = await enabledHarness();
    cleanupRTC = second.cleanup;
    const doneFailure = window.fetch('/ui/v1/done-callback');
    const doneRequest = second.channels[0].sent.find(
      (frame) => frame.path === '/ui/v1/done-callback',
    )!;
    second.channels[0].receive({ id: doneRequest.id, type: 'done', status: 101 });
    await expect(doneFailure).resolves.toMatchObject({ ok: true });
    expect(second.apiCalls()).toBe(1);
  });

  it('routes a 100 KiB body over WebRTC and larger bodies over HTTPS without degradation', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    const limit = 100 * 1024;
    const atLimit = window.fetch('/ui/v1/at-limit', {
      method: 'POST',
      body: 'a'.repeat(limit),
      __termLLMRetrySafe: true,
    } as RequestInit);
    const request = channel.sent.find((frame) => frame.path === '/ui/v1/at-limit');
    expect(request?.id).toBeTruthy();
    channel.receive({ id: request?.id, type: 'done', status: 200 });
    await expect(atLimit).resolves.toMatchObject({ ok: true });
    await expect(
      window.fetch('/ui/v1/oversized', { method: 'POST', body: 'a'.repeat(limit + 1) }),
    ).resolves.toMatchObject({ ok: true });
    expect(channel.sent.some((frame) => frame.path === '/ui/v1/oversized')).toBe(false);
    expect(harness.apiCalls()).toBe(1);
    expect(harness.recoveries()).toBe(0);
    expect(channel.readyState).toBe('open');
  });

  it('keeps same-path foreign-origin requests on HTTPS', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    await expect(
      window.fetch('https://elsewhere.test/ui/v1/sessions/status'),
    ).resolves.toMatchObject({ ok: true });
    expect(channel.sent.some((frame) => frame.path === '/ui/v1/sessions/status')).toBe(false);
    expect(harness.apiCalls()).toBe(1);
  });

  it('degrades the transport when delivering a rendered response chunk fails', async () => {
    const NativeTextEncoder = TextEncoder;
    vi.stubGlobal(
      'TextEncoder',
      class extends NativeTextEncoder {
        override encode(_input?: string): Uint8Array<ArrayBuffer> {
          throw new Error('simulated render delivery failure');
        }
      },
    );
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    const patched = window.fetch;
    const pending = window.fetch('/ui/v1/responses/r1/events');
    const request = channel.sent.find((frame) => frame.path === '/ui/v1/responses/r1/events')!;
    channel.receive({ id: request.id, type: 'headers', status: 200, headers: {} });
    const response = await pending;
    channel.receive({ id: request.id, type: 'chunk', data: 'data: rendered\n\n' });
    await expect(response.text()).rejects.toThrow('simulated render delivery failure');
    expect(harness.recoveries()).toBe(1);
    expect(window.fetch).not.toBe(patched);
  });

  it('never replays an unsafe mutation whose first-frame outcome is ambiguous', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const pending = window.fetch('/ui/v1/non-idempotent-action', { method: 'POST', body: '{}' });
    harness.channels[0].close();
    await expect(pending).rejects.toMatchObject({ name: 'UnknownMutationOutcomeError' });
    expect(harness.apiCalls()).toBe(0);
  });

  it('keeps HTTPS alive through an outage and immediately retries signaling when online', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    harness.setSignalingOnline(false);
    harness.channels[0].close();
    await expect(window.fetch('/ui/v1/sessions/status')).resolves.toMatchObject({ ok: true });
    await vi.advanceTimersByTimeAsync(20_000);
    await flush();
    expect(harness.channels).toHaveLength(1);
    expect(harness.apiCalls()).toBe(1);
    harness.setSignalingOnline(true);
    window.dispatchEvent(new Event('online'));
    await vi.advanceTimersByTimeAsync(0);
    await flush();
    expect(harness.channels).toHaveLength(2);
    expect(window.fetch).not.toBe(harness.fetch);
  });

  it('preserves one armed signaling backoff through visibility churn', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    harness.setSignalingOnline(false);
    harness.channels[0].close();
    for (let index = 0; index < 8; index += 1)
      document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(1_999);
    await flush();
    expect(harness.channels).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    await flush();
    expect(harness.channels).toHaveLength(1);
  });

  it('cancels an abandoned peer response exactly once but not a completed request', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    const pending = window.fetch('/ui/v1/responses/resp-1/events');
    const request = channel.sent.find((frame) => frame.path?.includes('/events'))!;
    channel.receive({ id: request.id, type: 'headers', status: 200, headers: {} });
    const response = await pending;
    await response.body?.cancel();
    expect(
      channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === request.id),
    ).toHaveLength(1);
    const completed = window.fetch('/ui/v1/sessions/status');
    const completeRequest = channel.sent.find((frame) => frame.path === '/ui/v1/sessions/status')!;
    channel.receive({ id: completeRequest.id, type: 'done', status: 200 });
    await completed;
    expect(
      channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === completeRequest.id),
    ).toHaveLength(0);
  });

  it('does not cancel or replay unanswered mutation work after its timeout', async () => {
    const harness = await enabledHarness();
    cleanupRTC = harness.cleanup;
    const channel = harness.channels[0];
    const pending = window.fetch('/ui/v1/worktrees', { method: 'POST', body: '{}' }).then(
      () => null,
      (error: unknown) => error,
    );
    const request = channel.sent.find((frame) => frame.path === '/ui/v1/worktrees')!;
    await vi.advanceTimersByTimeAsync(5_000);
    await flush();
    expect(await pending).toMatchObject({ name: 'UnknownMutationOutcomeError' });
    expect(
      channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === request.id),
    ).toHaveLength(0);
    expect(harness.apiCalls()).toBe(0);
  });
});

describe('withDeadline', () => {
  it('combines parent cancellation with a bounded timeout and cleanup', () => {
    vi.useFakeTimers();
    const parent = new AbortController();
    const first = withDeadline(parent.signal, 100);
    parent.abort(new DOMException('Parent stopped', 'AbortError'));
    expect(first.signal.aborted).toBe(true);
    expect(first.signal.reason).toEqual(expect.objectContaining({ name: 'AbortError' }));
    first.cleanup();

    const preAborted = new AbortController();
    preAborted.abort(new DOMException('Already stopped', 'AbortError'));
    const immediate = withDeadline(preAborted.signal, 100);
    expect(immediate.signal.aborted).toBe(true);
    expect(immediate.signal.reason).toBe(preAborted.signal.reason);
    immediate.cleanup();

    const second = withDeadline(undefined, 100);
    vi.advanceTimersByTime(100);
    expect(second.signal.aborted).toBe(true);
    expect(second.signal.reason).toEqual(expect.objectContaining({ name: 'TimeoutError' }));
    second.cleanup();
    vi.useRealTimers();
  });
});
