import { APIClient, APIError, type UploadControls } from './client';

export interface UploadedAttachment {
  upload_id: string;
  filename: string;
  media_type: string;
  size_bytes: number;
  download_url?: string;
}

function waitForUploadSlot(delay: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const abort = () => {
      clearTimeout(timer);
      signal?.removeEventListener('abort', abort);
      reject(signal?.reason || new DOMException('Upload cancelled', 'AbortError'));
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', abort);
      resolve();
    }, delay);
    if (signal?.aborted) abort();
    else signal?.addEventListener('abort', abort, { once: true });
  });
}

/** One bounded saturation retry; ambiguous uploads are left to explicit Retry. */
export async function uploadAttachment(
  api: APIClient,
  blob: Blob,
  filename: string,
  controls: UploadControls = {},
): Promise<UploadedAttachment> {
  const send = () =>
    api.upload<UploadedAttachment>('/uploads', blob, {
      ...controls,
      // Two uploads can queue behind bounded three-minute reverse-Hub writes.
      // Keep a finite client timeout, but do not expire before that transport budget.
      timeoutMs: 7 * 60_000,
      expectedStatus: 201,
      headers: {
        'Content-Type': blob.type || 'application/octet-stream',
        'X-Term-LLM-Upload-Filename': encodeURIComponent(filename),
      },
    });
  try {
    return await send();
  } catch (error) {
    if (!(error instanceof APIError) || error.status !== 503) throw error;
    await waitForUploadSlot(error.retryAfterMs || 1_000, controls.signal);
    return send();
  }
}
