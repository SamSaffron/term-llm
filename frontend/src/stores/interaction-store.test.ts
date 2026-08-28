import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AskUserPrompt } from '../domain/types';
import { AppStore } from './app-store';
import { testConfig } from './store-test-fixtures';

beforeEach(() => localStorage.clear());

describe('InteractionStore', () => {
  it('owns prompt presentation and terminal cancellation independently of run transport', () => {
    const store = new AppStore(testConfig);
    try {
      const prompt: AskUserPrompt = {
        sessionId: 's1',
        callId: 'ask-1',
        questions: [{ header: 'Choice', question: 'Continue?', options: [] }],
      };

      store.interactionStore.present('ask-user', 's1', 'r1', 'ask-1', prompt);

      expect(store.interactionStore.askUser.value).toBe(prompt);
      expect(store.interactionStore.order.value).toHaveLength(1);
      expect(Object.values(store.interactionStore.interactions.value)[0]).toMatchObject({
        sessionId: 's1',
        responseId: 'r1',
        requestId: 'ask-1',
        state: 'waiting',
      });

      store.interactionStore.cancelOutstandingForResponse('s1', 'r1');
      expect(Object.values(store.interactionStore.interactions.value)[0]).toMatchObject({
        state: 'cancelled-by-agent',
        outcome: 'Decision no longer needed',
      });
    } finally {
      store.dispose();
    }
  });

  it('deduplicates concurrent submissions inside the interaction owner', async () => {
    const store = new AppStore(testConfig);
    try {
      const prompt: AskUserPrompt = {
        sessionId: 's1',
        callId: 'ask-1',
        questions: [{ header: 'Choice', question: 'Continue?', options: [] }],
      };
      let resolve!: (value: Record<string, unknown>) => void;
      store.endpoints.askUser = vi.fn(
        () =>
          new Promise<Record<string, unknown>>((done) => {
            resolve = done;
          }),
      );

      const first = store.interactionStore.answerAskUser([], false, prompt);
      const second = store.interactionStore.answerAskUser([], false, prompt);
      expect(store.endpoints.askUser).toHaveBeenCalledOnce();

      resolve({});
      await Promise.all([first, second]);
      expect(Object.values(store.interactionStore.interactions.value)[0].state).toBe('accepted');
    } finally {
      store.dispose();
    }
  });
});
