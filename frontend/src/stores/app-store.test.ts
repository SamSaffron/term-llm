import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { initialProjection } from '../domain/response';
import type { ActiveRun, Session } from '../domain/types';
import { persistPendingIntent, readDrafts, saveDraft } from '../platform/storage';
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
  it('guards /side in drafts and closes active side work without waiting for cancellation', () => {
    const store = new AppStore(config);
    expect(store.openSideQuestion('Explain this')).toBe(false);
    expect(store.modal.value).toBe('');
    expect(store.toasts.value.at(-1)?.message).toBe(
      'Start the conversation before asking a side question.',
    );

    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.modal.value = 'side';
    store.sideQuestion.value = {
      sessionId: 's1',
      loading: false,
      running: true,
      draft: '',
      question: 'Explain this',
      response: '',
      error: '',
      history: [],
    };
    store.endpoints.cancelSideQuestion = vi.fn(() => new Promise<Response>(() => undefined));

    store.closeSideQuestion();
    expect(store.modal.value).toBe('');
    expect(store.sideQuestion.value.running).toBe(false);
    expect(store.endpoints.cancelSideQuestion).toHaveBeenCalledWith('s1');
  });

  it('cancels and isolates side-question state when leaving a session', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.modal.value = 'side';
    store.sideQuestion.value = {
      sessionId: 's1',
      loading: true,
      running: true,
      draft: 'private draft',
      question: 'Old question',
      response: 'Old answer',
      error: '',
      history: [{ question: 'Earlier', response: 'Private' }],
    };
    store.endpoints.cancelSideQuestion = vi.fn(async () => new Response());
    const pending = deferred<Record<string, unknown>>();
    store.endpoints.sideQuestionState = vi.fn(() => pending.promise);
    const recovery = store.recoverSideQuestion();

    store.newChat();
    pending.resolve({ history: [{ question: 'Stale', response: 'Must not appear' }] });
    await recovery;

    expect(store.endpoints.cancelSideQuestion).toHaveBeenCalledWith('s1');
    expect(store.modal.value).toBe('');
    expect(store.sideQuestion.value).toEqual({
      sessionId: '',
      loading: false,
      running: false,
      draft: '',
      question: '',
      response: '',
      error: '',
      history: [],
    });
  });

  it('keeps the current project when opening another chat unless No project is explicit', () => {
    const store = new AppStore(config);
    const projectSession = { ...session(), projectId: 'project-1', projectName: 'Project' };
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'project-1', name: 'Project', archived: false, available: true, sessions: [] },
    ];
    store.sessions.value = [projectSession];
    store.activeSessionId.value = projectSession.id;
    store.draftActive.value = false;

    store.newChat();
    expect(store.activeProjectId.value).toBe('project-1');

    store.newChat(true, '');
    expect(store.activeProjectId.value).toBe('');
  });

  it('keeps plan visibility session-safe and mutually exclusive with changes', () => {
    const store = new AppStore(config);
    store.currentPlan.value = {
      plan: [{ step: 'Polish the plan', status: 'in_progress' }],
    };
    store.diff.value = { ...store.diff.value, open: true };

    store.openPlan();
    expect(store.planVisible.value).toBe(true);
    expect(store.diff.value.open).toBe(false);
    expect(store.planSeen.value).not.toBeNull();

    store.currentPlan.value = null;
    expect(store.planVisible.value).toBe(false);

    store.currentPlan.value = {
      plan: [{ step: 'Polish the plan', status: 'in_progress' }],
    };
    store.newChat();
    expect(store.planOpen.value).toBe(false);
    expect(store.planSeen.value).toBeNull();
  });

  it('saves manual and generated session titles with the server metadata contract', async () => {
    const store = new AppStore(config);
    const target = session();
    store.sessions.value = [target];
    store.renameTarget.value = target;
    store.modal.value = 'rename';
    store.endpoints.patchSession = vi.fn(async () => ({}));
    store.refreshSidebar = vi.fn(async () => undefined);

    await store.renameSession({ name: '  Renamed session  ' });
    expect(store.endpoints.patchSession).toHaveBeenLastCalledWith('s1', {
      name: 'Renamed session',
    });
    expect(store.modal.value).toBe('');
    expect(store.renameTarget.value).toBeNull();

    store.renameTarget.value = target;
    store.modal.value = 'rename';
    await store.renameSession({
      generatedShortTitle: '  Generated title  ',
      generatedLongTitle: '  Generated detail  ',
    });
    expect(store.endpoints.patchSession).toHaveBeenLastCalledWith('s1', {
      name: '',
      generated_short_title: 'Generated title',
      generated_long_title: 'Generated detail',
    });
  });

  it('requests AI title suggestions in preview mode and reads server title fields', async () => {
    const store = new AppStore(config);
    const target = session();
    store.renameTarget.value = target;
    store.endpoints.refineTitle = vi.fn(async () => ({
      generated_short_title: 'Suggested title',
      generated_long_title: 'Suggested detail',
    }));

    await expect(store.improveTitle()).resolves.toEqual({
      title: 'Suggested title',
      detail: 'Suggested detail',
    });
    expect(store.endpoints.refineTitle).toHaveBeenCalledWith('s1');
  });

  it('preserves MCP server metadata and normalizes partial endpoint data', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.getMCP = vi.fn(async () => ({
      enabled: ['github'],
      servers: [
        {
          name: 'github',
          configured: true,
          status: 'ready',
          tools: 7,
          active: 3,
          deferred: 4,
          loading_mode: 'dynamic',
          refresh_warning: 'Using cached tools',
        },
        { name: 'minimal' },
        { status: 'failed' },
      ],
    }));

    await store.loadMCP();

    expect(store.mcp.value).toMatchObject({
      enabled: ['github'],
      loading: false,
      error: '',
      servers: [
        {
          name: 'github',
          enabled: true,
          status: 'ready',
          tools: 7,
          active: 3,
          deferred: 4,
          loadingMode: 'dynamic',
          refreshWarning: 'Using cached tools',
        },
        {
          name: 'minimal',
          configured: true,
          enabled: false,
          status: 'stopped',
          tools: 0,
        },
      ],
    });
    expect(store.activeSession.value?.mcpEnabled).toEqual(['github']);
  });

  it('rolls back an MCP toggle and exposes a recoverable save error', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.mcp.value = {
      servers: [
        {
          name: 'github',
          configured: true,
          enabled: false,
          status: 'stopped',
          error: '',
          refreshWarning: '',
          tools: 0,
          active: 0,
          deferred: 0,
          loadingMode: '',
        },
      ],
      enabled: [],
      loading: false,
      pending: '',
      error: '',
    };
    store.endpoints.setMCP = vi.fn(async () => {
      throw new Error('Could not save MCP servers');
    });

    await store.toggleMCP('github');

    expect(store.endpoints.setMCP).toHaveBeenCalledWith('s1', ['github']);
    expect(store.mcp.value.enabled).toEqual([]);
    expect(store.mcp.value.pending).toBe('');
    expect(store.mcp.value.error).toBe('Could not save MCP servers');
  });

  it('trusts the project-mode worktree capability instead of the server CWD bootstrap flag', async () => {
    const store = new AppStore({ ...config, worktrees: false });
    store.endpoints.capabilities = vi.fn(async () => ({
      projects: { enabled: true },
      worktrees: { enabled: true },
    }));
    store.endpoints.providers = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.sidebar = vi.fn(async () => ({ groups: [] }));
    (store as unknown as { startStatusPoll(): void }).startStatusPoll = vi.fn();

    await store.bootstrap();

    expect(store.projectsEnabled.value).toBe(true);
    expect(store.worktreesEnabled.value).toBe(true);
  });

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

  it('archives locally without discarding loaded no-project pages', async () => {
    const store = new AppStore(config);
    const loaded = Array.from({ length: 48 }, (_, index) => ({
      ...session(),
      id: `s${index + 1}`,
      title: `Conversation ${index + 1}`,
      lastMessageAt: 48 - index,
    }));
    store.projectsEnabled.value = true;
    store.sessions.value = loaded;
    store.noProjectCursor.value = 'cursor-48';
    store.endpoints.patchSession = vi.fn(async () => ({}));
    store.refreshSidebar = vi.fn(async () => undefined);

    await store.archiveSession(loaded[39]);

    expect(store.endpoints.patchSession).toHaveBeenCalledWith('s40', { archived: true });
    expect(store.refreshSidebar).not.toHaveBeenCalled();
    expect(store.sessions.value).toHaveLength(47);
    expect(store.sessions.value.some((entry) => entry.id === 's40')).toBe(false);
    expect(store.sessions.value.some((entry) => entry.id === 's48')).toBe(true);
    expect(store.noProjectCursor.value).toBe('cursor-48');
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

  it('continues a loaded session from its durable response anchor', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.sessionState = vi.fn(async () => ({ lastResponseId: 'resp_msg_42' }));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', title: 'Test' },
      selected_transcript: { bodies: { messages: [] } },
    }));

    await store.loadSession('s1');

    expect(store.activeSession.value?.lastResponseId).toBe('resp_msg_42');

    store.projectsEnabled.value = false;
    store.endpoints.sessions = vi.fn(async () => ({
      sessions: [{ id: 's1', title: 'Test' }],
    }));
    await store.refreshSidebar();
    expect(store.activeSession.value?.lastResponseId).toBe('resp_msg_42');

    store.prompt.value = 'continue this conversation';
    store.endpoints.createResponse = vi.fn(
      async () => new Response('stop after request capture', { status: 400 }),
    );
    await store.send();

    expect(store.endpoints.createResponse).toHaveBeenCalledWith(
      expect.objectContaining({ previous_response_id: 'resp_msg_42' }),
      's1',
      expect.any(String),
      expect.any(AbortSignal),
    );
  });

  it('reconciles a completed runtime response to the latest durable anchor', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), lastResponseId: 'resp_runtime_stale' }];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', title: 'Test' },
      selected_transcript: { bodies: { messages: [] } },
    }));
    store.endpoints.sessionState = vi.fn(async () => ({ lastResponseId: 'resp_msg_42' }));
    const internals = store as unknown as {
      refreshSessionMessages(sessionId: string): Promise<void>;
    };

    await internals.refreshSessionMessages('s1');

    expect(store.activeSession.value?.lastResponseId).toBe('resp_msg_42');
  });

  it('retires a terminal projection once its durable transcript revision is loaded', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    const projection = initialProjection({
      responseId: 'r1',
      sessionId: 's1',
      epoch: 1,
      status: 'streaming',
      lastSequence: 3,
      startedRev: 1,
      reconnects: 0,
    });
    store.runs.value = {
      s1: {
        ...projection,
        messages: [
          {
            id: 'r1:assistant:0',
            role: 'assistant',
            content: 'identical answer',
            created: 2,
            responseId: 'r1',
            assistantSegmentOrdinal: 0,
          },
        ],
        run: {
          ...projection.run,
          status: 'completed',
          finalRev: 5,
          durableHandoff: true,
        },
      },
    };
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', title: 'Test', transcript_rev: 6 },
      selected_transcript: {
        bodies: {
          rev: 5,
          // Deliberately omit response identity: retirement is revision-based.
          messages: [
            {
              id: 42,
              sequence: 1,
              role: 'assistant',
              parts: [{ type: 'text', text: 'identical answer' }],
            },
          ],
        },
      },
    }));
    store.endpoints.sessionState = vi.fn(async () => ({ lastResponseId: 'r1' }));
    const response = vi.fn(async () => ({}));
    store.endpoints.response = response;
    const internals = store as unknown as {
      refreshSessionMessages(sessionId: string, targetRev: number): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };

    await internals.refreshSessionMessages('s1', 5);

    expect(store.runs.value.s1).toBeUndefined();
    expect(store.visibleMessages.value.map((message) => message.content)).toEqual([
      'identical answer',
    ]);
    await internals.resumeResponse('s1', 'r1');
    expect(response).not.toHaveBeenCalled();
  });

  it('keeps the live projection when transcript bodies are older than the handoff', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), transcriptRev: 2 }];
    const projection = initialProjection({
      responseId: 'r1',
      sessionId: 's1',
      epoch: 1,
      status: 'streaming',
      lastSequence: 3,
      startedRev: 1,
      reconnects: 0,
    });
    store.runs.value = {
      s1: {
        ...projection,
        run: {
          ...projection.run,
          status: 'completed',
          finalRev: 5,
          durableHandoff: true,
        },
      },
    };
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', transcript_rev: 6 },
      selected_transcript: { bodies: { rev: 4, messages: [] } },
    }));
    store.endpoints.sessionState = vi.fn(async () => ({}));
    const internals = store as unknown as {
      refreshSessionMessages(sessionId: string, targetRev: number): Promise<void>;
    };

    await internals.refreshSessionMessages('s1', 5);

    expect(store.runs.value.s1?.run.responseId).toBe('r1');
    expect(store.sessions.value[0].transcriptRev).toBe(2);
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

  it('sends interjection images even when navigation occurs before acceptance', async () => {
    const store = new AppStore(config);
    const other = { ...session(), id: 's2', title: 'Other' };
    store.sessions.value = [session(), other];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.prompt.value = 'inspect this image';
    store.attachments.value = [
      {
        id: 'image-1',
        name: 'example.png',
        type: 'image/png',
        dataURL: 'data:image/png;base64,aW1hZ2U=',
        previewURL: 'data:image/png;base64,aW1hZ2U=',
      },
    ];
    const accepted = deferred<Record<string, unknown>>();
    store.endpoints.interrupt = vi.fn(() => accepted.promise);

    const request = store.interject(store.prompt.value);
    await vi.waitFor(() => expect(store.endpoints.interrupt).toHaveBeenCalledOnce());
    (store as unknown as { persistCurrentDraft(): void }).persistCurrentDraft();
    store.activeSessionId.value = 's2';
    store.prompt.value = 'draft in another conversation';
    store.attachments.value = [];
    accepted.resolve({});
    await request;

    expect(store.endpoints.interrupt).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({
        message: 'inspect this image',
        delivery: 'steer',
        content: [
          {
            type: 'input_image',
            image_url: 'data:image/png;base64,aW1hZ2U=',
            filename: 'example.png',
          },
          { type: 'input_text', text: 'inspect this image' },
        ],
      }),
      expect.any(String),
    );
    expect(store.prompt.value).toBe('draft in another conversation');
    expect(readDrafts(localStorage, store.keys.draftMessages)).not.toEqual(
      expect.arrayContaining([expect.objectContaining({ sessionId: 's1' })]),
    );
    expect(store.interjections.value).toEqual([
      expect.objectContaining({ sessionId: 's1', content: 'inspect this image', state: 'pending' }),
    ]);
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
    store.endpoints.diffComments = vi.fn(async () => ({ comments: [], transcript_rev: 0 }));
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

  it('sends one diff comment immediately without consuming the queued review', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.queueDiffComment({
      id: 'queued',
      path: 'queued.go',
      side: 'new',
      line: 3,
      body: 'Deliver this later.',
      scope: 'uncommitted',
      context: 'queued line',
    });
    let options: Record<string, unknown> | undefined;
    store.send = vi.fn(async (value) => {
      options = value as unknown as Record<string, unknown>;
    });

    await store.sendDiffComment({
      id: 'now',
      path: 'main.go',
      side: 'new',
      line: 12,
      body: 'Review this now.',
      scope: 'uncommitted',
      context: 'changed line',
    });

    expect(options).toMatchObject({
      inputText: expect.stringContaining('main.go:12'),
      displayContent: 'Review this now.',
      preserveComposer: true,
      diffComments: [expect.objectContaining({ id: 'now', body: 'Review this now.' })],
    });
    expect(options?.contentParts).toEqual([
      expect.objectContaining({
        type: 'diff_comment',
        diff_comment: expect.objectContaining({
          id: 'now',
          path: 'main.go',
          instruction: 'Review this now.',
        }),
      }),
    ]);
    expect(store.diff.value.comments).toEqual([
      expect.objectContaining({ id: 'queued', body: 'Deliver this later.' }),
    ]);
    expect(store.diff.value.historyComments).toEqual([
      expect.objectContaining({
        id: 'now',
        body: 'Review this now.',
        sessionId: 's1',
        optimistic: true,
      }),
    ]);
  });

  it('reconciles inline comment trails from the durable comment endpoint', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.diff.value = {
      ...store.diff.value,
      sessionId: 's1',
      historyComments: [
        {
          id: 'pending-comment',
          path: 'main.go',
          side: 'new',
          line: 12,
          body: 'Still sending.',
          sessionId: 's1',
          optimistic: true,
        },
      ],
    };
    store.endpoints.diffComments = vi.fn(async () => ({
      transcript_rev: 4,
      comments: [
        {
          message_id: 9,
          client_message_id: 'client-1',
          created_at: 1234,
          diff_comment: {
            id: 'durable-comment',
            path: 'main.go',
            scope: 'last_turn',
            side: 'new',
            line: 12,
            file_change_seq: 8,
            line_text: 'changed line',
            instruction: 'Durable instruction.',
          },
        },
      ],
    }));

    await store.refreshDiffComments('s1');

    expect(store.diff.value.historyComments).toEqual([
      expect.objectContaining({
        id: 'durable-comment',
        body: 'Durable instruction.',
        createdAt: 1234,
      }),
      expect.objectContaining({ id: 'pending-comment', optimistic: true }),
    ]);
  });

  it('refreshes an open inline comment trail when the transcript advances', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), transcriptRev: 1 }];
    store.activeSessionId.value = 's1';
    store.diff.value = { ...store.diff.value, open: true, sessionId: 's1' };
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [{ id: 's1', transcript_rev: 2 }],
    }));
    store.endpoints.diffComments = vi.fn(async () => ({ comments: [], transcript_rev: 2 }));

    await (store as unknown as { refreshStatus: () => Promise<void> }).refreshStatus();

    await vi.waitFor(() => expect(store.endpoints.diffComments).toHaveBeenCalledWith('s1'));
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

  it('does not reload models when the provider preference is unchanged', () => {
    const store = new AppStore(config);
    store.selectedProvider.value = 'chatgpt';
    store.selectedModel.value = 'gpt-5.6-sol';
    store.models.value = [{ id: 'gpt-5.6-sol', name: 'gpt-5.6-sol' }];
    store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));

    store.setPreference('provider', 'chatgpt', false);

    expect(store.endpoints.models).not.toHaveBeenCalled();
    expect(store.selectedModel.value).toBe('gpt-5.6-sol');
    expect(store.models.value.map((model) => model.id)).toEqual(['gpt-5.6-sol']);
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

  it('finishes bootstrap while a restored response stream remains open', async () => {
    const store = new AppStore(config);
    const openStream = deferred<void>();
    const active = { ...session(), active_response_id: 'r1' };
    store.endpoints.capabilities = vi.fn(async () => ({
      projects: { enabled: false },
      widgets: [{ id: 'status', name: 'Status', url: '/widgets/status/' }],
    }));
    store.endpoints.providers = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.sessions = vi.fn(async () => ({ sessions: [active] }));
    store.endpoints.sessionState = vi.fn(async () => ({
      active_response_id: 'r1',
      pending_ask_user: {
        call_id: 'call-1',
        questions: [{ question: 'Choose?', options: [{ label: 'Continue' }] }],
      },
    }));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: active,
      selected_transcript: { bodies: { messages: [] } },
    }));
    store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
    store.endpoints.response = vi.fn(async () => ({
      id: 'r1',
      session_id: 's1',
      run_epoch: 1,
      status: 'in_progress',
      last_sequence_number: 4,
    }));
    store.streamResponse = vi.fn(() => openStream.promise);
    (store as unknown as { startStatusPoll(): void }).startStatusPoll = vi.fn();

    await store.bootstrap();

    expect(store.startupDone.value).toBe(true);
    expect(store.askUser.value).toEqual(
      expect.objectContaining({ sessionId: 's1', callId: 'call-1' }),
    );
    await vi.waitFor(() => expect(store.streamResponse).toHaveBeenCalledWith('r1', 's1', 4));
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
