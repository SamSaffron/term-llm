import { describe, expect, it, vi } from 'vitest';
import type { StatsChild } from '../api/endpoints';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import { ChildSessionStore } from './child-session-store';

const session = (overrides: Partial<Session> = {}): Session => ({
  id: 'parent-1',
  name: '',
  title: 'Parent',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: 1,
  lastMessageAt: 1,
  messages: [],
  ...overrides,
});

const child = (overrides: Partial<StatsChild> = {}): StatsChild => ({
  session_id: 'child-1',
  parent_session_id: 'parent-1',
  parent_spawn_call_id: 'spawn-1',
  title: 'Research task',
  state: 'active',
  input_tokens: 0,
  output_tokens: 0,
  cached_input_tokens: 0,
  cache_write_tokens: 0,
  tool_calls: 0,
  llm_turns: 0,
  ...overrides,
});

function harness() {
  const endpoints = {
    sessionChildren: vi.fn(async () => ({ children: [child()], __etag: 'children-1' })),
  };
  const store = new ChildSessionStore({ endpoints } as unknown as AppStoreServices);
  return { store, endpoints };
}

describe('ChildSessionStore', () => {
  it('loads only the selected parent child provenance and revalidates after a change event', async () => {
    const { store, endpoints } = harness();

    store.selectSession(session());
    await vi.waitFor(() => expect(store.children.value).toEqual([child()]));
    expect(endpoints.sessionChildren).toHaveBeenNthCalledWith(
      1,
      'parent-1',
      expect.any(AbortSignal),
      '',
    );

    endpoints.sessionChildren.mockResolvedValueOnce({
      children: [child({ session_id: 'child-2' })],
      __etag: 'children-2',
    });
    store.childrenChanged('parent-1');
    await vi.waitFor(() => expect(store.children.value[0]?.session_id).toBe('child-2'));
    expect(endpoints.sessionChildren).toHaveBeenNthCalledWith(
      2,
      'parent-1',
      expect.any(AbortSignal),
      '',
    );

    store.dispose();
  });

  it('preserves Open-link provenance across the live-to-durable completion handoff', async () => {
    const { store, endpoints } = harness();
    store.selectSession(session());
    await vi.waitFor(() => expect(store.children.value).toEqual([child()]));

    // Finish removes the live server handle before the parent tool result is
    // durable, so this authoritative row briefly has no spawn identity.
    endpoints.sessionChildren.mockResolvedValueOnce({
      children: [
        child({
          state: 'complete',
          parent_spawn_call_id: undefined,
        }),
      ],
      __etag: 'children-complete',
    });
    store.childrenChanged('parent-1');
    await vi.waitFor(() => expect(store.children.value[0]?.state).toBe('complete'));
    expect(store.children.value[0]?.parent_spawn_call_id).toBe('spawn-1');

    // The response still owns row membership: preserving immutable provenance
    // must not resurrect a child the server has deleted.
    endpoints.sessionChildren.mockResolvedValueOnce({
      children: [],
      __etag: 'children-deleted',
    });
    store.childrenChanged('parent-1');
    await vi.waitFor(() => expect(store.children.value).toEqual([]));
    store.dispose();
  });

  it('retains missing spawn item identity while preferring newly supplied provenance', async () => {
    const { store, endpoints } = harness();
    endpoints.sessionChildren.mockResolvedValueOnce({
      children: [child({ parent_spawn_item_id: 41 })],
      __etag: 'children-with-item',
    });
    store.selectSession(session());
    await vi.waitFor(() => expect(store.children.value[0]?.parent_spawn_item_id).toBe(41));

    endpoints.sessionChildren.mockResolvedValueOnce({
      children: [child({ state: 'complete', parent_spawn_call_id: undefined })],
      __etag: 'children-without-provenance',
    });
    await store.refresh();
    expect(store.children.value[0]).toMatchObject({
      state: 'complete',
      parent_spawn_call_id: 'spawn-1',
      parent_spawn_item_id: 41,
    });

    endpoints.sessionChildren.mockResolvedValueOnce({
      children: [child({ parent_spawn_call_id: 'spawn-authoritative', parent_spawn_item_id: 42 })],
      __etag: 'children-authoritative',
    });
    await store.refresh();
    expect(store.children.value[0]).toMatchObject({
      parent_spawn_call_id: 'spawn-authoritative',
      parent_spawn_item_id: 42,
    });
    store.dispose();
  });

  it('does not carry spawn provenance across parent sessions', async () => {
    const { store, endpoints } = harness();
    store.selectSession(session());
    await vi.waitFor(() => expect(store.children.value).toEqual([child()]));

    endpoints.sessionChildren.mockResolvedValueOnce({
      children: [
        child({
          parent_session_id: 'parent-2',
          parent_spawn_call_id: undefined,
        }),
      ],
      __etag: 'children-parent-2',
    });
    store.selectSession(session({ id: 'parent-2' }));
    await vi.waitFor(() => expect(store.children.value[0]?.parent_session_id).toBe('parent-2'));
    expect(store.children.value[0]?.parent_spawn_call_id).toBeUndefined();
    store.dispose();
  });

  it('loads provenance for nested children while a delegated session is selected', async () => {
    const { store, endpoints } = harness();

    store.selectSession(
      session({
        id: 'child-1',
        parentSessionId: 'parent-1',
        delegated: true,
      }),
    );

    await vi.waitFor(() => expect(store.children.value).toEqual([child()]));
    expect(store.parentSessionId.value).toBe('child-1');
    expect(endpoints.sessionChildren).toHaveBeenCalledWith('child-1', expect.any(AbortSignal), '');
    store.dispose();
  });

  it('refetches rows instead of sending a stale ETag when returning to a session', async () => {
    const { store, endpoints } = harness();
    store.selectSession(session());
    await vi.waitFor(() => expect(store.children.value).toEqual([child()]));
    store.selectSession(null);
    store.selectSession(session());
    await vi.waitFor(() => expect(store.children.value).toEqual([child()]));
    expect(endpoints.sessionChildren).toHaveBeenLastCalledWith(
      'parent-1',
      expect.any(AbortSignal),
      '',
    );
    store.dispose();
  });
});
