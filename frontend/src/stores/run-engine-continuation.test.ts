import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { initialProjection } from '../domain/response';
import type { Session } from '../domain/types';
import { AppStore } from './app-store';
import type { ClientToolRunner } from './client-tool-runner';
import type { ClientToolResult } from './webmcp-store';

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
};

const deferred = <T>() => {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => (resolve = done));
  return { promise, resolve };
};

function projection(responseId = 'transport-1') {
  return initialProjection({
    responseId,
    sessionId: 's1',
    epoch: 1,
    status: 'streaming',
    lastSequence: 1,
    startedRev: 1,
    reconnects: 0,
  });
}

function setupStore(active = true) {
  const store = new AppStore(config);
  const session: Session = {
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
    activeProvider: 'old',
    activeModel: 'old-model',
    activeRun: active,
    activeResponseId: active ? 'transport-1' : null,
    lastResponseId: 'resp_msg_41',
    transcriptRev: 1,
    messageBodiesRev: 1,
  };
  store.sessions.value = [session];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.runs.value = { s1: projection() };
  store.endpoints.selectedSession = vi.fn(async () => ({
    selected_session: { id: 's1', title: 'Test', transcript_rev: 5 },
    selected_transcript: { bodies: { rev: 5, messages: [] } },
  }));
  store.endpoints.sessionState = vi.fn(async () => ({ last_response_id: 'resp_msg_42' }));
  return store;
}

async function expectModelSwitch(store: AppStore, continuationId: string | null = 'resp_msg_42') {
  expect(store.runActive.value).toBe(false);
  expect(store.activeSession.value?.lastResponseId).toBe(continuationId);
  store.selectedProvider.value = 'new';
  store.selectedModel.value = 'new-model';
  store.prompt.value = 'continue on another model';
  store.endpoints.createResponse = vi.fn(
    async () => new Response('stop after request capture', { status: 400 }),
  );
  await store.send();
  expect(store.endpoints.createResponse).toHaveBeenCalledWith(
    expect.objectContaining({
      ...(continuationId ? { previous_response_id: continuationId } : {}),
      provider: 'new',
      model: 'new-model',
      model_swap: { mode: 'auto', fallback: 'handover' },
    }),
    's1',
    expect.any(String),
    expect.any(AbortSignal),
  );
  if (!continuationId)
    expect(vi.mocked(store.endpoints.createResponse).mock.calls[0][0]).not.toHaveProperty(
      'previous_response_id',
    );
}

beforeEach(() => localStorage.clear());

describe('continuation cursor recovery', () => {
  it.each(['failed', 'cancelled', 'completed'])(
    'switches models immediately after a %s snapshot without waiting for transcript refresh',
    async (status) => {
      const store = setupStore();
      const selected = deferred<Record<string, unknown>>();
      store.endpoints.selectedSession = vi.fn(() => selected.promise);
      store.endpoints.response = vi.fn(async () => ({
        id: 'transport-1',
        status,
        continuation_response_id: 'resp_msg_42',
        final_rev: 5,
        durable_handoff: true,
      }));
      const recovery = store.runEngine.resumeResponse('s1', 'transport-1');
      try {
        await vi.waitFor(() => expect(store.runs.value.s1.run.status).toBe(status));
        expect(store.activeSession.value).toMatchObject({
          activeRun: false,
          activeResponseId: null,
          messageBodiesRev: 1,
        });
        await expectModelSwitch(store);
      } finally {
        selected.resolve({});
        await recovery;
        store.dispose();
      }
    },
  );

  it.each(['resp_msg_41', null, '', undefined])(
    'falls back to the known cursor %s before the transport ID when a snapshot omits continuation',
    async (knownCursor) => {
      const store = setupStore();
      store.sessions.value = [{ ...store.sessions.value[0], lastResponseId: knownCursor }];
      const selected = deferred<Record<string, unknown>>();
      store.endpoints.selectedSession = vi.fn(() => selected.promise);
      store.endpoints.response = vi.fn(async () => ({ id: 'transport-1', status: 'failed' }));
      const recovery = store.runEngine.resumeResponse('s1', 'transport-1');
      try {
        await vi.waitFor(() => expect(store.runs.value.s1.run.status).toBe('failed'));
        await expectModelSwitch(store, knownCursor === undefined ? 'transport-1' : knownCursor);
      } finally {
        selected.resolve({});
        await recovery;
        store.dispose();
      }
    },
  );

  it.each(['last_response_id', 'lastResponseId'])(
    'preserves the durable %s cursor when idle reconciliation retires a transport',
    async (field) => {
      // Status polling has already installed the authoritative idle flags, but
      // the expired transport projection is still blocking another send.
      const store = setupStore(false);
      store.endpoints.sessionState = vi.fn(async () => ({ [field]: 'resp_msg_42' }));
      try {
        await store.runEngine.reconcileServerIdleResponse('s1', 'transport-1', 5);
        expect(store.runs.value.s1).toBeUndefined();
        expect(store.activeSession.value?.messageBodiesRev).toBe(5);
        await expectModelSwitch(store);
      } finally {
        store.dispose();
      }
    },
  );

  it.each([
    { field: 'lastResponseId', value: '' },
    { field: 'lastResponseId', value: null },
    { field: 'last_response_id', value: '' },
    { field: 'last_response_id', value: null },
  ])(
    'does not resurrect a transport after idle state clears $field to $value',
    async ({ field, value }) => {
      const store = setupStore(false);
      store.endpoints.sessionState = vi.fn(async () => ({ [field]: value }));
      try {
        await store.runEngine.reconcileServerIdleResponse('s1', 'transport-1', 5);
        expect(store.runs.value.s1).toBeUndefined();
        await expectModelSwitch(store, null);
      } finally {
        store.dispose();
      }
    },
  );

  it.each(['', null])(
    'honors an explicitly cleared snapshot continuation (%s) before transcript refresh',
    async (continuationId) => {
      const store = setupStore();
      const selected = deferred<Record<string, unknown>>();
      store.endpoints.selectedSession = vi.fn(() => selected.promise);
      store.endpoints.response = vi.fn(async () => ({
        id: 'transport-1',
        status: 'completed',
        continuation_response_id: continuationId,
      }));
      const recovery = store.runEngine.resumeResponse('s1', 'transport-1');
      try {
        await vi.waitFor(() => expect(store.runs.value.s1.run.status).toBe('completed'));
        await expectModelSwitch(store, null);
      } finally {
        selected.resolve({});
        await recovery;
        store.dispose();
      }
    },
  );

  it('uses a cleared state cursor for snapshot title reconciliation without restoring the snapshot ID', async () => {
    const store = setupStore();
    store.endpoints.sessionState = vi.fn(async () => ({ lastResponseId: null }));
    store.endpoints.response = vi.fn(async () => ({
      id: 'transport-1',
      status: 'completed',
      continuation_response_id: 'resp_msg_42',
      final_rev: 5,
      durable_handoff: true,
    }));
    const title = vi.spyOn(store.runEngine, 'scheduleTitleReconciliation');
    try {
      await store.runEngine.resumeResponse('s1', 'transport-1');
      expect(title).toHaveBeenCalledWith('s1', '', expect.any(Number));
      await expectModelSwitch(store, null);
    } finally {
      store.dispose();
    }
  });

  it.each(['idle', 'snapshot'])(
    'preserves a known durable cursor through %s recovery when legacy state omits it',
    async (path) => {
      const store = setupStore(path === 'snapshot');
      store.endpoints.sessionState = vi.fn(async () => ({}));
      store.endpoints.response = vi.fn(async () => ({
        id: 'transport-1',
        status: 'failed',
        final_rev: 5,
        durable_handoff: true,
      }));
      try {
        if (path === 'idle')
          await store.runEngine.reconcileServerIdleResponse('s1', 'transport-1', 5);
        else await store.runEngine.resumeResponse('s1', 'transport-1');
        await expectModelSwitch(store, 'resp_msg_41');
      } finally {
        store.dispose();
      }
    },
  );

  it.each(['idle', 'snapshot'])(
    'does not let a delayed %s state refresh overwrite a newer run or its cursor',
    async (path) => {
      const store = setupStore(path === 'snapshot');
      const state = deferred<Record<string, unknown>>();
      const toolResult = deferred<ClientToolResult>();
      const inlineSignals = new Map<string, AbortSignal>();
      const clientTools = (store.runEngine as unknown as { clientTools: ClientToolRunner })
        .clientTools;
      const finished = vi.spyOn(clientTools, 'finished');
      const stop = vi.spyOn(clientTools, 'stop');
      store.endpoints.createResponse = vi.fn(
        async () => new Response('unexpected stale continuation', { status: 400 }),
      );
      store.endpoints.inlineClientToolResult = vi.fn(async () => undefined);
      if (path === 'snapshot') {
        store.webMCP.definitions = vi.fn(() => [
          {
            type: 'function' as const,
            name: 'webmcp__ping',
            description: 'ping',
            parameters: { type: 'object' },
          },
        ]);
        store.webMCP.run = vi.fn((call, signal) => {
          inlineSignals.set(call.callId, signal);
          return toolResult.promise;
        });
        clientTools.recordOffer('transport-1');
        clientTools.inlineRequested('s1', 'transport-1', {
          callId: 'inline-old',
          name: 'webmcp__ping',
          arguments: '{}',
        });
      }
      store.endpoints.sessionState = vi.fn(() => state.promise);
      store.endpoints.response = vi.fn(async () => ({
        id: 'transport-1',
        status: 'completed',
        continuation_response_id: 'resp_msg_42',
        final_rev: 5,
        durable_handoff: true,
        pending_client_calls: [{ call_id: 'pending-old', name: 'webmcp__ping', arguments: '{}' }],
      }));
      const title = vi.spyOn(store.runEngine, 'scheduleTitleReconciliation');
      const refresh =
        path === 'idle'
          ? store.runEngine.reconcileServerIdleResponse('s1', 'transport-1', 5)
          : store.runEngine.resumeResponse('s1', 'transport-1');
      try {
        await vi.waitFor(() => expect(store.activeSession.value?.messageBodiesRev).toBe(5));
        store.runs.value = { s1: projection('transport-2') };
        store.sessions.value = [
          {
            ...store.sessions.value[0],
            activeRun: true,
            activeResponseId: 'transport-2',
            lastResponseId: 'resp_msg_99',
          },
        ];
        if (path === 'snapshot') {
          clientTools.recordOffer('transport-2');
          clientTools.inlineRequested('s1', 'transport-2', {
            callId: 'inline-new',
            name: 'webmcp__ping',
            arguments: '{}',
          });
          expect(inlineSignals.get('inline-old')?.aborted).toBe(false);
        }
        state.resolve({ last_response_id: 'resp_msg_42' });
        await refresh;
        expect(store.runs.value.s1.run).toMatchObject({
          responseId: 'transport-2',
          status: 'streaming',
        });
        expect(store.activeSession.value).toMatchObject({
          activeRun: true,
          activeResponseId: 'transport-2',
          lastResponseId: 'resp_msg_99',
        });
        expect(title).not.toHaveBeenCalled();
        expect(finished).not.toHaveBeenCalled();
        expect(stop).not.toHaveBeenCalled();
        if (path === 'snapshot') {
          expect(inlineSignals.get('inline-old')?.aborted).toBe(true);
          expect(inlineSignals.get('inline-new')?.aborted).toBe(false);
          expect(clientTools.offers('transport-1')).toBe(false);
          expect(clientTools.offers('transport-2')).toBe(true);
          toolResult.resolve({ ok: true, output: 'tool output' });
          await vi.waitFor(() =>
            expect(store.endpoints.inlineClientToolResult).toHaveBeenCalledExactlyOnceWith(
              'transport-2',
              'inline-new',
              'tool output',
            ),
          );
          // The completed old snapshot had a pending call, but abandoning it
          // must not execute that call or admit a stale model continuation.
          expect(store.webMCP.run).toHaveBeenCalledTimes(2);
          expect(store.endpoints.createResponse).not.toHaveBeenCalled();
          expect(store.activeSession.value?.lastResponseId).toBe('resp_msg_99');
        }
      } finally {
        state.resolve({});
        toolResult.resolve({ ok: false, output: 'test closed' });
        await refresh;
        store.dispose();
      }
    },
  );

  it.each(['resp_msg_42', undefined])(
    'reconciles a completed snapshot title with its durable cursor (snapshot cursor: %s)',
    async (continuationId) => {
      const store = setupStore();
      store.endpoints.response = vi.fn(async () => ({
        id: 'transport-1',
        status: 'completed',
        continuation_response_id: continuationId,
        final_rev: 5,
        durable_handoff: true,
      }));
      const title = vi.spyOn(store.runEngine, 'scheduleTitleReconciliation');
      try {
        await store.runEngine.resumeResponse('s1', 'transport-1');
        expect(title).toHaveBeenCalledWith('s1', 'resp_msg_42', expect.any(Number));
        await expectModelSwitch(store);
      } finally {
        store.dispose();
      }
    },
  );
});
