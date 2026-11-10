import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { initialProjection } from '../domain/response';
import type { Session } from '../domain/types';
import { AppStore } from './app-store';

const config: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: [],
  title: '',
  locationSharing: true,
  worktrees: true,
  hub: null,
  vapidKey: '',
};

function setupStore() {
  const store = new AppStore(config);
  store.sessions.value = [
    {
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
      activeProvider: 'agy-bin',
      activeModel: 'old-model',
      activeEffort: 'high',
      activeRun: true,
      activeResponseId: 'r1',
    } satisfies Session,
  ];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.runs.value = {
    s1: initialProjection({
      responseId: 'r1',
      sessionId: 's1',
      epoch: 1,
      status: 'streaming',
      lastSequence: 0,
      startedRev: 1,
      reconnects: 0,
    }),
  };
  // Leave the resumed SSE idle: assertions must pass before the next event arrives.
  const internals = store as unknown as {
    streamResponse(responseId: string, sessionId: string, after: number): Promise<void>;
  };
  internals.streamResponse = vi.fn(async () => undefined);
  return store;
}

const progress = {
  stage: 'handover_done',
  message: 'Handover ready; retrying on chatgpt…',
  previous_provider: 'agy-bin',
  previous_model: 'old-model',
  previous_effort: 'high',
  target_provider: 'chatgpt',
  target_model: 'new-model',
  target_effort: '',
};

beforeEach(() => localStorage.clear());

describe('handover snapshot recovery', () => {
  it('restores the target header and handover progress before another SSE event', async () => {
    const store = setupStore();
    store.endpoints.response = vi.fn(async () => ({
      id: 'r1',
      status: 'in_progress',
      provider: 'chatgpt',
      model: 'new-model',
      reasoning_effort: '',
      last_sequence_number: 12,
      recovery: { events: [{ event: 'response.model_swap.progress', payload: progress }] },
    }));
    try {
      await store.runEngine.resumeResponse('s1', 'r1');
      expect(store.activeSession.value).toMatchObject({
        activeProvider: 'chatgpt',
        activeModel: 'new-model',
        activeEffort: '',
        activeRun: true,
      });
      expect(store.runs.value.s1.modelSwap).toMatchObject({
        stage: 'handover_done',
        content: progress.message,
        fromProvider: 'agy-bin',
        toProvider: 'chatgpt',
        toModel: 'new-model',
        swapStrategy: 'handover',
      });
      expect(store.runs.value.s1.run.lastSequence).toBe(12);
      // A concurrent transcript refresh may still carry uncommitted old settings.
      store.endpoints.sessionState = vi.fn(async () => ({}));
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: {
          id: 's1',
          provider: 'agy-bin',
          model: 'old-model',
          transcript_rev: 2,
        },
        selected_transcript: { bodies: { rev: 2, messages: [] } },
      }));
      await store.runEngine.refreshSessionMessages('s1');
      expect(store.activeSession.value).toMatchObject({
        activeProvider: 'chatgpt',
        activeModel: 'new-model',
        activeEffort: '',
      });
    } finally {
      store.dispose();
    }
  });

  it('clears stale handover progress when the authoritative snapshot has none', async () => {
    const store = setupStore();
    store.runs.value = {
      s1: {
        ...store.runs.value.s1,
        modelSwap: {
          stage: 'handover_start',
          content: 'Preparing handover',
          fromProvider: 'agy-bin',
          fromModel: 'old-model',
          fromEffort: '',
          toProvider: 'chatgpt',
          toModel: 'new-model',
          toEffort: '',
          swapStatus: 'started',
          swapStrategy: 'handover',
        },
      },
    };
    // Older servers may omit runtime identity: do not erase the known header.
    store.endpoints.response = vi.fn(async () => ({
      id: 'r1',
      status: 'in_progress',
      last_sequence_number: 13,
      recovery: {},
    }));
    try {
      await store.runEngine.resumeResponse('s1', 'r1');
      expect(store.runs.value.s1.modelSwap).toBeUndefined();
      expect(store.activeSession.value).toMatchObject({
        activeProvider: 'agy-bin',
        activeModel: 'old-model',
        activeEffort: 'high',
      });
    } finally {
      store.dispose();
    }
  });
});
