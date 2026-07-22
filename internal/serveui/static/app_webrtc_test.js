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
  let transportRecoveries = 0;
  let httpsAPICalls = 0;

  class FakeDataChannel {
    constructor() {
      this.readyState = 'open';
      this.throwOnSend = false;
      this.onopen = null;
      this.onclose = null;
      this.onerror = null;
      this.onmessage = null;
    }

    send() {
      if (this.throwOnSend) throw new Error('simulated channel send failure');
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
    addEventListener() {},
  };
  const window = {
    __WEBRTC_ENABLED__: true,
    __TERM_LLM_WEBRTC_TESTING__: true,
    __WEBRTC_SIGNALING_URL__: '/webrtc',
    TERM_LLM_UI_PREFIX: '/ui',
    location: { search: '', origin: 'https://example.test' },
    fetch: originalFetch,
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
      const handle = { fn, delay, cleared: false };
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
    window,
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

(async () => {
  testResponseTimeouts();
  await testChannelCloseSignalsTransportRecoveryOnce();
  await testSendFallbackSignalsTransportRecoveryOnce();
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
