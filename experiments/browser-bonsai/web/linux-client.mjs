let worker;
let busy = false;
// Preserve the paused guest between commands; discard it on cancel/error. The
// Go workspace is authoritative and is re-sent on the next invocation.
export function runLinuxShell(request, { signal } = {}) {
  if (busy) return Promise.reject(new Error('Linux shell is already running'));
  if (signal?.aborted) return Promise.reject(signal.reason);
  busy = true;
  try { worker ??= new Worker(new URL('./linux-worker.mjs', import.meta.url), { type: 'module' }); }
  catch (error) { busy = false; return Promise.reject(error); }
  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (error, result) => {
      if (settled) return;
      settled = true;
      clearTimeout(deadline);
      signal?.removeEventListener('abort', abort);
      worker?.removeEventListener('message', message);
      worker?.removeEventListener('error', failure);
      if (error) { worker?.terminate(); worker = undefined; }
      busy = false;
      error ? reject(error) : resolve(result);
    };
    const abort = () => finish(signal.reason ?? new DOMException('Aborted', 'AbortError'));
    const message = ({ data }) => finish(data.error ? new Error(data.error) : null, data.result);
    const failure = event => finish(new Error(event.message || 'Linux worker failed'));
    const deadline = setTimeout(() => finish(new Error('Linux startup/execution deadline exceeded; guest reset')), 180000 + Math.min(30, Number(request.timeout_seconds) || 10) * 1000);
    signal?.addEventListener('abort', abort, { once: true });
    worker.addEventListener('message', message);
    worker.addEventListener('error', failure);
    worker.postMessage(request);
  });
}
