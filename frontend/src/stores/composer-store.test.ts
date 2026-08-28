import { beforeEach, describe, expect, it } from 'vitest';
import { readDrafts } from '../platform/storage';
import { AppStore } from './app-store';
import { testConfig } from './store-test-fixtures';

beforeEach(() => localStorage.clear());

describe('ComposerStore', () => {
  it('owns draft identity, runtime metadata, and restore behavior', () => {
    const store = new AppStore(testConfig);
    try {
      store.activeProjectId.value = 'project-1';
      store.composer.prompt.value = 'Persist this draft';
      store.runtime.setPreference('provider', 'provider-1', false);
      store.runtime.setPreference('model', 'model-1', false);
      store.composer.selectedDraftWorktree.value = '/tmp/worktree';

      store.composer.persist();
      const id = store.composer.storageId();
      expect(readDrafts(localStorage, store.keys.draftMessages)).toEqual([
        expect.objectContaining({
          sessionId: id,
          content: 'Persist this draft',
          projectId: 'project-1',
          provider: 'provider-1',
          model: 'model-1',
          worktreeDir: '/tmp/worktree',
        }),
      ]);

      store.composer.prompt.value = '';
      store.composer.selectedDraftWorktree.value = '';
      store.composer.restore(id);
      expect(store.composer.prompt.value).toBe('Persist this draft');
      expect(store.composer.selectedDraftWorktree.value).toBe('/tmp/worktree');
    } finally {
      store.dispose();
    }
  });

  it('clears only the submitted composer generation', () => {
    const store = new AppStore(testConfig);
    try {
      store.activeSessionId.value = 's1';
      store.composer.prompt.value = 'newer text';
      store.composer.clearSubmitted('s1', 'older text', []);
      expect(store.composer.prompt.value).toBe('newer text');

      store.composer.clearSubmitted('s1', 'newer text', []);
      expect(store.composer.prompt.value).toBe('');
    } finally {
      store.dispose();
    }
  });
});
