import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import type { Message, Session } from '../domain/types';
import { MemoryBackend, PersistentCache, type CacheBackend } from '../platform/persistent-cache';
import {
  CACHED_MESSAGE_LIMIT,
  DISCOVERY_MAX_AGE_MS,
  WORKSPACE_MAX_AGE_MS,
  WorkspaceCache,
  cacheableSession,
  privateDisplayPolicy,
  restoredSession,
} from './workspace-cache';

const config: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: [],
  title: '',
  locationSharing: false,
  worktrees: false,
  hub: null,
  vapidKey: '',
  webRTC: false,
  signalingURL: '',
  cacheScope: 'abcdef0123456789',
  shellAuthorized: true,
};
const message = (id: number, role: Message['role'] = 'user'): Message => ({
  id: String(id),
  role,
  content: `message ${id}`,
  created: id,
  serverSeq: id,
});
const session = (): Session => ({
  id: 's1',
  number: 42,
  name: '',
  title: 'Saved',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: 1,
  lastMessageAt: 1,
  messageBodiesRev: 7,
  transcriptRev: 9,
  messages: [message(1)],
});
const sidebar = {
  projectsEnabled: false,
  worktreesEnabled: false,
  showArchived: false,
  payload: { sessions: [{ id: 's1' }] },
};
const caches: WorkspaceCache[] = [];
const create = (
  backend = new MemoryBackend(),
  override: Partial<AppConfig> = {},
  now = () => Date.now(),
) => {
  const cache = new WorkspaceCache(
    { ...config, ...override },
    new PersistentCache(backend, vi.fn(), now),
    now,
  );
  caches.push(cache);
  return cache;
};
const flush = async (cache: WorkspaceCache, backend: MemoryBackend, count: number) => {
  cache.flush();
  await vi.waitFor(() => expect(backend.rows.size).toBe(count));
};
afterEach(() => {
  caches.splice(0).forEach((cache) => cache.dispose());
  vi.useRealTimers();
});

describe('privateDisplayPolicy', () => {
  it.each([
    ['missing scope', { cacheScope: undefined }, 'disabled'],
    ['passkey', { shellAuthorized: false, passkeyAuth: true }, 'immediate'],
    [
      'Hub authenticated navigation',
      { shellAuthorized: false, hub: { nodeBasePath: '/nodes/a' } },
      'immediate',
    ],
    ['no auth', { shellAuthorized: true }, 'immediate'],
    ['bearer, including a saved token', { shellAuthorized: false }, 'after-auth'],
    [
      'Hub metadata without authenticated mount',
      { shellAuthorized: false, hub: { nodeId: 'a' } },
      'after-auth',
    ],
  ] as const)('%s', (_name, override, expected) => {
    expect(privateDisplayPolicy({ ...config, ...override })).toBe(expected);
  });
});

describe('cacheableSession', () => {
  it('keeps durable bodies but removes live, interaction and paging ownership', () => {
    const original: Session = {
      ...session(),
      activeRun: true,
      activeResponseId: 'response',
      interactionRequired: true,
      interactionResponseId: 'response',
      interactionStateRev: 4,
      pendingInteractionCount: 2,
      pendingInteractionKinds: ['approval'],
      interactionRequiredSince: 100,
      olderTranscriptAnchors: [1, 2],
      messages: [
        message(1),
        { ...message(2, 'assistant'), serverSeq: undefined, durableSourceRowIds: [2] },
        { id: 'optimistic', role: 'user', content: 'unsent', created: 3 },
      ],
    };
    const cached = cacheableSession(original)!;
    expect(cached.bodiesRev).toBe(7);
    expect(cached.session.transcriptRev).toBe(7);
    expect(cached.session.messages.map((entry) => entry.id)).toEqual(['1', '2']);
    for (const field of [
      'activeRun',
      'activeResponseId',
      'interactionRequired',
      'interactionResponseId',
      'interactionStateRev',
      'pendingInteractionCount',
      'pendingInteractionKinds',
      'interactionRequiredSince',
      'olderTranscriptAnchors',
      'messageBodiesRev',
    ])
      expect(cached.session).not.toHaveProperty(field);
    expect(restoredSession(cached)).toMatchObject({
      activeRun: false,
      activeResponseId: null,
      interactionRequired: false,
      pendingInteractionCount: 0,
      pendingInteractionKinds: [],
    });
    expect(original).toMatchObject({ activeRun: true, messageBodiesRev: 7, transcriptRev: 9 });
    expect(original.messages).toHaveLength(3);
  });

  it('bounds the durable tail and starts it at a user turn', () => {
    const messages = Array.from({ length: CACHED_MESSAGE_LIMIT + 3 }, (_, index) =>
      message(index, index % 2 ? 'assistant' : 'user'),
    );
    const cached = cacheableSession({ ...session(), messages })!;
    expect(cached.session.messages).toEqual(messages.slice(4));
    expect(cached.session.messages.length).toBeLessThanOrEqual(CACHED_MESSAGE_LIMIT);
    expect(cached.session.messages[0].role).toBe('user');
  });

  it('does not cache a tail consisting only of an incomplete assistant turn', () => {
    const messages = [
      message(0),
      ...Array.from({ length: CACHED_MESSAGE_LIMIT + 1 }, (_, index) =>
        message(index + 1, 'assistant'),
      ),
    ];
    expect(cacheableSession({ ...session(), messages })?.session.messages).toEqual([]);
  });

  it('scrubs inline, blob and credentialed URLs recursively without dropping relative URLs or prose', () => {
    const media = {
      ...message(1),
      media: [
        {
          url: 'data:image/png;base64,secret',
          file: { name: 'secret' },
          blobRef: 'local',
          dataURL: 'secret',
        },
        { previewURL: 'blob:local' },
        { src: 'https://example.test/image?X-Amz-Signature=secret' },
        { uri: '/image?access_token=secret' },
        { url: '/images/plain.png' },
        { url: 'https://example.test/image.png' },
      ],
      content: 'Prose mentioning data: should remain',
    };
    const cached = cacheableSession({ ...session(), messages: [media] })!;
    expect(cached.session.messages[0]).toMatchObject({
      content: media.content,
      media: [
        { url: '' },
        { previewURL: '' },
        { src: '' },
        { uri: '' },
        { url: '/images/plain.png' },
        { url: 'https://example.test/image.png' },
      ],
    });
    expect(JSON.stringify(cached)).not.toContain('secret');
    expect(JSON.stringify(cached)).not.toContain('blobRef');
  });

  it.each([
    { messageBodiesRev: undefined },
    { id: 'draft_local' },
    { delegated: true },
    { parentSessionId: 'parent' },
  ])('does not persist unhydrated or non-top-level sessions: %j', (override) => {
    expect(cacheableSession({ ...session(), ...override })).toBeNull();
  });
});

describe('WorkspaceCache persistence', () => {
  it('round trips startup state and resolves a durable #number alias', async () => {
    const backend = new MemoryBackend();
    const cache = create(backend);
    cache.saveSidebar(sidebar);
    cache.saveSession(session());
    await flush(cache, backend, 3);
    const restored = await create(backend).readStartup('42');
    expect(restored.sidebar?.value).toMatchObject(sidebar);
    expect(restored.session?.value.session.id).toBe('s1');
    expect(await cache.readSession('s1')).toEqual(restored.session);
    expect([...backend.rows.keys()].some((key) => key.endsWith('\u0000#42'))).toBe(true);
    cache.forgetSession('s1', 42);
    await vi.waitFor(() => expect(backend.rows.size).toBe(1));
    expect(await cache.readSession('42')).toBeNull();
  });

  it('ignores schema mismatches for workspace, session and discovery', async () => {
    const backend = new MemoryBackend();
    const cache = create(backend);
    cache.saveSidebar(sidebar);
    cache.saveSession(session());
    cache.saveProviders({ providers: [] });
    cache.saveModels('p', 'fp', []);
    await flush(cache, backend, 5);
    for (const row of backend.rows.values()) {
      const record: unknown = JSON.parse(row.json);
      if (record && typeof record === 'object')
        row.json = JSON.stringify({ ...record, schema: 999 });
    }
    expect(await cache.readStartup('s1')).toEqual({ sidebar: null, session: null });
    expect(await cache.readProviders()).toBeNull();
    expect(await cache.readModels('p', 'fp')).toBeNull();
  });

  it('ignores stale workspace and discovery snapshots', async () => {
    let now = 100;
    const backend = new MemoryBackend();
    const cache = create(backend, {}, () => now);
    cache.saveSidebar(sidebar);
    cache.saveSession(session());
    cache.saveProviders({ providers: [] });
    cache.saveModels('p', 'fp', []);
    await flush(cache, backend, 5);
    now += DISCOVERY_MAX_AGE_MS + 1;
    expect(await cache.readProviders()).toBeNull();
    expect(await cache.readModels('p', 'fp')).toBeNull();
    expect((await cache.readStartup('s1')).session).not.toBeNull();
    now = 100 + WORKSPACE_MAX_AGE_MS + 1;
    expect(await cache.readSession('42')).toBeNull();
    expect(await cache.readSession('s1')).toBeNull();
    expect(await cache.readStartup('s1')).toEqual({ sidebar: null, session: null });
  });

  it('cancels queued writes on purge', async () => {
    vi.useFakeTimers();
    const backend = new MemoryBackend();
    const cache = create(backend);
    cache.saveSidebar(sidebar);
    cache.saveSession(session());
    await cache.purge();
    cache.flush();
    await vi.advanceTimersByTimeAsync(600);
    expect(backend.rows.size).toBe(0);
  });

  it('purge wins even when a write completes after deletion', async () => {
    const backend = new MemoryBackend();
    const cache = create(backend);
    let release!: () => void;
    const blocked = new Promise<void>((resolve) => {
      release = resolve;
    });
    const put = backend.put.bind(backend);
    vi.spyOn(backend, 'put').mockImplementation(
      async (record: Parameters<CacheBackend['put']>[0]) => {
        await blocked;
        await put(record);
      },
    );
    const racing = { ...session(), number: 7 };
    cache.saveSession(racing);
    cache.flush();
    await vi.waitFor(() => expect(backend.put).toHaveBeenCalledOnce());
    await cache.purge();
    // A snapshot saved after the purge (e.g. after reauthentication) is written
    // by a newer generation and must survive the racing write's cleanup.
    cache.saveSidebar(sidebar);
    release();
    cache.flush();
    await vi.waitFor(() =>
      expect([...backend.rows.values()].map((row) => row.key.split('\u0000')[1])).toEqual([
        'sidebar',
      ]),
    );
  });

  it('deletes a model record if the provider fingerprint changed', async () => {
    const backend = new MemoryBackend();
    const cache = create(backend);
    cache.saveModels('p', 'old', [{ id: 'm', name: 'Model' }]);
    await flush(cache, backend, 1);
    expect(await cache.readModels('p', 'old')).not.toBeNull();
    expect(await cache.readModels('p', 'changed')).toBeNull();
    expect(backend.rows.size).toBe(0);
  });

  it.each([{ cacheScope: '1234567890abcdef' }, { hub: { nodeId: 'other' } }, { prefix: '/other' }])(
    'isolates scopes: %j',
    async (override) => {
      const backend = new MemoryBackend();
      const cache = create(backend);
      cache.saveSidebar(sidebar);
      cache.saveSession(session());
      await flush(cache, backend, 3);
      const isolated = create(backend, override);
      expect(await isolated.readStartup('s1')).toEqual({ sidebar: null, session: null });
      await isolated.purge();
      expect((await cache.readStartup('s1')).session?.value.session.id).toBe('s1');
    },
  );

  it('does not read or write private records without a server scope', async () => {
    const backend = new MemoryBackend();
    const cache = create(backend, { cacheScope: '' });
    cache.saveSidebar(sidebar);
    cache.saveSession(session());
    cache.saveProviders({});
    cache.saveModels('p', 'fp', []);
    cache.flush();
    expect(await cache.readStartup('s1')).toEqual({ sidebar: null, session: null });
    expect(await cache.readProviders()).toBeNull();
    expect(await cache.readModels('p', 'fp')).toBeNull();
    expect(backend.rows.size).toBe(0);
  });
});

describe('review regressions', () => {
  it('resolves #number routes without deleting the alias', async () => {
    const backend = new MemoryBackend();
    const cache = create(backend);
    cache.saveSession(session());
    await flush(cache, backend, 2);
    expect((await cache.readSession('#42'))?.value.session.id).toBe('s1');
    expect((await cache.readSession('42'))?.value.session.id).toBe('s1');
    expect(backend.rows.size).toBe(2);
  });

  it('scrubs signed, credentialed and inline-data URLs embedded in text and payloads', async () => {
    const backend = new MemoryBackend();
    const cache = create(backend);
    const inline = `data:image/png;base64,${'A'.repeat(400)}`;
    cache.saveSession({
      ...session(),
      messages: [
        {
          ...message(1),
          content: `see [file](https://cdn.example/a.png?X-Amz-Signature=abc&x=1) and https://user:pw@host.example/x ![i](${inline}) or /images/plain.png`,
        },
      ],
    });
    cache.saveSidebar({
      ...sidebar,
      payload: { sessions: [{ id: 's1', title: 'https://h.example/p?token=secret' }] },
    });
    cache.saveProviders({ data: [{ id: 'p', docs_url: 'https://h.example/d?sig=1' }] });
    await flush(cache, backend, 4);
    const stored = [...backend.rows.values()].map((row) => row.json).join('\n');
    for (const secret of ['X-Amz-Signature', 'user:pw', 'AAAA', 'token=secret', 'sig=1'])
      expect(stored).not.toContain(secret);
    expect(stored).toContain('https://cdn.example/a.png');
    expect(stored).toContain('/images/plain.png');
  });
});
