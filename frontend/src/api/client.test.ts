import { describe, expect, it, vi } from 'vitest';
import { APIClient, APIError, decodeSSE } from './client';
import { readInjectedConfig } from '../app/config';
import { endpoints } from './endpoints';

const config = readInjectedConfig({
  TERM_LLM_UI_PREFIX: '/ui',
  TERM_LLM_UI_VERSION: 'v1',
} as Window);

describe('API transport', () => {
  it('adds auth/version headers and preserves the base path', async () => {
    const request = vi.fn(
      async () =>
        new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
    );
    vi.stubGlobal('fetch', request);
    const api = new APIClient(config, { getToken: () => 'secret', onAuthRequired: vi.fn() });
    await api.get('/v1/providers');
    expect(api.url('/ui/v1/sessions/s1/skill-runs/r1/events')).toBe(
      '/ui/v1/sessions/s1/skill-runs/r1/events',
    );
    expect(request).toHaveBeenCalledOnce();
    const [url, init] = request.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/ui/v1/providers');
    expect((init.headers as Headers).get('Authorization')).toBe('Bearer secret');
    expect((init.headers as Headers).get('X-Term-LLM-UI-Version')).toBe('v1');
    expect((init as RequestInit & { __termLLMRetrySafe?: boolean }).__termLLMRetrySafe).toBe(true);
  });

  it('never forwards the stored bearer token to a foreign origin', async () => {
    const request = vi.fn(
      async () =>
        new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
    );
    vi.stubGlobal('fetch', request);
    const api = new APIClient(config, { getToken: () => 'secret', onAuthRequired: vi.fn() });
    await api.request(
      'https://elsewhere.test/v1/data',
      { headers: { Authorization: 'Bearer accidental' } },
      { policy: 'safe-read' },
    );
    const [, init] = request.mock.calls[0] as unknown as [string, RequestInit];
    expect((init.headers as Headers).has('Authorization')).toBe(false);
    expect(init.credentials).toBe('omit');
  });

  it('coordinates auth-required and typed errors', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('{"error":"bad token"}', { status: 401 })),
    );
    const auth = vi.fn();
    const api = new APIClient(config, { getToken: () => '', onAuthRequired: auth });
    await expect(api.get('/v1/providers')).rejects.toEqual(
      expect.objectContaining<Partial<APIError>>({ status: 401, message: 'bad token' }),
    );
    expect(auth).toHaveBeenCalledOnce();
  });

  it('sends session ownership on every session-bound skill request', async () => {
    const request = vi.fn(
      async () =>
        new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
    );
    vi.stubGlobal('fetch', request);
    const api = new APIClient(config, { getToken: () => 'secret', onAuthRequired: vi.fn() });
    const skills = endpoints(api);
    await skills.skills('session/1');
    await skills.invokeSkill('session/1', { name: 'review' }, 'request-1');
    await skills.skillRun('session/1', 'run/1');
    await skills.cancelSkillRun('session/1', 'run/1');
    expect(request).toHaveBeenCalledTimes(4);
    const calls = request.mock.calls as unknown as Array<[string, RequestInit]>;
    for (const [, init] of calls) {
      expect(new Headers(init.headers).get('X-Term-LLM-Session-ID')).toBe('session/1');
    }
    expect(calls.map(([url]) => String(url))).toEqual([
      '/ui/v1/sessions/session%2F1/skills',
      '/ui/v1/sessions/session%2F1/skills/invoke',
      '/ui/v1/sessions/session%2F1/skill-runs/run%2F1',
      '/ui/v1/sessions/session%2F1/skill-runs/run%2F1',
    ]);
  });

  it('decodes fragmented CRLF and multiline SSE events', async () => {
    const chunks = [
      'event: response.output_text.delta\r\ndata: {"delta":',
      '"hi"}\r\nid: 2\r\n\r\n',
      ': keepalive\n\ndata: done\n\n',
    ];
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        chunks.forEach((chunk) => controller.enqueue(new TextEncoder().encode(chunk)));
        controller.close();
      },
    });
    const events = [];
    for await (const event of decodeSSE(stream)) events.push(event);
    expect(events).toEqual([
      { event: 'response.output_text.delta', data: '{"delta":"hi"}', id: '2' },
      { event: 'message', data: 'done', id: undefined },
    ]);
  });
});
