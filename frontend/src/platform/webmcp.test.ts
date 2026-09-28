import { afterEach, describe, expect, it, vi } from 'vitest';
import { pageToolHost } from './webmcp';

type TestWindow = Window & { __termLLMDeviceTools?: unknown };

function installModelContext(tools: unknown[], executeTool = vi.fn(async () => 'ok')) {
  const context = Object.assign(new EventTarget(), {
    getTools: vi.fn(async () => tools),
    executeTool,
  });
  Object.defineProperty(document, 'modelContext', { value: context, configurable: true });
  return context;
}

afterEach(() => {
  delete (document as { modelContext?: unknown }).modelContext;
  delete (window as TestWindow).__termLLMDeviceTools;
});

describe('pageToolHost', () => {
  it('reports no tools when the page has no WebMCP', async () => {
    const host = pageToolHost();
    expect(await host.list()).toEqual([]);
    expect(host.provider()).toBe('');
    expect(host.subscribe(() => undefined)).toBeTypeOf('function');
    await expect(host.execute('ping', {}, new AbortController().signal)).rejects.toThrow(
      'no longer provides tools',
    );
  });

  it('normalizes tool descriptors, including string schemas', async () => {
    installModelContext([
      {
        name: 'ping',
        title: 'Ping',
        description: 'Check reachability',
        inputSchema: { type: 'object', properties: { message: { type: 'string' } } },
        annotations: { readOnlyHint: true },
      },
      { name: 'legacy', inputSchema: '{"type":"object","properties":{"a":{"type":"number"}}}' },
      { name: 'broken', inputSchema: '{nope' },
      { title: 'nameless' },
    ]);
    expect(await pageToolHost().list()).toEqual([
      {
        name: 'ping',
        title: 'Ping',
        description: 'Check reachability',
        inputSchema: { type: 'object', properties: { message: { type: 'string' } } },
        readOnly: true,
      },
      {
        name: 'legacy',
        title: '',
        description: '',
        inputSchema: { type: 'object', properties: { a: { type: 'number' } } },
        readOnly: false,
      },
      {
        name: 'broken',
        title: '',
        description: '',
        inputSchema: { type: 'object', properties: {} },
        readOnly: false,
      },
    ]);
  });

  it('executes with the descriptor getTools returned, and forwards the signal', async () => {
    const descriptor = { name: 'ping', description: 'Ping' };
    const executeTool = vi.fn(async () => 'pong');
    installModelContext([descriptor], executeTool);
    const host = pageToolHost();
    await host.list();
    const signal = new AbortController().signal;
    await expect(host.execute('ping', { message: 'hi' }, signal)).resolves.toBe('pong');
    expect(executeTool).toHaveBeenCalledWith(descriptor, { message: 'hi' }, { signal });
  });

  it('follows toolchange events until unsubscribed', () => {
    const context = installModelContext([]);
    const listener = vi.fn();
    const unsubscribe = pageToolHost().subscribe(listener);
    context.dispatchEvent(new Event('toolchange'));
    unsubscribe();
    context.dispatchEvent(new Event('toolchange'));
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('names the device that injected the tools', () => {
    installModelContext([]);
    (window as TestWindow).__termLLMDeviceTools = { device: 'iPhone' };
    expect(pageToolHost().provider()).toBe('iPhone');
    (window as TestWindow).__termLLMDeviceTools = { device: 7 };
    expect(pageToolHost().provider()).toBe('');
    (window as TestWindow).__termLLMDeviceTools = {
      device: `Sam's\n\u202eiPhone${'!'.repeat(60)}`,
    };
    expect(pageToolHost().provider()).toBe(`Sam's iPhone${'!'.repeat(28)}`);
  });

  it('shows and runs the first of tools that share a name', async () => {
    const first = { name: 'ping', description: 'First' };
    const executeTool = vi.fn(async () => 'pong');
    installModelContext([first, { name: 'ping', description: 'Second' }], executeTool);
    const host = pageToolHost();
    expect((await host.list()).map((tool) => tool.description)).toEqual(['First']);
    await host.execute('ping', {}, new AbortController().signal);
    expect(executeTool).toHaveBeenCalledWith(first, {}, expect.anything());
  });

  it('runs the descriptor of the newest listing when listings finish out of order', async () => {
    const answers: Array<(tools: unknown[]) => void> = [];
    const executeTool = vi.fn(async () => 'pong');
    const context = installModelContext([], executeTool);
    context.getTools.mockImplementation(
      () => new Promise<unknown[]>((resolve) => answers.push(resolve)),
    );
    const host = pageToolHost();
    const older = host.list();
    const newer = host.list();
    const fresh = { name: 'ping', description: 'Fresh' };
    answers[1]([fresh]);
    await newer;
    answers[0]([{ name: 'ping', description: 'Stale' }, null]);
    await older;
    await host.execute('ping', {}, new AbortController().signal);
    expect(executeTool).toHaveBeenCalledWith(fresh, {}, expect.anything());
  });

  it('refuses tools the page did not list', async () => {
    const executeTool = vi.fn(async () => 'pong');
    installModelContext([{ name: 'ping' }], executeTool);
    const host = pageToolHost();
    await host.list();
    await expect(host.execute('wipe', {}, new AbortController().signal)).rejects.toThrow(
      'does not provide the tool wipe',
    );
    expect(executeTool).not.toHaveBeenCalled();
  });

  it('finds tools a native host injects after this script ran', () => {
    const readyState = vi.spyOn(document, 'readyState', 'get').mockReturnValue('loading');
    try {
      const listener = vi.fn();
      const unsubscribe = pageToolHost().subscribe(listener);
      const context = installModelContext([]);
      window.dispatchEvent(new Event('load'));
      expect(listener).toHaveBeenCalledTimes(1);
      context.dispatchEvent(new Event('toolchange'));
      expect(listener).toHaveBeenCalledTimes(2);
      unsubscribe();
      context.dispatchEvent(new Event('toolchange'));
      expect(listener).toHaveBeenCalledTimes(2);
    } finally {
      readyState.mockRestore();
    }
  });
});
