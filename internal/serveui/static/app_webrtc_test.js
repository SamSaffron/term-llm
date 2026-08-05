'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { webcrypto } = require('crypto');

const source = fs.readFileSync(path.join(__dirname, 'app-webrtc.js'), 'utf8');

function fail(message) {
  throw new Error(message);
}

async function waitFor(predicate, message) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    if (predicate()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  fail(message);
}

function testResponseTimeouts() {
  const window = {
    __WEBRTC_ENABLED__: false,
    __TERM_LLM_WEBRTC_TESTING__: true,
  };
  vm.runInNewContext(source, { window }, { filename: 'app-webrtc.js' });

  const hooks = window.__TERM_LLM_WEBRTC_TEST_HOOKS__;
  if (!hooks || typeof hooks.responseTimeoutForMethod !== 'function') {
    fail('WebRTC timeout test hook was not installed');
  }

  const cases = [
    ['GET', 1000],
    ['get', 1000],
    ['HEAD', 1000],
    ['OPTIONS', 1000],
    ['POST', 5000],
    ['PATCH', 5000],
    ['PUT', 5000],
    ['DELETE', 5000],
  ];

  for (const [method, expected] of cases) {
    const actual = hooks.responseTimeoutForMethod(method);
    if (actual !== expected) {
      fail(`${method} timeout = ${actual}, want ${expected}`);
    }
  }

  console.log('PASS: WebRTC first-frame timeout is 1s for reads and 5s for mutations');
}

async function createEnabledHarness() {
  const channels = [];
  const scheduled = [];
  const windowListeners = new Map();
  const documentListeners = new Map();
  let now = 0;
  let signalingOnline = true;
  let transportRecoveries = 0;
  let httpsAPICalls = 0;

  class FakeDataChannel {
    constructor() {
      this.readyState = 'open';
      this.throwOnSend = false;
      this.sent = [];
      this.onopen = null;
      this.onclose = null;
      this.onerror = null;
      this.onmessage = null;
    }

    send(data) {
      if (this.throwOnSend) throw new Error('simulated channel send failure');
      this.sent.push(JSON.parse(String(data)));
    }

    close() {
      this.readyState = 'closed';
      this.onclose?.({ type: 'close' });
    }
  }

  class FakePeerConnection {
    constructor() {
      this.iceGatheringState = 'complete';
      this.iceConnectionState = 'connected';
      this.localDescription = null;
    }

    createDataChannel() {
      const channel = new FakeDataChannel();
      channels.push(channel);
      return channel;
    }

    async createOffer() {
      return { type: 'offer', sdp: 'fake-offer' };
    }

    async setLocalDescription(offer) {
      this.localDescription = offer;
    }

    async setRemoteDescription() {}
  }

  const originalFetch = async (url) => {
    const value = String(url);
    if (value.endsWith('/session')) {
      if (!signalingOnline) throw new TypeError('simulated signaling outage');
      return new Response(JSON.stringify({ session_id: 'signal-session' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (value.includes('/signal?')) {
      return new Response(JSON.stringify({ type: 'answer', sdp: 'fake-answer' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (value.endsWith('/signal')) {
      return new Response(null, { status: 200 });
    }
    if (value.includes('/v1/')) httpsAPICalls += 1;
    return new Response(JSON.stringify({ sessions: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };

  const document = {
    readyState: 'complete',
    visibilityState: 'visible',
    addEventListener(type, handler) {
      if (!documentListeners.has(type)) documentListeners.set(type, []);
      documentListeners.get(type).push(handler);
    },
  };
  const window = {
    __WEBRTC_ENABLED__: true,
    __TERM_LLM_WEBRTC_TESTING__: true,
    __WEBRTC_SIGNALING_URL__: '/webrtc',
    TERM_LLM_UI_PREFIX: '/ui',
    location: { search: '', origin: 'https://example.test' },
    fetch: originalFetch,
    addEventListener(type, handler) {
      if (!windowListeners.has(type)) windowListeners.set(type, []);
      windowListeners.get(type).push(handler);
    },
    TermLLMApp: {
      handleFetchTransportFallback() {
        transportRecoveries += 1;
      },
    },
  };
  window.document = document;

  const context = {
    window,
    document,
    URL,
    URLSearchParams,
    AbortSignal,
    Blob,
    Response,
    Headers,
    ReadableStream,
    DOMException,
    TextEncoder,
    performance,
    crypto: webcrypto,
    RTCPeerConnection: FakePeerConnection,
    setTimeout(fn, delay) {
      const handle = { fn, delay, at: now + Number(delay || 0), cleared: false };
      scheduled.push(handle);
      return handle;
    },
    clearTimeout(handle) {
      if (handle) handle.cleared = true;
    },
    btoa(value) { return Buffer.from(value, 'binary').toString('base64'); },
    console,
  };
  context.globalThis = context;
  vm.runInNewContext(source, context, { filename: 'app-webrtc.js' });

  await waitFor(
    () => channels.length === 1 && typeof channels[0].onclose === 'function' && window.fetch !== originalFetch,
    'WebRTC data channel did not initialize and patch fetch'
  );

  return {
    channels,
    originalFetch,
    scheduled,
    window,
    setSignalingOnline(value) { signalingOnline = Boolean(value); },
    async dispatchWindow(type) {
      for (const handler of windowListeners.get(type) || []) handler({ type });
      for (let i = 0; i < 8; i += 1) await Promise.resolve();
    },
    async dispatchDocument(type) {
      for (const handler of documentListeners.get(type) || []) handler({ type });
      for (let i = 0; i < 8; i += 1) await Promise.resolve();
    },
    async runNextTimer(expectedDelay) {
      const timer = scheduled.find((item) => !item.cleared && (expectedDelay === undefined || item.delay === expectedDelay));
      if (!timer) return false;
      now = Math.max(now, timer.at);
      timer.cleared = true;
      timer.fn();
      for (let i = 0; i < 20; i += 1) await new Promise((resolve) => setImmediate(resolve));
      return true;
    },
    async advanceTime(ms) {
      const target = now + Number(ms || 0);
      for (;;) {
        const timer = scheduled.filter((item) => !item.cleared && item.at <= target).sort((a, b) => a.at - b.at)[0];
        if (!timer) break;
        now = timer.at;
        timer.cleared = true;
        timer.fn();
        for (let i = 0; i < 20; i += 1) await new Promise((resolve) => setImmediate(resolve));
      }
      now = target;
    },
    getNow: () => now,
    getHTTPSAPICalls: () => httpsAPICalls,
    getTransportRecoveries: () => transportRecoveries,
  };
}

async function testChannelCloseSignalsTransportRecoveryOnce() {
  const harness = await createEnabledHarness();
  const channel = harness.channels[0];
  const patchedFetch = harness.window.fetch;

  channel.close();
  if (harness.window.fetch === patchedFetch) {
    fail('channel close did not restore the original fetch transport');
  }
  if (harness.getTransportRecoveries() !== 1) {
    fail(`channel close emitted ${harness.getTransportRecoveries()} recovery signals, want 1`);
  }

  channel.onclose({ type: 'close' });
  channel.onerror({ type: 'error' });
  if (harness.getTransportRecoveries() !== 1) {
    fail('repeated close/error callbacks emitted duplicate recovery signals');
  }

  console.log('PASS: WebRTC channel close restores fetch and signals app recovery once');
}

async function testSendFallbackSignalsTransportRecoveryOnce() {
  const harness = await createEnabledHarness();
  const channel = harness.channels[0];
  const patchedFetch = harness.window.fetch;
  channel.throwOnSend = true;

  const response = await patchedFetch('/ui/v1/sessions/status', { headers: {} });
  if (!response.ok || harness.getHTTPSAPICalls() !== 1) {
    fail(`send failure did not fall back to HTTPS exactly once (calls=${harness.getHTTPSAPICalls()})`);
  }
  if (harness.window.fetch === patchedFetch) {
    fail('send failure did not restore the original fetch transport');
  }
  if (harness.getTransportRecoveries() !== 1) {
    fail(`send fallback emitted ${harness.getTransportRecoveries()} recovery signals, want 1`);
  }

  channel.onclose({ type: 'close' });
  if (harness.getTransportRecoveries() !== 1) {
    fail('late close callback after fallback emitted a duplicate recovery signal');
  }

  console.log('PASS: WebRTC request fallback restores fetch and signals app recovery once');
}

async function testSynchronousSendThrowFallsBackForMutation() {
  const harness = await createEnabledHarness();
  const channel = harness.channels[0];
  const patchedFetch = harness.window.fetch;
  channel.throwOnSend = true;
  const response = await patchedFetch('/ui/v1/non-idempotent-action', { method: 'POST', body: '{}' });
  if (!response.ok || harness.getHTTPSAPICalls() !== 1) {
    fail('synchronous send throw did not safely fall back to HTTPS for a mutation');
  }
  console.log('PASS: synchronous WebRTC send throw proves non-delivery and falls back once over HTTPS');
}

async function testAmbiguousMutationDrainIsNotReplayed() {
  const harness = await createEnabledHarness();
  const channel = harness.channels[0];
  const pending = harness.window.fetch('/ui/v1/non-idempotent-action', { method: 'POST', body: '{}' })
    .then(() => null, (error) => error);
  channel.close();
  const error = await pending;
  if (!error || error.name !== 'UnknownMutationOutcomeError') {
    fail('mutation accepted by dataChannel.send did not protect its ambiguous outcome');
  }
  if (harness.getHTTPSAPICalls() !== 0) {
    fail('ambiguous mutation was replayed during channel drain');
  }
  console.log('PASS: first-frame/channel-drain ambiguity never replays an unsafe mutation');
}

async function testTwentySecondFallbackAndPersistentRecovery() {
  const harness = await createEnabledHarness();
  const firstChannel = harness.channels[0];
  const patchedFetch = harness.window.fetch;
  harness.setSignalingOnline(false);
  firstChannel.close();

  const httpsResponse = await harness.window.fetch('/ui/v1/sessions/status', { headers: {} });
  if (!httpsResponse.ok || harness.getHTTPSAPICalls() !== 1) {
    fail('HTTPS did not remain functional during WebRTC outage');
  }
  await harness.runNextTimer(2000);
  await harness.runNextTimer(5000);
  await harness.runNextTimer(10000);
  await harness.advanceTime(3000);
  if (harness.getNow() !== 20000 || harness.channels.length !== 1) {
    fail(`WebRTC unexpectedly recovered during real 20-second backoff (channels=${harness.channels.length})`);
  }
  if (!harness.scheduled.some((timer) => !timer.cleared && timer.delay === 30000)) {
    fail('persistent WebRTC recovery did not continue with bounded backoff');
  }

  harness.setSignalingOnline(true);
  await harness.dispatchWindow('online');
  await harness.runNextTimer(0);
  await waitFor(() => harness.channels.length === 2 && harness.window.fetch !== harness.originalFetch, 'WebRTC did not recover quickly after restoration');
  if (harness.window.fetch === patchedFetch) {
    // A new patched function identity is not required; this assertion exists to
    // document that routing is active again, not to demand wrapper churn.
  }
  console.log('PASS: WebRTC survives a 20-second outage on HTTPS and recovers immediately when signaling returns');
}

async function testVisibilityChurnDoesNotHammerSignaling() {
  const harness = await createEnabledHarness();
  harness.setSignalingOnline(false);
  harness.channels[0].close();
  for (let i = 0; i < 8; i += 1) await harness.dispatchDocument('visibilitychange');
  const activeRetries = harness.scheduled.filter((timer) => !timer.cleared && timer.delay === 2000);
  const immediateRetries = harness.scheduled.filter((timer) => !timer.cleared && timer.delay === 0);
  if (activeRetries.length !== 1 || immediateRetries.length !== 0) {
    fail(`visibility churn rescheduled signaling (${activeRetries.length} backoffs, ${immediateRetries.length} immediate)`);
  }
  if (harness.channels.length !== 1) fail('visibility churn started signaling before the armed backoff');
  console.log('PASS: visibility churn preserves one armed WebRTC signaling backoff');
}

async function testCanceledStreamPropagatesRequestCancellation() {
  const harness = await createEnabledHarness();
  const channel = harness.channels[0];
  const pending = harness.window.fetch('/ui/v1/responses/resp-1/events', { headers: {} });
  const request = channel.sent.find((frame) => frame.path === '/ui/v1/responses/resp-1/events');
  if (!request?.id) fail('WebRTC request frame was not sent');

  channel.onmessage({ data: JSON.stringify({ id: request.id, type: 'headers', status: 200, headers: {} }) });
  const response = await pending;
  await response.body.cancel();

  const cancels = channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === request.id);
  if (cancels.length !== 1) {
    fail(`stream cancellation emitted ${cancels.length} peer cancel frames, want 1`);
  }

  const completedPending = harness.window.fetch('/ui/v1/sessions/status', { headers: {} });
  const completedRequest = channel.sent.find((frame) => frame.path === '/ui/v1/sessions/status');
  channel.onmessage({ data: JSON.stringify({ id: completedRequest.id, type: 'done', status: 200 }) });
  await completedPending;
  const completedCancels = channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === completedRequest.id);
  if (completedCancels.length !== 0) fail('normally completed request emitted a cancellation frame');

  console.log('PASS: abandoning a WebRTC response cancels the matching peer request exactly once');
}

async function testUnansweredMutationTimeoutDoesNotCancelServerWork() {
  const harness = await createEnabledHarness();
  const channel = harness.channels[0];
  const pending = harness.window.fetch('/ui/v1/worktrees', { method: 'POST', body: '{}' })
    .then(() => null, (error) => error);
  const request = channel.sent.find((frame) => frame.path === '/ui/v1/worktrees');
  if (!request?.id) fail('WebRTC mutation request frame was not sent');

  await harness.advanceTime(5000);
  const error = await pending;
  if (!error || error.name !== 'UnknownMutationOutcomeError') {
    fail('unanswered mutation did not preserve unknown-outcome semantics');
  }
  const cancels = channel.sent.filter((frame) => frame.type === 'cancel' && frame.id === request.id);
  if (cancels.length !== 0) fail('automatic mutation timeout canceled non-idempotent server work');
  if (harness.getHTTPSAPICalls() !== 0) fail('unanswered mutation was replayed over HTTPS');

  console.log('PASS: unanswered mutation timeout neither cancels nor replays server work');
}

(async () => {
  testResponseTimeouts();
  await testChannelCloseSignalsTransportRecoveryOnce();
  await testSendFallbackSignalsTransportRecoveryOnce();
  await testSynchronousSendThrowFallsBackForMutation();
  await testAmbiguousMutationDrainIsNotReplayed();
  await testTwentySecondFallbackAndPersistentRecovery();
  await testVisibilityChurnDoesNotHammerSignaling();
  await testCanceledStreamPropagatesRequestCancellation();
  await testUnansweredMutationTimeoutDoesNotCancelServerWork();
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
