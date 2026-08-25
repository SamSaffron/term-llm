import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { initialProjection } from '../domain/response';
import type { ActiveRun, Session } from '../domain/types';
import { persistPendingIntent, saveDraft } from '../platform/storage';
import { AppStore } from './app-store';

const config: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: ['jarvis'],
  title: '',
  locationSharing: true,
  worktrees: true,
  hub: null,
  vapidKey: '',
  webRTC: false,
  signalingURL: '',
};
const session = (): Session => ({
  id: 's1',
  title: 'Test',
  name: '',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: 1,
  lastMessageAt: 1,
  messages: [],
});
const deferred = <T>() => {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
};

beforeEach(() => localStorage.clear());

describe('AppStore compatibility behavior', () => {
  it('bootstraps no-project mode without calling the project-only sidebar endpoint', async () => {
    const store = new AppStore(config);
    store.endpoints.capabilities = vi.fn(async () => ({
      projects: { enabled: false },
      worktrees: { enabled: true },
    }));
    store.endpoints.providers = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.sessions = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.sidebar = vi.fn(async () => {
      throw new Error('project sidebar must not be called');
    });
    (store as unknown as { startStatusPoll(): void }).startStatusPoll = vi.fn();
    await store.bootstrap();
    expect(store.endpoints.sessions).toHaveBeenCalledWith('limit=30&include_archived=0');
    expect(store.endpoints.sidebar).not.toHaveBeenCalled();
    expect(store.startupDone.value).toBe(true);
    expect(store.draftActive.value).toBe(true);
    expect(store.projectsEnabled.value).toBe(false);
  });

  it('paginates the no-project sidebar group with the cursor from the sidebar payload', async () => {
    const store = new AppStore(config);
    store.projectsEnabled.value = true;
    store.endpoints.sidebar = vi.fn(async () => ({
      groups: [
        {
          no_project: true,
          next_cursor: 'cursor-1',
          sessions: [{ id: 's1', title: 'First', created_at: 2, last_message_at: 2 }],
        },
      ],
    }));
    await store.refreshSidebar();
    expect(store.noProjectCursor.value).toBe('cursor-1');

    store.endpoints.noProjectSessions = vi.fn(async () => ({
      sessions: [{ id: 's2', title: 'Older', created_at: 1, last_message_at: 1 }],
    }));
    await store.loadMoreNoProject();
    expect(store.endpoints.noProjectSessions).toHaveBeenCalledWith('cursor-1', false);
    expect(store.sessions.value.map((session) => session.id)).toEqual(['s1', 's2']);
    expect(store.noProjectCursor.value).toBe('');
    // Without a cursor no further pages are requested.
    await store.loadMoreNoProject();
    expect(store.endpoints.noProjectSessions).toHaveBeenCalledTimes(1);
  });

  it('paginates the flat listing with the top-level cursor when projects are disabled', async () => {
    const store = new AppStore(config);
    store.projectsEnabled.value = false;
    store.endpoints.sessions = vi.fn(async () => ({
      sessions: [{ id: 's1', title: 'First', created_at: 2, last_message_at: 2 }],
      next_cursor: 'flat-cursor',
    }));
    await store.refreshSidebar();
    expect(store.noProjectCursor.value).toBe('flat-cursor');

    store.endpoints.noProjectSessions = vi.fn(async () => ({
      sessions: [{ id: 's2', title: 'Older', created_at: 1, last_message_at: 1 }],
    }));
    await store.loadMoreNoProject();
    expect(store.endpoints.noProjectSessions).toHaveBeenCalledWith('flat-cursor', false);
    expect(store.sessions.value.map((session) => session.id)).toEqual(['s1', 's2']);
    expect(store.noProjectCursor.value).toBe('');
  });

  it('restores persisted pending intents and draft worktree selection', () => {
    const seed = new AppStore(config);
    persistPendingIntent(localStorage, seed.keys.pendingIntents, 's1', {
      id: 'pending-c1',
      clientMessageId: 'c1',
      content: 'safe pending message',
      created: 2,
    });
    saveDraft(localStorage, seed.keys.draftMessages, {
      sessionId: 'draft:project-1',
      content: 'draft text',
      projectId: 'project-1',
      worktreeDir: '/tmp/feature',
      updated: 1,
    });

    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    expect(store.visibleMessages.value).toEqual([
      expect.objectContaining({
        clientMessageId: 'c1',
        content: 'safe pending message',
        pending: true,
      }),
    ]);

    store.projects.value = [
      { id: 'project-1', name: 'Project', archived: false, available: true, sessions: [] },
    ];
    store.newChat(true, 'project-1');
    expect(store.prompt.value).toBe('draft text');
    expect(store.selectedDraftWorktree.value).toBe('/tmp/feature');
  });

  it('rolls back an optimistic message when the response was never accepted', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.prompt.value = 'never submitted';
    store.endpoints.createResponse = vi.fn(
      async () => new Response('invalid request', { status: 400 }),
    );

    await store.send();

    expect(store.sessions.value[0].messages).toEqual([]);
    expect(store.pendingIntents.value).toEqual({});
    expect(store.visibleMessages.value).toEqual([
      expect.objectContaining({ role: 'error', content: 'invalid request' }),
    ]);
    expect(store.prompt.value).toBe('never submitted');
  });

  it('clears uncommitted persisted intents after an authoritative idle reload', async () => {
    const seed = new AppStore(config);
    persistPendingIntent(localStorage, seed.keys.pendingIntents, 's1', {
      id: 'pending_stale',
      clientMessageId: 'stale',
      content: 'never reached the server',
      created: Date.now() - 20 * 60_000,
    });
    const store = new AppStore(config);
    store.sessions.value = [
      {
        ...session(),
        messages: [
          {
            id: 'pending_stale',
            role: 'user',
            content: 'never reached the server',
            created: Date.now() - 20 * 60_000,
            clientMessageId: 'stale',
          },
        ],
      },
    ];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1' },
      selected_transcript: { bodies: { messages: [] } },
    }));

    await store.loadSession('s1');

    expect(store.pendingIntents.value).toEqual({});
    expect(store.sessions.value[0].messages).toEqual([]);
    expect(store.visibleMessages.value).toEqual([]);
  });

  it('reconciles stale pending intents during idle status polling', async () => {
    const seed = new AppStore(config);
    persistPendingIntent(localStorage, seed.keys.pendingIntents, 's1', {
      id: 'pending_polled',
      clientMessageId: 'polled',
      content: 'not submitted',
      created: Date.now() - 20 * 60_000,
    });
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [{ id: 's1', active_response_id: '', transcript_rev: 1 }],
    }));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', transcript_rev: 1 },
      selected_transcript: { bodies: { messages: [] } },
    }));
    const internals = store as unknown as { refreshStatus(): Promise<void> };

    await internals.refreshStatus();

    await vi.waitFor(() => expect(store.pendingIntents.value).toEqual({}));
    expect(store.visibleMessages.value).toEqual([]);
  });

  it('keeps an unresolved persisted intent while the server reports an active response', async () => {
    const seed = new AppStore(config);
    persistPendingIntent(localStorage, seed.keys.pendingIntents, 's1', {
      id: 'pending_active',
      clientMessageId: 'active',
      content: 'still submitting',
      created: Date.now(),
    });
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.sessionState = vi.fn(async () => ({ active_response_id: 'r1' }));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1' },
      selected_transcript: { bodies: { messages: [] } },
    }));

    await store.loadSession('s1');

    expect(store.pendingIntents.value.s1).toHaveLength(1);
    expect(store.visibleMessages.value).toEqual([
      expect.objectContaining({ clientMessageId: 'active', pending: true }),
    ]);
  });

  it('applies runtime metadata from response lifecycle events', () => {
    const store = new AppStore(config);
    const active = session();
    store.sessions.value = [active];
    store.activeSessionId.value = active.id;
    const run: ActiveRun = {
      responseId: 'r1',
      sessionId: active.id,
      epoch: 1,
      status: 'connecting',
      lastSequence: 0,
      startedRev: 0,
      reconnects: 0,
    };
    store.runs.value = { [active.id]: initialProjection(run) };
    store.applyResponseEvent(active.id, {
      type: 'response.created',
      response_id: 'r1',
      run_epoch: 1,
      sequence_number: 1,
      response: { model: 'gpt-test', provider: 'openai', reasoning_effort: 'high' },
    });
    expect(store.activeSession.value).toMatchObject({
      activeModel: 'gpt-test',
      activeProvider: 'openai',
      activeEffort: 'high',
    });
    store.applyResponseEvent(active.id, {
      type: 'response.model_switch',
      response_id: 'r1',
      run_epoch: 1,
      sequence_number: 2,
      model: 'gpt-next',
      reasoning_effort: 'medium',
    });
    expect(store.activeSession.value).toMatchObject({
      activeModel: 'gpt-next',
      activeEffort: 'medium',
    });
  });

  it('loads file summaries and structured hunks from the real diff contracts', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.fileChanges = vi.fn(async () => ({
      scope: 'last_turn',
      git: true,
      file_changes: [
        { path: 'main.go', kind: 'modify', adds: 2, dels: 1, seq: 9, snapshot_seq: 8 },
      ],
    }));
    store.endpoints.fileDiff = vi.fn(async () => ({
      path: 'main.go',
      kind: 'modify',
      context: 3,
      old_line_count: 3,
      new_line_count: 3,
      hunks: [
        {
          old_start: 2,
          new_start: 2,
          lines: [
            { t: 'del', s: 'old' },
            { t: 'add', s: 'new' },
          ],
        },
      ],
    }));
    await store.loadDiff();
    expect(store.diff.value.files[0]).toMatchObject({
      path: 'main.go',
      additions: 2,
      deletions: 1,
      sequence: 9,
      snapshotSeq: 8,
    });
    await store.expandDiff(store.diff.value.files[0]);
    expect(store.endpoints.fileDiff).toHaveBeenCalledWith('s1', 'main.go', 'last_turn', 0, 8);
    expect(store.diff.value.files[0]).toMatchObject({
      expanded: true,
      context: 3,
      oldLineCount: 3,
      newLineCount: 3,
      lines: [
        { kind: 'hunk', content: '@@ -2 +2 @@' },
        { kind: 'delete', content: 'old', oldLine: 2 },
        { kind: 'add', content: 'new', newLine: 2 },
      ],
    });
  });

  it('sends queued diff comments as response input parts, not the read-only history endpoint', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.queueDiffComment({
      path: 'main.go',
      side: 'new',
      line: 12,
      body: 'Keep this guard.',
      scope: 'last_turn',
      context: 'if ready {',
      fileChangeSeq: 9,
    });
    let options: Record<string, unknown> | undefined;
    store.send = vi.fn(async (value) => {
      options = value as unknown as Record<string, unknown>;
      (value as { onTransportStarted?: () => void }).onTransportStarted?.();
    });
    await store.sendDiffComments();
    expect(options).toMatchObject({
      inputText: expect.stringContaining('main.go:12'),
      displayContent: 'Keep this guard.',
      preserveComposer: true,
    });
    expect(options?.contentParts).toEqual([
      expect.objectContaining({
        type: 'diff_comment',
        diff_comment: expect.objectContaining({
          path: 'main.go',
          line: 12,
          file_change_seq: 9,
          line_text: 'if ready {',
          instruction: 'Keep this guard.',
        }),
      }),
    ]);
    expect(store.diff.value.comments).toEqual([]);
  });

  it('keeps the Paths control hidden until the server reports multiple paths', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.endpoints.tree = vi.fn(async () => ({
      root_session_id: 's1',
      active_session_id: 's1',
      path_count: 1,
      nodes: [{ session_id: 's1' }],
    }));
    await store.refreshBranchTree();
    expect(store.branchPathCount.value).toBe(1);
    expect(store.modal.value).toBe('');
    store.endpoints.tree = vi.fn(async () => ({
      root_session_id: 's1',
      active_session_id: 's1',
      path_count: 2,
      nodes: [{ session_id: 's1' }, { session_id: 's2' }],
    }));
    await store.loadBranchTree();
    expect(store.branchPathCount.value).toBe(2);
    expect(store.modal.value).toBe('branch');
  });

  it('starts normal and isolated skill runs from their server responses', async () => {
    const normal = new AppStore(config);
    normal.sessions.value = [session()];
    normal.activeSessionId.value = 's1';
    normal.draftActive.value = false;
    normal.endpoints.invokeSkill = vi.fn(async () => ({
      execution: 'inline',
      response_id: 'r-skill',
      run_epoch: 2,
      started_rev: 4,
    }));
    normal.streamResponse = vi.fn(async () => undefined);
    await normal.invokeSkill('summarize', 'briefly');
    expect(normal.runs.value.s1?.run).toMatchObject({
      responseId: 'r-skill',
      epoch: 2,
      startedRev: 4,
    });
    expect(normal.streamResponse).toHaveBeenCalledWith('r-skill', 's1', 0);
    expect(normal.sessions.value[0].messages[0]).toMatchObject({
      role: 'user',
      content: '/summarize briefly',
    });

    const isolated = new AppStore(config);
    isolated.sessions.value = [session()];
    isolated.activeSessionId.value = 's1';
    isolated.draftActive.value = false;
    isolated.endpoints.invokeSkill = vi.fn(async () => ({
      execution: 'isolated',
      run_id: 'run-1',
      status: 'running',
      events_url: '/ui/v1/sessions/s1/skill-runs/run-1/events',
    }));
    isolated.api.request = vi.fn(
      async () =>
        new Response(
          'data: {"sequence":1,"type":"skill_run.completed","data":{"status":"completed","output":"done"}}\n\n',
          { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
        ),
    );
    await isolated.invokeSkill('research', 'topic');
    await vi.waitFor(() =>
      expect(
        isolated.sessions.value[0].messages.find((message) => message.role === 'skill-run'),
      ).toMatchObject({ status: 'completed', content: 'done' }),
    );
    expect(isolated.api.request).toHaveBeenCalledWith(
      expect.stringContaining('/ui/v1/sessions/s1/skill-runs/run-1/events?after=0'),
      expect.objectContaining({
        headers: expect.objectContaining({ 'X-Term-LLM-Session-ID': 's1' }),
      }),
      expect.objectContaining({ policy: 'stream' }),
    );
  });

  it('submits ask-user dismissal through the server contract', async () => {
    const store = new AppStore(config);
    const submit = vi.fn(async () => ({}));
    store.endpoints.askUser = submit;
    store.askUser.value = {
      sessionId: 's1',
      callId: 'ask1',
      questions: [{ question: 'Continue?', options: [] }],
    };
    store.modal.value = 'ask-user';
    await store.answerAskUser([], true);
    expect(submit).toHaveBeenCalledWith('s1', { call_id: 'ask1', cancelled: true });
    expect(store.askUser.value).toBeNull();
  });

  it('parses provider and model metadata from the real OpenAI list contracts', async () => {
    const store = new AppStore(config);
    const internals = store as unknown as { applyProviders(data: Record<string, unknown>): void };
    internals.applyProviders({
      object: 'list',
      data: [
        {
          name: 'openai',
          configured: true,
          is_default: true,
          default_model: 'gpt-5-high',
          models: ['gpt-5'],
        },
      ],
    });
    expect(store.providers.value).toEqual([
      expect.objectContaining({
        id: 'openai',
        name: 'openai',
        is_default: true,
        default_model: 'gpt-5-high',
        models: ['gpt-5'],
      }),
    ]);
    store.selectedProvider.value = 'openai';
    store.endpoints.models = vi.fn(async () => ({
      object: 'list',
      data: [
        {
          id: 'gpt-5',
          reasoning_efforts: ['low', 'high'],
          default_reasoning_effort: 'high',
          reasoning_modes: ['standard', 'pro'],
        },
      ],
    }));
    await store.loadModels();
    expect(store.models.value).toEqual([
      expect.objectContaining({
        id: 'gpt-5',
        name: 'gpt-5',
        provider: 'openai',
        efforts: ['low', 'high'],
        default_effort: 'high',
        reasoning_modes: ['standard', 'pro'],
      }),
    ]);
  });

  it('clears invalid persisted providers and stale models when the provider changes', async () => {
    const store = new AppStore(config);
    localStorage.setItem(store.keys.selectedProvider, 'removed');
    store.selectedProvider.value = 'removed';
    const internals = store as unknown as { applyProviders(data: Record<string, unknown>): void };
    internals.applyProviders({ object: 'list', data: [{ name: 'new', models: ['fallback'] }] });
    expect(store.selectedProvider.value).toBe('');
    expect(localStorage.getItem(store.keys.selectedProvider)).toBeNull();
    store.selectedModel.value = 'old';
    localStorage.setItem(store.keys.selectedModel, 'old');
    store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [{ id: 'fresh' }] }));
    store.setPreference('provider', 'new');
    expect(store.selectedModel.value).toBe('');
    expect(store.models.value.map((entry) => entry.id)).toEqual(['fallback']);
    await vi.waitFor(() => expect(store.models.value.map((entry) => entry.id)).toEqual(['fresh']));
  });

  it('routes every provider model refresh through one abortable generation guard', async () => {
    const store = new AppStore(config);
    const first = deferred<Record<string, unknown>>();
    const second = deferred<Record<string, unknown>>();
    const requests: Array<{ provider: string; signal?: AbortSignal }> = [];
    store.endpoints.models = vi.fn((provider, signal) => {
      requests.push({ provider, signal });
      return provider === 'first' ? first.promise : second.promise;
    });
    const old = store.loadModels('first');
    const current = store.loadModels('second');
    expect(requests[0].signal?.aborted).toBe(true);
    second.resolve({ object: 'list', data: [{ id: 'new', name: 'New' }] });
    await current;
    first.resolve({ object: 'list', data: [{ id: 'stale', name: 'Stale' }] });
    await old;
    expect(store.models.value.map((model) => model.id)).toEqual(['new']);
  });

  it('treats an aborted stale model refresh as successful cancellation', async () => {
    const store = new AppStore(config);
    store.endpoints.models = vi.fn((provider, signal) => {
      if (provider === 'current')
        return Promise.resolve({ object: 'list', data: [{ id: 'new', name: 'New' }] });
      return new Promise<Record<string, unknown>>((_resolve, reject) => {
        signal?.addEventListener(
          'abort',
          () => reject(signal.reason || new DOMException('Aborted', 'AbortError')),
          { once: true },
        );
      });
    });
    const stale = store.loadModels('stale');
    await expect(store.loadModels('current')).resolves.toBeUndefined();
    await expect(stale).resolves.toBeUndefined();
    expect(store.models.value.map((model) => model.id)).toEqual(['new']);
  });

  it('does not let stale session state overwrite a prompt opened by the live stream', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    const state = deferred<Record<string, unknown>>();
    store.endpoints.sessionState = vi.fn(() => state.promise);
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1' },
      selected_transcript: { bodies: { messages: [] } },
    }));
    const loading = store.loadSession('s1');
    const live = {
      sessionId: 's1',
      callId: 'live',
      questions: [{ question: 'Live?', options: [] }],
    };
    store.askUser.value = live;
    state.resolve({
      pending_ask_user: { call_id: 'stale', questions: [{ question: 'Stale?', options: [] }] },
    });
    await loading;
    expect(store.askUser.value).toBe(live);
  });

  it('coalesces simultaneous lifecycle recovery before opening one SSE subscription', async () => {
    const store = new AppStore(config);
    const active = session();
    store.sessions.value = [active];
    store.activeSessionId.value = active.id;
    const run: ActiveRun = {
      responseId: 'r1',
      sessionId: active.id,
      epoch: 1,
      status: 'streaming',
      lastSequence: 4,
      startedRev: 0,
      reconnects: 0,
    };
    store.runs.value = { s1: initialProjection(run) };
    const status = deferred<void>();
    const internals = store as unknown as {
      recover(): Promise<void>;
      refreshStatus(): Promise<void>;
    };
    internals.refreshStatus = vi.fn(() => status.promise);
    store.streamResponse = vi.fn(async () => undefined);
    const first = internals.recover();
    const second = internals.recover();
    expect(internals.refreshStatus).toHaveBeenCalledOnce();
    status.resolve();
    await Promise.all([first, second]);
    expect(store.streamResponse).toHaveBeenCalledOnce();
    (store as unknown as { streamAborts: Map<string, AbortController> }).streamAborts.set(
      's1',
      new AbortController(),
    );
    internals.refreshStatus = vi.fn(async () => undefined);
    await internals.recover();
    expect(store.streamResponse).toHaveBeenCalledOnce();
  });
});
