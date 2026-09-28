import { describe, expect, it } from 'vitest';
import { parseSidebarCategories, readInjectedConfig, rebaseHubAssetURL } from './config';
import { mergePendingIntents, migrateScopedStorage, storageKeys } from '../platform/storage';

describe('bootstrap configuration and storage', () => {
  it('installs complete isolated Web Storage globals on modern Node', () => {
    localStorage.setItem('key', 'local');
    sessionStorage.setItem('key', 'session');
    expect(localStorage.getItem('key')).toBe('local');
    expect(sessionStorage.getItem('key')).toBe('session');
    expect(localStorage).not.toBe(sessionStorage);
    localStorage.removeItem('key');
    expect(localStorage.length).toBe(0);
  });

  it('reads injected values only when bootstrap asks for them', () => {
    const target = {
      TERM_LLM_UI_PREFIX: '/chat/nodes/alpha',
      TERM_LLM_UI_VERSION: 'abc',
      TERM_LLM_AGENT_NAMES: ['a', 'a', 'b'],
      TERM_LLM_APPROVALS_ENABLED: false,
      TERM_LLM_APPROVAL_MODE: 'yolo',
      TERM_LLM_HUB: { nodeId: 'alpha', nodeBasePath: '/chat/nodes/alpha' },
      __WEBRTC_ENABLED__: true,
      __WEBRTC_SIGNALING_URL__: 'https://signal',
    } as Window;
    expect(readInjectedConfig(target)).toMatchObject({
      prefix: '/chat/nodes/alpha',
      version: 'abc',
      agentNames: ['a', 'b'],
      approvals: false,
      approvalMode: 'yolo',
      webRTC: true,
      signalingURL: 'https://signal',
    });
    expect(parseSidebarCategories('recent,pinned,recent')).toEqual(['recent', 'pinned']);
    expect(parseSidebarCategories([])).toEqual(['all']);
  });

  it('preserves direct-node token scope while scoping other Hub preferences', () => {
    expect(storageKeys({ nodeId: 'n1' }).token).toBe('term_llm_token');
    expect(storageKeys({ nodeId: 'n1' }).activeSession).toBe('term_llm_active_session:n1');
    expect(storageKeys({ nodeId: 'n1', nodeBasePath: '/nodes/n1' }).token).toBe(
      'term_llm_token:n1',
    );
  });

  it('migrates unscoped preferences once without overwriting scoped values', () => {
    localStorage.setItem('term_llm_active_session', 'old');
    const keys = migrateScopedStorage(localStorage, { nodeId: 'n1', nodeBasePath: '/nodes/n1' });
    expect(localStorage.getItem(keys.activeSession)).toBe('old');
    localStorage.setItem(keys.activeSession, 'new');
    migrateScopedStorage(localStorage, { nodeId: 'n1', nodeBasePath: '/nodes/n1' });
    expect(localStorage.getItem(keys.activeSession)).toBe('new');
  });

  it('migrates legacy archived-session preferences without overwriting newer choices', () => {
    localStorage.clear();
    const legacy = 'term_llm_show_hidden_sessions';
    const current = 'term_llm_show_archived_sessions';
    localStorage.setItem(legacy, '1');
    const direct = migrateScopedStorage(localStorage, null);
    expect(direct.showArchivedSessions).toBe(current);
    expect(localStorage.getItem(direct.showArchivedSessions)).toBe('1');
    expect(localStorage.getItem(legacy)).toBeNull();

    // A node's explicit disabled choice wins over the inherited unscoped one.
    localStorage.setItem(`${legacy}:n1`, '0');
    const hub = { nodeId: 'n1', nodeBasePath: '/nodes/n1' };
    const scoped = migrateScopedStorage(localStorage, hub);
    expect(localStorage.getItem(scoped.showArchivedSessions)).toBe('0');
    expect(localStorage.getItem(`${legacy}:n1`)).toBeNull();
    localStorage.setItem(`${legacy}:n1`, '1');
    migrateScopedStorage(localStorage, hub);
    expect(localStorage.getItem(scoped.showArchivedSessions)).toBe('0');
    expect(localStorage.getItem(`${legacy}:n1`)).toBeNull();

    // Nodes without a saved choice still inherit the unscoped preference.
    const other = migrateScopedStorage(localStorage, { nodeId: 'n2', nodeBasePath: '/nodes/n2' });
    expect(localStorage.getItem(other.showArchivedSessions)).toBe('1');
    localStorage.setItem(legacy, '0');
    migrateScopedStorage(localStorage, null);
    expect(localStorage.getItem(current)).toBe('1');
    expect(localStorage.getItem(legacy)).toBeNull();
  });

  it('does not block startup when an old archive preference cannot be written', () => {
    const legacy = 'term_llm_show_hidden_sessions';
    const current = 'term_llm_show_archived_sessions';
    localStorage.clear();
    localStorage.setItem(legacy, '1');
    const storage: Storage = {
      getItem: (key: string) => localStorage.getItem(key),
      setItem: () => {
        throw new DOMException('Storage quota exceeded', 'QuotaExceededError');
      },
      removeItem: (key: string) => localStorage.removeItem(key),
      clear: () => localStorage.clear(),
      key: (index: number) => localStorage.key(index),
      get length() {
        return localStorage.length;
      },
    };

    expect(() => migrateScopedStorage(storage, null)).not.toThrow();
    expect(localStorage.getItem(legacy)).toBe('1');
    expect(localStorage.getItem(current)).toBeNull();
    localStorage.setItem('term_llm_active_session', 's1');
    expect(() =>
      migrateScopedStorage(storage, { nodeId: 'n1', nodeBasePath: '/nodes/n1' }),
    ).not.toThrow();
    expect(localStorage.getItem(legacy)).toBe('1');
  });

  it('merges cross-tab pending intents by client identity and creation order', () => {
    expect(
      mergePendingIntents(
        { s1: [{ id: 'a', clientMessageId: 'a', content: 'A', created: 2 }] },
        { s1: [{ id: 'b', clientMessageId: 'b', content: 'B', created: 1 }] },
      ).s1.map((entry) => entry.id),
    ).toEqual(['b', 'a']);
  });

  it('rebases node-local media from the backend base onto the public Hub mount', () => {
    const config = readInjectedConfig({
      TERM_LLM_UI_PREFIX: '/node/Dev',
      TERM_LLM_HUB: { nodeId: 'Dev', nodeBasePath: '/ui' },
    } as Window);
    expect(new URL(rebaseHubAssetURL(config, '/ui/images/history-a.png')).pathname).toBe(
      '/node/Dev/images/history-a.png',
    );
    expect(new URL(rebaseHubAssetURL(config, '/files/a.png?download=1')).pathname).toBe(
      '/node/Dev/files/a.png',
    );
    expect(new URL(rebaseHubAssetURL(config, '/node/Dev/images/history-a.png')).pathname).toBe(
      '/node/Dev/images/history-a.png',
    );
    expect(rebaseHubAssetURL(config, 'https://elsewhere.test/a')).toBe('https://elsewhere.test/a');
  });
});
