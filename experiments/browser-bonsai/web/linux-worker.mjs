import { LinuxSandbox } from './linux.mjs';
const sandbox = new LinuxSandbox();
let busy = false;
const fail = error => postMessage({ error: error?.message ?? String(error) });
self.addEventListener('unhandledrejection', event => { event.preventDefault(); fail(event.reason); });
self.addEventListener('message', async ({ data }) => {
  if (busy) return fail(new Error('Linux shell is already running'));
  busy = true;
  try { postMessage({ result: await sandbox.execute(data) }); }
  catch (error) { fail(error); }
  finally { busy = false; }
});
