import { describe, expect, it } from 'vitest';
import {
  initialProjection,
  reduceResponse,
  RESPONSE_EVENT_TYPES,
  ResponseProtocolError,
} from './response';
import { convertServerMessages } from './transcript';
import type { ActiveRun } from './types';

const run: ActiveRun = {
  responseId: 'r1',
  sessionId: 's1',
  epoch: 1,
  status: 'connecting',
  lastSequence: 0,
  startedRev: 0,
  reconnects: 0,
};
const event = (type: string, sequence_number: number, rest: Record<string, unknown> = {}) => ({
  type,
  response_id: 'r1',
  run_epoch: 1,
  sequence_number,
  ...rest,
});

describe('response projection', () => {
  it('inventories every server event handled by the reducer', () => {
    expect(RESPONSE_EVENT_TYPES).toEqual(
      expect.arrayContaining([
        'response.created',
        'response.output_text.delta',
        'response.output_item.added',
        'response.function_call_arguments.delta',
        'response.tool_exec.start',
        'response.tool_exec.progress',
        'response.tool_exec.end',
        'response.guardian.review',
        'response.compaction',
        'response.model_swap.progress',
        'response.model_switch',
        'response.steering',
        'response.ask_user.prompt',
        'response.approval.prompt',
        'response.file_change',
        'response.completed',
        'response.cancelled',
        'response.failed',
        'response.stream_error',
      ]),
    );
    expect(new Set(RESPONSE_EVENT_TYPES).size).toBe(RESPONSE_EVENT_TYPES.length);
  });

  it('accepts early absolute subagent progress, rejects stale seq, and folds terminal state', () => {
    let projection = reduceResponse(
      initialProjection(run),
      event('response.tool_exec.progress', 1, {
        call_id: 'spawn-early',
        tool_name: 'spawn_agent',
        seq: 2,
        state: 'running',
        phase: 'running_tools',
        calls_started: 12,
        calls_active: 2,
        current_tool: 'shell',
      }),
    );
    expect(projection.messages[0].tools?.[0].subagentProgress).toMatchObject({
      seq: 2,
      callsStarted: 12,
      callsActive: 2,
      currentTool: 'shell',
    });
    projection = reduceResponse(
      projection,
      event('response.tool_exec.progress', 2, {
        call_id: 'spawn-early',
        seq: 1,
        state: 'running',
        calls_started: 99,
        calls_active: 99,
      }),
    );
    expect(projection.messages[0].tools?.[0].subagentProgress?.callsStarted).toBe(12);
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 3, { call_id: 'spawn-early', success: true }),
    );
    expect(projection.messages[0].tools?.[0].subagentProgress).toMatchObject({
      state: 'completed',
      callsStarted: 12,
      callsActive: 0,
    });
  });

  it('tracks authoritative tool timing without resetting the original start', () => {
    let projection = reduceResponse(
      initialProjection(run),
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'spawn-1', name: 'spawn_agent' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.start', 2, { call_id: 'spawn-1', started_at: 10_000 }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.start', 3, { call_id: 'spawn-1', started_at: 20_000 }),
    );
    expect(projection.messages[0].tools?.[0].startedAt).toBe(10_000);

    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 4, {
        call_id: 'spawn-1',
        success: true,
        started_at: 10_000,
        ended_at: 22_345,
        duration_ms: 12_345,
      }),
    );
    expect(projection.messages[0].tools?.[0]).toMatchObject({
      status: 'done',
      startedAt: 10_000,
      endedAt: 22_345,
      durationMs: 12_345,
    });
  });

  it('adopts the server epoch from the initial response.created event', () => {
    const projection = reduceResponse(
      initialProjection(run),
      event('response.created', 1, { run_epoch: 1788084563000000 }),
    );

    expect(projection.run).toMatchObject({
      responseId: 'r1',
      epoch: 1788084563000000,
      lastSequence: 1,
      status: 'streaming',
    });
    expect(() =>
      reduceResponse(projection, event('response.output_text.delta', 2, { run_epoch: 1 })),
    ).toThrow(ResponseProtocolError);
  });

  it('does not project the compaction resume handoff below its transcript boundary', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.phase', 1, { text: 'Compacting: summarize history' }),
    );
    expect(projection.phase).toBe('Compacting: summarize history');

    projection = reduceResponse(
      projection,
      event('response.compaction', 2, { compaction_seq: 41, compaction_count: 3 }),
    );
    expect(projection.messages.at(-1)).toMatchObject({
      role: 'compaction-boundary',
      content: 'Context compacted',
      compactionSeq: 41,
      compactionCount: 3,
    });
    expect(projection.phase).toBeUndefined();

    projection = reduceResponse(
      projection,
      event('response.phase', 3, { text: 'Compacting: resume task' }),
    );
    expect(projection.phase).toBeUndefined();
  });

  it('projects an authoritative model boundary between tool groups', () => {
    let projection = reduceResponse(
      initialProjection(run),
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'before', name: 'shell' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 2, { call_id: 'before', success: true }),
    );
    projection = reduceResponse(
      projection,
      event('response.model_switch', 3, {
        boundary_id: 'r1:model-switch:1',
        from_provider: 'chatgpt',
        from_model: 'gpt-5.6-luna-high',
        from_reasoning_effort: 'high',
        to_provider: 'chatgpt',
        to_model: 'gpt-5.6-sol-high',
        to_reasoning_effort: 'high',
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 4, {
        item: { type: 'function_call', call_id: 'after', name: 'read_file' },
      }),
    );

    expect(projection.messages.map((message) => message.role)).toEqual([
      'tool-group',
      'model-swap',
      'tool-group',
    ]);
    expect(projection.messages[1]).toMatchObject({
      id: 'r1:model-switch:1',
      eventSequence: 3,
      fromModel: 'gpt-5.6-luna-high',
      toModel: 'gpt-5.6-sol-high',
    });
  });

  it('rejects model boundaries without both authoritative endpoints', () => {
    expect(() =>
      reduceResponse(
        initialProjection(run),
        event('response.model_switch', 1, {
          boundary_id: 'r1:model-switch:1',
          to_model: 'gpt-5.6-sol-high',
        }),
      ),
    ).toThrow(ResponseProtocolError);
  });

  it('does not project terminal model-swap progress below the chronological transcript', () => {
    let projection = reduceResponse(
      initialProjection(run),
      event('response.model_swap.progress', 1, {
        stage: 'naive_start',
        message:
          'Switching model: chatgpt:gpt-6-astra → chatgpt:gpt-5.6-sol; trying existing context…',
        previous_provider: 'chatgpt',
        previous_model: 'gpt-6-astra',
        previous_effort: 'high',
        target_provider: 'chatgpt',
        target_model: 'gpt-5.6-sol',
        target_effort: 'high',
      }),
    );
    expect(projection.modelSwap).toMatchObject({
      content:
        'Switching model: chatgpt:gpt-6-astra → chatgpt:gpt-5.6-sol; trying existing context…',
      fromProvider: 'chatgpt',
      fromModel: 'gpt-6-astra',
      toProvider: 'chatgpt',
      toModel: 'gpt-5.6-sol',
      swapStatus: 'started',
    });

    projection = reduceResponse(
      projection,
      event('response.output_item.added', 2, {
        item: { type: 'function_call', call_id: 'after-switch', name: 'read_file' },
      }),
    );
    expect(projection.modelSwap).toBeUndefined();

    projection = reduceResponse(
      projection,
      event('response.model_swap.progress', 3, {
        stage: 'handover_done',
        message: 'Handover ready…',
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.model_swap.progress', 4, {
        stage: 'complete',
        message: 'Continuing on the new model.',
      }),
    );
    expect(projection.modelSwap).toBeUndefined();

    projection = reduceResponse(
      projection,
      event('response.model_swap.progress', 5, {
        stage: 'handover_start',
        message: 'Preparing handover…',
      }),
    );
    projection = reduceResponse(projection, event('response.attempt.discard', 6));
    expect(projection.modelSwap).toBeUndefined();

    projection = reduceResponse(
      projection,
      event('response.model_swap.progress', 7, {
        stage: 'handover_start',
        message: 'Preparing handover…',
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_text.new_segment', 8, { assistant_segment_ordinal: 1 }),
    );
    expect(projection.modelSwap).toBeUndefined();

    projection = reduceResponse(
      projection,
      event('response.model_swap.progress', 9, {
        stage: 'handover_start',
        message: 'Preparing handover…',
      }),
    );
    projection = reduceResponse(projection, event('response.completed', 10));
    expect(projection.modelSwap).toBeUndefined();
  });

  it('keeps structured model identities across progress stages', () => {
    let projection = reduceResponse(
      initialProjection(run),
      event('response.model_swap.progress', 1, {
        stage: 'naive_start',
        message: 'Trying existing context…',
        previous_provider: 'chatgpt',
        previous_model: 'gpt-6-astra',
        target_provider: 'chatgpt',
        target_model: 'gpt-5.6-sol',
      }),
    );

    projection = reduceResponse(
      projection,
      event('response.model_swap.progress', 2, {
        stage: 'handover_start',
        message: 'Preparing handover…',
        previous_provider: '',
        previous_model: '',
        previous_effort: '',
        target_provider: '',
        target_model: '',
        target_effort: '',
      }),
    );

    expect(projection.modelSwap).toMatchObject({
      fromProvider: 'chatgpt',
      fromModel: 'gpt-6-astra',
      toProvider: 'chatgpt',
      toModel: 'gpt-5.6-sol',
      swapStatus: 'started',
      swapStrategy: 'handover',
    });
  });

  it('projects steering image attachments before transcript reload', () => {
    const projection = reduceResponse(
      initialProjection(run),
      event('response.steering', 1, {
        text: 'inspect this image',
        client_message_id: 'steering-image-1',
        attachments: [
          {
            name: 'image 1',
            type: 'image/png',
            url: '/ui/images/steering.png',
            width: 320,
            height: 180,
          },
        ],
      }),
    );

    expect(projection.messages).toEqual([
      expect.objectContaining({
        role: 'user',
        content: 'inspect this image',
        attachments: [
          {
            name: 'image 1',
            type: 'image/png',
            url: '/ui/images/steering.png',
            width: 320,
            height: 180,
          },
        ],
      }),
    ]);
  });

  it('projects image-only steering before transcript reload', () => {
    const projection = reduceResponse(
      initialProjection(run),
      event('response.steering', 1, {
        text: '',
        client_message_id: 'steering-image-only-1',
        attachments: [
          {
            name: 'image 1',
            type: 'image/png',
            url: '/ui/images/image-only.png',
          },
        ],
      }),
    );

    expect(projection.messages[0]).toMatchObject({
      role: 'user',
      content: '',
      attachments: [
        {
          name: 'image 1',
          type: 'image/png',
          url: '/ui/images/image-only.png',
        },
      ],
    });
  });

  it('streams stable assistant segments without replacing earlier content', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(projection, event('response.created', 1));
    projection = reduceResponse(
      projection,
      event('response.output_text.delta', 2, { delta: 'Hello', assistant_segment_ordinal: 0 }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_text.new_segment', 3, { assistant_segment_ordinal: 1 }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_text.delta', 4, { delta: 'after tool', assistant_segment_ordinal: 1 }),
    );
    expect(projection.messages.map((message) => message.content)).toEqual(['Hello', 'after tool']);
    expect(projection.messages.map((message) => message.id)).toEqual([
      'r1:assistant:0',
      'r1:assistant:1',
    ]);
  });

  it('assembles tool calls, arguments, guardian review and completion', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'c1', name: 'shell' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.function_call_arguments.delta', 2, { call_id: 'c1', delta: '{"command":' }),
    );
    projection = reduceResponse(
      projection,
      event('response.function_call_arguments.delta', 3, { call_id: 'c1', delta: '"pwd"}' }),
    );
    projection = reduceResponse(
      projection,
      event('response.guardian.review', 4, { call_id: 'c1', outcome: 'approved', message: 'safe' }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 5, { call_id: 'c1', output: '/tmp' }),
    );
    expect(projection.messages[0].tools?.[0]).toMatchObject({
      name: 'shell',
      arguments: '{"command":"pwd"}',
      status: 'done',
      result: '/tmp',
      guardianReviews: [{ outcome: 'approved', message: 'safe' }],
    });
  });

  it('attaches live ask_user answers to the originating tool call', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.ask_user.prompt', 1, {
        call_id: 'ask-1',
        questions: [{ question: 'Choose?', options: [] }],
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 2, {
        call_id: 'ask-1',
        tool_name: 'ask_user',
        success: true,
        ask_user_summary: 'Frontend: Vendor xterm.js',
      }),
    );

    expect(projection.messages).toHaveLength(1);
    expect(projection.messages[0].tools?.[0]).toMatchObject({
      id: 'ask-1',
      name: 'ask_user',
      status: 'done',
      arguments: '{"questions":[{"question":"Choose?","options":[]}]}',
      argumentsFinalized: true,
      askUserAnswer: 'Frontend: Vendor xterm.js',
    });
  });

  it('keeps interleaved parallel tool arguments attached to stable call and item identities', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 1, {
        item: { id: 'item-1', type: 'function_call', call_id: 'call-1', name: 'shell' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 2, {
        item: { id: 'item-2', type: 'function_call', call_id: 'call-2', name: 'read_file' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.function_call_arguments.delta', 3, {
        item_id: 'item-1',
        delta: '{"command":',
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.function_call_arguments.delta', 4, {
        call_id: 'call-2',
        delta: '{"path":"README"}',
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.function_call_arguments.delta', 5, {
        call_id: 'call-1',
        delta: '"pwd"}',
      }),
    );

    expect(projection.messages[0].tools).toEqual([
      expect.objectContaining({
        id: 'call-1',
        itemId: 'item-1',
        arguments: '{"command":"pwd"}',
      }),
      expect.objectContaining({
        id: 'call-2',
        itemId: 'item-2',
        arguments: '{"path":"README"}',
      }),
    ]);
  });

  it('rejects an unidentified argument delta when parallel tools are running', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 1, {
        item: { id: 'item-1', type: 'function_call', call_id: 'call-1', name: 'shell' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 2, {
        item: { id: 'item-2', type: 'function_call', call_id: 'call-2', name: 'read_file' },
      }),
    );

    expect(() =>
      reduceResponse(
        projection,
        event('response.function_call_arguments.delta', 3, { delta: '{}' }),
      ),
    ).toThrow(ResponseProtocolError);
  });

  it('keeps sequential tool activity in one group until a transcript boundary', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'c1', name: 'shell' },
      }),
    );
    const groupID = projection.messages[0].id;
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 2, { call_id: 'c1', output: '/tmp' }),
    );
    expect(projection.messages[0]).toMatchObject({ status: 'done', toolGroupClosed: false });

    projection = reduceResponse(
      projection,
      event('response.output_item.added', 3, {
        item: { type: 'function_call', call_id: 'c2', name: 'read_file' },
      }),
    );
    expect(projection.messages).toHaveLength(1);
    expect(projection.messages[0]).toMatchObject({ id: groupID, status: 'running' });
    expect(projection.messages[0].tools?.map((tool) => tool.id)).toEqual(['c1', 'c2']);

    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 4, { call_id: 'c2', output: 'contents' }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_text.delta', 5, {
        delta: 'between batches',
        assistant_segment_ordinal: 0,
      }),
    );
    expect(projection.messages[0]).toMatchObject({ status: 'done', toolGroupClosed: true });
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 6, {
        item: { type: 'function_call', call_id: 'c3', name: 'grep' },
      }),
    );
    expect(projection.messages.map((message) => message.role)).toEqual([
      'tool-group',
      'assistant',
      'tool-group',
    ]);
    expect(projection.messages[2].tools?.map((tool) => tool.id)).toEqual(['c3']);
  });

  it('does not reopen a closed tool-group that remains at the transcript tail', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'c1', name: 'shell' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 2, { call_id: 'c1', output: '/tmp' }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_text.new_segment', 3, { assistant_segment_ordinal: 1 }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 4, {
        item: { type: 'function_call', call_id: 'c2', name: 'read_file' },
      }),
    );

    expect(projection.messages.map((message) => message.role)).toEqual([
      'tool-group',
      'assistant',
      'tool-group',
    ]);
    expect(projection.messages[0]).toMatchObject({
      role: 'tool-group',
      toolGroupClosed: true,
    });
    expect(projection.messages[2].tools?.map((tool) => tool.id)).toEqual(['c2']);
  });

  it('closes recovery-seeded membership without fabricating tool completion', () => {
    let projection: ReturnType<typeof initialProjection> = {
      ...initialProjection(run),
      messages: [
        {
          id: 'recovered-tools',
          role: 'tool-group' as const,
          content: '',
          created: 1,
          responseId: 'r1',
          status: 'done',
          tools: [{ id: 'c1', name: 'shell', status: 'running' as const }],
        },
      ],
    };
    projection = reduceResponse(
      projection,
      event('response.output_text.delta', 1, {
        delta: 'after recovery',
        assistant_segment_ordinal: 1,
      }),
    );

    expect(projection.messages[0]).toMatchObject({
      toolGroupClosed: true,
      tools: [{ id: 'c1', status: 'running' }],
    });
  });

  it('preserves recovery provenance when a sibling tool joins before closure', () => {
    let projection: ReturnType<typeof initialProjection> = {
      ...initialProjection(run),
      messages: [
        {
          id: 'recovered-tools',
          role: 'tool-group' as const,
          content: '',
          created: 1,
          responseId: 'r1',
          status: 'done',
          tools: [{ id: 'c1', name: 'shell', status: 'running' as const }],
        },
      ],
    };
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'c2', name: 'read_file' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 2, { call_id: 'c2', output: 'contents' }),
    );
    projection = reduceResponse(projection, event('response.completed', 3));

    expect(projection.messages).toHaveLength(1);
    expect(projection.messages[0]).toMatchObject({
      toolGroupClosed: true,
      tools: [
        { id: 'c1', status: 'running' },
        { id: 'c2', status: 'done' },
      ],
    });
  });

  it('matches durable grouping for sequential completed tool calls', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'c1', name: 'shell', arguments: '{}' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 2, { call_id: 'c1', output: '/tmp' }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 3, {
        item: { type: 'function_call', call_id: 'c2', name: 'read_file', arguments: '{}' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 4, { call_id: 'c2', output: 'contents' }),
    );

    const durable = convertServerMessages([
      {
        id: 1,
        role: 'assistant',
        response_id: 'r1',
        parts: [{ type: 'function_call', call_id: 'c1', name: 'shell', arguments: '{}' }],
      },
      {
        id: 2,
        role: 'tool',
        response_id: 'r1',
        parts: [{ type: 'tool_result', call_id: 'c1', name: 'shell', output: '/tmp' }],
      },
      {
        id: 3,
        role: 'assistant',
        response_id: 'r1',
        parts: [{ type: 'function_call', call_id: 'c2', name: 'read_file', arguments: '{}' }],
      },
      {
        id: 4,
        role: 'tool',
        response_id: 'r1',
        parts: [{ type: 'tool_result', call_id: 'c2', name: 'read_file', output: 'contents' }],
      },
    ]);
    const shape = (messages: typeof projection.messages) =>
      messages.map((message) => ({
        role: message.role,
        tools: message.tools?.map((tool) => ({
          id: tool.id,
          name: tool.name,
          status: tool.status,
        })),
      }));
    expect(shape(projection.messages)).toEqual(shape(durable));
  });

  it('ignores duplicates and rejects sequence gaps or stale epochs for snapshot recovery', () => {
    const first = reduceResponse(
      initialProjection(run),
      event('response.output_text.delta', 1, { delta: 'one' }),
    );
    expect(
      reduceResponse(first, event('response.output_text.delta', 1, { delta: 'duplicate' })),
    ).toBe(first);
    expect(() =>
      reduceResponse(first, event('response.output_text.delta', 3, { delta: 'gap' })),
    ).toThrow(ResponseProtocolError);
    expect(() =>
      reduceResponse(first, {
        ...event('response.output_text.delta', 2, { delta: 'old' }),
        run_epoch: 0,
      }),
    ).toThrow(ResponseProtocolError);
  });

  it('rejects incomplete ownership envelopes instead of accepting ambiguous events', () => {
    const projection = initialProjection(run);
    expect(() =>
      reduceResponse(projection, {
        type: 'response.output_text.delta',
        run_epoch: 1,
        sequence_number: 1,
        delta: 'missing owner',
      }),
    ).toThrow(ResponseProtocolError);
    expect(() =>
      reduceResponse(projection, {
        type: 'response.output_text.delta',
        response_id: 'r1',
        sequence_number: 1,
        delta: 'missing epoch',
      }),
    ).toThrow(ResponseProtocolError);
  });

  it('ignores empty and finalized argument deltas and flushes unmatched guardian reviews', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(projection, event('response.output_text.delta', 1, { delta: '' }));
    expect(projection.messages).toEqual([]);
    projection = reduceResponse(
      projection,
      event('response.output_item.added', 2, {
        item: { type: 'function_call', call_id: 'c1', name: 'shell', arguments: '{"done":true}' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.output_item.done', 3, {
        item: { type: 'function_call', call_id: 'c1', arguments: '{"done":true}' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.function_call_arguments.delta', 4, { call_id: 'c1', delta: 'ignored' }),
    );
    projection = reduceResponse(
      projection,
      event('response.guardian.review', 5, {
        call_id: 'missing',
        outcome: 'warning',
        message: 'manual review',
      }),
    );
    projection = reduceResponse(projection, event('response.completed', 6));
    expect(projection.messages[0].tools?.[0].arguments).toBe('{"done":true}');
    expect(projection.messages.at(-1)).toMatchObject({
      role: 'guardian-notice',
      content: 'manual review (unmatched tool call missing)',
    });
  });

  it('projects prompts, plans, file changes and terminal states', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.ask_user.prompt', 1, {
        call_id: 'ask1',
        questions: [{ question: 'Choose?', options: [] }],
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.approval.prompt', 2, { approval_id: 'a1', path: '/tmp' }),
    );
    projection = reduceResponse(projection, event('response.file_change', 3));
    projection = reduceResponse(
      projection,
      event('response.completed', 4, { usage: { total_tokens: 12 } }),
    );
    expect(projection.askUser?.callId).toBe('ask1');
    expect(projection.approval?.id).toBe('a1');
    expect(projection.approval?.scope).toBe('local');
    expect(projection.fileChangeRevision).toBe(1);
    expect(projection.run.status).toBe('completed');
    expect(projection.usage?.total_tokens).toBe(12);
  });

  it('projects typed media from live tool completion events', () => {
    let projection = reduceResponse(
      initialProjection(run),
      event('response.output_item.added', 1, {
        item: { type: 'function_call', call_id: 'media-1', name: 'show_media' },
      }),
    );
    projection = reduceResponse(
      projection,
      event('response.tool_exec.end', 2, {
        call_id: 'media-1',
        media: [
          {
            reference: '0123456789abcdef0123456789abcdef',
            url: '/ui/media/hash.mp4',
            type: 'video/mp4',
            name: 'demo.mp4',
            caption: 'Demo',
          },
        ],
      }),
    );
    expect(projection.messages[0].tools?.[0].media).toEqual([
      {
        reference: '0123456789abcdef0123456789abcdef',
        url: '/ui/media/hash.mp4',
        type: 'video/mp4',
        name: 'demo.mp4',
        caption: 'Demo',
      },
    ]);
  });
});
