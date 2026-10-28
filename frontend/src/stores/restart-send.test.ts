import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '../api/client';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

const restarted = () =>
  new Response(
    JSON.stringify({
      error: {
        type: 'server_instance_changed',
        message: 'server instance changed; the earlier send may still be running',
      },
    }),
    { status: 409 },
  );

function chat(newChat = false): AppStore {
  const store = new AppStore(testConfig);
  if (newChat) store.newChat(true);
  else {
    store.sessions.value = [testSession({ lastResponseId: 'previous-response' })];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
  }
  store.prompt.value = 'Send this after restarting';
  store.runEngine.admissionInstance.value = 'old-process';
  store.endpoints.capabilities = vi.fn(async () => ({
    response_admission: { version: 1, instance_id: 'new-process' },
  }));
  store.endpoints.cancelResponse = vi.fn(async () => ({}));
  return store;
}

beforeEach(() => {
  localStorage.clear();
  vi.useFakeTimers();
});
afterEach(() => vi.useRealTimers());

describe('sending after a server restart', () => {
  it.each([false, true])(
    'silently refreshes a definitively rejected first send and accepts it once (new chat=%s)',
    async (newChat) => {
      const store = chat(newChat);
      store.attachments.value = [
        {
          id: 'draft-file',
          name: 'note.txt',
          type: 'text/plain',
          dataURL: 'data:text/plain;base64,bm90ZQ==',
          status: 'ready',
        },
      ];
      let controller!: ReadableStreamDefaultController<Uint8Array>;
      const stream = new ReadableStream<Uint8Array>({
        start(value) {
          controller = value;
        },
      });
      store.endpoints.createResponse = vi
        .fn()
        .mockResolvedValueOnce(restarted())
        .mockResolvedValueOnce(
          new Response(stream, {
            headers: { 'x-response-id': 'accepted-response', 'x-session-id': 's1' },
          }),
        );
      const sending = store.send();
      try {
        await vi.advanceTimersByTimeAsync(0);
        expect(store.endpoints.capabilities).toHaveBeenCalledOnce();
        const calls = vi.mocked(store.endpoints.createResponse).mock.calls;
        expect(calls).toHaveLength(2);
        expect(calls[1][0]).toBe(calls[0][0]);
        expect(calls[1][1]).toBe(calls[0][1]);
        expect(calls[1][2]).toBe(calls[0][2]);
        expect(calls.map((call) => call[5]?.serverInstance)).toEqual([
          'old-process',
          'new-process',
        ]);
        if (!newChat)
          expect(calls[1][0]).toMatchObject({ previous_response_id: 'previous-response' });
        expect(store.runEngine.admissionInstance.value).toBe('new-process');
        expect(store.activeSession.value?.activeResponseId).toBe('accepted-response');
        expect(store.pendingIntents.value).toEqual({});
        expect(store.prompt.value).toBe('');
        expect(
          store.visibleMessages.value.filter((message) => message.role === 'user'),
        ).toHaveLength(1);
        expect(store.visibleMessages.value.some((message) => message.role === 'error')).toBe(false);
        expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
      } finally {
        store.dispose();
        controller.close();
        await sending;
      }
    },
  );

  it.each(['unavailable', 'legacy', 'unchanged', 'empty', 'restarted-again'])(
    'restores the rejected draft with a useful message if refresh is %s, without looping',
    async (failure) => {
      const store = chat();
      if (failure === 'unavailable')
        store.endpoints.capabilities = vi.fn().mockRejectedValue(new TypeError('offline'));
      else if (failure !== 'restarted-again')
        store.endpoints.capabilities = vi.fn(async () => ({
          response_admission: {
            version: failure === 'legacy' ? 0 : 1,
            instance_id: failure === 'empty' ? '' : 'old-process',
          },
        }));
      store.endpoints.createResponse = vi.fn(async () => restarted());
      try {
        await store.send();
        await vi.advanceTimersByTimeAsync(300_000);
        expect(store.endpoints.capabilities).toHaveBeenCalledOnce();
        expect(store.endpoints.createResponse).toHaveBeenCalledTimes(
          failure === 'restarted-again' ? 2 : 1,
        );
        expect(store.prompt.value).toBe('Send this after restarting');
        expect(store.pendingIntents.value).toEqual({});
        expect(store.runs.value.s1.run.status).toBe('failed');
        expect(store.sendPending.value).toBe(false);
        expect(store.sendBlocked.value).toBe(false);
        expect(store.visibleMessages.value.filter((message) => message.role === 'error')).toEqual([
          expect.objectContaining({
            content:
              'The server restarted before your message was sent. Your draft is still available; please try sending again.',
          }),
        ]);
        expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
      } finally {
        store.dispose();
      }
    },
  );

  it('does not refresh or replay across a restart after an ambiguous POST', async () => {
    const store = chat();
    store.endpoints.createResponse = vi
      .fn()
      .mockRejectedValueOnce(new TypeError('lost acceptance'))
      .mockResolvedValueOnce(restarted());
    try {
      await store.send();
      await vi.advanceTimersByTimeAsync(300_000);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2);
      expect(store.endpoints.capabilities).not.toHaveBeenCalled();
      expect(
        vi.mocked(store.endpoints.createResponse).mock.calls.map((call) => call[5]?.serverInstance),
      ).toEqual(['old-process', 'old-process']);
      expect(store.pendingIntents.value.s1[0].state).toBe('unconfirmed');
      expect(store.prompt.value).toBe('');
      expect(store.visibleMessages.value.some((message) => message.role === 'error')).toBe(false);
    } finally {
      store.dispose();
    }
  });

  it('freezes the refreshed fence if the replacement POST has an ambiguous outcome', async () => {
    const store = chat();
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(restarted())
      .mockRejectedValueOnce(new TypeError('lost acceptance on new process'))
      .mockResolvedValueOnce(restarted());
    try {
      await store.send();
      store.runEngine.admissionInstance.value = 'third-process';
      await vi.advanceTimersByTimeAsync(300_000);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(3);
      expect(store.endpoints.capabilities).toHaveBeenCalledOnce();
      const calls = vi.mocked(store.endpoints.createResponse).mock.calls;
      expect(calls.map((call) => call[5]?.serverInstance)).toEqual([
        'old-process',
        'new-process',
        'new-process',
      ]);
      expect(calls.every((call) => call[0] === calls[0][0] && call[2] === calls[0][2])).toBe(true);
      expect(store.pendingIntents.value.s1[0].state).toBe('unconfirmed');
      expect(store.prompt.value).toBe('');
      expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
    } finally {
      store.dispose();
    }
  });

  it('does not refresh for an unrelated conflict', async () => {
    const store = chat();
    store.endpoints.createResponse = vi
      .fn()
      .mockRejectedValue(new APIError('session is busy', 409));
    try {
      await store.send();
      expect(store.endpoints.createResponse).toHaveBeenCalledOnce();
      expect(store.endpoints.capabilities).not.toHaveBeenCalled();
      expect(store.prompt.value).toBe('Send this after restarting');
    } finally {
      store.dispose();
    }
  });
});
