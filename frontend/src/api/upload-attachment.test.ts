import { afterEach, describe, expect, it, vi } from 'vitest';
import { APIClient, APIError } from './client';
import { endpoints } from './endpoints';
import { readInjectedConfig } from '../app/config';

const config = readInjectedConfig({
  TERM_LLM_UI_PREFIX: '/ui',
  TERM_LLM_UI_VERSION: 'build-one',
} as Window);
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('binary upload endpoint', () => {
  it('uses exact raw /uploads, URI filename, original MIME, auth/version/progress, and waits for 201', async () => {
    const requests: FakeXHR[] = [];
    class FakeXHR {
      status = 201;
      responseText = JSON.stringify({
        upload_id: `upl_${'a'.repeat(43)}`,
        filename: 'my photo.png',
        media_type: 'image/png',
        size_bytes: 4,
      });
      upload: { onprogress?: (event: unknown) => void } = {};
      headers: Record<string, string> = {};
      timeout = 0;
      withCredentials = false;
      open = vi.fn();
      send = vi.fn();
      abort = vi.fn();
      onload = () => {};
      ontimeout = () => {};
      setRequestHeader(name: string, value: string) {
        this.headers[name] = value;
      }
      getAllResponseHeaders = () => 'X-Term-LLM-UI-Version: build-one';
      constructor() {
        requests.push(this);
      }
    }
    vi.stubGlobal('XMLHttpRequest', FakeXHR);
    const api = new APIClient(config, { getToken: () => 'test-token', onAuthRequired: vi.fn() });
    const progress = vi.fn();
    const blob = new Blob(['data'], { type: 'image/png' });
    const uploading = endpoints(api).uploadAttachment(blob, 'my photo #é.png', {
      onProgress: progress,
    });
    const xhr = requests[0];
    expect(xhr.open).toHaveBeenCalledWith('POST', '/ui/uploads', true);
    expect(xhr.send).toHaveBeenCalledWith(blob);
    expect(xhr.timeout).toBe(7 * 60_000);
    expect(xhr.headers).toMatchObject({
      'Content-Type': 'image/png',
      'X-Term-LLM-Upload-Filename': encodeURIComponent('my photo #é.png'),
      Authorization: 'Bearer test-token',
      'X-Term-LLM-UI-Version': 'build-one',
    });
    expect(xhr.withCredentials).toBe(true);
    xhr.upload.onprogress?.({ loaded: 4, total: 4, lengthComputable: true });
    expect(progress).toHaveBeenCalledWith(4, 4);
    xhr.onload();
    await expect(uploading).resolves.toMatchObject({ upload_id: `upl_${'a'.repeat(43)}` });
  });

  it('retries writer saturation only once and honors Retry-After, keeping the same Blob and controls', async () => {
    vi.useFakeTimers();
    const api = new APIClient(config, { getToken: () => '', onAuthRequired: vi.fn() });
    const saturation = new APIError('writers busy', 503);
    saturation.retryAfterMs = 2_000;
    vi.spyOn(api, 'upload').mockRejectedValue(saturation);
    const blob = new Blob(['bytes']);
    const result = endpoints(api)
      .uploadAttachment(blob, 'file.bin')
      .catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(1999);
    expect(api.upload).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(1);
    expect(await result).toBe(saturation);
    expect(api.upload).toHaveBeenCalledTimes(2);
    expect(vi.mocked(api.upload).mock.calls[1][1]).toBe(blob);
    expect(vi.mocked(api.upload).mock.calls[0][2]?.headers?.['Content-Type']).toBe(
      'application/octet-stream',
    );
    await vi.advanceTimersByTimeAsync(300_000);
    expect(api.upload).toHaveBeenCalledTimes(2);
  });

  it('aborts saturation backoff without starting a second request', async () => {
    vi.useFakeTimers();
    const api = new APIClient(config, { getToken: () => '', onAuthRequired: vi.fn() });
    vi.spyOn(api, 'upload').mockRejectedValue(new APIError('writers busy', 503));
    const controller = new AbortController();
    const result = endpoints(api)
      .uploadAttachment(new Blob(['bytes']), 'file', { signal: controller.signal })
      .catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(0);
    controller.abort();
    expect(await result).toMatchObject({ name: 'AbortError' });
    await vi.advanceTimersByTimeAsync(30_000);
    expect(api.upload).toHaveBeenCalledOnce();
  });

  it('sends the frozen instance precondition and disables inner createResponse retries', async () => {
    const api = new APIClient(config, { getToken: () => '', onAuthRequired: vi.fn() });
    vi.spyOn(api, 'request').mockResolvedValue(new Response());
    await endpoints(api).createResponse({ stream: true }, 's1', 'request-1', undefined, undefined, {
      serverInstance: 'frozen-instance',
    });
    expect(api.request).toHaveBeenCalledWith(
      '/v1/responses',
      expect.objectContaining({
        headers: expect.objectContaining({ 'X-Term-LLM-Server-Instance': 'frozen-instance' }),
      }),
      expect.objectContaining({ retries: 0, timeoutMs: 0 }),
    );
  });
});
