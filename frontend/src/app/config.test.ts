import { describe, expect, it } from 'vitest';
import { parseSidebarCategories, readInjectedConfig, rebaseHubAssetURL } from './config';
import { mergePendingIntents, migrateScopedStorage, storageKeys } from '../platform/storage';

describe('bootstrap configuration and storage', () => {
  it('reads injected values only when bootstrap asks for them', () => {
    const target = { TERM_LLM_UI_PREFIX: '/chat/nodes/alpha', TERM_LLM_UI_VERSION: 'abc', TERM_LLM_AGENT_NAMES: ['a', 'a', 'b'], TERM_LLM_HUB: { nodeId: 'alpha', nodeBasePath: '/chat/nodes/alpha' }, __WEBRTC_ENABLED__: true, __WEBRTC_SIGNALING_URL__: 'https://signal' } as Window;
    expect(readInjectedConfig(target)).toMatchObject({ prefix: '/chat/nodes/alpha', version: 'abc', agentNames: ['a', 'b'], webRTC: true, signalingURL: 'https://signal' });
    expect(parseSidebarCategories('recent,pinned,recent')).toEqual(['recent', 'pinned']);
    expect(parseSidebarCategories([])).toEqual(['all']);
  });

  it('preserves direct-node token scope while scoping other Hub preferences', () => {
    expect(storageKeys({ nodeId: 'n1' }).token).toBe('term_llm_token');
    expect(storageKeys({ nodeId: 'n1' }).activeSession).toBe('term_llm_active_session:n1');
    expect(storageKeys({ nodeId: 'n1', nodeBasePath: '/nodes/n1' }).token).toBe('term_llm_token:n1');
  });

  it('migrates unscoped preferences once without overwriting scoped values', () => {
    localStorage.setItem('term_llm_active_session', 'old');
    const keys = migrateScopedStorage(localStorage, { nodeId: 'n1', nodeBasePath: '/nodes/n1' });
    expect(localStorage.getItem(keys.activeSession)).toBe('old');
    localStorage.setItem(keys.activeSession, 'new');
    migrateScopedStorage(localStorage, { nodeId: 'n1', nodeBasePath: '/nodes/n1' });
    expect(localStorage.getItem(keys.activeSession)).toBe('new');
  });

  it('merges cross-tab pending intents by client identity and creation order', () => {
    expect(mergePendingIntents({ s1: [{ id: 'a', clientMessageId: 'a', content: 'A', created: 2 }] }, { s1: [{ id: 'b', clientMessageId: 'b', content: 'B', created: 1 }] }).s1.map((entry) => entry.id)).toEqual(['b', 'a']);
  });

  it('rebases node-local media without touching foreign origins or double rebasing', () => {
    const config = readInjectedConfig({ TERM_LLM_UI_PREFIX: '/chat/nodes/n1', TERM_LLM_HUB: { nodeId: 'n1', nodeBasePath: '/chat/nodes/n1' } } as Window);
    expect(new URL(rebaseHubAssetURL(config, '/files/a.png')).pathname).toBe('/chat/nodes/n1/files/a.png');
    expect(new URL(rebaseHubAssetURL(config, '/chat/nodes/n1/files/a.png')).pathname).toBe('/chat/nodes/n1/files/a.png');
    expect(rebaseHubAssetURL(config, 'https://elsewhere.test/a')).toBe('https://elsewhere.test/a');
  });
});
