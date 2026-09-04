import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { APIError } from '../api/client';
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
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
};

beforeEach(() => localStorage.clear());

describe('AppStore compatibility behavior', () => {
  it('acknowledges only the exact visible marker after final transcript bodies load', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      {
        ...session(),
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 42,
        attentionResponseId: 'resp-a',
        attentionFinalRev: 7,
        seenThroughSeq: 0,
        attentionUnseen: true,
      },
    ];
    store.activeSessionId.value = 's1';
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: {
        id: 's1',
        attention_store_instance_id: 'store-a',
        attention_seq: 42,
        attention_response_id: 'resp-a',
        attention_final_rev: 7,
        seen_through_seq: 0,
        attention_unseen: true,
      },
      selected_transcript: { bodies: { rev: 7, messages: [] } },
    }));
    store.endpoints.markAttentionSeen = vi.fn(async (_id, _store, throughSeq) => ({
      store_instance_id: 'store-a',
      latest_attention_seq: 42,
      seen_through_seq: throughSeq,
      attention_unseen: false,
    }));

    await store.loadSession('s1');
    await vi.waitFor(() =>
      expect(store.endpoints.markAttentionSeen).toHaveBeenCalledWith('s1', 'store-a', 42),
    );

    expect(store.sessions.value[0]).toMatchObject({ seenThroughSeq: 42, attentionUnseen: false });
    store.dispose();
  });

  it('acknowledges an orphan-style zero-revision marker after an authoritative load', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      {
        ...session(),
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 42,
        attentionResponseId: 'resp-a',
        attentionFinalRev: 0,
        attentionUnseen: true,
      },
    ];
    store.activeSessionId.value = 's1';
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: {
        id: 's1',
        attention_store_instance_id: 'store-a',
        attention_seq: 42,
        attention_response_id: 'resp-a',
        attention_final_rev: 0,
        attention_unseen: true,
      },
      selected_transcript: { bodies: { rev: 0, messages: [] } },
    }));
    store.endpoints.markAttentionSeen = vi.fn(async () => ({
      store_instance_id: 'store-a',
      latest_attention_seq: 42,
      seen_through_seq: 42,
      attention_unseen: false,
    }));

    await store.loadSession('s1');
    await vi.waitFor(() => expect(store.endpoints.markAttentionSeen).toHaveBeenCalledOnce());
    expect(store.sessions.value[0]).toMatchObject({ attentionUnseen: false, seenThroughSeq: 42 });
    store.dispose();
  });

  it('keeps a newer completion unseen when an older acknowledgement resolves late', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      {
        ...session(),
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 42,
        attentionResponseId: 'resp-a',
        attentionFinalRev: 7,
        seenThroughSeq: 0,
        attentionUnseen: true,
      },
    ];
    store.activeSessionId.value = 's1';
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: {
        id: 's1',
        attention_store_instance_id: 'store-a',
        attention_seq: 42,
        attention_response_id: 'resp-a',
        attention_final_rev: 7,
        seen_through_seq: 0,
        attention_unseen: true,
      },
      selected_transcript: { bodies: { rev: 7, messages: [] } },
    }));
    store.endpoints.markAttentionSeen = vi.fn(async () => ({
      store_instance_id: 'store-a',
      latest_attention_seq: 43,
      seen_through_seq: 42,
      response_id: 'resp-b',
      final_rev: 9,
      outcome: 'failed',
      terminal_at: '2026-08-30T12:00:00Z',
      attention_unseen: true,
    }));

    await store.loadSession('s1');
    await vi.waitFor(() => expect(store.endpoints.markAttentionSeen).toHaveBeenCalledOnce());

    expect(store.sessions.value[0]).toMatchObject({
      attentionSeq: 43,
      seenThroughSeq: 42,
      attentionResponseId: 'resp-b',
      attentionFinalRev: 9,
      attentionOutcome: 'failed',
      attentionUnseen: true,
    });
    store.dispose();
  });

  it('does not acknowledge a completion loaded in a hidden tab', async () => {
    const store = new AppStore(config);
    const originalVisibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    try {
      store.sessions.value = [
        {
          ...session(),
          attentionStoreInstanceId: 'store-a',
          attentionSeq: 42,
          attentionFinalRev: 7,
          attentionUnseen: true,
        },
      ];
      store.activeSessionId.value = 's1';
      store.endpoints.sessionState = vi.fn(async () => ({}));
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: {
          id: 's1',
          attention_store_instance_id: 'store-a',
          attention_seq: 42,
          attention_final_rev: 7,
          attention_unseen: true,
        },
        selected_transcript: { bodies: { rev: 7, messages: [] } },
      }));
      store.endpoints.markAttentionSeen = vi.fn();

      await store.loadSession('s1');
      await Promise.resolve();

      expect(store.endpoints.markAttentionSeen).not.toHaveBeenCalled();
      expect(store.sessions.value[0]).toMatchObject({ attentionUnseen: true });
    } finally {
      if (originalVisibility)
        Object.defineProperty(document, 'visibilityState', originalVisibility);
      else delete (document as unknown as Record<string, unknown>).visibilityState;
      store.dispose();
    }
  });

  it('does not acknowledge a stale selection after the route changes', async () => {
    const store = new AppStore(config);
    const selected = deferred<Record<string, unknown>>();
    store.sessions.value = [
      {
        ...session(),
        attentionStoreInstanceId: 'store-a',
        attentionSeq: 42,
        attentionFinalRev: 7,
        attentionUnseen: true,
      },
    ];
    store.activeSessionId.value = 's1';
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(() => selected.promise);
    store.endpoints.markAttentionSeen = vi.fn();

    const loading = store.loadSession('s1');
    store.newChat();
    selected.resolve({
      selected_session: {
        id: 's1',
        attention_store_instance_id: 'store-a',
        attention_seq: 42,
        attention_final_rev: 7,
        attention_unseen: true,
      },
      selected_transcript: { bodies: { rev: 7, messages: [] } },
    });
    await loading;
    await Promise.resolve();

    expect(store.endpoints.markAttentionSeen).not.toHaveBeenCalled();
    store.dispose();
  });

  it('opens a pending draft shell and materializes a blank session on demand', async () => {
    const store = new AppStore(config);
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.projectsEnabled.value = false;
    store.shellStore.enabled.value = true;
    store.prompt.value = 'keep this draft';
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

    store.openShell();
    expect(store.shellStore.visible.value).toBe(true);
    expect(store.shellStore.sessionId.value).toBe('');
    expect(store.prompt.value).toBe('keep this draft');
    expect(store.toasts.value).toHaveLength(0);

    await expect(store.ensureShellSession()).resolves.toBe('session-created');
    expect(store.endpoints.createBlankSession).toHaveBeenCalledWith(
      expect.objectContaining({ use_default_workspace: true }),
    );
    expect(store.activeSessionId.value).toBe('session-created');
    expect(store.draftActive.value).toBe(false);
    expect(store.sessions.value.map((entry) => entry.id)).toEqual(['session-created']);
    expect(store.prompt.value).toBe('keep this draft');
    store.dispose();
  });

  it('detaches and hides a bound shell before selecting or creating a conversation', async () => {
    const store = new AppStore(config);
    const first = session();
    const second = { ...session(), id: 's2', title: 'Second' };
    store.sessions.value = [first, second];
    store.activeSessionId.value = first.id;
    store.draftActive.value = false;
    store.shellStore.enabled.value = true;
    expect(store.shellStore.show(first.id)).toBe(true);
    store.shellStore.status.value = 'running';
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(async (id: string) => ({
      selected_session: { id },
      selected_transcript: { bodies: { messages: [] } },
    }));
    store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
    store.endpoints.tree = vi.fn(async () => ({}));

    const selecting = store.selectSession(second);
    expect(store.shellStore.visible.value).toBe(false);
    expect(store.activeSessionId.value).toBe(second.id);
    await selecting;

    expect(store.shellStore.show(second.id)).toBe(true);
    store.newChat();
    expect(store.shellStore.visible.value).toBe(false);
    expect(store.activeSessionId.value).toBe('');
    store.dispose();
  });

  it('moves the active session to root after worktree cleanup', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [
        {
          ...session(),
          projectId: 'project-1',
          workingDir: '/worktrees/feature',
          worktreeDir: '/worktrees/feature',
        },
      ];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.selectedDraftWorktree.value = '/worktrees/stale-draft';
      store.projectsEnabled.value = true;
      store.endpoints.mergeWorktree = vi.fn(async () => ({
        result: { root_dir: '/repo' },
        cleanup: { removed: true },
        session: { id: 's1', cwd: '/repo', worktree_dir: '' },
      }));
      store.endpoints.projectWorktrees = vi.fn(async () => ({ worktrees: [] }));

      await store.mergeWorktree('/worktrees/feature');

      expect(store.endpoints.mergeWorktree).toHaveBeenCalledWith(
        'project-1',
        '/worktrees/feature',
        's1',
        false,
      );
      expect(store.activeSession.value).toMatchObject({ worktreeDir: '', workingDir: '/repo' });
      expect(store.selectedDraftWorktree.value).toBe('/worktrees/stale-draft');
      expect(store.currentWorktreeDir.value).toBe('');
    } finally {
      store.dispose();
    }
  });

  it('moves to root before sending the shared assisted-recovery prompt', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [
        {
          ...session(),
          projectId: 'project-1',
          workingDir: '/worktrees/feature',
          worktreeDir: '/worktrees/feature',
        },
      ];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.projectsEnabled.value = true;
      store.modal.value = 'worktrees';
      store.endpoints.assistedMergeWorktree = vi.fn(async () => ({
        result: { root_dir: '/repo', changed_files: ['file.txt'] },
        session: { id: 's1', cwd: '/repo', worktree_dir: '' },
        notice: 'Assisted recovery started.',
        prompt: 'Resolve the worktree conflict in root.',
      }));
      store.endpoints.projectWorktrees = vi.fn(async () => ({ worktrees: [] }));
      store.endpoints.createResponse = vi.fn(
        async () => new Response('stop after request capture', { status: 400 }),
      );

      await store.recoverWorktree('/worktrees/feature');

      expect(store.endpoints.assistedMergeWorktree).toHaveBeenCalledWith(
        'project-1',
        '/worktrees/feature',
        's1',
      );
      expect(store.activeSession.value).toMatchObject({ worktreeDir: '', workingDir: '/repo' });
      expect(store.modal.value).toBe('');
      expect(store.endpoints.createResponse).toHaveBeenCalledWith(
        expect.not.objectContaining({ worktree_dir: expect.anything() }),
        's1',
        expect.any(String),
        expect.any(AbortSignal),
      );
      const request = vi.mocked(store.endpoints.createResponse).mock.calls[0]?.[0];
      expect(JSON.stringify(request)).toContain('Resolve the worktree conflict in root.');
    } finally {
      store.dispose();
    }
  });

  it('adopts the root session when recovery fails after the server move', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [
        {
          ...session(),
          projectId: 'project-1',
          workingDir: '/worktrees/feature',
          worktreeDir: '/worktrees/feature',
        },
      ];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.projectsEnabled.value = true;
      store.endpoints.assistedMergeWorktree = vi.fn(async () => {
        throw new APIError(
          'root became dirty',
          409,
          JSON.stringify({
            error: 'root_dirty',
            result: { root_dir: '/repo' },
            session: { id: 's1', cwd: '/repo', worktree_dir: '' },
          }),
        );
      });

      await expect(store.recoverWorktree('/worktrees/feature')).rejects.toThrow(
        'root became dirty',
      );

      expect(store.activeSession.value).toMatchObject({ worktreeDir: '', workingDir: '/repo' });
    } finally {
      store.dispose();
    }
  });

  it('transfers a draft worktree to the session before the first request', async () => {
    const store = new AppStore(config);
    try {
      store.projectsEnabled.value = true;
      store.activeProjectId.value = 'project-1';
      store.selectedDraftWorktree.value = '/worktrees/feature';
      store.prompt.value = 'Start in the selected checkout';
      store.endpoints.createResponse = vi.fn(
        async () => new Response('stop after request capture', { status: 400 }),
      );

      await store.send();

      expect(store.endpoints.createResponse).toHaveBeenCalledWith(
        expect.objectContaining({
          project_id: 'project-1',
          worktree_dir: '/worktrees/feature',
        }),
        expect.stringMatching(/^draft_/),
        expect.any(String),
        expect.any(AbortSignal),
      );
      expect(store.activeSession.value?.worktreeDir).toBe('/worktrees/feature');
      expect(store.currentWorktreeDir.value).toBe('/worktrees/feature');
      expect(store.selectedDraftWorktree.value).toBe('');
    } finally {
      store.dispose();
    }
  });

  it('keeps the first message visible while a new session transcript catches up', async () => {
    const store = new AppStore(config);
    let streamController: ReadableStreamDefaultController<Uint8Array> | undefined;
    let sending: Promise<void> = Promise.resolve();
    try {
      store.sessions.value = [];
      store.activeSessionId.value = '';
      store.draftActive.value = true;
      store.endpoints.createResponse = vi.fn(
        async () =>
          new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                streamController = controller;
              },
            }),
            { headers: { 'x-response-id': 'r1', 'x-session-id': 's1' } },
          ),
      );
      store.endpoints.sessionState = vi.fn(async () => ({
        active_run: true,
        active_response_id: 'r1',
      }));
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: { id: 's1', active_run: true, active_response_id: 'r1' },
        selected_transcript: { bodies: { rev: 0, messages: [] } },
      }));
      vi.spyOn(store.runEngine, 'resumeResponse').mockResolvedValue(undefined);
      store.prompt.value = 'My first message';

      sending = store.send();
      await vi.waitFor(() => expect(store.activeSessionId.value).toBe('s1'));
      expect(store.visibleMessages.value.map((message) => message.content)).toEqual([
        'My first message',
      ]);

      // Session hydration may beat durable transcript persistence immediately
      // after response admission. It must not erase the locally submitted row.
      await store.loadSession('s1');

      expect(store.activeSession.value?.messages).toEqual([]);
      expect(store.visibleMessages.value.map((message) => message.content)).toEqual([
        'My first message',
      ]);
    } finally {
      streamController?.close();
      store.dispose();
      await sending;
    }
  });

  it('switches the active conversation to a selected worktree', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [{ ...session(), projectId: 'project-1', workingDir: '/repo' }];
      store.activeSessionId.value = 's1';
      store.projectsEnabled.value = true;
      store.endpoints.switchWorktree = vi.fn(async () => ({
        cwd: '/worktrees/feature',
        worktree_dir: '/worktrees/feature',
      }));
      store.endpoints.projectWorktrees = vi.fn(async () => ({ worktrees: [] }));

      await store.switchWorktree('/worktrees/feature');

      expect(store.endpoints.switchWorktree).toHaveBeenCalledWith(
        'project-1',
        '/worktrees/feature',
        's1',
      );
      expect(store.activeSession.value).toMatchObject({
        worktreeDir: '/worktrees/feature',
        workingDir: '/worktrees/feature',
      });
    } finally {
      store.dispose();
    }
  });

  it('clears a draft selection when its worktree is removed', async () => {
    const store = new AppStore(config);
    try {
      store.projectsEnabled.value = true;
      store.activeProjectId.value = 'project-1';
      store.draftActive.value = true;
      store.prompt.value = 'Draft prompt';
      store.chooseDraftWorktree('/worktrees/feature');
      store.modal.value = 'worktrees';
      store.endpoints.removeWorktree = vi.fn(async () => ({ ok: true }));
      store.endpoints.projectWorktrees = vi.fn(async () => ({ worktrees: [] }));

      await store.removeWorktree('/worktrees/feature', true);

      expect(store.selectedDraftWorktree.value).toBe('');
      expect(store.modal.value).toBe('worktrees');
      expect(readDrafts(localStorage, store.keys.draftMessages)[0]?.worktreeDir).toBe('');
    } finally {
      store.dispose();
    }
  });

  it('forwards clean worktree creation to the project endpoint', async () => {
    const store = new AppStore(config);
    try {
      store.projectsEnabled.value = true;
      store.activeProjectId.value = 'project-1';
      store.endpoints.createProjectWorktree = vi.fn(async () => ({}));
      store.endpoints.projectWorktrees = vi.fn(async () => ({ worktrees: [] }));

      await store.createWorktree('clean-tree', true);

      expect(store.endpoints.createProjectWorktree).toHaveBeenCalledWith('project-1', {
        name: 'clean-tree',
        clean: true,
      });
    } finally {
      store.dispose();
    }
  });

  it('dismisses a toast without waiting for its automatic timeout', () => {
    vi.useFakeTimers();
    const store = new AppStore(config);
    try {
      store.toast('Something needs attention.', 'error');
      const [toast] = store.toasts.value;

      store.dismissToast(toast.id);
      expect(store.toasts.value).toEqual([
        expect.objectContaining({ id: toast.id, leaving: true }),
      ]);

      vi.advanceTimersByTime(160);
      expect(store.toasts.value).toEqual([]);
    } finally {
      store.dispose();
      vi.useRealTimers();
    }
  });

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

  it('closes mobile navigation synchronously before session loading finishes', async () => {
    const store = new AppStore(config);
    const next = { ...session(), id: 's2', title: 'Next session' };
    store.sessions.value = [session(), next];
    store.sidebarOpen.value = true;
    const state = deferred<Record<string, unknown>>();
    const selected = deferred<Record<string, unknown>>();
    store.endpoints.sessionState = vi.fn(() => state.promise);
    store.endpoints.selectedSession = vi.fn(() => selected.promise);
    store.endpoints.skills = vi.fn(async () => ({ skills: [] }));

    const selection = store.selectSession(next);

    expect(store.sidebarOpen.value).toBe(false);
    state.resolve({});
    selected.resolve({ selected_session: next, selected_transcript: { bodies: { messages: [] } } });
    await selection;

    store.sidebarOpen.value = true;
    store.newChat();
    expect(store.sidebarOpen.value).toBe(false);
  });

  it('preserves a running sidebar session while selected-session data loads', async () => {
    const store = new AppStore(config);
    const running = {
      ...session(),
      id: 's2',
      title: 'Running session',
      activeRun: true,
      activeResponseId: 'r2',
    };
    store.sessions.value = [session(), running];
    const state = deferred<Record<string, unknown>>();
    const selected = deferred<Record<string, unknown>>();
    store.endpoints.sessionState = vi.fn(() => state.promise);
    store.endpoints.selectedSession = vi.fn(() => selected.promise);
    store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
    const resumeResponse = vi.fn(async () => undefined);
    store.runEngine.resumeResponse = resumeResponse;

    const selection = store.selectSession(running);
    expect(store.streaming.value).toBe(true);
    expect(resumeResponse).toHaveBeenCalledWith('s2', 'r2');
    const observed: boolean[] = [];
    const unsubscribe = store.streaming.subscribe((value) => observed.push(value));

    state.resolve({ active_run: true, active_response_id: 'r2' });
    selected.resolve({
      // Selected transcript payloads intentionally omit live-run metadata.
      selected_session: { id: 's2', title: 'Running session' },
      selected_transcript: { bodies: { messages: [] } },
    });
    await selection;
    unsubscribe();

    expect(observed).toEqual([true]);
    expect(store.activeSession.value).toMatchObject({
      id: 's2',
      activeRun: true,
      activeResponseId: 'r2',
    });
    expect(resumeResponse).toHaveBeenCalledWith('s2', 'r2');
    store.dispose();
  });

  it('stops the visible run immediately while server cancellation winds down', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), activeResponseId: 'r1' }];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 1,
          startedRev: 0,
          reconnects: 0,
        }),
        messages: [
          {
            id: 'tools',
            role: 'tool-group',
            content: '',
            created: 1,
            status: 'running',
            toolGroupClosed: false,
            tools: [{ id: 'tool-1', name: 'slow tool', status: 'running' }],
          },
        ],
      },
    };
    const acknowledgement = deferred<Record<string, unknown>>();
    store.endpoints.cancelResponse = vi.fn(() => acknowledgement.promise);

    const stopping = store.cancel();

    expect(store.streaming.value).toBe(false);
    expect(store.runs.value.s1.run.status).toBe('cancelled');
    expect(store.runs.value.s1.messages[0]).toMatchObject({
      status: 'done',
      toolGroupClosed: true,
      tools: [{ status: 'cancelled' }],
    });
    // A frame that was already queued when Stop was clicked cannot restart UI.
    store.applyResponseEvent('s1', {
      type: 'response.output_text.delta',
      response_id: 'r1',
      run_epoch: 1,
      sequence_number: 2,
      delta: 'late',
    });
    expect(store.runs.value.s1.run.status).toBe('cancelled');
    expect(store.runs.value.s1.messages.some((message) => message.content === 'late')).toBe(false);

    acknowledgement.resolve({ id: 'r1', status: 'cancelling' });
    await stopping;
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

  it('clears session-only goal and changes panel state for a new chat', () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.goal.value = { objective: 'Finish the current session', status: 'active' };
    store.diff.value = {
      ...store.diff.value,
      open: true,
      maximized: true,
      sessionId: 's1',
    };

    store.newChat();

    expect(store.goal.value).toBeNull();
    expect(store.diff.value).toMatchObject({ open: false, maximized: false });
  });

  it('clears assignment state before opening add project', () => {
    const store = new AppStore(config);
    store.projectTarget.value = session();

    store.openAddProject();

    expect(store.projectTarget.value).toBeNull();
    expect(store.modal.value).toBe('project');
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

  it('returns the editable fallback when AI title refinement abstains', async () => {
    const store = new AppStore(config);
    const target = session();
    store.renameTarget.value = target;
    store.endpoints.refineTitle = vi.fn(async () => ({
      refinement_status: 'abstained',
      short_title: target.title,
      long_title: target.longTitle,
    }));

    await expect(store.improveTitle()).resolves.toEqual({
      title: target.title,
      detail: '',
      abstained: true,
    });
  });

  it('loads configured MCP servers before the first message', async () => {
    const store = new AppStore(config);
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.endpoints.getMCP = vi.fn(async () => ({
      enabled: [],
      servers: [{ name: 'github', configured: true, status: 'stopped' }],
    }));

    await store.loadMCP();

    expect(store.endpoints.getMCP).toHaveBeenCalledWith(expect.stringMatching(/^draft_/));
    expect(store.mcp.value.servers).toEqual([
      expect.objectContaining({ name: 'github', configured: true, status: 'stopped' }),
    ]);
  });

  it('keeps pre-message MCP changes on the draft used by the first send', async () => {
    const store = new AppStore(config);
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.mcp.value = {
      ownerId: store.composer.runtimeDraftId(),
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
    store.endpoints.setMCP = vi.fn(async (_id, enabled) => ({
      enabled,
      servers: [{ name: 'github', enabled: true, status: 'ready' }],
    }));
    store.endpoints.createResponse = vi.fn(async () => {
      const encoder = new TextEncoder();
      const frames = [
        ['response.created', { response: { id: 'r1', status: 'in_progress' } }],
        ['response.completed', { response: { id: 'r1', status: 'completed' }, final_rev: 1 }],
      ] as const;
      return new Response(
        new ReadableStream<Uint8Array>({
          start(controller) {
            frames.forEach(([type, payload], index) =>
              controller.enqueue(
                encoder.encode(
                  `event: ${type}\ndata: ${JSON.stringify({
                    ...payload,
                    response_id: 'r1',
                    run_epoch: 1,
                    sequence_number: index + 1,
                  })}\n\n`,
                ),
              ),
            );
            controller.enqueue(encoder.encode('data: [DONE]\n\n'));
            controller.close();
          },
        }),
        {
          headers: {
            'x-response-id': 'r1',
            'x-session-id': 's1',
            'x-session-number': '7406',
          },
        },
      );
    });

    await store.toggleMCP('github');
    const draftId = vi.mocked(store.endpoints.setMCP).mock.calls[0]?.[0];
    const composerId = store.composer.storageId();
    store.storage.setItem(store.keys.draftSessionActive, composerId);
    history.replaceState(null, '', '/ui/');
    expect(draftId).toMatch(/^draft_/);
    expect(composerId).toMatch(/^draft:/);
    expect(draftId?.slice('draft_'.length)).not.toBe(composerId.slice('draft:'.length));

    store.prompt.value = 'Use the configured server';
    await store.send();

    expect(store.endpoints.createResponse).toHaveBeenCalledWith(
      expect.any(Object),
      draftId,
      expect.any(String),
      expect.any(AbortSignal),
    );
    expect(store.activeSessionId.value).toBe('s1');
    expect(store.activeSession.value).toMatchObject({ number: 7406, mcpEnabled: ['github'] });
    expect(location.pathname).toBe('/ui/chat/7406');
    expect(store.storage.getItem(store.keys.draftSessionActive)).toBeNull();
    expect(store.mcp.value).toMatchObject({ ownerId: 's1', enabled: ['github'] });
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

  it('starts MCP OAuth from a user popup and keeps flow data ephemeral', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    const assign = vi.fn();
    const close = vi.fn();
    const popup = { location: { assign }, close } as unknown as Window;
    vi.spyOn(window, 'open').mockReturnValue(popup);
    store.endpoints.startMCPOAuth = vi.fn(async () => ({
      flow_id: 'flow-id',
      authorization_url: 'https://auth.example/authorize?state=capability',
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      state: 'pending' as const,
    }));
    store.endpoints.cancelMCPOAuth = vi.fn(async () => ({}));
    store.endpoints.getMCP = vi.fn(async () => ({ servers: [], enabled: [] }));

    await store.startMCPOAuth('protected');

    expect(window.open).toHaveBeenCalledWith('', '_blank', 'popup=yes,width=560,height=720');
    expect(assign).toHaveBeenCalledWith('https://auth.example/authorize?state=capability');
    expect(store.mcp.value.oauth?.protected).toMatchObject({
      flowId: 'flow-id',
      state: 'pending',
      popupBlocked: false,
    });
    const browserStorage = Array.from({ length: localStorage.length }, (_, index) =>
      localStorage.getItem(localStorage.key(index) || ''),
    ).join('');
    expect(browserStorage).not.toContain('capability');

    await store.cancelMCPOAuth('protected');
    expect(store.endpoints.cancelMCPOAuth).toHaveBeenCalledWith('s1', 'protected', 'flow-id');
    expect(close).toHaveBeenCalled();
    expect(store.mcp.value.oauth?.protected).toBeUndefined();
  });

  it('uses the fresh draft identity for MCP OAuth before the first message', async () => {
    const store = new AppStore(config);
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    vi.spyOn(window, 'open').mockReturnValue(null);
    store.endpoints.startMCPOAuth = vi.fn(async () => ({
      flow_id: 'draft-flow',
      authorization_url: 'https://auth.example/authorize',
      expires_at: new Date(Date.now() + 60_000).toISOString(),
      state: 'pending' as const,
    }));
    store.endpoints.cancelMCPOAuth = vi.fn(async () => ({}));
    store.endpoints.getMCP = vi.fn(async () => ({ servers: [], enabled: [] }));

    await store.startMCPOAuth('protected');
    const draftId = vi.mocked(store.endpoints.startMCPOAuth).mock.calls[0]?.[0];
    expect(draftId).toMatch(/^draft_/);

    await store.cancelMCPOAuth('protected');
    expect(store.endpoints.cancelMCPOAuth).toHaveBeenCalledWith(draftId, 'protected', 'draft-flow');
  });

  it('stops MCP OAuth polling when the flow no longer exists server-side', async () => {
    vi.useFakeTimers();
    try {
      const store = new AppStore(config);
      store.sessions.value = [session()];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      vi.spyOn(window, 'open').mockReturnValue(null);
      store.endpoints.startMCPOAuth = vi.fn(async () => ({
        flow_id: 'flow-id',
        authorization_url: 'https://auth.example/authorize',
        expires_at: new Date(Date.now() + 60_000).toISOString(),
        state: 'pending' as const,
      }));
      const getFlow = vi.fn(async () => {
        throw new APIError('flow not found', 404);
      });
      store.endpoints.getMCPOAuthFlow = getFlow;

      await store.startMCPOAuth('protected');
      await vi.advanceTimersByTimeAsync(1000);
      expect(getFlow).toHaveBeenCalledTimes(1);
      expect(store.mcp.value.oauth?.protected).toMatchObject({ state: 'failed' });

      await vi.advanceTimersByTimeAsync(10_000);
      expect(getFlow).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows an MCP server starting while enablement is in flight', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.mcp.value = {
      ownerId: 's1',
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
    const request = deferred<{ servers: Record<string, unknown>[]; enabled: string[] }>();
    store.endpoints.setMCP = vi.fn(() => request.promise);

    const toggling = store.toggleMCP('github');

    expect(store.mcp.value).toMatchObject({
      enabled: ['github'],
      pending: 'github',
      servers: [{ name: 'github', enabled: true, status: 'starting' }],
    });

    request.resolve({
      enabled: ['github'],
      servers: [{ name: 'github', enabled: true, status: 'ready', tools: 3 }],
    });
    await toggling;
    expect(store.mcp.value).toMatchObject({
      pending: '',
      servers: [{ name: 'github', enabled: true, status: 'ready', tools: 3 }],
    });
  });

  it('rolls back an MCP toggle and exposes a recoverable save error', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.mcp.value = {
      ownerId: 's1',
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

  it('restores an active new-chat draft on reload without reporting a stale-write conflict', async () => {
    const seed = new AppStore(config);
    const draftID = 'draft:reload';
    localStorage.setItem(seed.keys.draftSessionActive, draftID);
    saveDraft(localStorage, seed.keys.draftMessages, {
      sessionId: draftID,
      content: 'keep this draft through reload',
      updated: Date.now(),
      rev: 0,
      model: 'test-model',
    });

    const store = new AppStore(config);
    store.endpoints.capabilities = vi.fn(async () => ({ projects: { enabled: false } }));
    store.endpoints.providers = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
    store.endpoints.sessions = vi.fn(async () => ({ object: 'list', data: [] }));
    (store as unknown as { startStatusPoll(): void }).startStatusPoll = vi.fn();

    await store.bootstrap();

    expect(store.draftActive.value).toBe(true);
    expect(store.prompt.value).toBe('keep this draft through reload');
    expect(store.toasts.value).toEqual([]);
    expect(readDrafts(localStorage, store.keys.draftMessages)).toEqual([
      expect.objectContaining({
        sessionId: draftID,
        content: 'keep this draft through reload',
        rev: 1,
      }),
    ]);
  });

  it('persists and paginates the cross-project Recent view without duplicating rows', async () => {
    const store = new AppStore(config);
    expect(store.sidebarView.value).toBe('recent');
    store.setSidebarView('projects');
    expect(localStorage.getItem(store.keys.sidebarView)).toBe('projects');
    expect(new AppStore(config).sidebarView.value).toBe('projects');

    store.projectsEnabled.value = true;
    store.endpoints.sidebar = vi.fn(async () => ({
      groups: [
        {
          project: { id: 'p1', name: 'Alpha' },
          sessions: [{ id: 's1', title: 'First', created_at: 3, last_message_at: 3 }],
        },
      ],
      recent_sessions: [
        {
          id: 's1',
          title: 'First',
          project_id: 'p1',
          project_name: 'Alpha',
          created_at: 3,
          last_message_at: 3,
        },
        { id: 's2', title: 'Second', created_at: 2, last_message_at: 2 },
      ],
      recent_next_cursor: 'recent-cursor',
    }));
    await store.refreshSidebar();
    expect(store.recentSessions.value.map((session) => session.id)).toEqual(['s1', 's2']);
    expect(store.sessions.value.map((session) => session.id)).toEqual(['s1', 's2']);
    expect(store.recentCursor.value).toBe('recent-cursor');

    store.endpoints.recentSessions = vi.fn(async () => ({
      sessions: [
        { id: 's2', title: 'Second', created_at: 2, last_message_at: 2 },
        { id: 's3', title: 'Third', created_at: 1, last_message_at: 1 },
      ],
      next_cursor: 'deep-cursor',
    }));
    await store.loadMoreRecent();
    expect(store.endpoints.recentSessions).toHaveBeenCalledWith('recent-cursor', false);
    expect(store.recentSessions.value.map((session) => session.id)).toEqual(['s1', 's2', 's3']);
    expect(store.recentCursor.value).toBe('deep-cursor');

    store.sessionStore.applySidebar({
      groups: [
        {
          project: { id: 'p1', name: 'Alpha' },
          sessions: [{ id: 's1', title: 'First', created_at: 4, last_message_at: 4 }],
        },
      ],
      recent_sessions: [
        { id: 's1', title: 'First', created_at: 4, last_message_at: 4 },
        { id: 's2', title: 'Second', created_at: 2, last_message_at: 2 },
      ],
      recent_next_cursor: 'refreshed-first-page-cursor',
    });
    expect(store.recentSessions.value.map((session) => session.id)).toEqual(['s1', 's2', 's3']);
    expect(store.recentCursor.value).toBe('deep-cursor');

    store.endpoints.recentSessions = vi.fn(async () => ({ sessions: [] }));
    await store.loadMoreRecent();
    expect(store.endpoints.recentSessions).toHaveBeenCalledWith('deep-cursor', false);
    expect(store.recentCursor.value).toBe('');
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

  it('drops a restored draft attachment when its prepared blob is missing', async () => {
    const seed = new AppStore(config);
    const draftID = 'draft:missing-attachment';
    localStorage.setItem(seed.keys.draftSessionActive, draftID);
    saveDraft(localStorage, seed.keys.draftMessages, {
      sessionId: draftID,
      content: 'keep the rest of this draft',
      updated: 1,
      attachments: [
        {
          id: 'missing-image',
          blobRef: 'missing-image',
          draftId: draftID,
          name: 'image.png',
          type: 'image/png',
          size: 10,
          status: 'ready',
        },
      ],
    });

    const store = new AppStore(config);
    store.newChat(true, '', false);

    await vi.waitFor(() => expect(store.attachments.value).toEqual([]));
    expect(store.prompt.value).toBe('keep the rest of this draft');
    expect(store.toasts.value).toEqual([]);
    expect(readDrafts(localStorage, store.keys.draftMessages)).toEqual([
      expect.objectContaining({ sessionId: draftID, attachments: [] }),
    ]);
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

  it('keeps a known transcript revision when a sidebar summary omits it', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), transcriptRev: 11 }];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.projectsEnabled.value = false;
    store.endpoints.sessions = vi.fn(async () => ({
      sessions: [{ id: 's1', title: 'Sidebar summary without a revision' }],
    }));

    await store.refreshSidebar();

    expect(store.activeSession.value?.transcriptRev).toBe(11);
  });

  it('uses selected transcript bodies.rev for undo concurrency', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    const selected = {
      selected_session: { id: 's1', title: 'Test' },
      selected_transcript: {
        bodies: {
          rev: 7,
          messages: [
            { id: 41, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'question' }] },
            {
              id: 42,
              sequence: 1,
              role: 'assistant',
              parts: [{ type: 'text', text: 'answer' }],
            },
          ],
        },
      },
    };
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.selectedSession = vi.fn(async () => selected);
    store.endpoints.mutateTranscript = vi.fn(async () => ({ rev: 8, user_text: 'question' }));

    await store.loadSession('s1');
    await store.mutateTranscript('undo');

    expect(store.endpoints.mutateTranscript).toHaveBeenCalledWith('s1', 'undo', {
      expected_rev: 7,
      expected_head_id: 42,
    });
  });

  it('forks a settled loaded session from its latest durable message row', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.sessionState = vi.fn(async () => ({ lastResponseId: 'resp_msg_42' }));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', title: 'Test' },
      selected_transcript: {
        bodies: {
          rev: 3,
          messages: [
            {
              id: 41,
              sequence: 1,
              role: 'user',
              parts: [{ type: 'text', text: 'question' }],
            },
            {
              id: 42,
              sequence: 2,
              role: 'assistant',
              response_id: 'r1',
              parts: [{ type: 'text', text: 'settled answer' }],
            },
            {
              id: 43,
              sequence: 3,
              role: 'event',
              parts: [{ type: 'model_swap', text: 'Model switched' }],
            },
          ],
        },
      },
    }));
    store.endpoints.branch = vi.fn(async () => ({ session: { id: 's2', title: 'Fork' } }));
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);

    await store.loadSession('s1');
    expect(store.activeSession.value?.transcriptRev).toBe(3);
    await store.branchCommand('fork');

    expect(store.endpoints.branch).toHaveBeenCalledWith('s1', {
      anchor_message_id: 42,
      idempotency_key: expect.any(String),
    });
    expect(store.toasts.value).toEqual([]);
  });

  it('allows a fork before an in-flight first response has a durable boundary', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), activeResponseId: 'r1' }];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 1,
          startedRev: 0,
          reconnects: 0,
        }),
        messages: [
          {
            id: 'pending-user',
            role: 'user',
            content: 'first question',
            created: 1,
            pending: true,
          },
        ],
      },
    };
    store.endpoints.branch = vi.fn(async () => ({ session: { id: 's2', title: 'Fork' } }));
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);

    await store.branchCommand('fork');

    expect(store.endpoints.branch).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({ anchor_message_id: 0 }),
    );
    expect(store.toasts.value).toEqual([]);
  });

  it('shows branch failures and reuses the same idempotency key on retry', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);
    store.endpoints.branch = vi
      .fn()
      .mockRejectedValueOnce(new Error('branch service unavailable'))
      .mockResolvedValueOnce({ session: { id: 's2', title: 'Retry child' } });

    expect(await store.branchFrom('42', 'clean')).toBe(false);
    expect(store.branchError.value).toBe('branch service unavailable');
    expect(store.toasts.value.at(-1)?.message).toBe('branch service unavailable');
    expect(await store.branchFrom('42', 'clean')).toBe(true);

    const firstBody = vi.mocked(store.endpoints.branch).mock.calls[0][1] as Record<string, unknown>;
    const secondBody = vi.mocked(store.endpoints.branch).mock.calls[1][1] as Record<
      string,
      unknown
    >;
    expect(firstBody.idempotency_key).toBeTruthy();
    expect(secondBody.idempotency_key).toBe(firstBody.idempotency_key);
    expect(firstBody).not.toHaveProperty('expected_rev');
  });

  it('guards an in-flight branch against duplicate clicks', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    const request = deferred<Record<string, unknown>>();
    store.endpoints.branch = vi.fn(() => request.promise);
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);

    const first = store.branchFrom('42', 'clean');
    const second = store.branchFrom('42', 'clean');
    expect(store.branchBusy.value).toBe(true);
    expect(await second).toBe(false);
    expect(store.endpoints.branch).toHaveBeenCalledOnce();

    request.resolve({ session: { id: 's2', title: 'Single child' } });
    expect(await first).toBe(true);
    expect(store.branchBusy.value).toBe(false);
  });

  it('notifies when fork or thread commands collide with in-flight branch creation', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    const request = deferred<Record<string, unknown>>();
    store.endpoints.branch = vi.fn(() => request.promise);
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);

    const first = store.branchFrom('42', 'clean');
    await store.branchCommand('fork', 'first follow up');
    await store.branchCommand('thread', 'second follow up');

    expect(store.endpoints.branch).toHaveBeenCalledOnce();
    expect(store.toasts.value.slice(-2)).toEqual([
      expect.objectContaining({
        message: 'A conversation path is already being created.',
        kind: 'info',
      }),
      expect.objectContaining({
        message: 'A conversation path is already being created.',
        kind: 'info',
      }),
    ]);
    request.resolve({ session: { id: 's2', title: 'Single child' } });
    expect(await first).toBe(true);
  });

  it('treats an auto-send failure as follow-on work and clears branch retry identity', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.branch = vi
      .fn()
      .mockResolvedValueOnce({ session: { id: 's2', title: 'First child' } })
      .mockResolvedValueOnce({ session: { id: 's3', title: 'Second child' } });
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);
    store.send = vi.fn(async () => {
      throw new Error('send transport failed');
    });

    expect(await store.branchFrom('42', 'clean', '', 'follow up')).toBe(true);
    expect(store.branchError.value).toBe('');
    expect(store.toasts.value.at(-1)).toMatchObject({
      message: 'New path created, but its first message could not be sent: send transport failed',
      kind: 'error',
    });
    expect(await store.branchFrom('42', 'clean')).toBe(true);

    const firstBody = vi.mocked(store.endpoints.branch).mock.calls[0][1] as Record<string, unknown>;
    const secondBody = vi.mocked(store.endpoints.branch).mock.calls[1][1] as Record<
      string,
      unknown
    >;
    expect(secondBody.idempotency_key).not.toBe(firstBody.idempotency_key);
  });

  it('does not dismiss a different modal when branch creation completes', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.branchTarget.value = '42';
    store.modal.value = 'branch-context';
    const request = deferred<Record<string, unknown>>();
    store.endpoints.branch = vi.fn(() => request.promise);
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);

    const branch = store.branchFrom('42', 'clean');
    store.modal.value = 'settings';
    request.resolve({ session: { id: 's2', title: 'Child' } });

    expect(await branch).toBe(true);
    expect(store.modal.value).toBe('settings');
  });

  it('keeps and opens a created child when path-note preparation fails', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.branch = vi.fn(async () => ({ session: { id: 's2', title: 'Child' } }));
    store.endpoints.pathNotes = vi.fn(async () => {
      throw new Error('notes helper failed');
    });
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);

    expect(await store.branchFrom('42', 'notes')).toBe(true);

    expect(store.sessions.value.some((entry) => entry.id === 's2')).toBe(true);
    expect(store.selectSession).toHaveBeenCalledWith(expect.objectContaining({ id: 's2' }));
    expect(store.endpoints.branch).toHaveBeenCalledOnce();
    expect(store.toasts.value.at(-1)?.message).toContain(
      'New path created, but its additional context could not be prepared: notes helper failed',
    );
  });

  it('reports when active path notes omit in-progress source output', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.branch = vi.fn(async () => ({ session: { id: 's2', title: 'Child' } }));
    store.endpoints.pathNotes = vi.fn(async () => ({
      ready: true,
      limited: true,
      message: 'In-progress output was omitted.',
    }));
    store.refreshSidebar = vi.fn(async () => undefined);
    store.selectSession = vi.fn(async () => undefined);

    expect(await store.branchFrom('42', 'focused', 'keep durable findings')).toBe(true);

    expect(store.toasts.value.at(-1)).toMatchObject({
      message: 'In-progress output was omitted.',
      kind: 'info',
    });
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

  it('normalizes recovery interjection identity before durable reconciliation', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      {
        ...session(),
        messages: [
          {
            id: 'durable-first',
            role: 'user',
            content: 'this is me interjecting as a demo',
            created: 2,
            clientMessageId: 'interject-1',
          },
          {
            id: 'durable-second',
            role: 'user',
            content: 'and a second time',
            created: 3,
            clientMessageId: 'interject-2',
          },
          {
            id: 'durable-tools',
            role: 'tool-group',
            content: '',
            created: 4,
            responseId: 'r1',
            tools: [{ id: 'tool-after', name: 'shell', status: 'done' }],
          },
        ],
      },
    ];
    store.activeSessionId.value = 's1';
    store.endpoints.response = vi.fn(async () => ({
      status: 'in_progress',
      run_epoch: 1,
      last_sequence_number: 8,
      recovery: {
        sequence_number: 8,
        messages: [
          {
            id: 'interject-1',
            role: 'user',
            content: 'this is me interjecting as a demo',
            client_message_id: 'interject-1',
            response_id: 'r1',
            interruptState: 'interject',
            created: 2,
          },
          {
            id: 'interject-2',
            role: 'user',
            content: 'and a second time',
            client_message_id: 'interject-2',
            response_id: 'r1',
            interruptState: 'interject',
            created: 3,
          },
        ],
      },
    }));
    const internals = store as unknown as {
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
      streamResponse(responseId: string, sessionId: string, sequence: number): Promise<void>;
    };
    internals.streamResponse = vi.fn(async () => undefined);

    await internals.resumeResponse('s1', 'r1');

    expect(store.runs.value.s1.messages.map((message) => message.clientMessageId)).toEqual([
      'interject-1',
      'interject-2',
    ]);
    expect(store.visibleMessages.value.map((message) => message.id)).toEqual([
      'durable-first',
      'durable-second',
      'durable-tools',
    ]);
  });

  it('re-anchors running tool duration from recovery snapshots', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [session()];
      store.activeSessionId.value = 's1';
      store.endpoints.response = vi.fn(async () => ({
        status: 'in_progress',
        run_epoch: 1,
        last_sequence_number: 8,
        recovery: {
          sequence_number: 8,
          messages: [
            {
              id: 'tools',
              role: 'tool-group',
              created: 1,
              tools: [
                {
                  id: 'spawn-1',
                  name: 'spawn_agent',
                  status: 'running',
                  startedAt: 1,
                  durationMs: 5_000,
                },
              ],
            },
          ],
        },
      }));
      const internals = store as unknown as {
        resumeResponse(sessionId: string, responseId: string): Promise<void>;
        streamResponse(responseId: string, sessionId: string, sequence: number): Promise<void>;
      };
      internals.streamResponse = vi.fn(async () => undefined);
      const recoveredAt = Date.now();

      await internals.resumeResponse('s1', 'r1');

      const tool = store.runs.value.s1.messages[0].tools?.[0];
      expect(tool).toMatchObject({ id: 'spawn-1', status: 'running', durationMs: 5_000 });
      expect(tool?.startedAt).toBeGreaterThanOrEqual(recoveredAt - 5_000);
      expect(tool?.startedAt).toBeLessThanOrEqual(Date.now() - 5_000);
    } finally {
      store.dispose();
    }
  });

  it('keeps the initiating user row when recovery only returns response output', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [session()];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.runs.value = {
        s1: {
          ...initialProjection({
            responseId: 'r1',
            sessionId: 's1',
            epoch: 1,
            status: 'connecting',
            lastSequence: 0,
            startedRev: 0,
            reconnects: 0,
          }),
          messages: [
            {
              id: 'pending_c1',
              role: 'user',
              content: 'My first message',
              created: 1,
              clientMessageId: 'c1',
            },
          ],
        },
      };
      store.endpoints.response = vi.fn(async () => ({
        id: 'r1',
        status: 'completed',
        run_epoch: 1,
        last_sequence_number: 2,
        final_rev: 1,
        durable_handoff: true,
        recovery: {
          sequence_number: 2,
          messages: [{ id: 'a1', role: 'assistant', content: 'Done.', created: 2 }],
        },
      }));
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: { id: 's1', transcript_rev: 0 },
        selected_transcript: { bodies: { rev: 0, messages: [] } },
      }));
      store.endpoints.sessionState = vi.fn(async () => ({}));

      await store.runEngine.resumeResponse('s1', 'r1');

      expect(store.runs.value.s1.messages.map((message) => message.content)).toEqual([
        'My first message',
        'Done.',
      ]);
      expect(store.visibleMessages.value.map((message) => message.content)).toEqual([
        'My first message',
        'Done.',
      ]);
    } finally {
      store.dispose();
    }
  });

  it('recovers an authoritative terminal snapshot after the server advances offline', async () => {
    const store = new AppStore(config);
    const staleAsk = { sessionId: 's1', callId: 'stale-call', questions: [] };
    store.sessions.value = [
      { ...session(), activeRun: true, activeResponseId: 'r1', transcriptRev: 1 },
    ];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'connecting',
          lastSequence: 2,
          startedRev: 1,
          reconnects: 1,
        }),
        askUser: staleAsk,
        messages: [
          {
            id: 'stale-partial',
            role: 'assistant',
            content: 'stale partial attempt',
            created: 1,
            responseId: 'r1',
          },
        ],
      },
    };
    store.askUser.value = staleAsk;
    store.interjections.value = [
      { id: 'interject-1', sessionId: 's1', content: 'change course', state: 'pending' },
    ];
    store.endpoints.response = vi.fn(async () => ({
      id: 'r1',
      session_id: 's1',
      status: 'completed',
      run_epoch: 1,
      last_sequence_number: 20,
      started_rev: 1,
      final_rev: 5,
      durable_handoff: true,
      recovery: {
        sequence_number: 20,
        messages: [
          {
            id: 'interject-1',
            role: 'user',
            content: 'change course',
            client_message_id: 'interject-1',
            interrupt_state: 'interject',
            response_id: 'r1',
            created: 2,
          },
          {
            id: 'recovered-tools',
            role: 'tool-group',
            status: 'done',
            response_id: 'r1',
            created: 3,
            tools: [
              { id: 'tool-1', name: 'shell', status: 'done' },
              {
                id: 'plan-1',
                name: 'update_plan',
                status: 'done',
                resultStatus: 'success',
                arguments: JSON.stringify({
                  plan: [{ step: 'Recovered work', status: 'completed' }],
                }),
              },
            ],
          },
          {
            id: 'recovered-compaction',
            role: 'compaction-ref',
            response_id: 'r1',
            compaction_sequence: 17,
            compaction_seq: 10,
            compaction_count: 2,
            created: 4,
          },
          {
            id: 'recovered-answer',
            role: 'assistant',
            content: 'Done after reconnect.',
            response_id: 'r1',
            assistant_segment_ordinal: 1,
            segment_start_sequence: 18,
            segment_end_sequence: 19,
            created: 4,
          },
        ],
      },
    }));
    const selected = deferred<Record<string, unknown>>();
    store.endpoints.selectedSession = vi.fn(() => selected.promise);
    store.endpoints.sessionState = vi.fn(async () => ({ last_response_id: 'r1' }));
    const internals = store as unknown as {
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };

    const recovery = internals.resumeResponse('s1', 'r1');
    await vi.waitFor(() => expect(store.runs.value.s1.run.lastSequence).toBe(20));

    expect(store.runs.value.s1).toMatchObject({
      askUser: null,
      run: { status: 'completed', finalRev: 5, durableHandoff: true },
    });
    expect(store.runs.value.s1.messages.map((message) => message.id)).toEqual([
      'interject-1',
      'recovered-tools',
      'recovered-compaction',
      'recovered-answer',
    ]);
    expect(store.runs.value.s1.messages[2]).toMatchObject({
      role: 'compaction-boundary',
      content: 'Context compacted',
      eventSequence: 17,
      compactionSeq: 10,
      compactionCount: 2,
    });
    expect(store.runs.value.s1.messages[3]).toMatchObject({
      segmentStartSequence: 18,
      segmentEndSequence: 19,
    });
    expect(store.interjections.value).toEqual([]);
    expect(store.currentPlan.value).toEqual({
      plan: [{ step: 'Recovered work', status: 'completed' }],
    });
    expect(store.askUser.value).toBeNull();
    expect(store.sessions.value[0]).toMatchObject({
      activeRun: false,
      activeResponseId: null,
      lastResponseId: 'r1',
    });

    selected.resolve({
      selected_session: { id: 's1', title: 'Test', transcript_rev: 5 },
      selected_transcript: {
        bodies: {
          rev: 5,
          messages: [
            {
              id: 11,
              sequence: 1,
              role: 'user',
              client_message_id: 'interject-1',
              interrupt_state: 'interject',
              parts: [{ type: 'text', text: 'change course' }],
            },
            {
              id: 12,
              sequence: 2,
              role: 'assistant',
              response_id: 'r1',
              parts: [{ type: 'text', text: 'Done after reconnect.' }],
            },
          ],
        },
      },
    });
    await recovery;

    expect(store.runs.value.s1.messages).toEqual([]);
    expect(store.visibleMessages.value.map((message) => message.content)).toEqual([
      'change course',
      'Done after reconnect.',
    ]);
    await internals.resumeResponse('s1', 'r1');
    expect(store.endpoints.response).toHaveBeenCalledOnce();
  });

  it('uses an empty recovery snapshot to remove stale attempt output', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'connecting',
          lastSequence: 3,
          startedRev: 0,
          reconnects: 1,
        }),
        messages: [
          { id: 'discarded', role: 'assistant', content: 'discarded attempt', created: 1 },
        ],
        pendingGuardian: {
          'discarded-call': [{ outcome: 'warning', message: 'stale review' }],
        },
      },
    };
    store.endpoints.response = vi.fn(async () => ({
      status: 'in_progress',
      run_epoch: 1,
      last_sequence_number: 12,
      recovery: { sequence_number: 12 },
    }));
    const internals = store as unknown as {
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
      streamResponse(responseId: string, sessionId: string, after: number): Promise<void>;
    };
    internals.streamResponse = vi.fn(async () => undefined);

    await internals.resumeResponse('s1', 'r1');

    expect(store.runs.value.s1.messages).toEqual([]);
    expect(store.runs.value.s1.pendingGuardian).toEqual({});
    expect(store.runs.value.s1.run.lastSequence).toBe(12);
    expect(internals.streamResponse).toHaveBeenCalledWith('r1', 's1', 12);
  });

  it('hydrates durable pending interjections when a session is opened in another tab', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.endpoints.sessionState = vi.fn(async () => ({
      pending_interjections: [
        { id: 'pending-1', text: 'change course', status: 'queued' },
        { id: 'pending-2', text: 'inspect image', attachment_summary: '[image]', status: 'queued' },
      ],
    }));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', title: 'Test' },
      selected_transcript: { bodies: { messages: [] } },
    }));

    await store.loadSession('s1');

    expect(store.interjections.value).toEqual([
      { id: 'pending-1', sessionId: 's1', content: 'change course', state: 'pending' },
      { id: 'pending-2', sessionId: 's1', content: 'inspect image', state: 'pending' },
    ]);

    store.endpoints.sessionState = vi.fn(async () => ({ pending_interjections: [] }));
    await store.loadSession('s1');
    expect(store.interjections.value).toEqual([]);

    store.endpoints.sessionState = vi.fn(async () => ({
      pending_interjection: { id: 'legacy-pending', text: 'legacy state shape', status: 'queued' },
    }));
    await store.loadSession('s1');
    expect(store.interjections.value).toEqual([
      {
        id: 'legacy-pending',
        sessionId: 's1',
        content: 'legacy state shape',
        state: 'pending',
      },
    ]);
  });

  it('ignores pending-state snapshots older than a local interjection mutation', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.endpoints.interrupt = vi.fn(async () => ({}));
    const internals = store as unknown as {
      interjectionRevision: number;
      reconcilePendingInterjections(
        sessionId: string,
        state: Record<string, unknown>,
        expectedRevision?: number,
      ): void;
    };
    const sampledRevision = internals.interjectionRevision;

    await store.interject('fresh local steering');
    internals.reconcilePendingInterjections('s1', { pending_interjections: [] }, sampledRevision);

    expect(store.interjections.value).toEqual([
      expect.objectContaining({ content: 'fresh local steering', state: 'pending' }),
    ]);
  });

  it('retains terminal run-center history while retiring its durable transport owner', async () => {
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

    expect(store.runs.value.s1).toMatchObject({
      messages: [],
      run: { responseId: 'r1', status: 'completed', summary: 'identical answer' },
    });
    expect(store.visibleMessages.value.map((message) => message.content)).toEqual([
      'identical answer',
    ]);
    await internals.resumeResponse('s1', 'r1');
    expect(response).not.toHaveBeenCalled();
  });

  it('does not let an old terminal transcript refresh overwrite a newer response', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), transcriptRev: 4, messageBodiesRev: 4 }];
    store.activeSessionId.value = 's1';
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'completed',
        lastSequence: 8,
        startedRev: 3,
        finalRev: 5,
        durableHandoff: true,
        reconnects: 0,
      }),
    };
    const selected = deferred<Record<string, unknown>>();
    store.endpoints.selectedSession = vi.fn(() => selected.promise);
    store.endpoints.sessionState = vi.fn(async () => ({ last_response_id: 'r1' }));
    const internals = store as unknown as {
      refreshSessionMessages(
        sessionId: string,
        targetRev: number,
        expectedResponseId: string,
      ): Promise<void>;
    };

    const refresh = internals.refreshSessionMessages('s1', 5, 'r1');
    await vi.waitFor(() => expect(store.endpoints.selectedSession).toHaveBeenCalledOnce());
    const newerMessage = {
      id: 'new-user',
      role: 'user' as const,
      content: 'new work',
      created: 10,
      clientMessageId: 'new-client',
    };
    store.sessions.value = [{ ...store.sessions.value[0], messages: [newerMessage] }];
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r2',
        sessionId: 's1',
        epoch: 2,
        status: 'streaming',
        lastSequence: 1,
        startedRev: 5,
        reconnects: 0,
      }),
    };
    selected.resolve({
      selected_session: { id: 's1', transcript_rev: 5 },
      selected_transcript: {
        bodies: {
          rev: 5,
          messages: [
            {
              id: 4,
              role: 'assistant',
              response_id: 'r1',
              parts: [{ type: 'text', text: 'old answer' }],
            },
          ],
        },
      },
    });
    await refresh;

    expect(store.runs.value.s1.run.responseId).toBe('r2');
    expect(store.sessions.value[0].messages).toEqual([newerMessage]);
  });

  it('preserves live ownership while refreshing active transcript bodies', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      { ...session(), activeRun: true, activeResponseId: 'r1', transcriptRev: 2 },
    ];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'connecting',
        lastSequence: 3,
        startedRev: 1,
        reconnects: 1,
      }),
    };
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', transcript_rev: 3 },
      selected_transcript: { bodies: { rev: 3, messages: [] } },
    }));
    store.endpoints.sessionState = vi.fn(async () => ({}));

    await store.runEngine.refreshSessionMessages('s1');

    expect(store.sessions.value[0]).toMatchObject({
      activeRun: true,
      activeResponseId: 'r1',
      messageBodiesRev: 3,
    });
    expect(store.streaming.value).toBe(true);
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

  it('clears submitted draft ownership immediately without erasing a newer identical draft', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.prompt.value = 'Again';
    saveDraft(localStorage, store.keys.draftMessages, {
      sessionId: 's1',
      content: 'Again',
      updated: 1,
    });
    const accepted = deferred<Response>();
    store.endpoints.createResponse = vi.fn(() => accepted.promise);
    let streamController!: ReadableStreamDefaultController<Uint8Array>;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        streamController = controller;
      },
    });

    const sending = store.send();
    await vi.waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledOnce());
    expect(store.prompt.value).toBe('');
    expect(readDrafts(localStorage, store.keys.draftMessages)).not.toEqual(
      expect.arrayContaining([expect.objectContaining({ sessionId: 's1', content: 'Again' })]),
    );

    // A later identical draft is a new user-owned edit, not the old submitted
    // value coming back. The old request's acknowledgement must preserve it.
    store.prompt.value = 'Again';
    saveDraft(localStorage, store.keys.draftMessages, {
      sessionId: 's1',
      content: 'Again',
      updated: 2,
    });
    accepted.resolve(
      new Response(stream, {
        status: 200,
        headers: { 'x-response-id': 'r1', 'x-session-id': 's1' },
      }),
    );

    await vi.waitFor(() => expect(store.sessions.value[0].activeResponseId).toBe('r1'));
    expect(store.streaming.value).toBe(true);
    expect(store.runLivenessUnknown.value).toBe(false);
    expect(store.prompt.value).toBe('Again');
    expect(readDrafts(localStorage, store.keys.draftMessages)).toEqual(
      expect.arrayContaining([expect.objectContaining({ sessionId: 's1', content: 'Again' })]),
    );
    store.dispose();
    streamController.close();
    await sending;
  });

  it('lets an admitted POST adopt the response discovered by status without aborting it', async () => {
    const store = new AppStore(config);
    const accepted = deferred<Response>();
    let streamController!: ReadableStreamDefaultController<Uint8Array>;
    try {
      store.sessions.value = [session()];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.prompt.value = 'Race status against admission';
      store.endpoints.createResponse = vi.fn(() => accepted.promise);
      store.endpoints.response = vi.fn(async () => ({
        id: 'r1',
        status: 'in_progress',
        run_epoch: 1,
        last_sequence_number: 0,
        recovery: { sequence_number: 0 },
      }));

      const sending = store.send();
      await vi.waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledOnce());
      const [rawBody, , , rawSignal] = vi.mocked(store.endpoints.createResponse).mock.calls[0];
      const body = rawBody as Record<string, unknown>;
      const signal = rawSignal as AbortSignal;
      const clientMessageId = String(body.client_message_id || '');
      expect(clientMessageId).not.toBe('');
      store.endpoints.sessionStatus = vi.fn(async () => ({
        sessions: [
          {
            id: 's1',
            active_run: true,
            active_response_id: 'r1',
            client_message_id: clientMessageId,
          },
        ],
      }));

      await (store as unknown as { refreshStatus(): Promise<void> }).refreshStatus();

      expect(signal.aborted).toBe(false);
      expect(store.endpoints.response).not.toHaveBeenCalled();

      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          streamController = controller;
        },
      });
      accepted.resolve(
        new Response(stream, {
          status: 200,
          headers: { 'x-response-id': 'r1', 'x-session-id': 's1' },
        }),
      );

      await vi.waitFor(() => expect(store.streaming.value).toBe(true));
      expect(store.runs.value.s1.run.responseId).toBe('r1');
      expect(store.runLivenessUnknown.value).toBe(false);
      streamController.close();
      await sending;
    } finally {
      store.dispose();
    }
  });

  it('keeps the admitted POST stream when response.created supplies the server epoch', async () => {
    const store = new AppStore(config);
    let postSignal: AbortSignal | undefined;
    try {
      store.sessions.value = [session()];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.prompt.value = 'Use the original response stream';
      store.endpoints.response = vi.fn(async () => {
        throw new Error('snapshot recovery should not run');
      });
      store.endpoints.createResponse = vi.fn(async (_body, _sessionId, _requestId, signal) => {
        postSignal = signal;
        const encoder = new TextEncoder();
        const frames = [
          ['response.created', { response: { id: 'r1', status: 'in_progress' } }],
          ['response.output_text.delta', { delta: 'Done.' }],
          ['response.completed', { response: { id: 'r1', status: 'completed' }, final_rev: 2 }],
        ] as const;
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            frames.forEach(([type, payload], index) =>
              controller.enqueue(
                encoder.encode(
                  `event: ${type}\ndata: ${JSON.stringify({
                    ...payload,
                    response_id: 'r1',
                    run_epoch: 1788084563000000,
                    sequence_number: index + 1,
                  })}\n\n`,
                ),
              ),
            );
            controller.enqueue(encoder.encode('data: [DONE]\n\n'));
            controller.close();
          },
        });
        return new Response(body, {
          headers: { 'x-response-id': 'r1', 'x-session-id': 's1' },
        });
      });
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: { id: 's1', transcript_rev: 2 },
        selected_transcript: { bodies: { rev: 2, messages: [] } },
      }));
      store.endpoints.sessionState = vi.fn(async () => ({}));

      await store.send();

      expect(postSignal?.aborted).toBe(true);
      expect(store.endpoints.response).not.toHaveBeenCalled();
      expect(store.runs.value.s1.run).toMatchObject({
        responseId: 'r1',
        epoch: 1788084563000000,
        status: 'completed',
        lastSequence: 3,
      });
      expect(store.runs.value.s1.messages).toEqual([
        expect.objectContaining({ role: 'user', content: 'Use the original response stream' }),
        expect.objectContaining({ role: 'assistant', content: 'Done.' }),
      ]);
    } finally {
      store.dispose();
    }
  });

  it('ignores late transport failures after a response is already complete', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'completed',
          lastSequence: 4,
          startedRev: 0,
          reconnects: 0,
        }),
        messages: [{ id: 'done', role: 'assistant', content: 'Done.', created: 2 }],
      },
    };
    store.endpoints.responseEvents = vi.fn(async () => {
      throw new TypeError('Load failed');
    });

    await store.streamResponse('r1', 's1', 4);
    await store.streamResponse('r1', 's1', 4);

    expect(store.endpoints.responseEvents).toHaveBeenCalledTimes(2);
    expect(store.runs.value.s1.run.status).toBe('completed');
    expect(store.runs.value.s1.messages).toEqual([
      expect.objectContaining({ role: 'assistant', content: 'Done.' }),
    ]);
    expect(store.visibleMessages.value.some((message) => message.role === 'error')).toBe(false);
    expect(store.toasts.value).toEqual([]);
  });

  it('acquires the send lock before attachment materialization yields', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.prompt.value = 'send exactly once';
    store.attachments.value = [
      {
        id: 'attachment-1',
        name: 'note.txt',
        type: 'text/plain',
        dataURL: 'data:text/plain;base64,bm90ZQ==',
        status: 'ready',
      },
    ];
    const prepared = deferred<Record<string, unknown>>();
    store.attachmentInput = vi.fn(() => prepared.promise);
    store.endpoints.createResponse = vi.fn(
      async () => new Response('invalid request', { status: 400 }),
    );

    const first = store.send();
    expect(store.sendPending.value).toBe(true);
    const second = store.send();
    prepared.resolve({ type: 'input_file', filename: 'note.txt', file_data: 'data:' });
    await Promise.all([first, second]);

    expect(store.endpoints.createResponse).toHaveBeenCalledOnce();
    expect(store.sendPending.value).toBe(false);
  });

  it('keeps an ambiguous send durable and retries the same idempotency key', async () => {
    vi.useFakeTimers();
    const store = new AppStore(config);
    try {
      store.sessions.value = [session()];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.prompt.value = 'do not duplicate this';
      store.endpoints.createResponse = vi
        .fn()
        .mockRejectedValueOnce(new TypeError('browser transport disconnected'))
        .mockResolvedValueOnce(new Response('authoritatively rejected', { status: 400 }));

      await store.send();

      expect(store.prompt.value).toBe('');
      expect(store.visibleMessages.value).toEqual([
        expect.objectContaining({
          role: 'user',
          content: 'do not duplicate this',
          interruptState: 'checking_send',
        }),
      ]);
      expect(store.visibleMessages.value.some((message) => message.role === 'error')).toBe(false);
      expect(store.runs.value.s1.run.status).toBe('checking');

      const restored = new AppStore(config);
      restored.sessions.value = [session()];
      restored.activeSessionId.value = 's1';
      expect(restored.sendBlocked.value).toBe(true);
      expect(restored.visibleMessages.value).toEqual([
        expect.objectContaining({ interruptState: 'checking_send' }),
      ]);
      restored.dispose();

      await vi.advanceTimersByTimeAsync(1_000);
      for (let index = 0; index < 10; index += 1) await Promise.resolve();

      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2);
      const first = vi.mocked(store.endpoints.createResponse).mock.calls[0];
      const second = vi.mocked(store.endpoints.createResponse).mock.calls[1];
      expect(second[0]).toEqual(first[0]);
      expect(second[2]).toBe(first[2]);
      expect(store.pendingIntents.value).toEqual({});
      expect(store.prompt.value).toBe('do not duplicate this');
    } finally {
      store.dispose();
      vi.useRealTimers();
    }
  });

  it('clears checking state from the projected message after retry admission', async () => {
    vi.useFakeTimers();
    const store = new AppStore(config);
    let streamController: ReadableStreamDefaultController<Uint8Array> | undefined;
    try {
      store.sessions.value = [session()];
      store.activeSessionId.value = 's1';
      store.draftActive.value = false;
      store.prompt.value = 'retry this message';
      store.endpoints.createResponse = vi
        .fn()
        .mockRejectedValueOnce(new TypeError('browser transport disconnected'))
        .mockResolvedValueOnce(
          new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                streamController = controller;
              },
            }),
            { headers: { 'x-response-id': 'r1', 'x-session-id': 's1' } },
          ),
        );

      await store.send();
      expect(store.visibleMessages.value[0]).toMatchObject({
        content: 'retry this message',
        interruptState: 'checking_send',
      });

      await vi.advanceTimersByTimeAsync(1_000);
      for (let index = 0; index < 10; index += 1) await Promise.resolve();
      expect(store.activeSession.value?.activeResponseId).toBe('r1');

      // If pre-commit hydration removes the session copy, the projection is
      // visible and must no longer claim that delivery is still unknown.
      store.sessions.value = [{ ...store.activeSession.value!, messages: [] }];
      expect(store.visibleMessages.value).toEqual([
        expect.objectContaining({
          content: 'retry this message',
          pending: false,
          interruptState: undefined,
        }),
      ]);
    } finally {
      streamController?.close();
      store.dispose();
      vi.useRealTimers();
    }
  });

  it('adopts a different active response instead of suppressing it with an old stop marker', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'old-response' }];
    store.activeSessionId.value = 's1';
    store.runs.value = {
      s1: initialProjection({
        responseId: 'old-response',
        sessionId: 's1',
        epoch: 1,
        status: 'cancelled',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    const internals = store as unknown as {
      locallyStoppedResponses: Set<string>;
      refreshStatus(): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };
    internals.locallyStoppedResponses.add('old-response');
    internals.resumeResponse = vi.fn(async () => undefined);
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [{ id: 's1', active_run: true, active_response_id: 'new-response' }],
    }));

    await internals.refreshStatus();

    expect(store.sessions.value[0]).toMatchObject({
      activeRun: true,
      activeResponseId: 'new-response',
    });
    expect(internals.resumeResponse).toHaveBeenCalledWith('s1', 'new-response');
    expect(internals.locallyStoppedResponses.has('old-response')).toBe(false);
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

  it('reconciles an active session created in another tab before applying status', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [
        { id: 's2', active_run: true, last_message_at: 1_800_000_004_000, message_count: 1 },
      ],
    }));
    store.endpoints.sessions = vi.fn(async () => ({
      data: [
        {
          id: 's2',
          short_title: 'Created elsewhere',
          origin: 'web',
          created_at: 1_800_000_004_000,
          last_message_at: 1_800_000_004_000,
          message_count: 1,
        },
        { id: 's1', short_title: 'Test', origin: 'web', created_at: 1 },
      ],
    }));

    await (store as unknown as { refreshStatus(): Promise<void> }).refreshStatus();

    expect(store.endpoints.sessions).toHaveBeenCalledOnce();
    expect(store.sessions.value[0]).toMatchObject({
      id: 's2',
      title: 'Created elsewhere',
      activeRun: true,
      messageCount: 1,
    });
  });

  it('preserves sanitizer-shaped session identities across an unchanged status poll', async () => {
    const store = new AppStore(config);
    try {
      const stable = session();
      store.sessions.value = [stable];
      store.endpoints.sessionStatus = vi.fn(async () => ({ sessions: [{ id: 's1' }] }));
      const sessions = store.sessions.value;

      await (store as unknown as { refreshStatus(): Promise<void> }).refreshStatus();

      expect(store.sessions.value).toBe(sessions);
      expect(store.sessions.value[0]).toBe(stable);
    } finally {
      store.dispose();
    }
  });

  it('applies changed status while preserving unchanged sibling identities', async () => {
    const store = new AppStore(config);
    try {
      const changed = session();
      const stable = { ...session(), id: 's2', title: 'Stable' };
      store.sessions.value = [changed, stable];
      const sessions = store.sessions.value;
      store.endpoints.sessionStatus = vi.fn(async () => ({
        sessions: [
          { id: 's1', short_title: 'Renamed', active_run: true, transcript_rev: 3 },
          { id: 's2' },
        ],
      }));

      await (store as unknown as { refreshStatus(): Promise<void> }).refreshStatus();

      expect(store.sessions.value).not.toBe(sessions);
      expect(store.sessions.value[0]).not.toBe(changed);
      expect(store.sessions.value[0]).toMatchObject({
        title: 'Renamed',
        activeRun: true,
        transcriptRev: 3,
      });
      expect(store.sessions.value[1]).toBe(stable);
    } finally {
      store.dispose();
    }
  });

  it('clears stale remote activity without dropping an out-of-scope response anchor', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      { ...session(), activeRun: true, activeResponseId: 'finished-elsewhere' },
    ];
    store.endpoints.sessionStatus = vi.fn(async () => ({ sessions: [] }));

    await (store as unknown as { refreshStatus(): Promise<void> }).refreshStatus();

    expect(store.sessions.value[0]).toMatchObject({
      activeRun: false,
      activeResponseId: 'finished-elsewhere',
    });
  });

  it('clears a response anchor when the authoritative status entry reports completion', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      { ...session(), activeRun: true, activeResponseId: 'finished-elsewhere' },
    ];
    store.endpoints.sessionStatus = vi.fn(async () => ({ sessions: [{ id: 's1' }] }));

    await (store as unknown as { refreshStatus(): Promise<void> }).refreshStatus();

    expect(store.sessions.value[0]).toMatchObject({ activeRun: false, activeResponseId: null });
  });

  it('blocks a duplicate send when status reports a remote run before this tab attaches', () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'remote-r1' }];
      store.activeSessionId.value = 's1';

      expect(store.runs.value).toEqual({});
      expect(store.streaming.value).toBe(true);
      expect(store.canStop.value).toBe(false);
      expect(store.sendBlocked.value).toBe(true);
    } finally {
      store.dispose();
    }
  });

  it('keeps server-confirmed runs active independently of transport attachment', () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'r1' }];
      store.activeSessionId.value = 's1';
      store.runs.value = {
        s1: initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 1,
          startedRev: 0,
          reconnects: 0,
        }),
      };

      expect(store.responseTransportAttached.value).toBe(false);
      expect(store.streaming.value).toBe(true);
      expect(store.runLivenessUnknown.value).toBe(false);
      expect(store.canStop.value).toBe(true);
      expect(store.canInterject.value).toBe(true);
      expect(store.sendBlocked.value).toBe(false);

      store.runEngine.markResponseTransportActive('s1', 'r1', 1);
      expect(store.responseTransportAttached.value).toBe(true);
      expect(store.streaming.value).toBe(true);
      expect(store.runLivenessUnknown.value).toBe(false);

      store.runEngine.markResponseTransportActive('s1', 'r1', 2);
      store.runEngine.clearResponseTransport('s1', 'r1', 1);
      expect(store.streaming.value).toBe(true);

      store.runEngine.clearResponseTransport('s1', 'r1', 2);
      expect(store.responseTransportAttached.value).toBe(false);
      expect(store.streaming.value).toBe(true);
      expect(store.runLivenessUnknown.value).toBe(false);
      expect(store.runs.value.s1.run.status).toBe('streaming');
      expect(store.sendBlocked.value).toBe(false);

      store.sessions.value = [
        { ...store.sessions.value[0], activeRun: false, activeResponseId: null },
      ];
      expect(store.streaming.value).toBe(true);
      expect(store.runLivenessUnknown.value).toBe(false);
      expect(store.canStop.value).toBe(true);
      expect(store.canInterject.value).toBe(true);
      expect(store.sendBlocked.value).toBe(false);

      const recoveryEvidence = store.runEngine as unknown as {
        markResponseRecoveryFailure(sessionId: string, responseId: string): void;
      };
      for (let attempt = 0; attempt < 6; attempt += 1)
        recoveryEvidence.markResponseRecoveryFailure('s1', 'r1');
      expect(store.runLivenessUnknown.value).toBe(false);
      recoveryEvidence.markResponseRecoveryFailure('s1', 'r1');
      expect(store.runLivenessUnknown.value).toBe(true);
      // Stale transport evidence may show a warning, but it never changes the
      // authoritative running controls.
      expect(store.streaming.value).toBe(true);
      expect(store.canStop.value).toBe(true);

      store.runEngine.markResponseTransportActive('s1', 'r1', 3);
      expect(store.runLivenessUnknown.value).toBe(false);

      store.runs.value = {
        s1: {
          ...store.runs.value.s1,
          run: { ...store.runs.value.s1.run, status: 'completed' },
        },
      };
      expect(store.streaming.value).toBe(false);
      expect(store.canStop.value).toBe(false);
      expect(store.canInterject.value).toBe(false);
      expect(store.runLivenessUnknown.value).toBe(false);
    } finally {
      store.dispose();
    }
  });

  it('projects a remotely running response before its snapshot returns', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'remote-r1' }];
      store.activeSessionId.value = 's1';
      const snapshot = deferred<Record<string, unknown>>();
      store.endpoints.response = vi.fn(() => snapshot.promise);
      const internals = store as unknown as {
        streamResponse(responseId: string, sessionId: string, after: number): Promise<void>;
      };
      internals.streamResponse = vi.fn(async () => undefined);

      const recovery = store.runEngine.resumeResponse('s1', 'remote-r1');
      const duplicateRecovery = store.runEngine.resumeResponse('s1', 'remote-r1');

      expect(store.runs.value.s1.run).toMatchObject({
        responseId: 'remote-r1',
        status: 'connecting',
      });
      expect(store.streaming.value).toBe(true);
      expect(store.canStop.value).toBe(true);
      expect(store.canInterject.value).toBe(true);
      await duplicateRecovery;
      expect(store.endpoints.response).toHaveBeenCalledOnce();
      snapshot.resolve({
        id: 'remote-r1',
        status: 'in_progress',
        run_epoch: 2,
        last_sequence_number: 4,
        recovery: { sequence_number: 4 },
      });
      await recovery;
      expect(internals.streamResponse).toHaveBeenCalledWith('remote-r1', 's1', 4);
    } finally {
      store.dispose();
    }
  });

  it('presents a running sidebar session as active before its stream attaches', () => {
    const store = new AppStore(config);
    try {
      const running = {
        ...session(),
        id: 's2',
        title: 'Running elsewhere',
        activeRun: true,
        activeResponseId: 'r2',
      };
      store.sessions.value = [session(), running];
      store.activeSessionId.value = 's1';
      store.runs.value = {
        s2: initialProjection({
          responseId: 'r2',
          sessionId: 's2',
          epoch: 1,
          status: 'connecting',
          lastSequence: 3,
          startedRev: 0,
          reconnects: 1,
        }),
      };

      expect(store.streaming.value).toBe(false);
      store.activeSessionId.value = 's2';

      expect(store.responseTransportAttached.value).toBe(false);
      expect(store.streaming.value).toBe(true);
      expect(store.canStop.value).toBe(true);
      expect(store.canInterject.value).toBe(true);
      expect(store.runLivenessUnknown.value).toBe(false);
    } finally {
      store.dispose();
    }
  });

  it('keeps a healthy owned response transport during lifecycle recovery', async () => {
    const store = new AppStore(config);
    let transportAborted = false;
    try {
      store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'r1' }];
      store.activeSessionId.value = 's1';
      store.runs.value = {
        s1: initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 1,
          startedRev: 0,
          reconnects: 0,
        }),
      };
      store.endpoints.responseEvents = vi.fn(async (_responseId, _after, signal) => {
        signal.addEventListener('abort', () => {
          transportAborted = true;
        });
        return new Response(new ReadableStream<Uint8Array>({ start() {} }));
      });
      store.endpoints.response = vi.fn(async () => ({
        id: 'r1',
        status: 'in_progress',
        run_epoch: 1,
        last_sequence_number: 1,
        recovery: { sequence_number: 1 },
      }));

      void store.streamResponse('r1', 's1', 1);
      await vi.waitFor(() => expect(store.streaming.value).toBe(true));

      store.runEngine.recoverActiveSupervisors();
      await Promise.resolve();

      expect(transportAborted).toBe(false);
      expect(store.endpoints.response).not.toHaveBeenCalled();
      expect(store.endpoints.responseEvents).toHaveBeenCalledOnce();
      expect(store.runLivenessUnknown.value).toBe(false);
    } finally {
      store.dispose();
    }
  });

  it('retries when a replacement response stream never returns headers', async () => {
    vi.useFakeTimers();
    const store = new AppStore(config);
    let connectSignal: AbortSignal | undefined;
    try {
      store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'r1' }];
      store.activeSessionId.value = 's1';
      store.runs.value = {
        s1: initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'connecting',
          lastSequence: 3,
          startedRev: 0,
          reconnects: 0,
        }),
      };
      store.endpoints.responseEvents = vi
        .fn()
        .mockImplementationOnce((_responseId, _after, signal) => {
          connectSignal = signal;
          // Model WebKit leaving fetch pending even after its signal is aborted.
          return new Promise<Response>(() => undefined);
        })
        .mockImplementationOnce(async () => {
          const body = new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(
                new TextEncoder().encode(
                  `event: response.completed\ndata: ${JSON.stringify({
                    response_id: 'r1',
                    run_epoch: 1,
                    sequence_number: 5,
                    response: { id: 'r1', status: 'completed' },
                    final_rev: 1,
                  })}\n\ndata: [DONE]\n\n`,
                ),
              );
              controller.close();
            },
          });
          return new Response(body);
        });
      store.endpoints.response = vi.fn(async () => ({
        id: 'r1',
        status: 'in_progress',
        run_epoch: 1,
        last_sequence_number: 4,
        recovery: { sequence_number: 4 },
      }));
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: { id: 's1' },
        selected_transcript: { bodies: { rev: 1, messages: [] } },
      }));
      store.endpoints.sessionState = vi.fn(async () => ({}));

      void store.streamResponse('r1', 's1', 3);
      await Promise.resolve();
      expect(connectSignal?.aborted).toBe(false);

      await vi.advanceTimersByTimeAsync(15_000);

      expect(connectSignal?.aborted).toBe(true);
      expect(store.runs.value.s1.run).toMatchObject({ status: 'connecting', reconnects: 1 });

      await vi.advanceTimersByTimeAsync(1_500);
      await vi.waitFor(() => expect(store.endpoints.response).toHaveBeenCalledOnce());
      await vi.waitFor(() => expect(store.endpoints.responseEvents).toHaveBeenCalledTimes(2));
      expect(store.endpoints.responseEvents).toHaveBeenLastCalledWith(
        'r1',
        4,
        expect.any(AbortSignal),
      );
      expect(store.runEngine.currentSupervisor('s1')).toBeUndefined();
    } finally {
      store.dispose();
      vi.useRealTimers();
    }
  });

  it('recovers an active run when no response transport is owned', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'r1' }];
      store.activeSessionId.value = 's1';
      store.runs.value = {
        s1: initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 3,
          startedRev: 0,
          reconnects: 0,
        }),
      };
      store.endpoints.response = vi.fn(async () => ({
        id: 'r1',
        status: 'in_progress',
        run_epoch: 1,
        last_sequence_number: 4,
        recovery: { sequence_number: 4 },
      }));
      const internals = store as unknown as {
        streamResponse(responseId: string, sessionId: string, after: number): Promise<void>;
      };
      internals.streamResponse = vi.fn(async () => undefined);

      expect(store.responseTransportAttached.value).toBe(false);
      expect(store.streaming.value).toBe(true);
      expect(store.runLivenessUnknown.value).toBe(false);
      store.runEngine.recoverActiveSupervisors();

      await vi.waitFor(() => expect(store.endpoints.response).toHaveBeenCalledOnce());
      await vi.waitFor(() => expect(internals.streamResponse).toHaveBeenCalledWith('r1', 's1', 4));
      expect(store.runEngine.currentSupervisor('s1')?.responseId).toBe('r1');
      expect(store.runs.value.s1.run).toMatchObject({ status: 'streaming', lastSequence: 4 });
    } finally {
      store.dispose();
    }
  });

  it('reconciles a locally running response when the server reports the session idle', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      { ...session(), activeRun: true, activeResponseId: 'finished-while-suspended' },
    ];
    store.runs.value = {
      s1: initialProjection({
        responseId: 'finished-while-suspended',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 4,
        startedRev: 1,
        reconnects: 0,
      }),
    };
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [{ id: 's1', transcript_rev: 2 }],
    }));
    const internals = store as unknown as {
      refreshStatus(): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };
    internals.resumeResponse = vi.fn(async () => undefined);

    await internals.refreshStatus();

    expect(store.sessions.value[0]).toMatchObject({ activeRun: false, activeResponseId: null });
    expect(internals.resumeResponse).toHaveBeenCalledWith('s1', 'finished-while-suspended');
  });

  it('retires an expired response after authoritative idle transcript reconciliation', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [
        {
          ...session(),
          activeRun: true,
          activeResponseId: 'expired-response',
          transcriptRev: 1,
          messageBodiesRev: 1,
        },
      ];
      store.activeSessionId.value = 's1';
      store.runs.value = {
        s1: initialProjection({
          responseId: 'expired-response',
          sessionId: 's1',
          epoch: 99,
          status: 'streaming',
          lastSequence: 25,
          startedRev: 1,
          reconnects: 0,
        }),
      };
      store.endpoints.sessionStatus = vi.fn(async () => ({
        sessions: [{ id: 's1', transcript_rev: 2 }],
        __etag: 'idle-etag',
      }));
      store.endpoints.response = vi.fn(async () => {
        throw new APIError('response not found', 404);
      });
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: { id: 's1', transcript_rev: 2 },
        selected_transcript: { bodies: { rev: 2, messages: [] } },
      }));
      store.endpoints.sessionState = vi.fn(async () => ({}));
      const internals = store as unknown as {
        refreshStatus(authoritative?: boolean): Promise<void>;
      };

      await internals.refreshStatus(true);

      await vi.waitFor(() => expect(store.runs.value.s1).toBeUndefined());
      expect(store.endpoints.sessionStatus).toHaveBeenCalledWith('s1', false, ['all'], '');
      expect(store.sessions.value[0]).toMatchObject({
        activeRun: false,
        activeResponseId: null,
        messageBodiesRev: 2,
        lastResponseId: 'expired-response',
      });
      expect(store.runEngine.currentSupervisor('s1')).toBeUndefined();
      expect(store.runActive.value).toBe(false);
      expect(store.streaming.value).toBe(false);
    } finally {
      store.dispose();
    }
  });

  it('does not probe a provisional response before the server admits it', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), activeRun: true }];
    store.runs.value = {
      s1: initialProjection({
        responseId: 'pending_local-response',
        sessionId: 's1',
        epoch: 1,
        status: 'connecting',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.endpoints.sessionStatus = vi.fn(async () => ({ sessions: [{ id: 's1' }] }));
    const internals = store as unknown as {
      refreshStatus(): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };
    internals.resumeResponse = vi.fn(async () => undefined);

    await internals.refreshStatus();

    expect(internals.resumeResponse).not.toHaveBeenCalled();
  });

  it('installs a peer prompt before streaming its active response', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), transcriptRev: 4, messageBodiesRev: 4 }];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    const order: string[] = [];
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [
        {
          id: 's1',
          active_run: true,
          active_response_id: 'peer-response',
          client_message_id: 'peer-message',
          transcript_rev: 5,
        },
      ],
    }));
    store.endpoints.sessions = vi.fn(async () => ({
      data: [{ id: 's1', short_title: 'Test', transcript_rev: 5 }],
    }));
    store.endpoints.selectedSession = vi.fn(async () => {
      order.push('transcript');
      return {
        selected_session: { id: 's1', title: 'Test' },
        selected_transcript: {
          bodies: {
            rev: 5,
            messages: [
              {
                id: 42,
                sequence: 1,
                role: 'user',
                client_message_id: 'peer-message',
                parts: [{ type: 'text', text: 'peer prompt' }],
              },
            ],
          },
        },
      };
    });
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.response = vi.fn(async () => {
      order.push('response');
      return {
        id: 'peer-response',
        status: 'in_progress',
        run_epoch: 1,
        started_rev: 4,
        last_sequence_number: 1,
        recovery: {
          sequence_number: 1,
          messages: [
            {
              id: 'peer-response:assistant:0',
              role: 'assistant',
              content: 'streamed reply',
              response_id: 'peer-response',
              assistant_segment_ordinal: 0,
            },
          ],
        },
      };
    });
    const internals = store as unknown as {
      refreshStatus(): Promise<void>;
      streamResponse(responseId: string, sessionId: string, after: number): Promise<void>;
    };
    internals.streamResponse = vi.fn(async () => {
      order.push('stream');
    });

    await store.refreshSidebar();
    expect(store.sessions.value[0]).toMatchObject({ transcriptRev: 5, messageBodiesRev: 4 });

    await internals.refreshStatus();
    await vi.waitFor(() => expect(order).toContain('stream'));

    expect(store.endpoints.selectedSession).toHaveBeenCalledOnce();
    expect(order).toEqual(['transcript', 'response', 'stream']);
    expect(store.visibleMessages.value.map((message) => message.content)).toEqual([
      'peer prompt',
      'streamed reply',
    ]);
    expect(store.sessions.value[0]).toMatchObject({
      transcriptRev: 5,
      messageBodiesRev: 5,
      activeResponseId: 'peer-response',
      activeRun: true,
    });
  });

  it('retries a peer attach without publishing an uninstalled transcript revision', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), transcriptRev: 4, messageBodiesRev: 4 }];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [
        {
          id: 's1',
          active_run: true,
          active_response_id: 'peer-response',
          client_message_id: 'peer-message',
          transcript_rev: 5,
        },
      ],
    }));
    const stalledTranscript = deferred<Record<string, unknown>>();
    store.endpoints.selectedSession = vi
      .fn()
      .mockImplementationOnce(() => stalledTranscript.promise)
      .mockResolvedValue({
        selected_session: { id: 's1', title: 'Test' },
        selected_transcript: {
          bodies: {
            rev: 5,
            messages: [
              {
                id: 42,
                sequence: 1,
                role: 'user',
                client_message_id: 'peer-message',
                parts: [{ type: 'text', text: 'peer prompt' }],
              },
            ],
          },
        },
      });
    store.endpoints.sessionState = vi.fn(async () => ({}));
    store.endpoints.response = vi.fn(async () => ({
      id: 'peer-response',
      status: 'in_progress',
      run_epoch: 1,
      last_sequence_number: 1,
      recovery: { sequence_number: 1 },
    }));
    const internals = store as unknown as {
      refreshStatus(authoritative?: boolean): Promise<void>;
      streamResponse(responseId: string, sessionId: string, after: number): Promise<void>;
    };
    internals.streamResponse = vi.fn(async () => undefined);

    await internals.refreshStatus();
    await vi.waitFor(() => expect(store.endpoints.selectedSession).toHaveBeenCalledOnce());

    await internals.refreshStatus(true);
    expect(store.endpoints.selectedSession).toHaveBeenCalledOnce();
    expect(store.sessions.value[0]).toMatchObject({ transcriptRev: 5, messageBodiesRev: 4 });
    expect(store.endpoints.response).not.toHaveBeenCalled();

    stalledTranscript.reject(new Error('temporary transcript failure'));
    await new Promise<void>((resolve) => window.setTimeout(resolve, 0));
    await internals.refreshStatus(true);
    await vi.waitFor(() => expect(internals.streamResponse).toHaveBeenCalledOnce());

    expect(store.endpoints.selectedSession).toHaveBeenCalledTimes(2);
    expect(store.sessions.value[0]).toMatchObject({
      transcriptRev: 5,
      messageBodiesRev: 5,
      activeResponseId: 'peer-response',
      activeRun: true,
    });
  });

  it('does not let an idle status response invalidate a run admitted after the request began', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    const status = deferred<Record<string, unknown>>();
    store.endpoints.sessionStatus = vi.fn(() => status.promise);
    const internals = store as unknown as {
      refreshStatus(): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };
    internals.resumeResponse = vi.fn(async () => undefined);

    const refresh = internals.refreshStatus();
    store.sessions.value = [
      { ...session(), activeRun: true, activeResponseId: 'newly-admitted-response' },
    ];
    store.runs.value = {
      s1: initialProjection({
        responseId: 'newly-admitted-response',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 1,
        startedRev: 1,
        startedAt: Date.now() + 1_000,
        reconnects: 0,
      }),
    };
    store.runEngine.markResponseTransportActive('s1', 'newly-admitted-response');
    status.resolve({ sessions: [{ id: 's1' }] });
    await refresh;

    expect(store.sessions.value[0]).toMatchObject({
      activeRun: true,
      activeResponseId: 'newly-admitted-response',
    });
    expect(store.streaming.value).toBe(true);
    expect(internals.resumeResponse).not.toHaveBeenCalled();
  });

  it('rejects an active status snapshot from an older response generation', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'r-old' }];
    store.activeSessionId.value = 's1';
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r-old',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 3,
        startedRev: 1,
        reconnects: 0,
      }),
    };
    const status = deferred<Record<string, unknown>>();
    store.endpoints.sessionStatus = vi.fn(() => status.promise);
    const internals = store as unknown as {
      refreshStatus(): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };
    internals.resumeResponse = vi.fn(async () => undefined);

    const refresh = internals.refreshStatus();
    store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'r-new' }];
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r-new',
        sessionId: 's1',
        epoch: 2,
        status: 'connecting',
        lastSequence: 0,
        startedRev: 2,
        reconnects: 0,
      }),
    };
    status.resolve({
      sessions: [{ id: 's1', active_run: true, active_response_id: 'r-old' }],
    });
    await refresh;

    expect(store.sessions.value[0]).toMatchObject({ activeRun: true, activeResponseId: 'r-new' });
    expect(store.runs.value.s1.run.responseId).toBe('r-new');
    expect(store.streaming.value).toBe(true);
    expect(internals.resumeResponse).not.toHaveBeenCalled();
  });

  it('does not reconcile a response while the server reports anonymous run activity', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'r1' }];
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 1,
        startedRev: 1,
        startedAt: 1,
        reconnects: 0,
      }),
    };
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [{ id: 's1', active_run: true }],
    }));
    const internals = store as unknown as {
      refreshStatus(): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };
    internals.resumeResponse = vi.fn(async () => undefined);

    await internals.refreshStatus();

    expect(store.sessions.value[0]?.activeRun).toBe(true);
    expect(internals.resumeResponse).not.toHaveBeenCalled();
  });

  it('does not treat an omitted status row as authoritative idle', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), activeRun: true, activeResponseId: 'out-of-scope' }];
    store.runs.value = {
      s1: initialProjection({
        responseId: 'out-of-scope',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 1,
        startedRev: 1,
        startedAt: 1,
        reconnects: 0,
      }),
    };
    store.endpoints.sessionStatus = vi.fn(async () => ({ sessions: [] }));
    const internals = store as unknown as {
      refreshStatus(): Promise<void>;
      resumeResponse(sessionId: string, responseId: string): Promise<void>;
    };
    internals.resumeResponse = vi.fn(async () => undefined);

    await internals.refreshStatus();

    expect(internals.resumeResponse).not.toHaveBeenCalled();
  });

  it('forces a post-mutation sidebar fetch after an opportunistic fetch already started', async () => {
    const store = new AppStore(config);
    const first = deferred<Record<string, unknown>>();
    store.endpoints.sessions = vi
      .fn()
      .mockImplementationOnce(() => first.promise)
      .mockResolvedValueOnce({ data: [{ id: 's1', short_title: 'Fresh title' }] });

    const opportunistic = store.refreshSidebar(false);
    const authoritative = store.refreshSidebar();
    first.resolve({ data: [{ id: 's1', short_title: 'Stale title' }] });
    await Promise.all([opportunistic, authoritative]);

    expect(store.endpoints.sessions).toHaveBeenCalledTimes(2);
    expect(store.sessions.value[0].title).toBe('Fresh title');
  });

  it('reconciles sidebar membership before status after a peer invalidation', async () => {
    const store = new AppStore(config);
    const order: string[] = [];
    store.endpoints.sessions = vi.fn(async () => {
      order.push('sidebar');
      return { data: [{ id: 's2', short_title: 'Peer session', origin: 'web' }] };
    });
    store.endpoints.sessionStatus = vi.fn(async () => {
      order.push('status');
      return { sessions: [{ id: 's2', active_run: false }] };
    });

    await (
      store as unknown as { reconcilePeerSessionChange(): Promise<void> }
    ).reconcilePeerSessionChange();

    expect(order).toEqual(['sidebar', 'status']);
    expect(store.sessions.value.map((entry) => entry.id)).toContain('s2');
  });

  it('refreshes authoritative sidebar membership when a tab regains focus', async () => {
    const store = new AppStore(config);
    store.refreshSidebar = vi.fn(async () => undefined);
    const internals = store as unknown as {
      installLifecycle(): void;
      refreshStatus(): Promise<void>;
    };
    internals.refreshStatus = vi.fn(async () => undefined);
    internals.installLifecycle();

    window.dispatchEvent(new Event('focus'));
    await vi.waitFor(() => expect(store.refreshSidebar).toHaveBeenCalledOnce());
    expect(internals.refreshStatus).toHaveBeenCalledOnce();
  });

  it('invalidates an old status generation before waiting for it to settle', async () => {
    const store = new AppStore(config);
    store.sessions.value = [{ ...session(), title: 'Current', transcriptRev: 5 }];
    store.activeSessionId.value = 's1';
    const oldStatus = deferred<Record<string, unknown>>();
    const newStatus = deferred<Record<string, unknown>>();
    store.endpoints.sessionStatus = vi
      .fn()
      .mockImplementationOnce(() => oldStatus.promise)
      .mockImplementationOnce(() => newStatus.promise);
    const internals = store as unknown as {
      refreshStatus(authoritative?: boolean): Promise<void>;
    };

    const oldRequest = internals.refreshStatus();
    const authoritative = internals.refreshStatus(true);
    oldStatus.resolve({
      sessions: [
        {
          id: 's1',
          short_title: 'Stale',
          active_response_id: 'old-response',
          transcript_rev: 1,
        },
      ],
    });
    await oldRequest;
    expect(store.sessions.value[0]).toMatchObject({ title: 'Current', transcriptRev: 5 });
    expect(store.sessions.value[0].activeResponseId).not.toBe('old-response');

    newStatus.resolve({
      sessions: [{ id: 's1', short_title: 'Newest', active_response_id: '', transcript_rev: 8 }],
    });
    await authoritative;
    expect(store.sessions.value[0]).toMatchObject({ title: 'Newest', transcriptRev: 8 });
  });

  it('coalesces non-authoritative status callers', async () => {
    const store = new AppStore(config);
    const status = deferred<Record<string, unknown>>();
    store.endpoints.sessionStatus = vi.fn(() => status.promise);
    const internals = store as unknown as {
      refreshStatus(authoritative?: boolean): Promise<void>;
    };

    const first = internals.refreshStatus();
    const second = internals.refreshStatus();
    expect(store.endpoints.sessionStatus).toHaveBeenCalledOnce();
    status.resolve({ sessions: [] });
    await Promise.all([first, second]);
  });

  it('reorders sessions when status polling observes activity from another tab', async () => {
    const store = new AppStore(config);
    store.sessions.value = [
      { ...session(), id: 'recent', number: 2, lastMessageAt: 1_800_000_002_000 },
      { ...session(), id: 'updated-elsewhere', number: 1, lastMessageAt: 1_800_000_001_000 },
    ];
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [
        { id: 'updated-elsewhere', last_message_at: 1_800_000_003_000 },
        { id: 'recent', last_message_at: 1_800_000_002_000 },
      ],
    }));

    await (store as unknown as { refreshStatus(): Promise<void> }).refreshStatus();

    expect(store.sessions.value.map((entry) => entry.id)).toEqual(['updated-elsewhere', 'recent']);
  });

  it('merges generated session titles from status polling without overriding an open rename', async () => {
    const store = new AppStore(config);
    const target = session();
    store.sessions.value = [target];
    store.activeSessionId.value = target.id;
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [
        {
          id: target.id,
          short_title: 'Fix Actions Cache Key',
          long_title: 'Fix stale GitHub Actions dependency caching',
          transcript_rev: 1,
        },
      ],
    }));
    const internals = store as unknown as { refreshStatus(): Promise<void> };

    await internals.refreshStatus();
    expect(store.sessions.value[0]).toMatchObject({
      title: 'Fix Actions Cache Key',
      longTitle: 'Fix stale GitHub Actions dependency caching',
    });

    store.sessions.value = [{ ...store.sessions.value[0], title: 'Editing this title' }];
    store.renameTarget.value = store.sessions.value[0];
    await internals.refreshStatus();
    expect(store.sessions.value[0].title).toBe('Editing this title');
  });

  it('bounds post-completion title reconciliation to two status refreshes', async () => {
    vi.useFakeTimers();
    try {
      const store = new AppStore(config);
      const internals = store as unknown as {
        refreshStatus(): Promise<void>;
        scheduleTitleReconciliation(sessionId: string): void;
      };
      internals.refreshStatus = vi.fn(async () => undefined);

      internals.scheduleTitleReconciliation('s1');
      await vi.advanceTimersByTimeAsync(2_000);
      expect(internals.refreshStatus).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(6_000);
      expect(internals.refreshStatus).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(30_000);
      expect(internals.refreshStatus).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
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
      from_provider: 'openai',
      from_model: 'gpt-test',
      from_reasoning_effort: 'high',
      to_provider: 'anthropic',
      to_model: 'gpt-next',
      to_reasoning_effort: 'medium',
      boundary_id: 'r1:model-switch:1',
      model: 'gpt-next',
      reasoning_effort: 'medium',
    });
    expect(store.activeSession.value).toMatchObject({
      activeModel: 'gpt-next',
      activeProvider: 'anthropic',
      activeEffort: 'medium',
    });
  });

  it('loads managed worktree patches into the rich diff state', async () => {
    const store = new AppStore(config);
    try {
      store.sessions.value = [{ ...session(), projectId: 'project-1' }];
      store.activeSessionId.value = 's1';
      store.diff.value = { ...store.diff.value, scope: 'staged' };
      store.queueDiffComment({
        path: 'queued.go',
        side: 'new',
        line: 3,
        body: 'Keep this queued.',
        scope: 'staged',
        context: 'queued',
      });
      store.endpoints.fileChanges = vi.fn(async () => ({ file_changes: [] }));
      store.endpoints.worktreeDiff = vi.fn(async () => ({
        diff: `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1 @@
-package old
+package main
`,
      }));

      await store.openWorktreeDiff('/worktrees/feature', 'feature');

      expect(store.diff.value).toMatchObject({
        open: true,
        worktreeDir: '/worktrees/feature',
        worktreeTitle: 'feature',
        readOnly: true,
        loading: false,
        scope: 'staged',
        comments: [expect.objectContaining({ path: 'queued.go', body: 'Keep this queued.' })],
      });
      expect(store.diff.value.files[0]).toMatchObject({
        path: 'main.go',
        additions: 1,
        deletions: 1,
        expanded: true,
      });
      expect(store.endpoints.worktreeDiff).toHaveBeenCalledWith('project-1', '/worktrees/feature');

      await store.loadDiff();
      expect(store.endpoints.fileChanges).not.toHaveBeenCalled();
      expect(store.diff.value.worktreeDir).toBe('/worktrees/feature');
    } finally {
      store.dispose();
    }
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
        {
          kind: 'gap',
          content: '1 hidden line',
          hiddenOld: 1,
          hiddenNew: 1,
          gapDirection: 'above',
        },
        { kind: 'hunk', content: '@@ -2 +2 @@' },
        { kind: 'delete', content: 'old', oldLine: 2 },
        { kind: 'add', content: 'new', newLine: 2 },
        {
          kind: 'gap',
          content: '1 hidden line',
          hiddenOld: 1,
          hiddenNew: 1,
          gapDirection: 'below',
        },
      ],
    });
  });

  it('exposes an uncommitted Markdown file as expanded while its diff is loading', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      scope: 'uncommitted',
      git: true,
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          contentAvailable: true,
        },
      ],
    };
    const response = deferred<Record<string, unknown>>();
    store.endpoints.fileDiff = vi.fn(() => response.promise);

    const expansion = store.expandDiff(store.diff.value.files[0]);
    expect(store.diff.value.files[0]).toMatchObject({ expanded: true, loading: true });

    response.resolve({
      kind: 'modify',
      content_available: true,
      hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: '# Plan' }] }],
    });
    await expansion;
    expect(store.diff.value.files[0]).toMatchObject({ expanded: true, loading: false });
  });

  it('loads Markdown source only on Rendered activation and caches the pinned turn version', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.diffComments = vi.fn(async () => ({ comments: [] }));
    let sequence = 7;
    store.endpoints.fileChanges = vi.fn(async () => ({
      scope: 'last_turn',
      file_changes: [
        {
          path: '/work/plan.md',
          kind: 'modify',
          seq: sequence,
          snapshot_seq: sequence,
          content_available: true,
        },
      ],
    }));
    store.endpoints.fileDiff = vi.fn(async () => ({
      kind: 'modify',
      hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: '# Plan' }] }],
    }));
    const nextPreview = deferred<string>();
    store.endpoints.fileText = vi
      .fn()
      .mockResolvedValueOnce('# Plan\n\nFirst version.\n')
      .mockReturnValueOnce(nextPreview.promise);

    await store.loadDiff();
    const file = store.diff.value.files[0];
    await store.expandDiff(file);
    expect(store.endpoints.fileText).not.toHaveBeenCalled();

    await store.setMarkdownView(store.diff.value.files[0], 'rendered');
    await vi.waitFor(() =>
      expect(store.diff.value.files[0].markdownPreview?.source).toContain('First version.'),
    );
    expect(store.endpoints.fileText).toHaveBeenCalledWith(
      's1',
      '/work/plan.md',
      'last_turn',
      'after',
      7,
      expect.any(AbortSignal),
    );
    expect(store.diff.value.files[0].markdownPreview?.blocks?.[0]).toMatchObject({
      type: 'heading',
      startLine: 1,
      anchorLine: 1,
    });

    await store.setMarkdownView(store.diff.value.files[0], 'diff');
    await store.setMarkdownView(store.diff.value.files[0], 'rendered');
    await store.loadDiff();
    expect(store.endpoints.fileText).toHaveBeenCalledTimes(1);
    expect(store.diff.value.files[0].markdownPreview?.source).toContain('First version.');

    sequence = 8;
    const refresh = store.loadDiff();
    await vi.waitFor(() => expect(store.endpoints.fileText).toHaveBeenCalledTimes(2));
    expect(store.diff.value.files[0].markdownPreview).toMatchObject({
      source: expect.stringContaining('First version.'),
      loading: true,
      sequence: 8,
      snapshotSeq: 8,
    });
    nextPreview.resolve('# Plan\n\nSecond version.\n');
    await refresh;
    expect(store.diff.value.files[0].markdownPreview?.source).toContain('Second version.');
    expect(store.diff.value.files[0].markdownPreview).toMatchObject({
      view: 'rendered',
      sequence: 8,
      snapshotSeq: 8,
    });
  });

  it('refreshes Git diff lines before switching back from a rendered Markdown preview', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.diff.value = { ...store.diff.value, scope: 'uncommitted' };
    store.endpoints.diffComments = vi.fn(async () => ({ comments: [] }));
    let version = 'Old version.';
    store.endpoints.fileChanges = vi.fn(async () => ({
      scope: 'uncommitted',
      git: true,
      file_changes: [
        {
          path: '/work/plan.md',
          kind: 'create',
          adds: 1,
          dels: 0,
          content_available: true,
        },
      ],
    }));
    store.endpoints.fileDiff = vi.fn(async () => ({
      kind: 'create',
      content_available: true,
      hunks: [{ old_start: 0, new_start: 1, lines: [{ t: 'add', s: version }] }],
    }));
    store.endpoints.fileText = vi.fn(async () => `${version}\n`);

    await store.loadDiff();
    await store.expandDiff(store.diff.value.files[0]);
    await store.setMarkdownView(store.diff.value.files[0], 'rendered');
    expect(store.diff.value.files[0].markdownPreview?.source).toContain('Old version.');

    version = 'Current version.';
    await store.loadDiff();
    expect(store.diff.value.files[0].markdownPreview?.source).toContain('Current version.');
    expect(store.diff.value.files[0].lines).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ kind: 'add', content: 'Current version.' }),
      ]),
    );

    await store.setMarkdownView(store.diff.value.files[0], 'diff');
    expect(store.diff.value.files[0].markdownPreview?.view).toBe('diff');
    expect(store.diff.value.files[0].lines).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ kind: 'add', content: 'Current version.' }),
      ]),
    );
    expect(store.endpoints.fileDiff).toHaveBeenCalledTimes(2);
    expect(store.endpoints.fileText).toHaveBeenCalledTimes(2);
  });

  it('ignores a superseded Markdown source response', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    const older = deferred<string>();
    const newer = deferred<string>();
    store.endpoints.fileText = vi
      .fn()
      .mockReturnValueOnce(older.promise)
      .mockReturnValueOnce(newer.promise);
    store.diff.value = {
      ...store.diff.value,
      sessionId: 's1',
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          sequence: 4,
          snapshotSeq: 3,
          contentAvailable: true,
          expanded: true,
        },
      ],
    };
    const file = store.diff.value.files[0];
    const first = store.loadMarkdownPreview(file, true);
    const second = store.loadMarkdownPreview(file, true);
    newer.resolve('# Newest\n');
    await second;
    older.resolve('# Stale\n');
    await first;
    expect(store.diff.value.files[0].markdownPreview?.source).toBe('# Newest\n');
  });

  it('keeps the last rendered Markdown document when a refresh fails', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.endpoints.fileText = vi
      .fn()
      .mockRejectedValueOnce(new Error('refresh failed'))
      .mockResolvedValueOnce('# Recovered version\n');
    store.endpoints.diffComments = vi.fn(async () => ({ comments: [] }));
    store.endpoints.fileChanges = vi.fn(async () => ({
      scope: 'last_turn',
      file_changes: [
        {
          path: '/work/plan.md',
          kind: 'modify',
          seq: 4,
          snapshot_seq: 3,
          content_available: true,
        },
      ],
    }));
    store.diff.value = {
      ...store.diff.value,
      sessionId: 's1',
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          sequence: 4,
          snapshotSeq: 3,
          contentAvailable: true,
          expanded: true,
          markdownPreview: {
            view: 'rendered',
            side: 'after',
            source: '# Last good version\n',
            blocks: [
              {
                id: 'markdown-block-0',
                type: 'heading',
                startLine: 1,
                endLine: 1,
                anchorLine: 1,
                commentable: true,
              },
            ],
            sequence: 4,
            snapshotSeq: 3,
            scope: 'last_turn',
          },
        },
      ],
    };

    await store.loadMarkdownPreview(store.diff.value.files[0], true);

    expect(store.diff.value.files[0].markdownPreview).toMatchObject({
      source: '# Last good version\n',
      loading: false,
      error: 'refresh failed',
    });

    await store.loadDiff();

    expect(store.endpoints.fileText).toHaveBeenCalledTimes(2);
    expect(store.diff.value.files[0].markdownPreview).toMatchObject({
      source: '# Recovered version\n',
      loading: false,
      error: '',
    });
  });

  it('refreshes expanded diffs without removing their visible lines', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.diff.value = { ...store.diff.value, scope: 'uncommitted' };
    store.endpoints.diffComments = vi.fn(async () => ({ comments: [] }));
    store.endpoints.fileChanges = vi.fn(async () => ({
      scope: 'uncommitted',
      git: true,
      file_changes: [
        {
          path: 'test.txt',
          kind: 'modify',
          adds: 1,
          dels: 1,
          content_available: true,
        },
      ],
    }));
    const refreshedDiff = deferred<Record<string, unknown>>();
    store.endpoints.fileDiff = vi
      .fn()
      .mockResolvedValueOnce({
        path: 'test.txt',
        kind: 'modify',
        context: 3,
        hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: 'old line' }] }],
      })
      .mockReturnValueOnce(refreshedDiff.promise);

    await store.loadDiff();
    await store.expandDiff(store.diff.value.files[0]);
    store.queueDiffComment({
      path: 'test.txt',
      side: 'new',
      line: 1,
      body: 'Keep the old line.',
      scope: 'uncommitted',
      context: 'old line',
    });
    const refresh = store.loadDiff();
    await vi.waitFor(() => expect(store.endpoints.fileDiff).toHaveBeenCalledTimes(2));

    expect(store.diff.value.files[0]).toMatchObject({
      expanded: true,
      loading: true,
      lines: expect.arrayContaining([expect.objectContaining({ content: 'old line' })]),
    });

    refreshedDiff.resolve({
      path: 'test.txt',
      kind: 'modify',
      context: 3,
      hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: 'new line' }] }],
    });
    await refresh;

    expect(store.diff.value.files[0]).toMatchObject({
      expanded: true,
      loading: false,
      lines: expect.arrayContaining([expect.objectContaining({ content: 'new line' })]),
    });
    expect(store.diff.value.comments[0]).toMatchObject({ state: 'stale' });
  });

  it('refreshes an expanded last-turn diff when an in-progress file change arrives', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.endpoints.diffComments = vi.fn(async () => ({ comments: [], transcript_rev: 0 }));
    let version = 1;
    store.endpoints.fileChanges = vi.fn(async () => ({
      scope: 'last_turn',
      file_changes: [
        version === 1
          ? { path: 'test.txt', kind: 'create', adds: 1, dels: 0, seq: 1 }
          : { path: 'test.txt', kind: 'modify', adds: 1, dels: 1, seq: 2 },
      ],
    }));
    store.endpoints.fileDiff = vi.fn(async () => ({
      path: 'test.txt',
      kind: version === 1 ? 'create' : 'modify',
      context: 3,
      hunks: [
        {
          old_start: 1,
          new_start: 1,
          lines:
            version === 1
              ? [{ t: 'add', s: '123' }]
              : [
                  { t: 'del', s: '123' },
                  { t: 'add', s: '234' },
                ],
        },
      ],
    }));

    await store.loadDiff();
    await store.expandDiff(store.diff.value.files[0]);
    expect(store.diff.value.files[0].lines).toEqual(
      expect.arrayContaining([expect.objectContaining({ kind: 'add', content: '123' })]),
    );

    store.diff.value = { ...store.diff.value, open: true };
    version = 2;
    store.applyResponseEvent('s1', {
      type: 'response.file_change',
      response_id: 'r1',
      run_epoch: 1,
      sequence_number: 1,
    });

    await vi.waitFor(() => expect(store.endpoints.fileDiff).toHaveBeenCalledTimes(2));
    await vi.waitFor(() =>
      expect(store.diff.value.files[0]).toMatchObject({
        status: 'modify',
        sequence: 2,
        expanded: true,
        loading: false,
        lines: [
          { kind: 'hunk', content: '@@ -1 +1 @@' },
          { kind: 'delete', content: '123', oldLine: 1 },
          { kind: 'add', content: '234', newLine: 1 },
        ],
      }),
    );
  });

  it('keeps the newest expansion when same-version diff requests finish out of order', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.diff.value = {
      ...store.diff.value,
      sessionId: 's1',
      files: [
        {
          path: 'test.txt',
          status: 'modify',
          sequence: 2,
          expanded: true,
          context: 3,
          lines: [{ kind: 'add', content: 'initial', newLine: 1 }],
        },
      ],
    };
    const older = deferred<Record<string, unknown>>();
    const newer = deferred<Record<string, unknown>>();
    store.endpoints.fileDiff = vi
      .fn()
      .mockReturnValueOnce(older.promise)
      .mockReturnValueOnce(newer.promise);

    const olderRequest = store.expandDiff(store.diff.value.files[0], 12);
    const newerRequest = store.expandDiff(store.diff.value.files[0], 48);
    newer.resolve({
      path: 'test.txt',
      kind: 'modify',
      context: 48,
      hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: 'newest' }] }],
    });
    await newerRequest;
    older.resolve({
      path: 'test.txt',
      kind: 'modify',
      context: 12,
      hunks: [{ old_start: 1, new_start: 1, lines: [{ t: 'add', s: 'stale' }] }],
    });
    await olderRequest;

    expect(store.diff.value.files[0]).toMatchObject({
      context: 48,
      loading: false,
      lines: [
        { kind: 'hunk', content: '@@ -1 +1 @@' },
        { kind: 'add', content: 'newest', newLine: 1 },
      ],
    });
  });

  it('revalidates queued rendered comments against live Git Markdown before sending', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.diff.value = {
      ...store.diff.value,
      sessionId: 's1',
      scope: 'unstaged',
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          contentAvailable: true,
        },
      ],
    };
    store.queueDiffComment({
      path: '/work/plan.md',
      side: 'new',
      line: 3,
      body: 'Keep this paragraph.',
      scope: 'unstaged',
      context: 'Original paragraph.',
      contextBefore: ['# Plan', ''],
      contextAfter: [''],
      fileChangeSeq: 0,
    });
    store.endpoints.fileText = vi.fn(async () => '# Plan\n\nChanged paragraph.\n');
    store.send = vi.fn(async () => undefined);

    await store.sendDiffComments();

    expect(store.endpoints.fileText).toHaveBeenCalledTimes(1);
    expect(store.send).not.toHaveBeenCalled();
    expect(store.diff.value.comments[0]).toMatchObject({ state: 'stale' });
    expect(store.diff.value.files[0].markdownPreview?.view).toBe('diff');
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
      contextBefore: ['prepare()'],
      contextAfter: ['run()', '}'],
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
          context_before: [{ side: 'new', line: 11, text: 'prepare()' }],
          context_after: [
            { side: 'new', line: 13, text: 'run()' },
            { side: 'new', line: 14, text: '}' },
          ],
          instruction: 'Keep this guard.',
        }),
      }),
    ]);
    expect(store.diff.value.comments).toEqual([]);
    expect(store.toasts.value).toEqual([]);
  });

  it('explicitly re-anchors a stale queued comment to the current source snapshot', () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.queueDiffComment({
      id: 'stale-comment',
      path: 'main.go',
      side: 'new',
      line: 12,
      body: 'Keep this guard.',
      scope: 'last_turn',
      context: 'old line',
      fileChangeSeq: 9,
    });
    store.diff.value = {
      ...store.diff.value,
      comments: store.diff.value.comments.map((comment) => ({ ...comment, state: 'stale' })),
    };

    store.reanchorDiffComment('stale-comment', {
      path: 'main.go',
      side: 'new',
      line: 14,
      scope: 'last_turn',
      context: 'new line',
      fileChangeSeq: 10,
    });

    expect(store.diff.value.comments).toEqual([
      expect.objectContaining({
        id: 'stale-comment',
        state: 'fresh',
        line: 14,
        context: 'new line',
        fileChangeSeq: 10,
        anchorFingerprint: expect.any(String),
      }),
    ]);
    store.dispose();
  });

  it('sends queued diff comments as typed interjections during an active response', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 1,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.runEngine.markResponseTransportActive('s1', 'r1');
    store.queueDiffComment({
      id: 'queued-comment',
      path: 'main.go',
      side: 'new',
      line: 12,
      body: 'Keep this guard.',
      scope: 'last_turn',
      context: 'if ready {',
      fileChangeSeq: 9,
    });
    store.send = vi.fn(async () => undefined);
    store.refreshDiffComments = vi.fn(async () => undefined);
    store.endpoints.interrupt = vi.fn(async () => ({}));

    await store.sendDiffComments();

    expect(store.send).not.toHaveBeenCalled();
    expect(store.endpoints.interrupt).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({
        message: 'Keep this guard.',
        content: [
          expect.objectContaining({
            type: 'diff_comment',
            diff_comment: expect.objectContaining({
              id: 'queued-comment',
              path: 'main.go',
              line: 12,
              instruction: 'Keep this guard.',
            }),
          }),
          expect.objectContaining({
            type: 'input_text',
            text: expect.stringContaining('main.go:12'),
          }),
        ],
        delivery: 'steer',
      }),
      expect.any(String),
    );
    expect(store.diff.value.comments).toEqual([]);
    expect(store.diff.value.historyComments).toEqual([
      expect.objectContaining({ id: 'queued-comment', optimistic: true }),
    ]);
    expect(store.interjections.value).toEqual([
      expect.objectContaining({ content: 'Keep this guard.', state: 'pending' }),
    ]);
  });

  it('sends an immediate diff comment as an interjection during an active response', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 1,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.runEngine.markResponseTransportActive('s1', 'r1');
    store.refreshDiffComments = vi.fn(async () => undefined);
    store.endpoints.interrupt = vi.fn(async () => ({}));

    await store.sendDiffComment({
      id: 'immediate-comment',
      path: 'main.go',
      side: 'new',
      line: 14,
      body: 'Handle this now.',
      scope: 'last_turn',
      context: 'return result',
      fileChangeSeq: 9,
    });

    expect(store.endpoints.interrupt).toHaveBeenCalledWith(
      's1',
      expect.objectContaining({
        message: 'Handle this now.',
        content: [
          expect.objectContaining({
            type: 'diff_comment',
            diff_comment: expect.objectContaining({
              id: 'immediate-comment',
              path: 'main.go',
              line: 14,
              instruction: 'Handle this now.',
            }),
          }),
          expect.objectContaining({ type: 'input_text' }),
        ],
        delivery: 'steer',
      }),
      expect.any(String),
    );
    expect(store.diff.value.historyComments).toEqual([
      expect.objectContaining({ id: 'immediate-comment', optimistic: true }),
    ]);
    expect(store.toasts.value).toEqual([]);
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

  it('opens the tree with editable branch points even when only one path exists', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    store.endpoints.tree = vi.fn(async () => ({
      root_session_id: 's1',
      active_session_id: 's1',
      path_count: 1,
      nodes: [{ session_id: 's1' }],
      branch_points: [
        {
          message_id: 11,
          anchor_message_id: 0,
          role: 'user',
          preview: 'First question',
          prefill: 'First question',
        },
      ],
    }));

    await store.loadBranchTree();

    expect(store.endpoints.tree).toHaveBeenCalledWith('s1', undefined, true);
    expect(store.branchPathCount.value).toBe(1);
    expect(store.modal.value).toBe('branch');
    expect(store.branchTree.value?.branch_points).toHaveLength(1);
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
    expect(submit).toHaveBeenCalledWith('s1', { call_id: 'ask1', cancelled: true }, 'ask1');
    expect(store.askUser.value).toBeNull();
  });

  it('deduplicates repeated ask-user submissions and locks the record synchronously', async () => {
    const store = new AppStore(config);
    const submitted = deferred<void>();
    const submit = vi.fn(() => submitted.promise.then(() => ({})));
    store.endpoints.askUser = submit;
    const prompt = {
      sessionId: 's1',
      callId: 'ask-once',
      questions: [{ question: 'Continue?', options: [] }],
    };
    store.askUser.value = prompt;

    const first = store.answerAskUser([{ selected: 'yes' }], false, prompt);
    const second = store.answerAskUser([{ selected: 'yes' }], false, prompt);
    expect(submit).toHaveBeenCalledOnce();
    expect(Object.values(store.interactions.value)[0]).toMatchObject({ state: 'submitting' });
    submitted.resolve();
    await Promise.all([first, second]);
    expect(Object.values(store.interactions.value)[0]).toMatchObject({
      state: 'accepted',
      outcome: 'answered',
    });
    store.dispose();
  });

  it('uses the authoritative outcome when an interaction was already resolved', async () => {
    const store = new AppStore(config);
    store.endpoints.approval = vi.fn(async () => ({
      status: 'already_resolved',
      outcome: 'denied',
      resolved_at: 1234,
    }));
    const prompt = {
      sessionId: 's1',
      id: 'approval-replay',
      options: [
        { index: 0, choice: 'allow', label: 'Allow' },
        { index: 1, choice: 'deny', label: 'Deny' },
      ],
    };
    store.approval.value = prompt;

    await store.decideApproval(0, false, prompt);

    expect(Object.values(store.interactions.value)[0]).toMatchObject({
      state: 'denied',
      outcome: 'denied',
      resolvedAt: 1234,
    });
    store.dispose();
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
      recovery: {
        sequence_number: 4,
        events: [
          {
            event: 'response.ask_user.prompt',
            payload: {
              call_id: 'call-1',
              questions: [{ question: 'Choose?', options: [{ label: 'Continue' }] }],
            },
          },
        ],
      },
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

  it('does not orphan a polling loop when event health re-arms reconciliation', async () => {
    vi.useFakeTimers();
    const store = new AppStore(config);
    const first = deferred<void>();
    const second = deferred<void>();
    const internals = store as unknown as {
      reconcile(reason: string, options: { authoritative: boolean }): Promise<void>;
      refreshSidebar(authoritative?: boolean): Promise<void>;
    };
    internals.refreshSidebar = vi.fn(async () => undefined);
    internals.reconcile = vi
      .fn()
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise)
      .mockResolvedValue(undefined);

    store.statusReconciler.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(internals.reconcile).toHaveBeenCalledTimes(1);
    store.statusReconciler.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(internals.reconcile).toHaveBeenCalledTimes(2);
    first.resolve();
    second.resolve();
    await Promise.resolve();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(internals.reconcile).toHaveBeenCalledTimes(3);
    store.dispose();
    vi.useRealTimers();
  });

  it('coalesces concurrent authoritative event recovery', async () => {
    const store = new AppStore(config);
    store.sessions.value = [session()];
    store.activeSessionId.value = 's1';
    const status = deferred<void>();
    const internals = store as unknown as {
      authoritativeRecovery(reason: string): Promise<void>;
      refreshStatus(): Promise<void>;
    };
    internals.refreshStatus = vi.fn(() => status.promise);
    store.endpoints.sessionState = vi.fn(async () => ({}));

    const first = internals.authoritativeRecovery('event-gap');
    const second = internals.authoritativeRecovery('event-instance');
    expect(first).toBe(second);
    expect(internals.refreshStatus).toHaveBeenCalledOnce();
    status.resolve();
    await Promise.all([first, second]);
  });
});
