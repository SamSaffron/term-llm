import { signal } from '@preact/signals';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AskUserPrompt } from '../domain/types';
import { AppStore } from './app-store';
import { InteractionStore } from './interaction-store';
import type { Modal } from './store-types';
import { testConfig } from './store-test-fixtures';

beforeEach(() => localStorage.clear());

describe('InteractionStore', () => {
  it('owns prompt presentation and terminal cancellation independently of run transport', () => {
    const store = new AppStore(testConfig);
    try {
      store.activeSessionId.value = 's1';
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

  it('records background prompts without assigning foreground modal signals', () => {
    const store = new AppStore(testConfig);
    try {
      store.activeSessionId.value = 's2';
      const prompt: AskUserPrompt = {
        sessionId: 's1',
        callId: 'shared-id',
        questions: [{ question: 'Background?', options: [] }],
      };
      store.interactionStore.present('ask-user', 's1', 'r1', 'shared-id', prompt);
      store.interactionStore.upsert('approval', 's1', 'r1', 'shared-id', {
        sessionId: 's1',
        id: 'shared-id',
        title: 'Approval',
      });

      expect(store.askUser.value).toBeNull();
      expect(store.approval.value).toBeNull();
      expect(store.interactionOrder.value).toHaveLength(2);
      expect(store.interactionStore.pendingForSession('s1')).toHaveLength(2);
    } finally {
      store.dispose();
    }
  });

  it('keeps reused request IDs isolated across responses', () => {
    const store = new AppStore(testConfig);
    try {
      const prompt: AskUserPrompt = {
        sessionId: 's1',
        callId: 'reused',
        questions: [{ question: 'First?', options: [] }],
      };
      store.interactionStore.upsert('ask-user', 's1', 'r1', 'reused', prompt, 2);
      store.interactionStore.resolve('ask-user', 's1', 'r1', 'reused', 'answered');
      store.interactionStore.upsert(
        'ask-user',
        's1',
        'r2',
        'reused',
        { ...prompt, questions: [{ question: 'Second?', options: [] }] },
        3,
      );

      expect(store.interactionOrder.value).toHaveLength(2);
      expect(store.interactionStore.find('ask-user', 's1', 'reused', 'r1')?.state).toBe('accepted');
      expect(store.interactionStore.find('ask-user', 's1', 'reused', 'r2')?.state).toBe('waiting');
    } finally {
      store.dispose();
    }
  });

  it('does not let an older coarse status clear a newer prompt', () => {
    const store = new AppStore(testConfig);
    try {
      const prompt: AskUserPrompt = {
        sessionId: 's1',
        callId: 'ask-1',
        questions: [{ question: 'Continue?', options: [] }],
      };
      store.interactionStore.upsert('ask-user', 's1', 'r1', 'ask-1', prompt, 5);

      store.interactionStore.reconcileSessionLevel('s1', 'r1', 4, false, true, Date.now());
      expect(store.interactionStore.pendingForSession('s1')).toHaveLength(1);

      store.interactionStore.reconcileSessionLevel('s1', 'r1', 5, false, true, Date.now());
      expect(store.interactionStore.pendingForSession('s1')).toHaveLength(0);
      expect(Object.values(store.interactions.value)[0]).toMatchObject({
        state: 'resolved-elsewhere',
      });
    } finally {
      store.dispose();
    }
  });

  it('does not let a status request started before a prompt clear it', () => {
    const store = new AppStore(testConfig);
    try {
      const requestedAt = Date.now() - 1;
      store.interactionStore.upsert(
        'ask-user',
        's1',
        'r1',
        'ask-1',
        {
          sessionId: 's1',
          callId: 'ask-1',
          questions: [{ question: 'Continue?', options: [] }],
        },
        5,
      );

      store.interactionStore.reconcileSessionLevel('s1', '', 0, false, false, requestedAt);
      expect(store.interactionStore.pendingForSession('s1')).toHaveLength(1);
    } finally {
      store.dispose();
    }
  });

  it('publishes only authoritative interaction transitions, not recovery refreshes', () => {
    const app = new AppStore(testConfig);
    const publish = vi.fn();
    const interactions = new InteractionStore(
      app.services,
      signal<Modal>(''),
      signal('s1'),
      publish,
    );
    const prompt: AskUserPrompt = {
      sessionId: 's1',
      callId: 'ask-1',
      questions: [{ header: 'Choice', question: 'Continue?', options: [] }],
    };
    try {
      interactions.upsert('ask-user', 's1', '', 'ask-1', prompt);
      interactions.upsert('ask-user', 's1', 'r1', 'ask-1', { ...prompt });
      expect(publish).toHaveBeenCalledTimes(1);
      expect(interactions.order.value).toHaveLength(1);

      interactions.resolve('ask-user', 's1', 'r2', 'ask-1', 'answered', 10);
      interactions.resolve('ask-user', 's1', 'r1', 'ask-1', 'answered', 20);
      expect(publish).toHaveBeenCalledTimes(3);
      expect(interactions.order.value).toHaveLength(2);
      expect(interactions.shouldOpen('ask-user', 's1', 'ask-1')).toBe(false);
    } finally {
      app.dispose();
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
