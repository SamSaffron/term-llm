import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppStore } from './app-store';
import type { AppConfig } from '../app/config';
import { testConfig, testSession } from './store-test-fixtures';
import { initialProjection } from '../domain/response';

const config: AppConfig = {
  ...testConfig,
  agentNames: ['developer', 'sysadmin', 'reviewer'],
  projectDefaultAgent: 'developer',
  agentPickerCommon: ['developer', 'sysadmin'],
  agentApprovalActions: { '': false, developer: true, sysadmin: true, reviewer: true },
};

const stores: AppStore[] = [];

function createStore(overrides: Partial<AppConfig> = {}): AppStore {
  const store = new AppStore({ ...config, ...overrides });
  stores.push(store);
  store.projects.value = [
    { id: 'p1', name: 'Alpha', archived: false, available: true, sessions: [] },
  ];
  store.projectsEnabled.value = true;
  store.draftActive.value = true;
  store.activeSessionId.value = '';
  store.activeProjectId.value = '';
  store.endpoints.createBlankSession = async () => ({ session: { id: 'created' } });
  return store;
}

afterEach(() => {
  for (const store of stores.splice(0)) store.dispose();
  localStorage.clear();
});

describe('new-chat agent resolution', () => {
  it('starts an automatic no-project draft at no agent (Chat)', () => {
    const store = createStore();
    expect(store.selectedAgent.value).toBe('');
    expect(store.agentChat.value).toBe(false);
    expect(store.effectiveAgent.value).toBe('');
    expect(store.resolvedAgentLabel.value).toBe('Chat');
    expect(store.draftApprovalActions.value).toBe(false);
  });

  it('resolves the configured project default without making it explicit', () => {
    const store = createStore();
    store.newChat(true, 'p1');
    expect(store.effectiveAgent.value).toBe('developer');
    expect(store.selectedAgent.value).toBe('');
    expect(store.agentChat.value).toBe(false);
    expect(store.agentAutomatic.value).toBe(true);
    expect(store.draftApprovalActions.value).toBe(true);
  });

  it('returns to Chat when leaving a project with an automatic selection', () => {
    const store = createStore();
    store.newChat(true, 'p1');
    expect(store.effectiveAgent.value).toBe('developer');
    store.newChat(true, '');
    expect(store.effectiveAgent.value).toBe('');
    expect(store.agentAutomatic.value).toBe(true);
  });

  it('preserves an explicit named agent across project transitions', () => {
    const store = createStore();
    store.setAgentChoice({ kind: 'agent', name: 'reviewer' });
    store.newChat(true, 'p1');
    expect(store.effectiveAgent.value).toBe('reviewer');
    store.newChat(true, '');
    expect(store.effectiveAgent.value).toBe('reviewer');
    expect(store.agentAutomatic.value).toBe(false);
  });

  it('replaces explicit Chat when a project is selected', () => {
    const store = createStore();
    store.setAgentChoice({ kind: 'chat' });
    expect(store.effectiveAgent.value).toBe('');
    store.newChat(true, 'p1');
    expect(store.agentChat.value).toBe(false);
    expect(store.effectiveAgent.value).toBe('developer');
    expect(store.agentAutomatic.value).toBe(true);
  });

  it('labels Automatic with what it resolves to, not the explicit choice', () => {
    const store = createStore();
    store.setAgentChoice({ kind: 'agent', name: 'sysadmin' });
    store.newChat(true, 'p1');
    expect(store.effectiveAgent.value).toBe('sysadmin');
    expect(store.resolvedAgentLabel.value).toBe('sysadmin');
    expect(store.automaticAgentLabel.value).toBe('developer');
  });

  it('honours a pinned server agent instead of claiming Chat', () => {
    const store = createStore({
      agentName: 'pinned-agent',
      agentNames: [],
      projectDefaultAgent: '',
    });
    expect(store.effectiveAgent.value).toBe('');
    expect(store.resolvedAgentLabel.value).toBe('pinned-agent');
  });

  it('resolves a restored project default even when Chat was persisted explicitly', () => {
    const first = createStore();
    first.setAgentChoice({ kind: 'chat' });
    const reloaded = createStore();
    expect(reloaded.agentChat.value).toBe(true);
    reloaded.activeProjectId.value = 'p1';
    expect(reloaded.effectiveAgent.value).toBe('developer');
    expect(reloaded.agentAutomatic.value).toBe(true);
    expect(reloaded.currentAgentChoiceValue()).toBe('auto');
  });

  it('persists explicit chat separately from automatic and named choices', () => {
    const first = createStore();
    first.setAgentChoice({ kind: 'chat' });
    expect(localStorage.getItem(first.keys.selectedAgentChat)).toBe('1');
    expect(localStorage.getItem(first.keys.selectedAgent)).toBeNull();

    const reloaded = createStore();
    expect(reloaded.agentChat.value).toBe(true);
    expect(reloaded.currentAgentChoiceValue()).toBe('chat');

    reloaded.setAgentChoice({ kind: 'agent', name: 'sysadmin' });
    expect(localStorage.getItem(reloaded.keys.selectedAgent)).toBe('sysadmin');
    expect(localStorage.getItem(reloaded.keys.selectedAgentChat)).toBeNull();

    reloaded.setAgentChoice({ kind: 'auto' });
    expect(localStorage.getItem(reloaded.keys.selectedAgent)).toBeNull();
    expect(localStorage.getItem(reloaded.keys.selectedAgentChat)).toBeNull();
  });
});

describe('new-chat agent request values', () => {
  it('materializes an automatic no-project draft with no agent', async () => {
    const store = createStore();
    const create = vi.fn(async () => ({ session: { id: 'created' } }));
    store.endpoints.createBlankSession = create;
    await expect(store.ensureSession()).resolves.toBe('created');
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ agent: '', no_project: true }));
  });

  it('materializes an automatic project draft with the configured default', async () => {
    const store = createStore();
    store.activeProjectId.value = 'p1';
    const create = vi.fn(async () => ({ session: { id: 'created' } }));
    store.endpoints.createBlankSession = create;
    await store.ensureSession();
    expect(create).toHaveBeenCalledWith(
      expect.objectContaining({ agent: 'developer', project_id: 'p1' }),
    );
  });

  it('sends the resolved automatic project agent on the normal first submit', async () => {
    const store = createStore();
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.activeProjectId.value = 'p1';
    store.prompt.value = 'hello';
    const createResponse = vi.fn(
      async (..._args: Parameters<typeof store.endpoints.createResponse>) =>
        new Response('stop', { status: 400 }),
    );
    store.endpoints.createResponse = createResponse;
    await store.send();
    const body = createResponse.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(body.agent).toBe('developer');
    expect(body.project_id).toBe('p1');
  });

  it('sends no agent for an automatic no-project first submit', async () => {
    const store = createStore();
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.prompt.value = 'hello';
    const createResponse = vi.fn(
      async (..._args: Parameters<typeof store.endpoints.createResponse>) =>
        new Response('stop', { status: 400 }),
    );
    store.endpoints.createResponse = createResponse;
    await store.send();
    const body = createResponse.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(body.agent).toBe('');
    expect(body.no_project).toBe(true);
  });

  it('never mutates an existing tool-less conversation from a browser preference', async () => {
    const store = createStore();
    store.draftActive.value = false;
    store.sessions.value = [testSession({ id: 's1', agent: '' })];
    store.activeSessionId.value = 's1';
    store.setAgentChoice({ kind: 'agent', name: 'sysadmin' });
    store.prompt.value = 'hello';
    const createResponse = vi.fn(
      async (..._args: Parameters<typeof store.endpoints.createResponse>) =>
        new Response('stop', { status: 400 }),
    );
    store.endpoints.createResponse = createResponse;
    await store.send();
    const body = createResponse.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(body.agent).toBe('');
  });

  it('materializes an explicit named agent unchanged', async () => {
    const store = createStore();
    store.activeProjectId.value = 'p1';
    store.setAgentChoice({ kind: 'agent', name: 'sysadmin' });
    const create = vi.fn(async () => ({ session: { id: 'created' } }));
    store.endpoints.createBlankSession = create;
    await store.ensureSession();
    expect(create).toHaveBeenCalledWith(
      expect.objectContaining({ agent: 'sysadmin', project_id: 'p1' }),
    );
  });
});

it('refreshes approval relevance from a live Guardian policy event', () => {
  const store = createStore();
  store.draftActive.value = false;
  store.sessions.value = [testSession({ id: 's1', approvalActions: false })];
  store.activeSessionId.value = 's1';
  store.runs.value = {
    s1: initialProjection({
      responseId: 'response-1',
      sessionId: 's1',
      epoch: 1,
      status: 'streaming',
      lastSequence: 1,
      startedRev: 1,
      reconnects: 0,
    }),
  };
  store.applyResponseEvent('s1', {
    type: 'response.guardian.review',
    response_id: 'response-1',
    run_epoch: 1,
    sequence_number: 2,
    approval_approval_actions: true,
    approval_guardian_available: true,
  });
  expect(store.activeSession.value?.approvalActions).toBe(true);
});
