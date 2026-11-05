import { describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { AppStore } from './app-store';

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
  vapidKey: 'AQID',
  pushSupported: true,
  notificationVersion: 2,
};

function draftStore(projectsEnabled: boolean, projectId = ''): AppStore {
  const store = new AppStore(config);
  store.sessions.value = [];
  store.activeSessionId.value = '';
  store.projectsEnabled.value = projectsEnabled;
  store.activeProjectId.value = projectId;
  store.draftActive.value = true;
  return store;
}

describe('skills in an unsent new chat', () => {
  it.each([
    { projectsEnabled: false, projectId: '', noProject: false },
    { projectsEnabled: true, projectId: '', noProject: true },
    { projectsEnabled: true, projectId: 'p1', noProject: false },
  ])(
    'lists skills for the workspace the draft will bind to (%o)',
    async ({ projectsEnabled, projectId, noProject }) => {
      const store = draftStore(projectsEnabled, projectId);
      store.endpoints.skills = vi.fn();
      store.endpoints.draftSkills = vi.fn(async () => ({
        skills: [{ name: 'review', execution: 'inline' }],
      }));

      await store.loadSkills();

      expect(store.endpoints.draftSkills).toHaveBeenCalledWith(projectId, '', noProject);
      expect(store.endpoints.skills).not.toHaveBeenCalled();
      expect(store.skills.value.map((skill) => skill.name)).toEqual(['review']);
      store.dispose();
    },
  );

  it('drops a draft listing that resolves after the draft became a session', async () => {
    const store = draftStore(false);
    let resolve!: (value: Record<string, unknown>) => void;
    store.endpoints.draftSkills = vi.fn(
      () => new Promise<Record<string, unknown>>((done) => (resolve = done)),
    );

    const loading = store.loadSkills();
    store.draftActive.value = false;
    resolve({ skills: [{ name: 'stale' }] });
    await loading;

    expect(store.skills.value).toEqual([]);
    store.dispose();
  });

  it('creates the session before running a skill from a new chat', async () => {
    const store = draftStore(false);
    store.endpoints.createBlankSession = vi.fn(async () => ({
      session: {
        id: 'session-created',
        title: 'New chat',
        mode: 'chat',
        origin: 'web',
        created: 10,
        last_message_at: 10,
      },
    }));
    store.endpoints.invokeSkill = vi.fn(async () => ({
      execution: 'inline',
      response_id: 'skill-response',
      run_epoch: 1,
    }));
    store.streamResponse = vi.fn(async () => undefined);
    store.skills.value = [{ name: 'review', execution: 'inline' }];

    await store.invokeSkill('review', 'changes');

    expect(store.endpoints.createBlankSession).toHaveBeenCalledTimes(1);
    expect(store.endpoints.invokeSkill).toHaveBeenCalledWith(
      'session-created',
      expect.objectContaining({ name: 'review', arguments: 'changes' }),
      expect.any(String),
      true,
    );
    expect(store.activeSessionId.value).toBe('session-created');
    expect(store.streamResponse).toHaveBeenCalledWith('skill-response', 'session-created', 0);
    expect(store.toasts.value.filter((toast) => toast.kind === 'error')).toEqual([]);
    store.dispose();
  });

  it('keeps the invocation in the composer when the session cannot be created', async () => {
    const store = draftStore(false);
    store.endpoints.createBlankSession = vi.fn(async () => {
      throw new Error('workspace unavailable');
    });
    store.endpoints.invokeSkill = vi.fn();
    store.prompt.value = '';

    await store.invokeSkill('review', 'changes');

    expect(store.endpoints.invokeSkill).not.toHaveBeenCalled();
    expect(store.prompt.value).toBe('/review changes');
    expect(store.draftActive.value).toBe(true);
    expect(store.toasts.value).toHaveLength(1);
    store.dispose();
  });
});
