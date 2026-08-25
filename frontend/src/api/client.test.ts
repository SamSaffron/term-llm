import { describe, expect, it, vi } from 'vitest';
import { APIClient, APIError, decodeSSE } from './client';
import { readInjectedConfig } from '../app/config';

const config = readInjectedConfig({ TERM_LLM_UI_PREFIX: '/ui', TERM_LLM_UI_VERSION: 'v1' } as Window);

describe('API transport', () => {
  it('adds auth/version headers and preserves the base path', async () => {
    const request = vi.fn(async () => new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', request);
    const api = new APIClient(config, { getToken: () => 'secret', onAuthRequired: vi.fn() });
    await api.get('/v1/providers');
    expect(api.url('/ui/v1/sessions/s1/skill-runs/r1/events')).toBe('/ui/v1/sessions/s1/skill-runs/r1/events');
    expect(request).toHaveBeenCalledOnce();
    const [url, init] = request.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/ui/v1/providers');
    expect((init.headers as Headers).get('Authorization')).toBe('Bearer secret');
    expect((init.headers as Headers).get('X-Term-LLM-UI-Version')).toBe('v1');
  });

  it('coordinates auth-required and typed errors', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{"error":"bad token"}', { status: 401 })));
    const auth = vi.fn(); const api = new APIClient(config, { getToken: () => '', onAuthRequired: auth });
    await expect(api.get('/v1/providers')).rejects.toEqual(expect.objectContaining<Partial<APIError>>({ status: 401, message: 'bad token' }));
    expect(auth).toHaveBeenCalledOnce();
  });

  it('decodes fragmented CRLF and multiline SSE events', async () => {
    const chunks = ['event: response.output_text.delta\r\ndata: {"delta":', '"hi"}\r\nid: 2\r\n\r\n', ': keepalive\n\ndata: done\n\n'];
    const stream = new ReadableStream<Uint8Array>({ start(controller) { chunks.forEach((chunk) => controller.enqueue(new TextEncoder().encode(chunk))); controller.close(); } });
    const events = []; for await (const event of decodeSSE(stream)) events.push(event);
    expect(events).toEqual([{ event: 'response.output_text.delta', data: '{"delta":"hi"}', id: '2' }, { event: 'message', data: 'done', id: undefined }]);
  });
});
