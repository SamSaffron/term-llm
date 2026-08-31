import { beforeEach, describe, expect, it } from 'vitest';
import { readDrafts, saveDraft } from '../platform/storage';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

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
      const runtimeId = store.composer.runtimeDraftId();
      expect(id).toMatch(/^draft:/);
      expect(runtimeId).toMatch(/^draft_/);
      expect(runtimeId.slice('draft_'.length)).not.toBe(id.slice('draft:'.length));
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
      store.composer.restore(id, 'draft');
      expect(store.composer.prompt.value).toBe('Persist this draft');
      expect(store.composer.selectedDraftWorktree.value).toBe('/tmp/worktree');
    } finally {
      store.dispose();
    }
  });

  it('does not persist or restore draft worktree selection for an existing session', () => {
    const store = new AppStore(testConfig);
    try {
      saveDraft(localStorage, store.keys.draftMessages, {
        sessionId: 's1',
        content: 'Legacy conversation reply',
        updated: 1,
        worktreeDir: '/worktrees/legacy-copy',
      });
      store.sessions.value = [
        testSession({ worktreeDir: '/worktrees/session', workingDir: '/worktrees/session' }),
      ];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;

      store.composer.restore('s1', 'session');
      expect(store.composer.selectedDraftWorktree.value).toBe('');

      store.composer.prompt.value = 'Conversation reply';
      store.composer.selectedDraftWorktree.value = '/worktrees/stale-draft';
      store.composer.persist();
      expect(
        readDrafts(localStorage, store.keys.draftMessages).find(
          (draft) => draft.sessionId === 's1',
        ),
      ).not.toHaveProperty('worktreeDir');
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
