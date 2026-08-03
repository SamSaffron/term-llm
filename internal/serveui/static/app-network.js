(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});
const { state } = app;
const UI_PREFIX = app.UI_PREFIX || window.TERM_LLM_UI_PREFIX || '/ui';
const POLICY = Object.freeze({
  safeRead: 'safe-read',
  idempotentMutation: 'idempotent-mutation',
  mutation: 'non-retryable-mutation',
  stream: 'stream',
});
const AUTH = Object.freeze({ session: 'session', caller: 'caller', ignore: 'ignore' });
const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);
const RETRYABLE_STATUSES = new Set([408, 425, 429]);
const DEFAULT_TIMEOUT_MS = 15000;
const DEFAULT_MUTATION_TIMEOUT_MS = 30000;
const MAX_RETRY_AFTER_MS = 60000;
const RECOVERY_HOOK_TIMEOUT_MS = 5000;
const diagnostics = [];
const waiters = new Map();
const recoveryHooks = new Set();
let waiterSequence = 0;
let coordinatedRecoveryPromise = null;
let activeRecoveryController = null;
let recoveryQueued = false;
let queuedRecoveryReason = '';
let recoveryRetryTimer = 0;
let recoveryProbeAttempt = 0;
let successfulRecoveryEpoch = 0;

const online = () => typeof navigator === 'undefined' || navigator.onLine !== false;
const visible = () => typeof document === 'undefined' || document.visibilityState !== 'hidden';
const retryableStatus = (status) => RETRYABLE_STATUSES.has(Number(status)) || Number(status) >= 500;
const retryDelay = (attempt) => {
  const normalized = Math.max(0, Number(attempt) || 0);
  return normalized >= 5 ? 60000 : Math.round(1000 * Math.pow(1.5, normalized));
};
const requestURL = (resource) => {
  if (typeof Request !== 'undefined' && resource instanceof Request) return resource.url;
  return String(resource);
};
const retryAfterDelay = (response, now = Date.now()) => {
  const value = String(response?.headers?.get?.('Retry-After') || '').trim();
  if (!value) return 0;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) return Math.min(MAX_RETRY_AFTER_MS, seconds * 1000);
  const at = Date.parse(value);
  return Number.isFinite(at) ? Math.min(MAX_RETRY_AFTER_MS, Math.max(0, at - now)) : 0;
};

const noteDiagnostic = (kind, detail = {}) => {
  diagnostics.push({ at: Date.now(), kind, ...detail });
  if (diagnostics.length > 100) diagnostics.splice(0, diagnostics.length - 100);
  if (state?.connectivity) state.connectivity.diagnostics = diagnostics;
};

const setConnectivity = (patch) => {
  if (typeof app.setConnectivityState === 'function') app.setConnectivityState(patch);
  else if (state) state.connectivity = { ...(state.connectivity || {}), ...patch };
};

const classifyRequest = (resource, options = {}, controls = {}) => {
  const isRequest = typeof Request !== 'undefined' && resource instanceof Request;
  const method = String(options.method || (isRequest ? resource.method : 'GET') || 'GET').toUpperCase();
  let policy = String(controls.policy || '').trim();
  if (!Object.values(POLICY).includes(policy)) policy = SAFE_METHODS.has(method) ? POLICY.safeRead : POLICY.mutation;

  const headers = new Headers(options.headers || (isRequest ? resource.headers : undefined));
  const hasIdempotencyKey = Boolean(headers.get('Idempotency-Key'));
  if (policy === POLICY.idempotentMutation && method === 'POST' && !hasIdempotencyKey) {
    noteDiagnostic('unsafe-idempotent-policy', { method, url: requestURL(resource) });
    policy = POLICY.mutation;
  }
  const retryable = policy === POLICY.safeRead || policy === POLICY.idempotentMutation;
  const pendingMutation = policy === POLICY.idempotentMutation;
  const authOwner = Object.values(AUTH).includes(controls.auth) ? controls.auth : AUTH.caller;
  const retries = retryable ? Math.max(0, Number.isFinite(Number(controls.retries)) ? Number(controls.retries) : 2) : 0;
  const timeoutMs = controls.timeoutMs === 0
    ? 0
    : Math.max(1, Number(controls.timeoutMs) || (SAFE_METHODS.has(method) ? DEFAULT_TIMEOUT_MS : DEFAULT_MUTATION_TIMEOUT_MS));
  return { method, policy, retryable, pendingMutation, authOwner, retries, timeoutMs, hasIdempotencyKey };
};

const suspendWaiter = (waiter) => {
  if (waiter.timer) window.clearTimeout(waiter.timer);
  waiter.timer = 0;
  waiter.remaining = Math.max(0, waiter.deadline - Date.now());
};

const armWaiter = (waiter) => {
  if (waiter.settled || waiter.timer || !online()) return;
  waiter.deadline = Date.now() + waiter.remaining;
  waiter.timer = window.setTimeout(() => waiter.finish('timer'), waiter.remaining);
};

const updatePendingSafe = (delta) => {
  setConnectivity({ pendingSafe: Math.max(0, Number(state?.connectivity?.pendingSafe || 0) + delta) });
};

const waitForNetworkRetry = (delay, controls = {}) => new Promise((resolve) => {
  const id = ++waiterSequence;
  const waiter = {
    id,
    key: String(controls.key || ''),
    reason: String(controls.reason || ''),
    pendingSafe: controls.pendingSafe === true,
    remaining: Math.max(0, Number(delay) || 0),
    deadline: Date.now() + Math.max(0, Number(delay) || 0),
    timer: 0,
    settled: false,
    signal: controls.signal || null,
    finish(wakeReason) {
      if (waiter.settled) return;
      waiter.settled = true;
      if (waiter.timer) window.clearTimeout(waiter.timer);
      waiter.signal?.removeEventListener?.('abort', waiter.onAbort);
      waiters.delete(id);
      if (waiter.pendingSafe) updatePendingSafe(-1);
      resolve(String(wakeReason || 'timer'));
    },
  };
  waiters.set(id, waiter);
  waiter.onAbort = () => waiter.finish('aborted');
  if (waiter.signal?.aborted) Promise.resolve().then(waiter.onAbort);
  else waiter.signal?.addEventListener?.('abort', waiter.onAbort, { once: true });
  if (waiter.pendingSafe) updatePendingSafe(1);
  if (online()) armWaiter(waiter);
  else setConnectivity({ network: 'offline', phase: 'offline' });
  noteDiagnostic('retry-wait', { key: waiter.key, reason: waiter.reason, delay: waiter.remaining, online: online() });
  const observedEpoch = Number(controls.afterRecoveryEpoch);
  if (online() && Number.isFinite(observedEpoch) && successfulRecoveryEpoch > observedEpoch) {
    Promise.resolve().then(() => waiter.finish('recovered-during-attempt'));
  }
});

const wakeNetworkRetry = (key, reason = 'wake') => {
  const normalized = String(key || '');
  let woke = false;
  for (const waiter of waiters.values()) {
    if (normalized && waiter.key !== normalized) continue;
    waiter.finish(reason);
    woke = true;
  }
  return woke;
};

const wakeAllNetworkRetries = (reason = 'wake') => wakeNetworkRetry('', reason);

const cancelNetworkRetries = (keyPrefix, reason = 'detached') => {
  const prefix = String(keyPrefix || '');
  let canceled = false;
  for (const waiter of waiters.values()) {
    if (prefix && !waiter.key.startsWith(prefix)) continue;
    waiter.finish(reason);
    canceled = true;
  }
  return canceled;
};

const composeAbortSignal = (parentSignal, timeoutMs) => {
  if (!timeoutMs) return { signal: parentSignal, cleanup() {}, timedOut: () => false };
  const controller = new AbortController();
  let timeout = false;
  let cleaned = false;
  let timer = 0;
  const cleanup = () => {
    if (cleaned) return;
    cleaned = true;
    if (timer) window.clearTimeout(timer);
    parentSignal?.removeEventListener?.('abort', onAbort);
  };
  const onAbort = () => {
    controller.abort(parentSignal?.reason);
    cleanup();
  };
  if (parentSignal?.aborted) {
    onAbort();
  } else {
    parentSignal?.addEventListener?.('abort', onAbort, { once: true });
    timer = window.setTimeout(() => {
      timeout = true;
      controller.abort();
      cleanup();
    }, timeoutMs);
  }
  return { signal: controller.signal, timedOut: () => timeout, cleanup };
};

// Per the Fetch spec, a response carrying a null body status must never be
// rebuilt with a body: `new Response(body, { status: 304 })` throws a
// TypeError. Chromium reports `response.body === null` for these, so the body
// check alone happens to work there, but Firefox exposes a non-null empty
// stream. The UI polls an ETagged endpoint that legitimately answers 304, so on
// Firefox every poll threw and drove connectivity to 'unstable' forever.
const NULL_BODY_STATUSES = new Set([101, 103, 204, 205, 304]);

const responseWithAbortLifetime = (response, cleanup) => {
  if (!response.body || NULL_BODY_STATUSES.has(response.status)) {
    cleanup();
    return response;
  }
  const reader = response.body.getReader();
  const body = new ReadableStream({
    async pull(controller) {
      try {
        const { done, value } = await reader.read();
        if (done) {
          cleanup();
          controller.close();
        } else {
          controller.enqueue(value);
        }
      } catch (error) {
        cleanup();
        controller.error(error);
      }
    },
    async cancel(reason) {
      try { await reader.cancel(reason); } finally { cleanup(); }
    },
  });
  const wrapped = new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers });
  for (const property of ['url', 'redirected', 'type']) {
    try { Object.defineProperty(wrapped, property, { value: response[property] }); } catch (_error) { /* optional metadata */ }
  }
  return wrapped;
};

const transportFetch = async (resource, options, timeoutMs, transportRetrySafe = false) => {
  const isRequest = typeof Request !== 'undefined' && resource instanceof Request;
  const parentSignal = options.signal || (isRequest ? resource.signal : null);
  const abort = composeAbortSignal(parentSignal, timeoutMs);
  const fetchOptions = { ...options, __termLLMRetrySafe: Boolean(transportRetrySafe) };
  if (abort.signal) fetchOptions.signal = abort.signal;
  try {
    const response = await window.fetch(resource, fetchOptions);
    return timeoutMs ? responseWithAbortLifetime(response, abort.cleanup) : response;
  } catch (error) {
    abort.cleanup();
    if (abort.timedOut()) {
      const timeoutError = new Error(`Request timed out after ${timeoutMs} ms.`);
      timeoutError.name = 'TimeoutError';
      timeoutError.status = 0;
      timeoutError.cause = error;
      throw timeoutError;
    }
    throw error;
  }
};

const apiFetch = async (resource, options = {}, controls = {}) => {
  const classification = classifyRequest(resource, options, controls);
  let attempt = 0;
  for (;;) {
    if (!online()) {
      setConnectivity({ network: 'offline', phase: 'offline' });
      if (!classification.retryable) {
        const error = new Error('Network offline. This action was not sent; retry it when online.');
        error.name = 'OfflineError';
        error.status = 0;
        error.networkClassification = classification;
        throw error;
      }
      await waitForNetworkRetry(0, {
        reason: 'offline', pendingSafe: classification.pendingMutation, signal: options.signal
      });
      if (options.signal?.aborted) {
        const error = new Error('The operation was aborted.');
        error.name = 'AbortError';
        throw error;
      }
    }

    const startedAt = Date.now();
    const attemptRecoveryEpoch = successfulRecoveryEpoch;
    try {
      const response = await transportFetch(resource, options, classification.timeoutMs, classification.retryable);
      noteDiagnostic('request', {
        method: classification.method,
        policy: classification.policy,
        status: response.status,
        attempt,
        durationMs: Date.now() - startedAt,
        url: requestURL(resource),
      });
      if (response.status === 401 && classification.authOwner === AUTH.session) {
        window.setTimeout(() => app.handleAuthFailure?.(), 0);
      }
      if (!retryableStatus(response.status) || !classification.retryable || attempt >= classification.retries) {
        if (response.status < 500) {
          const currentPhase = String(state?.connectivity?.phase || '');
          const recovering = currentPhase === 'recovering' || currentPhase === 'catching-up';
          setConnectivity({
            network: online() ? (recovering ? 'recovering' : 'healthy') : 'offline',
            phase: online() ? (recovering ? currentPhase : '') : 'offline',
            consecutiveFailures: 0,
            lastSuccessAt: Date.now()
          });
        }
        return response;
      }
      const delay = retryAfterDelay(response) || retryDelay(attempt);
      try { await response.body?.cancel(); } catch (_error) { /* retry supersedes this response */ }
      attempt += 1;
      setConnectivity({ network: 'unstable', phase: 'unstable', consecutiveFailures: Number(state?.connectivity?.consecutiveFailures || 0) + 1 });
       await waitForNetworkRetry(delay, {
         reason: `http-${response.status}`,
         pendingSafe: classification.pendingMutation,
         signal: options.signal,
         afterRecoveryEpoch: attemptRecoveryEpoch,
       });
    } catch (error) {
      if (options.signal?.aborted) throw error;
      const failures = Number(state?.connectivity?.consecutiveFailures || 0) + 1;
      setConnectivity({
        network: online() ? 'unstable' : 'offline',
        phase: online() ? 'unstable' : 'offline',
        consecutiveFailures: failures,
        lastFailureAt: Date.now(),
      });
      noteDiagnostic('network-error', {
        method: classification.method,
        policy: classification.policy,
        attempt,
        name: String(error?.name || ''),
        message: String(error?.message || error || ''),
        url: requestURL(resource),
      });
      if (!classification.retryable || attempt >= classification.retries) {
        error.networkClassification = classification;
        throw error;
      }
      const delay = retryDelay(attempt);
      attempt += 1;
      await waitForNetworkRetry(delay, {
        reason: String(error?.name || 'network-error'),
        pendingSafe: classification.pendingMutation,
        signal: options.signal,
        afterRecoveryEpoch: attemptRecoveryEpoch,
      });
    }
  }
};

const addNetworkRecoveryHook = (hook) => {
  if (typeof hook !== 'function') return () => {};
  recoveryHooks.add(hook);
  return () => recoveryHooks.delete(hook);
};

const healthProbe = async (signal) => {
  const headers = {};
  if (state?.token) headers.Authorization = `Bearer ${state.token}`;
  const response = await transportFetch(`${UI_PREFIX}/v1/providers`, { headers, signal }, 5000);
  const healthy = Boolean(response && response.status < 500);
  try { await response.body?.cancel(); } catch (_error) { /* status is sufficient */ }
  return healthy;
};

const scheduleRecoveryRetry = () => {
  if (recoveryRetryTimer || !online()) return;
  const delay = retryDelay(recoveryProbeAttempt);
  recoveryProbeAttempt += 1;
  setConnectivity({ network: 'unstable', phase: 'unstable', recoveryRetryScheduled: true });
  recoveryRetryTimer = window.setTimeout(() => {
    recoveryRetryTimer = 0;
    void runCoordinatedNetworkRecovery('health-backoff');
  }, delay);
};

const runBoundedRecoveryHook = async (hook, reason, signal) => {
  let timer = 0;
  let abortHandler = null;
  const hookResult = Promise.resolve().then(() => hook(reason, { signal })).then(
    () => ({ outcome: 'complete' }),
    (error) => ({ outcome: 'failed', error })
  );
  const timeoutResult = new Promise((resolve) => {
    timer = window.setTimeout(() => resolve({ outcome: 'timeout' }), RECOVERY_HOOK_TIMEOUT_MS);
  });
  const abortResult = new Promise((resolve) => {
    abortHandler = () => resolve({ outcome: 'aborted' });
    if (signal.aborted) abortHandler();
    else signal.addEventListener('abort', abortHandler, { once: true });
  });
  const result = await Promise.race([hookResult, timeoutResult, abortResult]);
  if (timer) window.clearTimeout(timer);
  signal.removeEventListener?.('abort', abortHandler);
  if (result.outcome !== 'complete') {
    noteDiagnostic('recovery-hook-' + result.outcome, {
      reason,
      message: String(result.error?.message || result.error || ''),
    });
  }
  return result.outcome;
};

const runRecoveryPass = async (reason, controller) => {
  if (!online()) return false;
  setConnectivity({ network: 'recovering', phase: 'recovering', recoveryReason: reason, recoveryRetryScheduled: false });
  noteDiagnostic('recovery-start', { reason, waiters: waiters.size });
  let healthy = false;
  try {
    healthy = await healthProbe(controller.signal);
  } catch (error) {
    noteDiagnostic('recovery-failed', { reason, message: String(error?.message || error || '') });
  }
  if (controller.signal.aborted) return false;
  if (!healthy) {
    setConnectivity({ network: 'unstable', phase: 'unstable', lastFailureAt: Date.now() });
    scheduleRecoveryRetry();
    noteDiagnostic('recovery-complete', { reason, healthy: false, waiters: waiters.size });
    return false;
  }

  recoveryProbeAttempt = 0;
  setConnectivity({ network: 'recovering', phase: 'catching-up', consecutiveFailures: 0, lastSuccessAt: Date.now() });
  await Promise.all(Array.from(recoveryHooks, (hook) => runBoundedRecoveryHook(hook, reason, controller.signal)));
  if (!online() || controller.signal.aborted) return false;

  successfulRecoveryEpoch += 1;
  wakeAllNetworkRetries(reason);
  setConnectivity({ network: 'healthy', phase: '', consecutiveFailures: 0, lastRecoveryAt: Date.now(), recoveryRetryScheduled: false });
  noteDiagnostic('recovery-complete', { reason, healthy: true, waiters: waiters.size });
  return true;
};

const runCoordinatedNetworkRecovery = (reason = 'recovery') => {
  if (coordinatedRecoveryPromise) {
    if (activeRecoveryController?.signal.aborted && online()) {
      recoveryQueued = true;
      queuedRecoveryReason = String(reason || 'recovery');
    }
    return coordinatedRecoveryPromise;
  }
  if (recoveryRetryTimer) {
    window.clearTimeout(recoveryRetryTimer);
    recoveryRetryTimer = 0;
  }
  coordinatedRecoveryPromise = (async () => {
    let nextReason = String(reason || 'recovery');
    let result = false;
    do {
      recoveryQueued = false;
      activeRecoveryController = new AbortController();
      result = await runRecoveryPass(nextReason, activeRecoveryController);
      nextReason = queuedRecoveryReason || 'recovery';
      queuedRecoveryReason = '';
    } while (recoveryQueued && online());
    return result;
  })().finally(() => {
    activeRecoveryController = null;
    coordinatedRecoveryPromise = null;
  });
  return coordinatedRecoveryPromise;
};

const createResumableStreamRecovery = (options = {}) => {
  let attempt = 0;
  let failures = 0;
  let stopped = false;
  let attemptRecoveryEpoch = successfulRecoveryEpoch;
  const key = String(options.key || `stream-${Date.now()}-${Math.random()}`);
  const terminal = () => stopped || Boolean(options.isTerminal?.());
  const describe = (phase, reason = '', delay = 0) => {
    const detail = {
      phase,
      reason,
      delay,
      attempt,
      cursor: Math.max(0, Number(options.getCursor?.() || 0)),
      heartbeat: Boolean(options.heartbeat),
    };
    options.onStatus?.(detail);
    noteDiagnostic('stream-recovery', { key, ...detail });
  };
  return {
    key,
    get attempt() { return attempt; },
    noteAttempt() { attemptRecoveryEpoch = successfulRecoveryEpoch; },
    noteConnected() { failures = 0; describe('connected'); },
    noteActivity() { attempt = 0; failures = 0; },
    async wait(reason = 'stream-ended') {
      if (terminal()) return 'terminal';
      failures += 1;
      const delay = retryDelay(attempt);
      describe(online() ? 'waiting' : 'offline', reason, delay);
      if (options.reconcile && failures >= Math.max(1, Number(options.reconcileEvery) || 5)) {
        failures = 0;
        describe('reconciling', reason, 0);
        await options.reconcile(reason).catch((error) => noteDiagnostic('stream-reconcile-failed', { key, message: String(error?.message || error || '') }));
        if (terminal()) return 'terminal';
      }
      attempt += 1;
      return waitForNetworkRetry(delay, {
        key, reason, signal: options.signal, afterRecoveryEpoch: attemptRecoveryEpoch
      });
    },
    wake(reason = 'wake') { return wakeNetworkRetry(key, reason); },
    stop() { stopped = true; wakeNetworkRetry(key, 'detached'); },
  };
};

const connectivityHeaderStatus = () => {
  const connectivity = state?.connectivity || {};
  const phase = String(connectivity.phase || connectivity.network || '');
  if (phase === 'offline') return {
    text: Number(connectivity.pendingSafe || 0) > 0
      ? 'Offline — message pending safely; reconnect to continue'
      : 'Offline — reconnect to continue',
    mode: 'bad', priority: 3,
  };
  if (phase === 'recovering') return { text: 'Network restored — recovering…', mode: 'retry', priority: 0 };
  if (phase === 'catching-up') return { text: 'Catching up with the server…', mode: 'retry', priority: 0 };
  if (phase === 'unstable') return {
    text: connectivity.recoveryRetryScheduled ? 'Connection unstable — checking again shortly…' : 'Connection unstable',
    mode: 'bad', priority: 0,
  };
  return null;
};

if (typeof window.addEventListener === 'function') {
  window.addEventListener('offline', () => {
    activeRecoveryController?.abort();
    if (recoveryRetryTimer) {
      window.clearTimeout(recoveryRetryTimer);
      recoveryRetryTimer = 0;
    }
    for (const waiter of waiters.values()) suspendWaiter(waiter);
    setConnectivity({ network: 'offline', phase: 'offline', recoveryRetryScheduled: false });
    noteDiagnostic('offline', { waiters: waiters.size });
  });
  window.addEventListener('online', () => { void runCoordinatedNetworkRecovery('online'); });
  window.addEventListener('pageshow', (event) => {
    if (!online()) return;
    if (event?.persisted) {
      void runCoordinatedNetworkRecovery('pageshow');
      return;
    }
    // Initial startup requests already prove connectivity. Give them a bounded
    // head start, then bootstrap recovery only if none has succeeded.
    window.setTimeout(() => {
      if (state?.connected || state?.connectivity?.lastSuccessAt || !online()) return;
      void runCoordinatedNetworkRecovery('startup-pageshow');
    }, 2000);
  });
}
if (typeof document?.addEventListener === 'function') {
  document.addEventListener('visibilitychange', () => {
    if (visible() && online()) void runCoordinatedNetworkRecovery('visibility');
  });
}

Object.assign(app, {
  API_FETCH_POLICY: POLICY,
  API_FETCH_AUTH: AUTH,
  apiFetch,
  classifyAPIRequest: classifyRequest,
  retryAfterDelay,
  waitForNetworkRetry,
  wakeNetworkRetry,
  wakeAllNetworkRetries,
  cancelNetworkRetries,
  addNetworkRecoveryHook,
  runCoordinatedNetworkRecovery,
  createResumableStreamRecovery,
  connectivityHeaderStatus,
  networkDiagnostics: diagnostics,
  noteNetworkDiagnostic: noteDiagnostic,
});
})();
