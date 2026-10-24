import type { AppConfig } from '../app/config';
import { APIError } from './client';

/** Bounds only response admission, never an accepted response's SSE lifetime. */
export async function admitResponse(
  submit: (signal: AbortSignal) => Promise<Response>,
  signal: AbortSignal,
  timeoutMs = 30_000,
): Promise<Response> {
  const controller = new AbortController();
  const abort = () => controller.abort(signal.reason);
  if (signal.aborted) abort();
  else signal.addEventListener('abort', abort, { once: true });
  let received: Response | undefined;
  let accepted = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<Response>((resolve, reject) => {
    timer = setTimeout(() => {
      // Received 4xx is definitive even when a proxy never finishes its body.
      if (
        received &&
        received.status >= 400 &&
        received.status < 500 &&
        ![408, 425, 429].includes(received.status)
      ) {
        resolve(new Response('', { status: received.status, headers: received.headers }));
      } else {
        reject(new DOMException('Could not confirm response admission', 'TimeoutError'));
      }
      controller.abort(new DOMException('Response admission timed out', 'TimeoutError'));
    }, timeoutMs);
  });
  try {
    return await Promise.race([
      deadline,
      (async () => {
        received = await submit(controller.signal);
        if (received.ok && received.headers.get('x-response-id')) {
          accepted = true;
          return received;
        }
        if (received.ok) throw new Error('Server did not return a response id.');
        let body = '';
        try {
          body = await received.text();
        } catch (error) {
          if (
            received.status < 400 ||
            received.status >= 500 ||
            [408, 425, 429].includes(received.status)
          )
            throw error;
          // The rejection status remains authoritative if reading its body fails.
        }
        return new Response(body, { status: received.status, headers: received.headers });
      })(),
    ]);
  } finally {
    clearTimeout(timer);
    // An accepted body still belongs to the supervisor's cancellation signal.
    // For an unaccepted response, no parent listener needs to remain attached.
    if (!accepted) signal.removeEventListener('abort', abort);
  }
}

/** Actual encoded JSON budget, not decoded attachment sizes or an estimate. */
export function assertResponseBudget(body: unknown, hub: AppConfig['hub']): void {
  // Direct nodes can advertise Hub discovery context without using its proxy.
  const hubRoute = Boolean(hub?.nodeBasePath);
  const limit = (hubRoute ? 32 : 50) * 1024 * 1024;
  if (new TextEncoder().encode(JSON.stringify(body)).byteLength > limit)
    throw new APIError(
      `This message exceeds the ${hubRoute ? 'Hub 32' : 'server 50'} MiB request limit. ` +
        'Remove some attachments or send them in separate messages. This server may need an upgrade for separate file uploads.',
      413,
    );
}
