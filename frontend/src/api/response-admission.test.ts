import { afterEach, describe, expect, it, vi } from 'vitest';
import { admitResponse, assertResponseBudget } from './response-admission';

afterEach(() => vi.useRealTimers());

describe('response admission bounds', () => {
  it('stops the deadline after an accepted response ID without timing out its open SSE body', async () => {
    vi.useFakeTimers();
    const parent = new AbortController();
    let receivedSignal!: AbortSignal;
    const body = new ReadableStream<Uint8Array>();
    const response = new Response(body, { headers: { 'x-response-id': 'r1' } });
    expect(
      await admitResponse(async (signal) => {
        receivedSignal = signal;
        return response;
      }, parent.signal),
    ).toBe(response);
    await vi.advanceTimersByTimeAsync(200_000);
    expect(receivedSignal.aborted).toBe(false);
    expect(body.locked).toBe(false);
    parent.abort();
    expect(receivedSignal.aborted).toBe(true);
  });

  it('preserves definitive error status if its body reader fails', async () => {
    const response = new Response(
      new ReadableStream({
        start(controller) {
          controller.error(new Error('body disconnected'));
        },
      }),
      { status: 413 },
    );
    const result = await admitResponse(async () => response, new AbortController().signal);
    expect(result.status).toBe(413);
    expect(await result.text()).toBe('');
  });

  it('rejects 33 MiB through the actual Hub proxy but allows a direct Hub-aware node up to 50 MiB', () => {
    const body = { input: [{ role: 'user', content: 'x'.repeat(33 * 1024 * 1024) }] };
    expect(() => assertResponseBudget(body, { nodeBasePath: '/chat/nodes/node-one' })).toThrow(
      'Hub 32 MiB request limit',
    );
    expect(() =>
      assertResponseBudget(body, {
        url: 'https://hub.example',
        nodeId: 'node-one',
        nodeName: 'Direct node',
      }),
    ).not.toThrow();
    expect(() => assertResponseBudget({ input: 'é'.repeat(26 * 1024 * 1024) }, null)).toThrow(
      'server 50 MiB request limit',
    );
  });
});
