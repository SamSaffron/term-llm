import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import { APIError } from '../api/client';
import { AppStore } from './app-store';

/**
 * End-to-end client (WebMCP) tool flow through the real store: the page
 * offers tools, the model calls one, the page runs it and continues the
 * response with a function_call_output item.
 */

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

type Frame = [string, Record<string, unknown>];
type TestWindow = Window & { __termLLMDeviceTools?: unknown };

function sse(responseId: string, frames: Frame[]): Response {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        frames.forEach(([type, payload], index) =>
          controller.enqueue(
            encoder.encode(
              `event: ${type}\ndata: ${JSON.stringify({
                ...payload,
                response_id: responseId,
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
    { headers: { 'x-response-id': responseId, 'x-session-id': 's1' } },
  );
}

const callFrames = (responseId: string, name: string, args: string): Frame[] => {
  const item = { type: 'function_call', id: 'fc_call_1', call_id: 'call_1', name };
  return [
    ['response.created', { response: { id: responseId, status: 'in_progress' } }],
    ['response.output_item.added', { item: { ...item, arguments: '' }, output_index: 0 }],
    ['response.output_item.done', { item: { ...item, arguments: args }, output_index: 0 }],
    ['response.completed', { response: { id: responseId, status: 'completed' }, final_rev: 1 }],
  ];
};

const textFrames = (responseId: string, text: string): Frame[] => [
  ['response.created', { response: { id: responseId, status: 'in_progress' } }],
  ['response.output_text.delta', { delta: text, output_index: 0 }],
  ['response.completed', { response: { id: responseId, status: 'completed' }, final_rev: 2 }],
];

function installDeviceTools(execute: (input: Record<string, unknown>) => Promise<unknown>) {
  const context = Object.assign(new EventTarget(), {
    getTools: vi.fn(async () => [
      {
        name: 'ping',
        description: 'Check the phone is reachable',
        inputSchema: { type: 'object', properties: { message: { type: 'string' } } },
        annotations: { readOnlyHint: true },
      },
    ]),
    executeTool: vi.fn(async (_tool: unknown, input: Record<string, unknown>) => execute(input)),
  });
  Object.defineProperty(document, 'modelContext', { value: context, configurable: true });
  (window as TestWindow).__termLLMDeviceTools = { device: 'iPhone' };
  return context;
}

async function chatStore(): Promise<AppStore> {
  const store = new AppStore(config);
  store.sessions.value = [];
  store.activeSessionId.value = '';
  store.draftActive.value = true;
  // Durable transcript refreshes are not under test.
  store.endpoints.selectedSession = vi.fn(async () => ({}));
  store.endpoints.sessionState = vi.fn(async () => ({}));
  store.endpoints.cancelResponse = vi.fn(async () => ({ status: 'cancelled' }));
  vi.spyOn(store, 'reconcile').mockResolvedValue(undefined);
  await vi.waitFor(() => expect(store.webMCP.available.value).toBe(true));
  // Page tools are off until turned on; these flows exercise them turned on.
  store.setWebMCPEnabled(true);
  return store;
}

const requestBody = (store: AppStore, index: number) =>
  vi.mocked(store.endpoints.createResponse).mock.calls[index]?.[0] as Record<string, unknown>;

beforeEach(() => localStorage.clear());
afterEach(() => {
  delete (document as { modelContext?: unknown }).modelContext;
  delete (window as TestWindow).__termLLMDeviceTools;
});

describe('client tool continuation', () => {
  it('runs an inline native tool while the ordinary typed response is still open', async () => {
    const context = installDeviceTools(async (input) => `pong: ${input.message}`);
    const store = await chatStore();
    const encode = new TextEncoder();
    let stream!: ReadableStreamDefaultController<Uint8Array>;
    const frame = (type: string, sequence: number, data: Record<string, unknown>) =>
      encode.encode(
        `event: ${type}\ndata: ${JSON.stringify({ response_id: 'r1', run_epoch: 1, sequence_number: sequence, ...data })}\n\n`,
      );
    store.endpoints.createResponse = vi.fn(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              stream = controller;
              controller.enqueue(
                frame('response.created', 1, { response: { id: 'r1', status: 'in_progress' } }),
              );
              controller.enqueue(
                frame('response.client_tool.requested', 2, {
                  call_id: 'call_phone',
                  name: 'webmcp__ping',
                  arguments: '{"message":"hi"}',
                }),
              );
              controller.enqueue(
                frame('response.client_tool.requested', 3, {
                  call_id: 'call_phone',
                  name: 'webmcp__ping',
                  arguments: '{"message":"hi"}',
                }),
              );
            },
          }),
          { headers: { 'x-response-id': 'r1', 'x-session-id': 's1' } },
        ),
    );
    store.endpoints.inlineClientToolResult = vi.fn(async () => {
      stream.enqueue(
        frame('response.completed', 4, {
          response: { id: 'r1', status: 'completed' },
          final_rev: 1,
        }),
      );
      stream.enqueue(encode.encode('data: [DONE]\n\n'));
      stream.close();
      return undefined;
    });
    store.prompt.value = 'Ping my phone';
    await store.send();
    await vi.waitFor(() =>
      expect(store.endpoints.inlineClientToolResult).toHaveBeenCalledWith(
        'r1',
        'call_phone',
        'pong: hi',
      ),
    );
    expect(context.executeTool).toHaveBeenCalledTimes(1);
    expect(store.endpoints.createResponse).toHaveBeenCalledTimes(1);
    await vi.waitFor(() => expect(store.runs.value.s1.run.status).toBe('completed'));
    store.dispose();
  });

  it('executes a missed inline request from the authoritative reconnect snapshot once', async () => {
    const context = installDeviceTools(async () => 'pong after reconnect');
    const store = await chatStore();
    store.endpoints.createResponse = vi.fn(async () =>
      sse('r1', [['response.created', { response: { id: 'r1', status: 'in_progress' } }]]),
    );
    store.endpoints.response = vi.fn(async () => ({
      id: 'r1',
      status: 'in_progress',
      run_epoch: 1,
      last_sequence_number: 2,
      pending_inline_client_tools: [
        { call_id: 'call_phone', name: 'webmcp__ping', arguments: '{"message":"hi"}' },
      ],
      recovery: { sequence_number: 2, messages: [] },
    }));
    store.endpoints.responseEvents = vi.fn(async () => new Promise<Response>(() => undefined));
    store.endpoints.inlineClientToolResult = vi.fn(async () => undefined);
    store.prompt.value = 'Ping my phone';
    void store.send();
    await vi.waitFor(() =>
      expect(store.endpoints.response).toHaveBeenCalledWith('r1', expect.anything()),
    );
    await vi.waitFor(() =>
      expect(store.endpoints.inlineClientToolResult).toHaveBeenCalledWith(
        'r1',
        'call_phone',
        'pong after reconnect',
      ),
    );
    expect(context.executeTool).toHaveBeenCalledTimes(1);
    expect(vi.mocked(store.endpoints.responseEvents).mock.calls[0]?.[1]).toBe(2);
    store.dispose();
  });

  it.each([
    ['live', 'resp_msg_42'],
    ['snapshot', 'resp_msg_42'],
    ['live', undefined],
    ['snapshot', undefined],
  ] as const)(
    'uses the %s handoff cursor (%s) instead of a newer session tail',
    async (source, cursor) => {
      let finish!: (value: string) => void;
      const context = installDeviceTools(() => new Promise((resolve) => (finish = resolve)));
      const store = await chatStore();
      const liveId = 'resp_live_handoff';
      const call = { call_id: 'call_1', name: 'webmcp__ping', arguments: '{}' };
      store.endpoints.selectedSession = vi.fn(async () => ({
        selected_session: { id: 's1', transcript_rev: 2 },
        selected_transcript: { bodies: { rev: 2, messages: [] } },
      }));
      store.endpoints.sessionState = vi.fn(async () => ({ last_response_id: 'resp_msg_99' }));
      const frames = callFrames(liveId, call.name, call.arguments);
      frames[3] = [
        'response.completed',
        { response: { id: cursor, status: 'completed' }, final_rev: 1 },
      ];
      store.endpoints.response = vi.fn(async () => ({
        id: liveId,
        status: 'completed',
        continuation_response_id: cursor,
        run_epoch: 1,
        last_sequence_number: 4,
        final_rev: 1,
        pending_client_calls: [call],
        recovery: {
          sequence_number: 4,
          messages: [
            {
              role: 'tool-group',
              response_id: liveId,
              tools: [{ id: call.call_id, name: call.name, arguments: '{}', status: 'pending' }],
            },
          ],
        },
      }));
      store.endpoints.createResponse = vi
        .fn()
        .mockResolvedValueOnce(sse(liveId, source === 'live' ? frames : frames.slice(0, 1)))
        .mockResolvedValueOnce(sse('r2', textFrames('r2', 'Done.')));

      try {
        store.prompt.value = 'Ping it';
        void store.send();
        await vi.waitFor(() => expect(context.executeTool).toHaveBeenCalledTimes(1));
        // Tool matching and supervisors must still identify the live response.
        expect(store.runs.value.s1.run).toMatchObject({ responseId: liveId, status: 'completed' });
        expect(
          store.runs.value.s1.messages.find((message) => message.role === 'tool-group'),
        ).toMatchObject({
          responseId: liveId,
          tools: [expect.objectContaining({ status: 'running' })],
        });
        if (source === 'snapshot')
          expect(store.endpoints.response).toHaveBeenCalledWith(liveId, expect.anything());
        store.applyResponseEvent('s1', {
          type: 'response.completed',
          response_id: 'resp_unrelated',
          response: { id: 'resp_msg_100' },
          run_epoch: 1,
          sequence_number: 5,
        });
        finish('pong');
        await vi.waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2));
        expect(store.endpoints.sessionState).toHaveBeenCalled();
        expect(requestBody(store, 1)).toMatchObject({
          previous_response_id: cursor || liveId,
          input: [{ type: 'function_call_output', call_id: 'call_1', output: 'pong' }],
        });
        await vi.waitFor(() => expect(store.runActive.value).toBe(false));
      } finally {
        store.dispose();
      }
    },
  );

  it('does not submit a captured handoff after another run replaces its owner', async () => {
    let finish!: (value: string) => void;
    const context = installDeviceTools(() => new Promise((resolve) => (finish = resolve)));
    const store = await chatStore();
    const frames = callFrames('r1', 'webmcp__ping', '{}');
    frames[3] = ['response.completed', { response: { id: 'resp_msg_42' }, final_rev: 1 }];
    store.endpoints.createResponse = vi.fn(async () => sse('r1', frames));
    try {
      store.prompt.value = 'Ping it';
      await store.send();
      await vi.waitFor(() => expect(context.executeTool).toHaveBeenCalledTimes(1));
      const previous = store.runs.value.s1;
      store.runs.value = {
        ...store.runs.value,
        s1: { ...previous, run: { ...previous.run, responseId: 'r2' } },
      };
      finish('too late');
      await vi.waitFor(() => expect(store.runActive.value).toBe(false));
      expect(store.endpoints.createResponse).toHaveBeenCalledTimes(1);
      expect(store.runs.value.s1.run.responseId).toBe('r2');
    } finally {
      store.dispose();
    }
  });

  it('runs the page tool the model called and continues with its result', async () => {
    const context = installDeviceTools(async (input) => `pong: ${input.message}`);
    const store = await chatStore();
    // The refreshed durable tail differs: continuation must prove the actual handoff.
    store.endpoints.selectedSession = vi.fn(async () => ({
      selected_session: { id: 's1', transcript_rev: 1 },
      selected_transcript: { bodies: { rev: 1, messages: [] } },
    }));
    store.endpoints.sessionState = vi.fn(async () => ({ last_response_id: 'resp_msg_1' }));
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', callFrames('r1', 'webmcp__ping', '{"message":"hi"}')))
      .mockResolvedValueOnce(sse('r2', textFrames('r2', 'Your iPhone answered.')));

    store.prompt.value = 'Is my phone there?';
    await store.send();

    expect(requestBody(store, 0).tools).toEqual([
      {
        type: 'function',
        name: 'webmcp__ping',
        description: "Check the phone is reachable Runs on the user's iPhone.",
        parameters: { type: 'object', properties: { message: { type: 'string' } } },
      },
    ]);
    await vi.waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2));
    expect(context.executeTool).toHaveBeenCalledWith(
      expect.objectContaining({ name: 'ping' }),
      { message: 'hi' },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(requestBody(store, 1)).toMatchObject({
      previous_response_id: 'r1',
      input: [{ type: 'function_call_output', call_id: 'call_1', output: 'pong: hi' }],
      tools: [expect.objectContaining({ name: 'webmcp__ping' })],
    });
    // First-party requests always carry an idempotency key.
    expect(requestBody(store, 1).client_message_id).toEqual(expect.any(String));
    expect(requestBody(store, 1).client_message_id).not.toBe(
      requestBody(store, 0).client_message_id,
    );
    await vi.waitFor(() => expect(store.runActive.value).toBe(false));
    expect(store.runs.value.s1.run).toMatchObject({ responseId: 'r2', status: 'completed' });
    expect(store.activeSession.value?.lastResponseId).toBe('r2');
  });

  it('sends tool failures back to the model', async () => {
    installDeviceTools(async () => Promise.reject(new Error('Apple Music is not authorized')));
    const store = await chatStore();
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', callFrames('r1', 'webmcp__ping', '{}')))
      .mockResolvedValueOnce(sse('r2', textFrames('r2', 'It failed.')));

    store.prompt.value = 'Ping it';
    await store.send();

    await vi.waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2));
    expect(requestBody(store, 1).input).toEqual([
      {
        type: 'function_call_output',
        call_id: 'call_1',
        output: 'Error: Apple Music is not authorized',
      },
    ]);
  });

  it('offers nothing when the conversation turned page tools off', async () => {
    installDeviceTools(async () => 'pong');
    const store = await chatStore();
    store.setWebMCPEnabled(false);
    expect(store.webMCPEnabled.value).toBe(false);
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', textFrames('r1', 'No tools needed.')));

    store.prompt.value = 'Hello';
    await store.send();

    expect(requestBody(store, 0)).not.toHaveProperty('tools');
    expect(store.webMCPEnabled.value).toBe(false);
  });

  it('keeps the chat busy while the device works, and Stop abandons the call', async () => {
    let finish: (value: string) => void = () => undefined;
    const context = installDeviceTools(() => new Promise((resolve) => (finish = resolve)));
    const store = await chatStore();
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', callFrames('r1', 'webmcp__ping', '{}')));

    store.prompt.value = 'Ping it';
    await store.send();
    await vi.waitFor(() => expect(context.executeTool).toHaveBeenCalled());
    expect(store.runActive.value).toBe(true);
    expect(store.sendBlocked.value).toBe(true);
    expect(store.canStop.value).toBe(true);
    // Progress lives in the tool block, not a transient phase line.
    expect(store.activeProjection.value?.phase).toBeUndefined();

    await store.cancel();
    expect(store.endpoints.cancelResponse).toHaveBeenCalledWith('r1');
    finish('too late');
    await vi.waitFor(() => expect(store.runActive.value).toBe(false));
    expect(store.activeProjection.value?.phase).toBeUndefined();
    expect(store.endpoints.createResponse).toHaveBeenCalledTimes(1);
  });

  it('stays busy while the finished turn loads, and Stop still abandons it', async () => {
    const context = installDeviceTools(async () => 'pong');
    const store = await chatStore();
    let loaded: () => void = () => undefined;
    const refresh = vi.fn(
      () => new Promise<Record<string, unknown>>((resolve) => (loaded = () => resolve({}))),
    );
    store.endpoints.selectedSession = refresh;
    store.endpoints.sessionState = refresh;
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', callFrames('r1', 'webmcp__ping', '{}')));

    store.prompt.value = 'Ping it';
    await store.send();
    await vi.waitFor(() => expect(context.executeTool).toHaveBeenCalled());
    await vi.waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(store.sendBlocked.value).toBe(true);
    expect(store.canStop.value).toBe(true);

    // Stop does not wait out a stalled refresh.
    await store.cancel();
    expect(store.endpoints.cancelResponse).toHaveBeenCalledWith('r1');
    await vi.waitFor(() => expect(store.runActive.value).toBe(false));
    loaded();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(store.endpoints.createResponse).toHaveBeenCalledTimes(1);
  });

  it('explicitly cancels the tool predecessor when a continuation is permanently rejected', async () => {
    installDeviceTools(async () => 'pong');
    const store = await chatStore();
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', callFrames('r1', 'webmcp__ping', '{}')))
      .mockRejectedValueOnce(new APIError('Invalid tool lineage', 400));
    store.prompt.value = 'Ping it';
    await store.send();
    await vi.waitFor(() => expect(store.endpoints.cancelResponse).toHaveBeenCalledWith('r1'));
    expect(requestBody(store, 1).previous_response_id).toBe('r1');
    expect(store.activeProjection.value?.run.status).toBe('failed');
    store.dispose();
  });

  it('Stop during retryable client-output transport failure cancels the predecessor, not a pending fake response', async () => {
    installDeviceTools(async () => 'pong');
    const store = await chatStore();
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', callFrames('r1', 'webmcp__ping', '{}')))
      .mockRejectedValue(new TypeError('Network unavailable'));
    store.prompt.value = 'Ping it';
    await store.send();
    await vi.waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2));
    expect(store.activeProjection.value?.run.status).toBe('connecting');
    expect(store.activeProjection.value?.run.continuationOf).toBe('r1');
    await store.cancel();
    expect(store.endpoints.cancelResponse).toHaveBeenCalledWith('r1');
    expect(store.activeProjection.value?.run.status).toBe('cancelled');
    store.dispose();
  });

  it('runs page calls that follow a tool-only server turn in the same response', async () => {
    // Session 5319: web_search, then read_url, then page calls, with no text
    // between provider turns. The live transcript folds every call into one
    // tool group, so only the server's pending list identifies what to run.
    const context = installDeviceTools(async (input) => `pong: ${input.message}`);
    const store = await chatStore();
    const server = { type: 'function_call', id: 'fc_call_s', call_id: 'call_s', name: 'read_url' };
    const page = {
      type: 'function_call',
      id: 'fc_call_2',
      call_id: 'call_2',
      name: 'webmcp__ping',
    };
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(
        sse('r1', [
          ['response.created', { response: { id: 'r1', status: 'in_progress' } }],
          ['response.output_item.added', { item: { ...server, arguments: '' }, output_index: 0 }],
          ['response.output_item.done', { item: { ...server, arguments: '{}' }, output_index: 0 }],
          ['response.output_item.added', { item: { ...page, arguments: '' }, output_index: 1 }],
          [
            'response.output_item.done',
            { item: { ...page, arguments: '{"message":"hi"}' }, output_index: 1 },
          ],
          [
            'response.completed',
            {
              response: {
                id: 'r1',
                status: 'completed',
                pending_client_calls: [
                  { call_id: 'call_2', name: 'webmcp__ping', arguments: '{"message":"hi"}' },
                ],
              },
              final_rev: 1,
            },
          ],
        ]),
      )
      .mockResolvedValueOnce(sse('r2', textFrames('r2', 'Your iPhone answered.')));

    store.prompt.value = 'Look it up, then ping my phone';
    await store.send();

    await vi.waitFor(() => expect(store.endpoints.createResponse).toHaveBeenCalledTimes(2));
    expect(context.executeTool).toHaveBeenCalledTimes(1);
    expect(requestBody(store, 1)).toMatchObject({
      input: [{ type: 'function_call_output', call_id: 'call_2', output: 'pong: hi' }],
    });
  });

  it('does not run calls for tools this page did not offer', async () => {
    const context = installDeviceTools(async () => 'pong');
    const store = await chatStore();
    store.setWebMCPEnabled(false);
    store.endpoints.createResponse = vi
      .fn()
      .mockResolvedValueOnce(sse('r1', callFrames('r1', 'webmcp__ping', '{}')));

    store.prompt.value = 'Ping it';
    await store.send();

    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(context.executeTool).not.toHaveBeenCalled();
    expect(store.endpoints.createResponse).toHaveBeenCalledTimes(1);
    expect(store.runActive.value).toBe(false);
  });
});
