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
        'response.tool_exec.end',
        'response.guardian.review',
        'response.compaction',
        'response.model_swap.progress',
        'response.interjection',
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

  it('does not project the compaction resume handoff below its transcript boundary', () => {
    let projection = initialProjection(run);
    projection = reduceResponse(
      projection,
      event('response.phase', 1, { text: 'Compacting: summarize history' }),
    );
    expect(projection.phase).toBe('Compacting: summarize history');

    projection = reduceResponse(projection, event('response.compaction', 2));
    expect(projection.messages.at(-1)).toMatchObject({
      role: 'compaction-boundary',
      content: 'Context compacted',
    });
    expect(projection.phase).toBeUndefined();

    projection = reduceResponse(
      projection,
      event('response.phase', 3, { text: 'Compacting: resume task' }),
    );
    expect(projection.phase).toBeUndefined();
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
    expect(projection.fileChangeRevision).toBe(1);
    expect(projection.run.status).toBe('completed');
    expect(projection.usage?.total_tokens).toBe(12);
  });
});
