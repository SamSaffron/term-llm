import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '../api/client';
import { persistPendingIntent, readPendingIntents, readDrafts } from '../platform/storage';
import { AppStore } from './app-store';
import { testConfig, testSession } from './store-test-fixtures';

const chat = (guarded = true) => {
  const store = new AppStore(testConfig);
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.prompt.value = 'Only send this once';
  store.runEngine.admissionInstance.value = guarded ? 'process-one' : '';
  store.endpoints.cancelResponse = vi.fn(async () => ({}));
  return store;
};
beforeEach(() => {
  localStorage.clear();
  vi.useFakeTimers();
});
afterEach(() => vi.useRealTimers());

describe('bounded unknown send admission', () => {
  it('owns exactly three guarded attempts at 0/1/4 seconds, freezes the process header, then durably pauses without cancellation', async () => {
    const store = chat();
    store.endpoints.createResponse = vi.fn().mockRejectedValue(new TypeError('lost reply'));
    try {
      await store.send();
      store.runEngine.admissionInstance.value = 'new-process-from-background-refresh';
      await vi.advanceTimersByTimeAsync(999);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(2999);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(1);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(3);
      const calls = vi.mocked(store.endpoints.createResponse).mock.calls;
      expect(calls.every((call) => call[2] === calls[0][2] && call[0] === calls[0][0])).toBe(true);
      expect(calls.map((call) => call[5])).toEqual(
        Array(3).fill({ serverInstance: 'process-one' }),
      );
      expect(store.runs.value.s1.run.status).toBe('unconfirmed');
      expect(store.runActive.value).toBe(false);
      expect(store.sendBlocked.value).toBe(true);
      expect(store.sendPending.value).toBe(false);
      expect(store.prompt.value).toBe('');
      expect(readPendingIntents(localStorage, store.keys.pendingIntents).s1[0].state).toBe(
        'unconfirmed',
      );
      expect(store.visibleMessages.value[0].interruptState).toBe('unconfirmed_send');
      expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(300_000);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(3);
      store.runEngine.dismissUnconfirmed('s1');
      expect(store.sendBlocked.value).toBe(false);
      expect(store.pendingIntents.value.s1[0].state).toBe('dismissed-unconfirmed');
      expect(store.prompt.value).toBe('');
      expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
    } finally {
      store.dispose();
    }
  });

  it.each([false, true])(
    'does not clear or submit when initial pending persistence fails (new chat=%s)',
    async (newChat) => {
      const store = newChat ? new AppStore(testConfig) : chat();
      if (newChat) store.newChat(true);
      store.prompt.value = 'Keep this exact draft';
      store.composer.selectedDraftWorktree.value = '/worktrees/keep-me';
      const attachments = [
        {
          id: 'original-attachment',
          name: 'important.txt',
          type: 'text/plain',
          size: 4,
          blobRef: 'original-blob',
          dataURL: 'data:text/plain;base64,ZGF0YQ==',
          status: 'ready' as const,
        },
      ];
      store.attachments.value = attachments;
      const draftId = store.composer.storageId();
      store.composer.persist();
      store.endpoints.createResponse = vi.fn();
      const clear = vi.spyOn(store.composer, 'clearSubmitted');
      const write = localStorage.setItem.bind(localStorage);
      vi.spyOn(localStorage, 'setItem').mockImplementation((key, value) => {
        if (key.startsWith(`${store.keys.pendingIntents}:`))
          throw new DOMException('Quota exceeded', 'QuotaExceededError');
        write(key, value);
      });
      try {
        await store.send();
        expect(store.endpoints.createResponse).not.toHaveBeenCalled();
        expect(clear).not.toHaveBeenCalled();
        expect(store.prompt.value).toBe('Keep this exact draft');
        expect(store.attachments.value).toBe(attachments);
        expect(store.composer.storageId()).toBe(draftId);
        expect(store.composer.selectedDraftWorktree.value).toBe('/worktrees/keep-me');
        expect(store.pendingIntents.value).toEqual({});
        expect(store.runs.value).toEqual({});
        expect(store.sendPending.value).toBe(false);
        if (newChat) expect(store.activeSession.value).toBeNull();
        store.composer.persist(); // Other draft writes remain available.
        expect(
          readDrafts(localStorage, store.keys.draftMessages).find(
            (draft) => draft.sessionId === draftId,
          ),
        ).toMatchObject({
          content: 'Keep this exact draft',
          attachments: [{ id: 'original-attachment', blobRef: 'original-blob' }],
        });
        await vi.advanceTimersByTimeAsync(300_000);
        expect(store.endpoints.createResponse).not.toHaveBeenCalled();
      } finally {
        store.dispose();
      }
    },
  );

  it('pauses legacy servers after the first ambiguous outcome, without any POST timer', async () => {
    const store = chat(false);
    store.endpoints.createResponse = vi.fn().mockRejectedValue(new TypeError('network'));
    try {
      await store.send();
      await vi.advanceTimersByTimeAsync(500_000);
      expect(store.endpoints.createResponse).toHaveBeenCalledOnce();
      expect(store.runs.value.s1.run.status).toBe('unconfirmed');
      expect(store.endpoints.cancelResponse).not.toHaveBeenCalled();
    } finally {
      store.dispose();
    }
  });

  it.each([401, 403, 409, 413])(
    'does not treat a later %s as proof that the first ambiguous POST was rejected',
    async (status) => {
      const store = chat();
      store.endpoints.createResponse = vi
        .fn()
        .mockRejectedValueOnce(new TypeError('lost acceptance'))
        .mockResolvedValueOnce(new Response('later rejection', { status }));
      try {
        await store.send();
        await vi.advanceTimersByTimeAsync(1_000);
        expect(store.runs.value.s1.run.status).toBe('unconfirmed');
        expect(store.pendingIntents.value.s1[0].state).toBe('unconfirmed');
        expect(store.prompt.value).toBe('');
        expect(store.visibleMessages.value.some((entry) => entry.role === 'error')).toBe(false);
      } finally {
        store.dispose();
      }
    },
  );

  it('bounds three stalled header attempts to 94 seconds, and never retries beyond the absolute window', async () => {
    const store = chat();
    const signals: AbortSignal[] = [];
    store.endpoints.createResponse = vi.fn((_body, _session, _id, signal) => {
      signals.push(signal!);
      return new Promise<Response>(() => {});
    });
    try {
      const sending = store.send();
      await vi.advanceTimersByTimeAsync(94_000);
      await sending;
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(3);
      expect(signals.every((signal) => signal.aborted)).toBe(true);
      expect(store.runs.value.s1.run.status).toBe('unconfirmed');
      await vi.advanceTimersByTimeAsync(100_000);
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(3);
    } finally {
      store.dispose();
    }
  });

  it('restores a definitive first 413 even when its error body never completes', async () => {
    const store = chat();
    store.endpoints.createResponse = vi.fn(
      async () => new Response(new ReadableStream(), { status: 413 }),
    );
    try {
      const sending = store.send();
      await vi.advanceTimersByTimeAsync(30_000);
      await sending;
      expect(store.endpoints.createResponse).toHaveBeenCalledOnce();
      expect(store.prompt.value).toBe('Only send this once');
      expect(store.pendingIntents.value).toEqual({});
      expect(store.runs.value.s1.run.status).toBe('failed');
      expect(store.visibleMessages.value[0].content).toContain('413');
    } finally {
      store.dispose();
    }
  });

  it('migrates legacy checking on construction, preserves paused intent on idle reconciliation, and Check status never POSTs', async () => {
    const seed = chat();
    persistPendingIntent(localStorage, seed.keys.pendingIntents, 's1', {
      id: 'pending-1',
      clientMessageId: 'client-1',
      content: 'possibly sent',
      created: Date.now(),
      state: 'checking',
      attachments: [
        {
          name: 'photo.png',
          type: 'image/png',
          uploadId: `upl_${'a'.repeat(43)}`,
          blobRef: 'keep-this',
        },
      ],
    });
    seed.dispose();
    const restored = chat();
    restored.prompt.value = '';
    restored.endpoints.createResponse = vi.fn();
    restored.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [{ id: 's1', active_response_id: '', transcript_rev: 1 }],
    }));
    restored.endpoints.sidebar = vi.fn(async () => ({ sessions: [testSession()] }));
    restored.endpoints.sessions = vi.fn(async () => ({ sessions: [testSession()] }));
    restored.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', transcript_rev: 1 },
      selected_transcript: { bodies: { rev: 1, messages: [] } },
    }));
    restored.endpoints.sessionState = vi.fn(async () => ({}));
    try {
      expect(restored.pendingIntents.value.s1[0].state).toBe('unconfirmed');
      expect(restored.runActive.value).toBe(false);
      restored.runEngine.reconcileLoadedIntents('s1', [], false);
      await restored.checkSendStatus();
      expect(restored.pendingIntents.value.s1[0]).toMatchObject({
        state: 'unconfirmed',
        attachments: [{ blobRef: 'keep-this' }],
      });
      await vi.advanceTimersByTimeAsync(300_000);
      expect(restored.endpoints.createResponse).not.toHaveBeenCalled();
      expect(restored.endpoints.cancelResponse).not.toHaveBeenCalled();
    } finally {
      restored.dispose();
    }
  });

  it('does not keep the fast active-run polling cadence for a paused intent', async () => {
    const store = chat(false);
    store.endpoints.createResponse = vi.fn().mockRejectedValue(new TypeError('network'));
    store.endpoints.sessionStatus = vi.fn(async () => ({
      sessions: [{ id: 's1', active_response_id: '', transcript_rev: 1 }],
    }));
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', transcript_rev: 1 },
      selected_transcript: { bodies: { rev: 1, messages: [] } },
    }));
    store.endpoints.sessionState = vi.fn(async () => ({}));
    vi.spyOn(store, 'refreshSidebar').mockResolvedValue(undefined);
    try {
      await store.send();
      (store as any).statusReconciler.start();
      await vi.advanceTimersByTimeAsync(0);
      expect(store.endpoints.sessionStatus).toHaveBeenCalledOnce();
      await vi.advanceTimersByTimeAsync(29_999);
      expect(store.endpoints.sessionStatus).toHaveBeenCalledOnce();
      await vi.advanceTimersByTimeAsync(1);
      expect(store.endpoints.sessionStatus).toHaveBeenCalledTimes(2);
      expect(store.endpoints.createResponse).toHaveBeenCalledOnce();
    } finally {
      store.dispose();
    }
  });

  it('retires paused uncertainty only when a matching client message is actually committed', async () => {
    const store = chat(false);
    store.endpoints.createResponse = vi.fn().mockRejectedValue(new APIError('gateway error', 502));
    try {
      await store.send();
      const id = store.pendingIntents.value.s1[0].clientMessageId;
      store.runEngine.reconcileLoadedIntents('s1', [], false);
      expect(store.sendBlocked.value).toBe(true);
      store.runEngine.retireCommittedIntents('s1', [
        {
          id: 'server-user',
          role: 'user',
          content: 'Only send this once',
          created: 1,
          clientMessageId: id,
        },
      ]);
      expect(store.pendingIntents.value).toEqual({});
      expect(store.sendBlocked.value).toBe(false);
      expect(store.visibleMessages.value[0].interruptState).toBeUndefined();
    } finally {
      store.dispose();
    }
  });
});
