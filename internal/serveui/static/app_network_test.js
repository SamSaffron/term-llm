'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, 'app-network.js'), 'utf8');

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function createHarness(fetchImpl, online = true) {
  const listeners = new Map();
  const documentListeners = new Map();
  const timers = [];
  let now = 0;
  const navigator = { onLine: online };
  const document = {
    visibilityState: 'visible',
    addEventListener(type, handler) { documentListeners.set(type, handler); },
  };
  const state = {
    token: 'token',
    connectivity: { network: online ? 'unknown' : 'offline', phase: '', pendingSafe: 0, consecutiveFailures: 0 },
  };
  const app = {
    UI_PREFIX: '/ui',
    state,
    setConnectivityState(patch) {
      state.connectivity = { ...state.connectivity, ...patch };
      return state.connectivity;
    },
  };
  const window = {
    TermLLMApp: app,
    TERM_LLM_UI_PREFIX: '/ui',
    fetch: fetchImpl,
    setTimeout(callback, delay) {
      const timer = { callback, at: now + Number(delay || 0), cleared: false };
      timers.push(timer);
      return timer;
    },
    clearTimeout(timer) { if (timer) timer.cleared = true; },
    addEventListener(type, handler) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(handler);
    },
  };
  const context = {
    window,
    document,
    navigator,
    console,
    URL,
    Request,
    Response,
    Headers,
    AbortController,
    DOMException,
    ReadableStream,
    Date,
    Math,
    Set,
    Map,
    Error,
  };
  context.globalThis = context;
  vm.runInNewContext(source, context, { filename: 'app-network.js' });

  const flush = async () => {
    for (let i = 0; i < 30; i += 1) await Promise.resolve();
  };
  const advance = async (ms) => {
    now += ms;
    for (;;) {
      const due = timers.filter((timer) => !timer.cleared && timer.at <= now).sort((a, b) => a.at - b.at)[0];
      if (!due) break;
      due.cleared = true;
      due.callback();
      await flush();
    }
  };
  const dispatch = async (type) => {
    for (const handler of listeners.get(type) || []) handler({ type });
    await flush();
  };
  return {
    app, state, navigator, document, timers, advance, dispatch, flush,
    dispatchDocument: async (type) => {
      const handler = documentListeners.get(type);
      if (handler) handler({ type });
      await flush();
    },
    activeTimers: () => timers.filter((timer) => !timer.cleared),
  };
}

async function testClassificationAndRetryAfter() {
  const harness = createHarness(async () => new Response('{}', { status: 200 }));
  const { app } = harness;
  const P = app.API_FETCH_POLICY;
  assert(app.classifyAPIRequest('/ui/v1/models').policy === P.safeRead, 'GET should default to safe-read');
  assert(app.classifyAPIRequest('/ui/admin/widgets/status').authOwner === app.API_FETCH_AUTH.caller, '401 ownership should default explicitly to the caller');
  assert(app.classifyAPIRequest('/ui/v1/action', { method: 'POST' }).policy === P.mutation, 'POST should default to non-retryable mutation');
  assert(app.classifyAPIRequest('/ui/v1/action', { method: 'POST' }, { policy: P.idempotentMutation }).policy === P.mutation,
    'POST without an idempotency key must not be treated as retry-safe');
  assert(app.classifyAPIRequest('/ui/v1/action', { method: 'POST', headers: { 'Idempotency-Key': 'stable' } }, { policy: P.idempotentMutation }).retryable,
    'POST with an explicit idempotency key should be retry-safe');
  assert(app.retryAfterDelay(new Response(null, { status: 429, headers: { 'Retry-After': '20' } })) === 20000,
    'Retry-After seconds should control retry delay');
  console.log('PASS: API policies classify reads and mutations safely and honor Retry-After');
}

async function testRetryPolicies() {
  let safeCalls = 0;
  const safeHarness = createHarness(async (url) => {
    if (String(url).endsWith('/providers')) return new Response('{}', { status: 200 });
    safeCalls += 1;
    return new Response('{}', { status: safeCalls === 1 ? 503 : 200 });
  });
  const safePromise = safeHarness.app.apiFetch('/ui/v1/models', {}, { policy: safeHarness.app.API_FETCH_POLICY.safeRead, retries: 1 });
  await safeHarness.flush();
  assert(safeCalls === 1, 'safe read should wait before retry');
  await safeHarness.advance(1000);
  assert((await safePromise).status === 200 && safeCalls === 2, 'safe read should retry once');

  let mutationCalls = 0;
  const mutationHarness = createHarness(async () => {
    mutationCalls += 1;
    return new Response('{}', { status: 503 });
  });
  const mutationResponse = await mutationHarness.app.apiFetch('/ui/v1/action', { method: 'POST' });
  assert(mutationResponse.status === 503 && mutationCalls === 1, 'non-idempotent mutation must never retry automatically');

  let idempotentCalls = 0;
  const idempotentHarness = createHarness(async () => {
    idempotentCalls += 1;
    return new Response('{}', {
      status: idempotentCalls === 1 ? 429 : 200,
      headers: idempotentCalls === 1 ? { 'Retry-After': '20' } : {},
    });
  });
  const idempotentPromise = idempotentHarness.app.apiFetch('/ui/v1/action', {
    method: 'POST',
    headers: { 'Idempotency-Key': 'turn-1' },
  }, { policy: idempotentHarness.app.API_FETCH_POLICY.idempotentMutation, retries: 1 });
  await idempotentHarness.flush();
  await idempotentHarness.advance(19999);
  assert(idempotentCalls === 1, 'idempotent mutation retried before Retry-After elapsed');
  await idempotentHarness.advance(1);
  assert((await idempotentPromise).status === 200 && idempotentCalls === 2, 'idempotent mutation did not retry after Retry-After');
  console.log('PASS: API retries safe reads and explicitly idempotent mutations only');
}

async function testTimeoutAuthAbortAndDiagnostics() {
  let authFailures = 0;
  const authHarness = createHarness(async () => new Response('{}', { status: 401 }));
  authHarness.app.handleAuthFailure = () => { authFailures += 1; };

  const optional = await authHarness.app.apiFetch('/ui/admin/widgets/status', {}, { retries: 0 });
  await authHarness.advance(0);
  assert(optional.status === 401 && authFailures === 0, 'optional/admin 401 must not clear a valid session token');

  const owned = await authHarness.app.apiFetch('/ui/v1/private', {}, {
    retries: 0,
    auth: authHarness.app.API_FETCH_AUTH.session,
  });
  await authHarness.advance(0);
  assert(owned.status === 401 && authFailures === 1, 'session-owned 401 should trigger auth handling exactly once');
  assert(authHarness.app.networkDiagnostics.some((entry) => entry.kind === 'request' && entry.status === 401), 'request diagnostics should record HTTP status');

  const request = new Request('https://example.test/ui/v1/request-diagnostic');
  await authHarness.app.apiFetch(request, {}, { retries: 0, auth: authHarness.app.API_FETCH_AUTH.ignore });
  assert(authHarness.app.networkDiagnostics.some((entry) => entry.url === request.url), 'Request diagnostics should use the normalized URL');

  const timeoutHarness = createHarness((_url, options) => new Promise((_resolve, reject) => {
    options.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
  }));
  const timed = timeoutHarness.app.apiFetch('/ui/v1/slow', {}, { policy: timeoutHarness.app.API_FETCH_POLICY.safeRead, retries: 0, timeoutMs: 2000 })
    .then(() => null, (error) => error);
  await timeoutHarness.advance(2000);
  const timeoutError = await timed;
  assert(timeoutError?.name === 'TimeoutError' && timeoutError?.networkClassification?.policy === timeoutHarness.app.API_FETCH_POLICY.safeRead,
    'timeout should retain network classification diagnostics');

  const bodyHarness = createHarness(async (_url, options) => new Response(new ReadableStream({
    start(controller) {
      options.signal.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')), { once: true });
    },
  }), { status: 200 }));
  const controller = new AbortController();
  const bodyResponse = await bodyHarness.app.apiFetch('/ui/v1/body', { signal: controller.signal }, {
    policy: bodyHarness.app.API_FETCH_POLICY.safeRead,
    retries: 0,
    timeoutMs: 5000,
  });
  controller.abort();
  const bodyError = await bodyResponse.text().then(() => null, (error) => error);
  assert(bodyError?.name === 'AbortError', 'caller abort after headers should abort response body consumption');
  assert(bodyHarness.activeTimers().length === 0, 'abort-after-headers leaked the composed timeout');
  console.log('PASS: API layer makes auth ownership explicit and preserves aborts through the response body');
}

async function testOfflineAbortClearsPendingRequest() {
  const harness = createHarness(async () => { throw new Error('transport should not run'); }, false);
  const controller = new AbortController();
  const outcome = harness.app.apiFetch('/ui/v1/search', { signal: controller.signal }, {
    policy: harness.app.API_FETCH_POLICY.safeRead,
  }).then(() => null, (error) => error);
  await harness.flush();
  assert(harness.state.connectivity.pendingSafe === 0, 'parked safe read was mislabeled as a pending message');
  controller.abort();
  await harness.flush();
  const error = await outcome;
  assert(error?.name === 'AbortError' && harness.state.connectivity.pendingSafe === 0, 'aborted offline request leaked a retry waiter');

  const mutationController = new AbortController();
  const mutation = harness.app.apiFetch('/ui/v1/responses', {
    method: 'POST',
    headers: { 'Idempotency-Key': 'message-1' },
    signal: mutationController.signal,
  }, { policy: harness.app.API_FETCH_POLICY.idempotentMutation }).catch((caught) => caught);
  await harness.flush();
  assert(harness.state.connectivity.pendingSafe === 1, 'retry-safe pending mutation was not represented in pending copy');
  mutationController.abort();
  await mutation;
  assert(harness.state.connectivity.pendingSafe === 0, 'pending mutation count did not clear after cancellation');
  console.log('PASS: pending-safe copy tracks mutations, not parked reads, and aborts detach waiters');
}

async function testTwentySecondOfflineWakeAndRecoveryGate() {
  const calls = [];
  const harness = createHarness(async (url) => {
    calls.push(String(url));
    return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
  }, false);
  let reconciled = 0;
  harness.app.addNetworkRecoveryHook(async () => { reconciled += 1; });
  const pending = harness.app.apiFetch('/ui/v1/models', {}, { policy: harness.app.API_FETCH_POLICY.safeRead, retries: 1 });
  await harness.flush();
  assert(calls.length === 0, 'offline safe read should not hit the transport');
  assert(harness.state.connectivity.pendingSafe === 0, 'parked read should not use pending-message copy');
  await harness.advance(20000);
  assert(calls.length === 0, '20-second offline interval should suspend retry timers');

  harness.navigator.onLine = true;
  await harness.dispatch('online');
  const response = await pending;
  assert(response.status === 200, 'pending safe read did not recover after online');
  assert(calls[0] === '/ui/v1/providers' && calls[1] === '/ui/v1/models', 'health/reconciliation gate did not precede retry');
  assert(reconciled === 1, 'coordinated recovery should run one reconciliation pass');
  assert(harness.state.connectivity.pendingSafe === 0 && harness.state.connectivity.network === 'healthy', 'connectivity did not return to healthy');
  console.log('PASS: 20-second offline retry is suspended then wakes behind one recovery gate');
}

async function testFailedProbeDoesNotReleaseRetries() {
  const calls = [];
  let healthy = false;
  const harness = createHarness(async (url) => {
    calls.push(String(url));
    if (String(url).endsWith('/providers')) return new Response('{}', { status: healthy ? 200 : 503 });
    return new Response('{}', { status: 200 });
  }, false);
  const first = harness.app.apiFetch('/ui/v1/models?a=1');
  const second = harness.app.apiFetch('/ui/v1/models?a=2');
  await harness.flush();

  harness.navigator.onLine = true;
  await harness.dispatch('online');
  assert(calls.length === 1 && calls[0] === '/ui/v1/providers', 'failed health probe released pending retries');
  assert(harness.state.connectivity.phase === 'unstable', 'failed health probe did not expose unstable state');
  await harness.advance(999);
  assert(calls.length === 1, 'coordinated probe backoff fired too early');

  healthy = true;
  await harness.advance(1);
  await Promise.all([first, second]);
  assert(calls.filter((url) => url === '/ui/v1/providers').length === 2, 'health recovery did not retry through one coordinated probe');
  assert(calls.filter((url) => url.startsWith('/ui/v1/models')).length === 2, 'healthy recovery did not release both reads');
  console.log('PASS: failed health probe retains all waiters behind one armed recovery backoff');
}

async function testDoubleFlapDoesNotDeadlockHookWaiter() {
  const calls = [];
  const harness = createHarness(async (url) => {
    calls.push(String(url));
    return new Response('{}', { status: 200 });
  }, false);
  let hookCalls = 0;
  harness.app.addNetworkRecoveryHook(() => {
    hookCalls += 1;
    if (hookCalls === 1) return harness.app.waitForNetworkRetry(60000, { key: 'hook:refresh', reason: 'hook-refresh' });
    return undefined;
  });
  const pending = harness.app.apiFetch('/ui/v1/models');
  await harness.flush();

  harness.navigator.onLine = true;
  await harness.dispatch('online');
  assert(hookCalls === 1, 'first recovery hook did not enter its wait');
  harness.navigator.onLine = false;
  await harness.dispatch('offline');
  harness.navigator.onLine = true;
  await harness.dispatch('online');
  const response = await pending;
  assert(response.status === 200, 'pending request remained deadlocked after second online transition');
  assert(hookCalls === 2, 'second online transition did not start a fresh bounded recovery pass');
  assert(calls.filter((url) => url === '/ui/v1/providers').length === 2, 'double flap should perform exactly one probe per online epoch');
  console.log('PASS: offline-online-offline-online restores waiter liveness while a recovery hook is waiting');
}

async function testRecoveryHooksAreIsolatedAndBounded() {
  const harness = createHarness(async () => new Response('{}', { status: 200 }), false);
  let secondHook = 0;
  harness.app.addNetworkRecoveryHook(() => new Promise(() => {}));
  harness.app.addNetworkRecoveryHook(() => { secondHook += 1; throw new Error('hook failed'); });
  const pending = harness.app.apiFetch('/ui/v1/models');
  await harness.flush();
  harness.navigator.onLine = true;
  await harness.dispatch('online');
  assert(secondHook === 1, 'blocked hook prevented independent hook execution');
  await harness.advance(4999);
  let settled = false;
  pending.then(() => { settled = true; });
  await harness.flush();
  assert(!settled, 'blocked hook was not bounded by the configured timeout');
  await harness.advance(1);
  assert((await pending).status === 200, 'hook timeout/failure prevented safe waiter release');
  assert(harness.app.networkDiagnostics.some((entry) => entry.kind === 'recovery-hook-timeout'), 'hook timeout was not diagnosed');
  assert(harness.app.networkDiagnostics.some((entry) => entry.kind === 'recovery-hook-failed'), 'hook failure was not diagnosed');
  console.log('PASS: recovery hooks run independently, time out, and cannot strand waiters');
}

async function testStreamRecoveryHasNoPostWakeHotSpin() {
  const harness = createHarness(async () => new Response('{}', { status: 200 }));
  const raced = harness.app.createResumableStreamRecovery({ key: 'stream:race' });
  raced.noteAttempt();
  await harness.app.runCoordinatedNetworkRecovery('online');
  const racedWake = await raced.wait('ended');
  assert(racedWake === 'recovered-during-attempt', 'recovery/wait race was not solved by the recovery epoch');

  const recovery = harness.app.createResumableStreamRecovery({ key: 'stream:backoff' });
  recovery.noteAttempt();
  let settled = false;
  const wait = recovery.wait('ended').then((reason) => { settled = true; return reason; });
  await harness.flush();
  assert(!settled, 'post-recovery stream retry hot-spun instead of arming delay');
  await harness.advance(999);
  assert(!settled, 'stream retry delay completed early');
  assert(await harness.advance(1).then(() => wait) === 'timer', 'armed stream backoff did not complete normally');
  console.log('PASS: stream wake race is direct and later retries retain their real backoff');
}

(async () => {
  await testClassificationAndRetryAfter();
  await testRetryPolicies();
  await testTimeoutAuthAbortAndDiagnostics();
  await testOfflineAbortClearsPendingRequest();
  await testTwentySecondOfflineWakeAndRecoveryGate();
  await testFailedProbeDoesNotReleaseRetries();
  await testDoubleFlapDoesNotDeadlockHookWaiter();
  await testRecoveryHooksAreIsolatedAndBounded();
  await testStreamRecoveryHasNoPostWakeHotSpin();
})().catch((error) => {
  console.error(error && error.stack ? error.stack : error);
  process.exitCode = 1;
});
