import { afterEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '../api/client';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

const rows = Array.from({ length: 60 }, (_, index) => ({
  id: index + 1,
  sequence: index,
  role: index % 2 === 0 ? 'user' : 'assistant',
  parts: [{ type: 'text', text: `Message ${index + 1}` }],
}));
const stores: AppStore[] = [];
afterEach(() => stores.splice(0).forEach((store) => store.dispose()));

async function setup() {
  const store = new AppStore(testConfig);
  stores.push(store);
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.endpoints.sessionState = vi.fn(async () => ({}));
  store.endpoints.selectedSession = vi.fn(async () => ({
    selected_session: { id: 's1', transcript_rev: 7 },
    selected_transcript: {
      index: { rev: 7, rows: { ids: rows.map((row) => row.id), roles: 'ua'.repeat(30) } },
      bodies: { rev: 7, messages: rows.slice(-18) },
    },
  }));
  store.endpoints.transcriptBodies = vi.fn(async (_id, anchors) => ({
    rev: 7,
    messages: rows.filter((row) => anchors.includes(row.role === 'user' ? row.id : row.id - 1)),
  }));
  await store.loadSession('s1');
  return store;
}

describe('older transcript pagination', () => {
  it('loads beyond the nine-turn sideload until the actual first message with no duplicates', async () => {
    const store = await setup();
    expect(store.activeSession.value?.messages[0].content).toBe('Message 43');
    const beforePrepend = vi.fn();
    for (let page = 0; page < 3; page++)
      await store.selectionStore.loadOlderMessages(beforePrepend);
    expect(store.activeSession.value?.messages.map((message) => message.content)).toEqual(
      rows.map((row) => row.parts[0].text),
    );
    expect(store.activeSession.value?.olderTranscriptAnchors).toEqual([]);
    expect(beforePrepend).toHaveBeenCalledTimes(3);
    await store.selectionStore.loadOlderMessages();
    expect(store.endpoints.transcriptBodies).toHaveBeenCalledTimes(3);
    expect(store.endpoints.transcriptBodies).toHaveBeenNthCalledWith(
      1,
      's1',
      [25, 27, 29, 31, 33, 35, 37, 39, 41],
      expect.any(AbortSignal),
    );
  });

  it('serializes page requests and leaves failed pages available for retry', async () => {
    const store = await setup();
    const fetch = store.endpoints.transcriptBodies;
    let reject!: (error: Error) => void;
    store.endpoints.transcriptBodies = vi.fn(
      () =>
        new Promise<Record<string, unknown>>((_resolve, fail) => {
          reject = fail;
        }),
    );
    const pending = store.selectionStore.loadOlderMessages();
    await store.selectionStore.loadOlderMessages();
    expect(store.endpoints.transcriptBodies).toHaveBeenCalledOnce();
    reject(new Error('offline'));
    await pending;
    expect(store.selectionStore.historyError.value).toBe('s1');
    expect(store.activeSession.value?.olderTranscriptAnchors).toHaveLength(21);
    store.endpoints.transcriptBodies = fetch;
    await store.selectionStore.loadOlderMessages();
    expect(store.selectionStore.historyError.value).toBe('');
    expect(store.activeSession.value?.olderTranscriptAnchors).toHaveLength(12);
  });

  it('does not advance the cursor for an incomplete page', async () => {
    const store = await setup();
    store.endpoints.transcriptBodies = vi.fn(async () => ({ rev: 7, messages: [] }));
    await store.selectionStore.loadOlderMessages();
    expect(store.activeSession.value?.olderTranscriptAnchors).toHaveLength(21);
    expect(store.selectionStore.historyError.value).toBe('s1');
  });

  it.each(['selection', 'refresh', 'dispose'])('discards a late page after %s', async (change) => {
    const store = await setup();
    let resolve!: (value: Record<string, unknown>) => void;
    store.endpoints.transcriptBodies = vi.fn(
      () =>
        new Promise<Record<string, unknown>>((done) => {
          resolve = done;
        }),
    );
    const beforePrepend = vi.fn();
    const pending = store.selectionStore.loadOlderMessages(beforePrepend);
    if (change === 'selection') {
      store.sessions.value = [...store.sessions.value, testSession({ id: 's2' })];
      store.activeSessionId.value = 's2';
    } else if (change === 'refresh') {
      const selected = await store.endpoints.selectedSession('s1');
      store.endpoints.selectedSession = vi.fn(async () => ({
        ...selected,
        selected_transcript: { bodies: { rev: 8, messages: rows.slice(-18) } },
      }));
      await store.loadSession('s1');
    } else store.selectionStore.dispose();
    resolve({ rev: 7, messages: rows.slice(24, 42) });
    await pending;
    expect(store.sessions.value[0].messages).toHaveLength(18);
    expect(beforePrepend).not.toHaveBeenCalled();
  });

  it('refreshes rather than splicing bodies from a changed transcript revision', async () => {
    const store = await setup();
    const beforePrepend = vi.fn();
    store.endpoints.transcriptBodies = vi.fn(async () => ({
      rev: 8,
      messages: rows.slice(24, 42),
    }));
    await store.selectionStore.loadOlderMessages(beforePrepend);
    expect(store.endpoints.selectedSession).toHaveBeenCalledTimes(2);
    expect(store.activeSession.value?.messages).toHaveLength(18);
    expect(beforePrepend).not.toHaveBeenCalled();
  });
  it.each(['selection', 'run'])(
    'keeps loaded history through a same-revision %s refresh',
    async (refresh) => {
      const store = await setup();
      await store.selectionStore.loadOlderMessages();
      await store.selectionStore.loadOlderMessages();
      const anchors = store.activeSession.value!.olderTranscriptAnchors;
      if (refresh === 'selection') await store.loadSession('s1');
      else await store.runEngine.refreshSessionMessages('s1');
      expect(store.activeSession.value!.messages).toHaveLength(54);
      expect(store.activeSession.value!.olderTranscriptAnchors).toBe(anchors);
      await store.selectionStore.loadOlderMessages();
      expect(store.activeSession.value!.messages).toHaveLength(60);
    },
  );

  it('accepts a page after a same-revision refresh replaces the messages array', async () => {
    const store = await setup();
    let resolve!: (value: Record<string, unknown>) => void;
    store.endpoints.transcriptBodies = vi.fn(
      () =>
        new Promise<Record<string, unknown>>((done) => {
          resolve = done;
        }),
    );
    const pending = store.selectionStore.loadOlderMessages();
    await store.runEngine.refreshSessionMessages('s1');
    resolve({ rev: 7, messages: rows.slice(24, 42) });
    await pending;
    expect(store.activeSession.value!.messages).toHaveLength(36);
  });

  it('lets a newly selected session load without waiting for a stale request', async () => {
    const store = await setup();
    const pendingRequests: Array<{
      resolve: (value: Record<string, unknown>) => void;
      signal?: AbortSignal;
    }> = [];
    store.endpoints.transcriptBodies = vi.fn(
      (_id, _anchors, signal) =>
        new Promise<Record<string, unknown>>((resolve) => {
          pendingRequests.push({ resolve, signal });
        }),
    );
    const first = store.selectionStore.loadOlderMessages();
    store.sessions.value = [...store.sessions.value, { ...store.activeSession.value!, id: 's2' }];
    store.activeSessionId.value = 's2';
    const second = store.selectionStore.loadOlderMessages();
    expect(pendingRequests).toHaveLength(2);
    expect(pendingRequests[0].signal?.aborted).toBe(true);
    pendingRequests[0].resolve({ rev: 7, messages: rows.slice(24, 42) });
    await first;
    expect(store.selectionStore.historyLoading.value).toBe('s2');
    pendingRequests[1].resolve({ rev: 7, messages: rows.slice(24, 42) });
    await second;
    expect(store.activeSession.value!.messages).toHaveLength(36);
    expect(store.sessions.value[0].messages).toHaveLength(18);
    expect(store.selectionStore.historyLoading.value).toBe('');
  });

  it('rejects revision-less bodies without entering an automatic refresh loop', async () => {
    const store = await setup();
    store.endpoints.transcriptBodies = vi.fn(async () => ({ messages: rows.slice(24, 42) }));
    await store.selectionStore.loadOlderMessages();
    expect(store.selectionStore.historyError.value).toBe('s1');
    expect(store.endpoints.selectedSession).toHaveBeenCalledOnce();
    expect(store.activeSession.value!.messages).toHaveLength(18);
  });

  it('refreshes a conflict then pauses automatic retries until explicit retry', async () => {
    const store = await setup();
    store.endpoints.transcriptBodies = vi.fn(async () => {
      throw new APIError('transcript changed', 409);
    });
    await store.selectionStore.loadOlderMessages();
    expect(store.endpoints.selectedSession).toHaveBeenCalledTimes(2);
    expect(store.selectionStore.historyError.value).toBe('s1');
    await store.loadSession('s1');
    expect(store.selectionStore.historyError.value).toBe('');
  });
});
