import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import { readDrafts, saveDraft } from '../platform/storage';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

const stores: AppStore[] = [];
beforeEach(() => localStorage.clear());
afterEach(() => {
  stores.splice(0).forEach((store) => store.dispose());
  vi.restoreAllMocks();
});

function setup(sessions: Session[] = [testSession({ id: 's1', title: 'First' })]) {
  const store = new AppStore(testConfig);
  stores.push(store);
  store.sessions.value = sessions;
  store.endpoints.selectedSession = vi.fn(async (id: string) => ({
    selected_session: {
      id,
      project_id: sessions.find((session) => session.id === id)?.projectId || '',
    },
    selected_transcript: { bodies: { messages: [] } },
  }));
  store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
  store.endpoints.tree = vi.fn(async () => ({}));
  store.endpoints.sessionState = vi.fn(async () => ({}));
  store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
  return store;
}

function completedResponse(sessionId: string): Response {
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
    { headers: { 'x-response-id': 'r1', 'x-session-id': sessionId } },
  );
}

describe('new chat draft', () => {
  it('resumes unsent new-chat text after visiting a conversation', async () => {
    const store = setup();
    store.newChat();
    store.prompt.value = 'half-written idea';

    await store.selectSession(store.sessions.peek()[0]);
    expect(store.prompt.value).toBe('');
    store.newChat();

    expect(store.draftActive.value).toBe(true);
    expect(store.prompt.value).toBe('half-written idea');
  });

  it('keeps the resumed draft on one record instead of forking it', async () => {
    const store = setup();
    store.newChat();
    store.prompt.value = 'first pass';
    const draftID = store.composer.storageId();

    await store.selectSession(store.sessions.peek()[0]);
    store.newChat();
    store.prompt.value = 'first pass, revised';
    await store.selectSession(store.sessions.peek()[0]);

    const unsent = readDrafts(store.storage, store.keys.draftMessages).filter((draft) =>
      draft.sessionId.startsWith('draft:'),
    );
    expect(unsent).toEqual([
      expect.objectContaining({ sessionId: draftID, content: 'first pass, revised' }),
    ]);
  });

  it('recovers the newest draft orphaned by an earlier visit', async () => {
    const store = setup();
    const now = vi.spyOn(Date, 'now').mockReturnValue(1_000);
    saveDraft(store.storage, store.keys.draftMessages, {
      sessionId: 'draft:older',
      content: 'older text',
      projectId: '',
      updated: 1,
    });
    now.mockReturnValue(2_000);
    saveDraft(store.storage, store.keys.draftMessages, {
      sessionId: 'draft:newer',
      content: 'newer text',
      projectId: '',
      updated: 2,
    });
    now.mockRestore();
    await store.selectSession(store.sessions.peek()[0]);

    store.newChat();

    expect(store.composer.storageId()).toBe('draft:newer');
    expect(store.prompt.value).toBe('newer text');
  });

  it('only resumes drafts that belong to the new chat project', async () => {
    const store = setup([testSession({ id: 's1', title: 'First', projectId: 'p2' })]);
    store.projects.value = [
      { id: 'p1', name: 'One' },
      { id: 'p2', name: 'Two' },
    ] as typeof store.projects.value;
    saveDraft(store.storage, store.keys.draftMessages, {
      sessionId: 'draft:p1-idea',
      content: 'belongs to project one',
      projectId: 'p1',
      updated: 1,
    });
    await store.selectSession(store.sessions.peek()[0]);

    store.newChat();
    expect(store.activeProjectId.value).toBe('p2');
    expect(store.prompt.value).toBe('');

    await store.selectSession(store.sessions.peek()[0]);
    store.newChat(false, 'p1');
    expect(store.prompt.value).toBe('belongs to project one');
  });

  it('starts blank after the draft has been sent', async () => {
    const store = setup([]);
    store.endpoints.createBlankSession = vi.fn(async () => ({
      session: { id: 's9', number: 9 },
    })) as never;
    store.endpoints.createResponse = vi.fn(async () => completedResponse('s9'));
    store.newChat();
    store.prompt.value = 'ship it';

    await store.send();
    expect(store.activeSessionId.value).toBe('s9');
    store.newChat();

    expect(store.draftActive.value).toBe(true);
    expect(store.prompt.value).toBe('');
  });

  it.each(['', 'describe this picture'])(
    'removes the sent picture draft with text %j',
    async (text) => {
      const store = setup([]);
      store.endpoints.createResponse = vi.fn(async () => completedResponse('s9'));
      store.newChat();
      store.attachments.value = [
        {
          id: 'picture-1',
          name: 'photo.png',
          type: 'image/png',
          dataURL: 'data:image/png;base64,AA==',
          status: 'ready',
        },
      ];
      // Attachment preparation saves immediately, before the user finishes typing.
      store.composer.persist();
      const draftID = store.composer.storageId();
      store.prompt.value = text;

      await store.send();

      expect(store.endpoints.createResponse).toHaveBeenCalledOnce();
      expect(store.attachments.value).toEqual([]);
      expect(store.composer.unsentNewChats.value).toEqual([]);
      expect(
        readDrafts(store.storage, store.keys.draftMessages).find(
          (draft) => draft.sessionId === draftID,
        )?.attachments || [],
      ).toEqual([]);
      store.newChat();
      expect(store.prompt.value).toBe('');
      expect(store.attachments.value).toEqual([]);
    },
  );

  it('lists an unsent draft only while it is not open in the composer', async () => {
    const store = setup();
    store.newChat();
    store.prompt.value = 'come back to this';
    store.composer.persist();
    expect(store.composer.unsentNewChats.value).toEqual([]);

    await store.selectSession(store.sessions.peek()[0]);
    expect(store.composer.unsentNewChats.value).toEqual([
      expect.objectContaining({ content: 'come back to this' }),
    ]);

    store.newChat();
    expect(store.composer.unsentNewChats.value).toEqual([]);
  });

  it('opens a listed draft from another project without losing the open one', async () => {
    const store = setup([testSession({ id: 's2', title: 'Second', projectId: 'p2' })]);
    store.projects.value = [
      { id: 'p1', name: 'One' },
      { id: 'p2', name: 'Two' },
    ] as typeof store.projects.value;
    store.newChat(false, 'p1');
    store.prompt.value = 'project one text';
    const first = store.composer.storageId();
    await store.selectSession(store.sessions.peek()[0]);
    store.newChat();
    expect(store.activeProjectId.value).toBe('p2');
    store.prompt.value = 'project two text';
    store.composer.persist();

    const listed = store.composer.unsentNewChats.value;
    expect(listed).toEqual([expect.objectContaining({ sessionId: first, projectId: 'p1' })]);
    store.openNewChatDraft(listed[0]!);

    expect(store.activeProjectId.value).toBe('p1');
    expect(store.prompt.value).toBe('project one text');
    expect(store.composer.unsentNewChats.value).toEqual([
      expect.objectContaining({ content: 'project two text', projectId: 'p2' }),
    ]);
  });

  it('does not overwrite a newer edit from another tab when undoing discard', async () => {
    const store = setup();
    store.newChat();
    store.prompt.value = 'original idea';
    await store.selectSession(store.sessions.peek()[0]);
    const [draft] = store.composer.unsentNewChats.value;
    store.discardNewChatDraft(draft!.sessionId);
    const undo = store.toasts.value.at(-1)!.action!;

    // Another tab still has the discarded draft open and saves a newer edit.
    saveDraft(store.storage, store.keys.draftMessages, {
      ...draft!,
      content: 'newer idea from another tab',
    });
    undo.run();

    expect(readDrafts(store.storage, store.keys.draftMessages)).toEqual([
      expect.objectContaining({ content: 'newer idea from another tab' }),
    ]);
    expect(store.toasts.value.at(-1)).toMatchObject({ kind: 'error' });
  });

  it('does not discard the draft currently open in the composer', () => {
    const store = setup();
    store.newChat();
    store.prompt.value = 'still editing';
    store.composer.persist();

    store.discardNewChatDraft(store.composer.storageId());

    expect(readDrafts(store.storage, store.keys.draftMessages)).toEqual([
      expect.objectContaining({ content: 'still editing' }),
    ]);
    expect(store.toasts.value).toEqual([]);
  });

  it('discards a listed draft and restores it on undo', async () => {
    const store = setup();
    store.newChat();
    store.prompt.value = 'maybe not';
    await store.selectSession(store.sessions.peek()[0]);
    const [draft] = store.composer.unsentNewChats.value;

    store.discardNewChatDraft(draft!.sessionId);
    expect(store.composer.unsentNewChats.value).toEqual([]);
    const toast = store.toasts.value.at(-1);
    expect(toast).toMatchObject({ message: 'Draft discarded', action: { label: 'Undo' } });

    toast!.action!.run();
    expect(store.composer.unsentNewChats.value).toEqual([
      expect.objectContaining({ sessionId: draft!.sessionId, content: 'maybe not' }),
    ]);
    store.newChat();
    expect(store.prompt.value).toBe('maybe not');
  });
});
