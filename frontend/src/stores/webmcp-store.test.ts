import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  CLIENT_TOOL_PREFIX,
  MAX_CLIENT_TOOL_OUTPUT,
  type ClientTool,
} from '../domain/client-tools';
import { storageKeys } from '../platform/storage';
import type { PageToolHost } from '../platform/webmcp';
import type { AppStoreServices } from './app-store-services';
import { CLIENT_TOOL_TIMEOUT_MS, WebMCPStore } from './webmcp-store';

const ping: ClientTool = {
  name: 'ping',
  title: 'Ping',
  description: 'Check the phone',
  inputSchema: { type: 'object', properties: {} },
  readOnly: true,
};

function fakeHost(overrides: Partial<PageToolHost> = {}) {
  let listener: () => void = () => undefined;
  const host = {
    list: vi.fn(async () => [ping, { ...ping, name: 'not valid' }]),
    execute: vi.fn(
      async (_name: string, input: Record<string, unknown>) => `pong ${input.message}`,
    ),
    subscribe: vi.fn((next: () => void) => {
      listener = next;
      return () => (listener = () => undefined);
    }),
    provider: () => 'iPhone',
    ...overrides,
  } satisfies PageToolHost;
  return { host, changed: () => listener() };
}

const services = () =>
  ({ storage: localStorage, keys: storageKeys(null) }) as unknown as AppStoreServices;

/** A store that has loaded `host`'s tools. */
async function loaded(host: PageToolHost): Promise<WebMCPStore> {
  const store = new WebMCPStore(services(), host);
  await store.refresh();
  return store;
}

const call = (args = '{"message":"hi"}') => ({ callId: 'call_1', name: 'ping', arguments: args });

beforeEach(() => localStorage.clear());
afterEach(() => vi.useRealTimers());

describe('WebMCPStore', () => {
  it('loads usable page tools and follows changes', async () => {
    const { host, changed } = fakeHost();
    const store = new WebMCPStore(services(), host);
    store.start();
    await vi.waitFor(() => expect(store.available.value).toBe(true));
    expect(store.tools.value.map((tool) => tool.name)).toEqual(['ping']);
    expect(store.provider()).toBe('iPhone');

    vi.mocked(host.list).mockResolvedValueOnce([]);
    changed();
    await vi.waitFor(() => expect(store.available.value).toBe(false));
    store.dispose();
    changed();
    expect(host.list).toHaveBeenCalledTimes(2);
  });

  it('treats a failing page as having no tools', async () => {
    const store = new WebMCPStore(
      services(),
      fakeHost({ list: vi.fn(async () => Promise.reject(new Error('boom'))) }).host,
    );
    store.tools.value = [ping];
    await store.refresh();
    expect(store.tools.value).toEqual([]);
    expect(store.provider()).toBe('browser');
  });

  it('is off by default and remembers per-conversation choices', async () => {
    const store = await loaded(fakeHost().host);
    expect(store.enabledFor('s1')).toBe(false);
    expect(store.definitions('s1')).toEqual([]);

    store.setEnabled('s1', true);
    expect(store.definitions('s1').map((tool) => tool.name)).toEqual([`${CLIENT_TOOL_PREFIX}ping`]);
    expect(new WebMCPStore(services(), fakeHost().host).enabledFor('s1')).toBe(true);

    store.setEnabled('s1', false);
    expect(store.definitions('s1')).toEqual([]);
    expect(new WebMCPStore(services(), fakeHost().host).enabledFor('s1')).toBe(false);
  });

  it('makes the last choice the default for new conversations', async () => {
    const store = await loaded(fakeHost().host);
    store.setEnabled('s1', true);
    expect(store.enabledFor('s2')).toBe(true);
    expect(new WebMCPStore(services(), fakeHost().host).enabledFor('new')).toBe(true);

    store.setEnabled('s2', false);
    expect(store.enabledFor('s3')).toBe(false);
    expect(store.enabledFor('s1')).toBe(true);
  });

  it('keeps a conversation on the choice it first sent with', async () => {
    const store = await loaded(fakeHost().host);
    expect(store.definitions('old')).toEqual([]); // sent while the default was off
    store.setEnabled('s1', true); // default now on
    expect(store.enabledFor('old')).toBe(false);
    expect(store.enabledFor('untouched')).toBe(true);

    expect(store.definitions('pinned-on')).toHaveLength(1);
    store.setEnabled('s2', false);
    expect(store.definitions('pinned-on')).toHaveLength(1);
  });

  it('does not pin conversations while the page has no tools', () => {
    const store = new WebMCPStore(services(), fakeHost().host);
    expect(store.definitions('s1')).toEqual([]);
    expect(localStorage.getItem(storageKeys(null).webMCPSessions)).toBeNull();
    store.setEnabled('other', true);
    expect(store.enabledFor('s1')).toBe(true);
  });

  it('carries a draft choice to the durable conversation', () => {
    const store = new WebMCPStore(services(), fakeHost().host);
    store.setEnabled('draft_1', true);
    store.rekey('draft_1', 's1');
    expect(store.enabledFor('s1')).toBe(true);
    store.setEnabled('draft_2', false);
    expect(store.enabledFor('draft_1')).toBe(false); // falls back to the default again
    store.rekey('draft_missing', 's2');
    expect(store.enabledFor('s2')).toBe(false);

    store.setEnabled('draft_3', true);
    store.rekey('draft_3', 's1');
    const stored = JSON.parse(localStorage.getItem(storageKeys(null).webMCPSessions)!);
    expect(stored).toEqual([
      ['draft_2', false],
      ['s1', true],
    ]);
  });

  it('keeps conversations turned off before page tools were off by default', () => {
    const keys = storageKeys(null);
    localStorage.setItem(keys.webMCPLegacyDisabledSessions, JSON.stringify(['s1', 7]));
    const store = new WebMCPStore(services(), fakeHost().host);
    expect(localStorage.getItem(keys.webMCPLegacyDisabledSessions)).toBeNull();
    store.setEnabled('s2', true);
    expect(store.enabledFor('s1')).toBe(false);
    expect(store.enabledFor('s3')).toBe(true);
  });

  it('keeps default and choices separate per Hub node', () => {
    const hub = (nodeId: string) =>
      ({
        storage: localStorage,
        keys: storageKeys({ nodeId, nodeBasePath: `/n/${nodeId}` } as never),
      }) as unknown as AppStoreServices;
    new WebMCPStore(hub('a'), fakeHost().host).setEnabled('s1', true);
    expect(new WebMCPStore(hub('a'), fakeHost().host).enabledFor('new')).toBe(true);
    expect(new WebMCPStore(hub('b'), fakeHost().host).enabledFor('new')).toBe(false);
  });

  it('follows and keeps choices made in another tab', () => {
    const tabA = new WebMCPStore(services(), fakeHost().host);
    const tabB = new WebMCPStore(services(), fakeHost().host);
    tabA.setEnabled('s1', true);
    expect(tabB.enabledFor('s1')).toBe(false);
    tabB.reloadSettings();
    expect(tabB.enabledFor('s1')).toBe(true);
    expect(tabB.enabledFor('new')).toBe(true);

    // A stale tab's own change must not undo the other tab's.
    const stale = new WebMCPStore(services(), fakeHost().host);
    tabA.setEnabled('s2', true);
    stale.setEnabled('s3', true);
    tabA.reloadSettings();
    expect(['s1', 's2', 's3'].map((id) => tabA.enabledFor(id))).toEqual([true, true, true]);
  });

  it('keeps the newest tool listing when listings finish out of order', async () => {
    const answers: Array<(tools: ClientTool[]) => void> = [];
    const store = new WebMCPStore(
      services(),
      fakeHost({ list: vi.fn(() => new Promise<ClientTool[]>((resolve) => answers.push(resolve))) })
        .host,
    );
    const older = store.refresh();
    const newer = store.refresh();
    answers[1]([]);
    await newer;
    answers[0]([ping]);
    await older;
    expect(store.tools.value).toEqual([]);
  });

  it('runs only tools the page currently offers', async () => {
    const { host } = fakeHost();
    const store = new WebMCPStore(services(), host);
    await expect(store.run(call(), new AbortController().signal)).resolves.toEqual({
      ok: false,
      output: 'Error: this page no longer provides the tool ping',
    });
    expect(host.execute).not.toHaveBeenCalled();
  });

  it('limits what a tool can send back', async () => {
    const huge = fakeHost({ execute: vi.fn(async () => 'x'.repeat(MAX_CLIENT_TOOL_OUTPUT + 5)) });
    const result = await (await loaded(huge.host)).run(call(), new AbortController().signal);
    expect(result.output).toBe(`${'x'.repeat(MAX_CLIENT_TOOL_OUTPUT)}\n[truncated 5 characters]`);
  });

  it('runs tools with parsed arguments', async () => {
    const { host } = fakeHost();
    const store = await loaded(host);
    await expect(store.run(call(), new AbortController().signal)).resolves.toEqual({
      ok: true,
      output: 'pong hi',
    });
    expect(host.execute).toHaveBeenCalledWith('ping', { message: 'hi' }, expect.any(AbortSignal));
  });

  it('reports failures to the model instead of throwing', async () => {
    const failing = fakeHost({
      execute: vi.fn(async () => Promise.reject(new Error('MusicKit is not authorized'))),
    });
    const store = await loaded(failing.host);
    await expect(store.run(call(), new AbortController().signal)).resolves.toEqual({
      ok: false,
      output: 'Error: MusicKit is not authorized',
    });
    await expect(store.run(call('[1]'), new AbortController().signal)).resolves.toEqual({
      ok: false,
      output: 'Error: Tool arguments must be a JSON object',
    });
    expect(failing.host.execute).toHaveBeenCalledTimes(1);
  });

  it('gives up on a tool that never answers', async () => {
    vi.useFakeTimers();
    const hung = fakeHost({ execute: vi.fn(() => new Promise<never>(() => undefined)) });
    const result = (await loaded(hung.host)).run(call(), new AbortController().signal);
    await vi.advanceTimersByTimeAsync(CLIENT_TOOL_TIMEOUT_MS);
    await expect(result).resolves.toEqual({ ok: false, output: 'Error: timed out after 60s' });
  });

  it('stops when the caller aborts', async () => {
    const hung = fakeHost({ execute: vi.fn(() => new Promise<never>(() => undefined)) });
    const controller = new AbortController();
    const store = await loaded(hung.host);
    const result = store.run(call(), controller.signal);
    controller.abort(new Error('Stopped by the user'));
    await expect(result).resolves.toEqual({ ok: false, output: 'Error: Stopped by the user' });

    const already = new AbortController();
    already.abort(new Error('Stopped by the user'));
    await expect(store.run(call(), already.signal)).resolves.toEqual({
      ok: false,
      output: 'Error: Stopped by the user',
    });
    expect(hung.host.execute).toHaveBeenCalledTimes(1);
  });
});
