import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { initialProjection } from '../domain/response';
import type { ActiveRun, Session } from '../domain/types';
import { persistPendingIntent, saveDraft } from '../platform/storage';
import { AppStore } from './app-store';

const config: AppConfig = { prefix: '/ui', version: 'v1', sidebarCategories: ['all'], agentName: '', agentNames: ['jarvis'], title: '', locationSharing: true, worktrees: true, hub: null, vapidKey: '', webRTC: false, signalingURL: '' };
const session = (): Session => ({ id: 's1', title: 'Test', name: '', mode: 'chat', origin: 'web', archived: false, pinned: false, created: 1, lastMessageAt: 1, messages: [] });
const deferred = <T,>() => { let resolve!: (value: T) => void; const promise = new Promise<T>((done) => { resolve = done; }); return { promise, resolve }; };

beforeEach(() => localStorage.clear());

describe('AppStore compatibility behavior', () => {
  it('restores persisted pending intents and draft worktree selection', () => {
    const seed = new AppStore(config);
    persistPendingIntent(localStorage, seed.keys.pendingIntents, 's1', { id: 'pending-c1', clientMessageId: 'c1', content: 'safe pending message', created: 2 });
    saveDraft(localStorage, seed.keys.draftMessages, { sessionId: 'draft:project-1', content: 'draft text', projectId: 'project-1', worktreeDir: '/tmp/feature', updated: 1 });

    const store = new AppStore(config);
    store.sessions.value = [session()]; store.activeSessionId.value = 's1'; store.draftActive.value = false;
    expect(store.visibleMessages.value).toEqual([expect.objectContaining({ clientMessageId: 'c1', content: 'safe pending message', pending: true })]);

    store.projects.value = [{ id: 'project-1', name: 'Project', archived: false, available: true, sessions: [] }];
    store.newChat(true, 'project-1');
    expect(store.prompt.value).toBe('draft text');
    expect(store.selectedDraftWorktree.value).toBe('/tmp/feature');
  });

  it('applies runtime metadata from response lifecycle events', () => {
    const store = new AppStore(config); const active = session(); store.sessions.value = [active]; store.activeSessionId.value = active.id;
    const run: ActiveRun = { responseId: 'r1', sessionId: active.id, epoch: 1, status: 'connecting', lastSequence: 0, startedRev: 0, reconnects: 0 };
    store.runs.value = { [active.id]: initialProjection(run) };
    store.applyResponseEvent(active.id, { type: 'response.created', response_id: 'r1', run_epoch: 1, sequence_number: 1, response: { model: 'gpt-test', provider: 'openai', reasoning_effort: 'high' } });
    expect(store.activeSession.value).toMatchObject({ activeModel: 'gpt-test', activeProvider: 'openai', activeEffort: 'high' });
    store.applyResponseEvent(active.id, { type: 'response.model_switch', response_id: 'r1', run_epoch: 1, sequence_number: 2, model: 'gpt-next', reasoning_effort: 'medium' });
    expect(store.activeSession.value).toMatchObject({ activeModel: 'gpt-next', activeEffort: 'medium' });
  });

  it('starts normal and isolated skill runs from their server responses', async () => {
    const normal = new AppStore(config); normal.sessions.value = [session()]; normal.activeSessionId.value = 's1'; normal.draftActive.value = false;
    normal.endpoints.invokeSkill = vi.fn(async () => ({ execution: 'inline', response_id: 'r-skill', run_epoch: 2, started_rev: 4 }));
    normal.streamResponse = vi.fn(async () => undefined);
    await normal.invokeSkill('summarize', 'briefly');
    expect(normal.runs.value.s1?.run).toMatchObject({ responseId: 'r-skill', epoch: 2, startedRev: 4 });
    expect(normal.streamResponse).toHaveBeenCalledWith('r-skill', 's1', 0);
    expect(normal.sessions.value[0].messages[0]).toMatchObject({ role: 'user', content: '/summarize briefly' });

    const isolated = new AppStore(config); isolated.sessions.value = [session()]; isolated.activeSessionId.value = 's1'; isolated.draftActive.value = false;
    isolated.endpoints.invokeSkill = vi.fn(async () => ({ execution: 'isolated', run_id: 'run-1', status: 'running', events_url: '/ui/v1/sessions/s1/skill-runs/run-1/events' }));
    isolated.api.request = vi.fn(async () => new Response('data: {"sequence":1,"type":"skill_run.completed","data":{"status":"completed","output":"done"}}\n\n', { status: 200, headers: { 'Content-Type': 'text/event-stream' } }));
    await isolated.invokeSkill('research', 'topic');
    await vi.waitFor(() => expect(isolated.sessions.value[0].messages.find((message) => message.role === 'skill-run')).toMatchObject({ status: 'completed', content: 'done' }));
    expect(isolated.api.request).toHaveBeenCalledWith(expect.stringContaining('/ui/v1/sessions/s1/skill-runs/run-1/events?after=0'), expect.anything(), expect.objectContaining({ policy: 'stream' }));
  });

  it('submits ask-user dismissal through the server contract', async () => {
    const store = new AppStore(config); const submit = vi.fn(async () => ({})); store.endpoints.askUser = submit;
    store.askUser.value = { sessionId: 's1', callId: 'ask1', questions: [{ question: 'Continue?', options: [] }] };
    store.modal.value = 'ask-user';
    await store.answerAskUser([], true);
    expect(submit).toHaveBeenCalledWith('s1', { call_id: 'ask1', cancelled: true });
    expect(store.askUser.value).toBeNull();
  });

  it('routes every provider model refresh through one abortable generation guard', async () => {
    const store = new AppStore(config); const first = deferred<Record<string, unknown>>(); const second = deferred<Record<string, unknown>>();
    const requests: Array<{ provider: string; signal?: AbortSignal }> = [];
    store.endpoints.models = vi.fn((provider, signal) => { requests.push({ provider, signal }); return provider === 'first' ? first.promise : second.promise; });
    const old = store.loadModels('first'); const current = store.loadModels('second');
    expect(requests[0].signal?.aborted).toBe(true);
    second.resolve({ models: [{ id: 'new', name: 'New' }] }); await current;
    first.resolve({ models: [{ id: 'stale', name: 'Stale' }] }); await old;
    expect(store.models.value.map((model) => model.id)).toEqual(['new']);
  });

  it('does not let stale session state overwrite a prompt opened by the live stream', async () => {
    const store = new AppStore(config); store.sessions.value = [session()]; store.activeSessionId.value = 's1'; store.draftActive.value = false;
    const state = deferred<Record<string, unknown>>(); store.endpoints.sessionState = vi.fn(() => state.promise); store.endpoints.selectedSession = vi.fn(async () => ({ selected_session: { id: 's1' }, selected_transcript: { bodies: { messages: [] } } }));
    const loading = store.loadSession('s1');
    const live = { sessionId: 's1', callId: 'live', questions: [{ question: 'Live?', options: [] }] }; store.askUser.value = live;
    state.resolve({ pending_ask_user: { call_id: 'stale', questions: [{ question: 'Stale?', options: [] }] } }); await loading;
    expect(store.askUser.value).toBe(live);
  });

  it('coalesces simultaneous lifecycle recovery before opening one SSE subscription', async () => {
    const store = new AppStore(config); const active = session(); store.sessions.value = [active]; store.activeSessionId.value = active.id;
    const run: ActiveRun = { responseId: 'r1', sessionId: active.id, epoch: 1, status: 'streaming', lastSequence: 4, startedRev: 0, reconnects: 0 }; store.runs.value = { s1: initialProjection(run) };
    const status = deferred<void>(); const internals = store as unknown as { recover(): Promise<void>; refreshStatus(): Promise<void> };
    internals.refreshStatus = vi.fn(() => status.promise); store.streamResponse = vi.fn(async () => undefined);
    const first = internals.recover(); const second = internals.recover(); expect(internals.refreshStatus).toHaveBeenCalledOnce();
    status.resolve(); await Promise.all([first, second]); expect(store.streamResponse).toHaveBeenCalledOnce();
    (store as unknown as { streamAborts: Map<string, AbortController> }).streamAborts.set('s1', new AbortController());
    internals.refreshStatus = vi.fn(async () => undefined); await internals.recover(); expect(store.streamResponse).toHaveBeenCalledOnce();
  });
});
