import { runLinuxShell } from './linux-client.mjs';
import { installBrowserTransport, memoryStorage, installStaticNavigation } from './transport.mjs';

installStaticNavigation(window.history, location.href);
Object.defineProperty(window, 'localStorage', { configurable: true, value: memoryStorage() });
Object.defineProperty(window, 'sessionStorage', { configurable: true, value: memoryStorage() });
Object.assign(window, {
  TERM_LLM_UI_PREFIX: '/demo-api', TERM_LLM_SHELL_AUTHORIZED: true,
  TERM_LLM_UI_TITLE: 'term-llm · browser demo', TERM_LLM_AGENT_NAME: 'developer',
  TERM_LLM_APPROVAL_MODE: 'yolo',
  TERM_LLM_CACHE_SCOPE: '', TERM_LLM_WORKTREES_ENABLED: false,
  TERM_LLM_LOCATION_SHARING_ENABLED: false, TERM_LLM_PUSH_SUPPORTED: false,
});

const style = document.createElement('style');
style.textContent = `
.demo-gate,.demo-gate *{box-sizing:border-box}
.demo-gate{position:fixed;inset:0;z-index:2000;display:flex;flex-direction:column;align-items:center;overflow-y:auto;padding: max(24px,env(safe-area-inset-top)) max(20px,env(safe-area-inset-right)) max(24px,env(safe-area-inset-bottom)) max(20px,env(safe-area-inset-left));background:#16191c;color:#e9edf0;font:16px/1.5 system-ui;-webkit-text-size-adjust:100%;text-size-adjust:100%}
.demo-gate article{width:100%;max-width:480px;min-width:0;flex:none;margin:auto 0;padding:12px 0}
.demo-eyebrow{color:#94cdb9;font-size:12px;font-weight:600;letter-spacing:.12em;text-transform:uppercase}
.demo-gate h1{margin:12px 0 16px;font-size:clamp(26px,6vw,34px);line-height:1.15;letter-spacing:-.8px}
.demo-gate p{margin:0 0 16px;color:#b6c0c8;font-size:15px}
.demo-facts{display:grid;gap:8px;padding:0;margin:20px 0 24px;list-style:none;color:#b6c0c8;font-size:14px}
.demo-actions{display:flex;flex-direction:column;gap:12px;width:100%}
.demo-gate button{display:block;width:100%;min-height:48px;margin:0;padding:12px 16px;border:1px solid #3f8e76;border-radius:10px;background:#205c49;color:#fff;font:600 16px/1.35 system-ui;text-align:center;white-space:normal;cursor:pointer;touch-action:manipulation}
.demo-gate button.demo-secondary{background:transparent;border-color:#42504f;color:#d4dfdc}
.demo-gate button:disabled{opacity:.6;cursor:wait}.demo-gate [hidden]{display:none!important}
.demo-status{margin-top:20px;min-width:0}.demo-stage{font-size:14px;color:#b6e4d3;overflow-wrap:anywhere}
.demo-clock{margin-top:6px;color:#8d9b9f;font:12px/1.4 ui-monospace,monospace}
.demo-gate progress{display:block;width:100%;height:7px;margin:12px 0;accent-color:#72b99f}
.demo-gate details{margin-top:12px;color:#a6b7bc;font-size:13px}
.demo-gate pre{max-width:100%;max-height:160px;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;word-break:break-word;font:12px/1.5 ui-monospace,monospace}
`;
document.head.append(style);
const gate = document.createElement('section');
gate.className = 'demo-gate';
gate.innerHTML = `<article><div class="demo-eyebrow">Local browser demo</div><h1>term-llm, on your device.</h1><p>The real agent and web frontend. No account, API key, or application backend.</p><ul class="demo-facts"><li>Bonsai 2 2B · WebGPU inference</li><li>About 430 MB on first use · model can be cached</li><li>Chats and workspace files disappear on reset</li></ul><div class="demo-actions"><button id="demo-start" aria-label="Load Bonsai & start demo">Load Bonsai & start demo</button><button id="demo-inspect" class="demo-secondary" aria-label="Inspect workspace only">Inspect workspace only</button><button id="demo-cancel" class="demo-secondary" hidden>Cancel loading</button></div><div class="demo-status" role="status" aria-live="polite"><div id="demo-progress" class="demo-stage">Developer agent · YOLO · real Linux shell + Python (boots on first use). WebGPU required.</div><progress id="demo-meter" hidden></progress><div id="demo-clock" class="demo-clock"></div></div><details id="demo-error-details" hidden><summary>Error details</summary><pre id="demo-error"></pre></details></article>`;
document.body.append(gate);
const status = gate.querySelector('#demo-progress');
const clock = gate.querySelector('#demo-clock');
const meter = gate.querySelector('#demo-meter');
const start = gate.querySelector('#demo-start');
const inspect = gate.querySelector('#demo-inspect');
const cancel = gate.querySelector('#demo-cancel');
const errorDetails = gate.querySelector('#demo-error-details');
const errorText = gate.querySelector('#demo-error');
let shellAbort;
let worker, readyResolve, readyReject, modelResolve, modelReject;
let busy = false, opened = false, failed = false, startedAt = 0, lastProgress = 0, timer;
let currentStage = 'Starting demo';
const ready = new Promise((resolve, reject) => { readyResolve = resolve; readyReject = reject; });
ready.catch(() => {});

function fail(error, stage = currentStage) {
  const detail = new Error(`${stage}: ${error.message || error}`);
  detail.details = `${stage}\n${error.stack || error}`;
  readyReject(detail);
  modelReject?.(detail);
}

function startWorker() {
  worker = new Worker(new URL('./worker.mjs', import.meta.url), { type: 'module' });
  worker.addEventListener('message', ({ data }) => {
    lastProgress = Date.now();
    if (data.type === 'run-shell') {
      const controller = new AbortController();
      shellAbort = { id: data.id, controller };
      runLinuxShell(JSON.parse(data.request), { signal: controller.signal }).then(
        result => worker.postMessage({ type: 'shell-result', id: data.id, result: JSON.stringify(result) }),
        error => worker.postMessage({ type: 'shell-result', id: data.id, error: error.message }),
      ).finally(() => { if (shellAbort?.id === data.id) shellAbort = undefined; });
    } else if (data.type === 'cancel-shell') {
      if (shellAbort?.id === data.id) shellAbort.controller.abort();
    } else if (data.type === 'ready') readyResolve(data.info);
    else if (data.type === 'model-ready') modelResolve?.(data);
    else if (data.type === 'progress') {
      const event = data.event;
      const percent = typeof event.progress === 'number' ? ` · ${event.progress.toFixed(1)}%` : '';
      currentStage = `${event.status || 'Loading'}${event.file ? ' · ' + event.file : ''}`;
      status.textContent = currentStage + percent;
      if (typeof event.progress === 'number') { meter.max = 100; meter.value = event.progress; }
      else meter.removeAttribute('value');
    } else if (data.type === 'error' || data.type === 'fatal') {
      const error = new Error(data.error);
      if (data.stack) error.stack = data.stack;
      fail(error, data.stage || currentStage);
    }
  });
  worker.addEventListener('error', event => fail(event.error || new Error(`${event.message || 'Worker failed'}\n${event.filename || ''}:${event.lineno || 0}`)));
  worker.postMessage({ type: 'boot' });
}

async function openUI(withModel) {
  if (failed) { location.reload(); return; }
  if (busy) return;
  busy = true;
  startedAt = lastProgress = Date.now();
  delete window.__termLLMDemoError;
  start.disabled = inspect.disabled = true;
  start.textContent = 'Loading demo…';
  inspect.hidden = true;
  cancel.hidden = false;
  meter.hidden = false;
  status.textContent = 'Starting term-llm…';
  clock.textContent = '0:00 elapsed';
  timer = setInterval(() => {
    if (document.hidden) { lastProgress = Date.now(); return; }
    const elapsed = Math.floor((Date.now() - startedAt) / 1000);
    clock.textContent = `${Math.floor(elapsed / 60)}:${String(elapsed % 60).padStart(2, '0')} elapsed`;
    if (Date.now() - lastProgress > 90000) {
      fail(new Error('No progress for 90 seconds. The device may have stopped the worker; cancel or reload to retry.'));
      worker?.terminate();
    }
  }, 1000);
  try {
    // Paint the phone's loading state before creating/compiling the runtime.
    await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    startWorker();
    await ready;
    if (withModel) {
      const loaded = new Promise((resolve, reject) => { modelResolve = resolve; modelReject = reject; });
      worker.postMessage({ type: 'load-model' });
      await loaded;
    }
    installBrowserTransport(worker);
    const entry = window.TERM_LLM_DEMO_ENTRY;
    if (!entry) throw new Error('Build did not inject the frontend entry URL');
    await import(new URL(entry, location.href).href);
    opened = true;
    gate.remove();
    window.__termLLMDemo = Object.freeze({ worker, reset: () => { worker.terminate(); location.reload(); }, inferenceLoaded: withModel });
  } catch (error) {
    failed = true;
    window.__termLLMDemoError = error.details || error.stack || error.message;
    status.textContent = error.message || 'Demo could not start';
    errorText.textContent = window.__termLLMDemoError;
    errorDetails.hidden = false;
    errorDetails.open = true;
    start.textContent = 'Reload and retry';
    start.disabled = false;
    inspect.hidden = true;
    cancel.textContent = 'Reload page';
  } finally {
    busy = false;
    clearInterval(timer);
    meter.hidden = true;
    if (opened) worker.postMessage({ type: 'workspace' });
  }
}
cancel.addEventListener('click', () => { worker?.terminate(); location.reload(); });
start.addEventListener('click', () => openUI(true));
inspect.addEventListener('click', () => openUI(false));
