import { describe, expect, it, vi } from 'vitest';
import {
  convertServerMessages,
  indexTranscriptTurns,
  mergeDurableProjection,
  sanitizeSession,
  windowTranscript,
} from './transcript';
import type { Message } from './types';

describe('transcript domain', () => {
  it('converts text, measured media, tools and compaction rows', () => {
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const messages = convertServerMessages([
      {
        id: 1,
        sequence: 0,
        role: 'user',
        client_message_id: 'c1',
        interrupt_state: 'queue',
        parts: [
          { type: 'text', text: 'hello' },
          {
            type: 'image',
            image_url: '/image.png',
            mime_type: 'image/png',
            width: 800,
            height: 400,
          },
        ],
      },
      {
        id: 2,
        sequence: 1,
        role: 'assistant',
        response_id: 'r1',
        assistant_segment_ordinal: 0,
        parts: [{ type: 'text', text: 'answer' }],
      },
      {
        id: 3,
        sequence: 2,
        role: 'tool',
        parts: [{ type: 'function_call', call_id: 't1', name: 'read_file', arguments: '{}' }],
      },
      { id: 4, sequence: 3, role: 'compaction', parts: [{ type: 'text', text: 'summary' }] },
    ]);
    expect(messages[0]).toMatchObject({
      role: 'user',
      content: 'hello',
      clientMessageId: 'c1',
      interruptState: 'queue',
      attachments: [{ width: 800, height: 400 }],
    });
    expect(messages[1]).toMatchObject({ role: 'assistant', responseId: 'r1', content: 'answer' });
    expect(messages[2].tools?.[0]).toMatchObject({ id: 't1', name: 'read_file' });
    expect(messages[3]).toMatchObject({ role: 'compaction-boundary', rawContent: 'summary' });
  });

  it('keeps durable spawn_agent calls running until their result arrives', () => {
    const call = {
      id: 1,
      sequence: 0,
      role: 'assistant',
      parts: [
        {
          type: 'tool_call',
          tool_call_id: 'spawn-1',
          tool_name: 'spawn_agent',
          tool_arguments: '{"agent_name":"reviewer"}',
        },
      ],
    };

    const pending = convertServerMessages([call]);
    expect(pending[0].tools?.[0]).toMatchObject({
      id: 'spawn-1',
      name: 'spawn_agent',
      status: 'running',
    });

    const completed = convertServerMessages([
      call,
      {
        id: 2,
        sequence: 1,
        role: 'tool',
        parts: [
          {
            type: 'tool_result',
            tool_call_id: 'spawn-1',
            tool_name: 'spawn_agent',
            spawn_agent: {
              agent_name: 'reviewer',
              output: 'No issues found.',
              duration_ms: 250,
            },
          },
        ],
      },
    ]);
    expect(completed[0].tools?.[0]).toMatchObject({
      id: 'spawn-1',
      status: 'done',
      resultStatus: 'success',
      subagent: { agentName: 'reviewer', output: 'No issues found.', durationMs: 250 },
    });
  });

  it('sanitizes server session defaults and seconds timestamps', () => {
    const session = sanitizeSession({
      id: 's1',
      created_at: 1_700_000_000,
      title: '',
      pinned: 1,
      file_change_summary: { file_count: 2, adds: 7, dels: 3, git: true },
      messages: [],
    });
    expect(session).toMatchObject({
      id: 's1',
      title: 'New chat',
      mode: 'chat',
      origin: 'web',
      pinned: true,
      created: 1_700_000_000_000,
      fileChangeSummary: { fileCount: 2, additions: 7, deletions: 3, git: true },
    });
  });

  it('prefers server-resolved titles while retaining editable generated metadata', () => {
    const session = sanitizeSession({
      id: 's1',
      name: 'Manual name',
      short_title: 'Manual name',
      long_title: 'Preferred detail',
      generated_short_title: 'Older generated title',
      generated_long_title: 'Older generated detail',
      messages: [],
    });
    expect(session).toMatchObject({
      name: 'Manual name',
      title: 'Manual name',
      longTitle: 'Preferred detail',
      generatedShortTitle: 'Older generated title',
      generatedLongTitle: 'Older generated detail',
    });
  });

  it('windows old turns behind a stable gap and always keeps the tail', () => {
    const messages = Array.from({ length: 240 }, (_, index): Message => ({
      id: String(index),
      role: index % 3 === 0 ? 'user' : 'assistant',
      content: String(index),
      created: index,
    }));
    const runs = windowTranscript(messages, 3, true);
    expect(runs[0]).toMatchObject({ type: 'gap' });
    expect(runs[1].messages?.at(-1)?.id).toBe('239');
    expect(runs[1].messages?.filter((message) => message.role === 'user').length).toBe(3);
  });

  it('indexes assistant response text in one transcript pass', () => {
    const messages = Array.from({ length: 1_000 }, (_, index): Message => ({
      id: String(index),
      role: index % 5 === 0 ? 'user' : 'assistant',
      content: String(index),
      created: index,
    }));
    const read = vi.fn((message: Message) => message.content);
    const contexts = indexTranscriptTurns(messages, read);
    expect(read).toHaveBeenCalledTimes(messages.length);
    expect(contexts.size).toBe(messages.length);
    expect(contexts.get(messages[1])?.copyTarget).toBe(false);
    expect(contexts.get(messages[4])).toMatchObject({
      responseText: ['1', '2', '3', '4'].join('\n\n'),
      copyTarget: true,
    });
  });

  it('excludes prompts, tools, and older responses from copied response text', () => {
    const messages: Message[] = [
      { id: 'u1', role: 'user', content: 'Question', created: 1 },
      {
        id: 'a1',
        role: 'assistant',
        content: 'Discarded response',
        created: 2,
        responseId: 'response-1',
      },
      {
        id: 't1',
        role: 'tool-group',
        content: 'shell\nran noisy command',
        created: 3,
        responseId: 'response-2',
      },
      {
        id: 'a2',
        role: 'assistant',
        content: 'First segment',
        created: 4,
        responseId: 'response-2',
      },
      {
        id: 'a3',
        role: 'assistant',
        content: 'Final segment',
        created: 5,
        responseId: 'response-2',
      },
    ];

    const contexts = indexTranscriptTurns(messages, (message) => message.content);

    expect(contexts.get(messages[4])).toMatchObject({
      responseText: 'First segment\n\nFinal segment',
      copyTarget: true,
    });
  });

  it('normalizes camel-case response identity from recovery payloads', () => {
    const messages = convertServerMessages([
      {
        id: 1,
        sequence: 0,
        role: 'assistant',
        responseId: 'r1',
        assistantSegmentOrdinal: 2,
        segment_start_sequence: 7,
        segment_end_sequence: 9,
        parts: [{ type: 'text', text: 'answer' }],
      },
    ]);
    expect(messages[0]).toMatchObject({
      responseId: 'r1',
      assistantSegmentOrdinal: 2,
      segmentStartSequence: 7,
      segmentEndSequence: 9,
      content: 'answer',
    });
  });

  it('hands projected rows off atomically to matching durable identities', () => {
    const projected: Message[] = [
      { id: 'p1', role: 'user', content: 'question', created: 1, clientMessageId: 'c1' },
      {
        id: 'p2',
        role: 'assistant',
        content: 'answer',
        created: 2,
        responseId: 'r1',
        assistantSegmentOrdinal: 0,
      },
    ];
    const durable: Message[] = projected.map((message, index) => ({
      ...message,
      id: `d${index}`,
      durableRowId: index + 1,
    }));
    expect(mergeDurableProjection(durable, projected)).toEqual(durable);
  });

  it('keeps the newest assistant segment coverage during partial durable handoff', () => {
    const durable: Message[] = [
      {
        id: 'durable-answer',
        role: 'assistant',
        content: 'partial',
        created: 1,
        durableRowId: 8,
        responseId: 'r1',
        assistantSegmentOrdinal: 0,
        segmentStartSequence: 2,
        segmentEndSequence: 5,
      },
    ];
    const projected: Message[] = [
      {
        id: 'projected-answer',
        role: 'assistant',
        content: 'partial and recovered suffix',
        created: 2,
        responseId: 'r1',
        assistantSegmentOrdinal: 0,
        segmentStartSequence: 2,
        segmentEndSequence: 12,
      },
    ];

    expect(mergeDurableProjection(durable, projected)).toEqual([
      expect.objectContaining({
        id: 'durable-answer',
        durableRowId: 8,
        content: 'partial and recovered suffix',
        segmentEndSequence: 12,
      }),
    ]);
  });

  it('adopts an exact durable compaction at its ordered live stream position', () => {
    const durable: Message[] = [
      { id: 'question', role: 'user', content: 'question', created: 1, serverSeq: 1 },
      {
        id: 'historical-compaction',
        role: 'compaction',
        content: 'Context compacted',
        rawContent: 'older summary',
        created: 2,
        serverSeq: 2,
      },
      {
        id: 'durable-before-tools',
        role: 'tool-group',
        content: '',
        created: 3,
        responseId: 'r1',
        tools: [{ id: 'already-before', name: 'grep', status: 'done' }],
      },
      {
        id: 'active-compaction',
        role: 'compaction',
        content: 'Context compacted',
        rawContent: 'current summary',
        created: 3,
        serverSeq: 10,
      },
      {
        id: 'durable-after-tools',
        role: 'tool-group',
        content: '',
        created: 6,
        responseId: 'r1',
        tools: [{ id: 'after', name: 'read_file', status: 'done' }],
      },
    ];
    const projected: Message[] = [
      {
        id: 'before-tools',
        role: 'tool-group',
        content: '',
        created: 4,
        responseId: 'r1',
        tools: [
          { id: 'already-before', name: 'grep', status: 'running' },
          { id: 'before', name: 'shell', status: 'done' },
        ],
      },
      {
        id: 'live-compaction',
        role: 'compaction-boundary',
        content: 'Context compacted',
        created: 5,
        responseId: 'r1',
        compactionSeq: 10,
      },
      {
        id: 'after-tools',
        role: 'tool-group',
        content: '',
        created: 6,
        responseId: 'r1',
        tools: [{ id: 'after', name: 'read_file', status: 'running' }],
      },
    ];

    const merged = mergeDurableProjection(durable, projected);
    expect(
      merged.map((message) =>
        message.role === 'tool-group'
          ? message.tools?.map((tool) => tool.id).join(',')
          : message.id,
      ),
    ).toEqual([
      'question',
      'historical-compaction',
      'already-before,before',
      'active-compaction',
      'after',
    ]);
    expect(merged[2].tools?.[0]).toMatchObject({ id: 'already-before', status: 'done' });
    expect(merged.filter((message) => message.id === 'active-compaction')).toHaveLength(1);
    expect(merged[3]).toMatchObject({ rawContent: 'current summary', serverSeq: 10 });
    expect(merged[4].tools?.[0]).toMatchObject({ id: 'after', status: 'done' });
  });

  it('keeps multiple adopted compactions in live stream order', () => {
    const toolGroup = (id: string, created: number): Message => ({
      id,
      role: 'tool-group',
      content: '',
      created,
      responseId: 'r1',
      tools: [{ id, name: 'shell', status: 'done' }],
    });
    const durable: Message[] = [
      {
        id: 'first-boundary',
        role: 'compaction',
        content: 'Context compacted',
        created: 2,
        serverSeq: 10,
      },
      {
        id: 'second-boundary',
        role: 'compaction',
        content: 'Context compacted',
        created: 4,
        serverSeq: 20,
      },
    ];
    const projected: Message[] = [
      toolGroup('first-tools', 1),
      {
        id: 'first-live',
        role: 'compaction-boundary',
        content: 'Context compacted',
        created: 2,
        compactionSeq: 10,
      },
      toolGroup('second-tools', 3),
      {
        id: 'second-live',
        role: 'compaction-boundary',
        content: 'Context compacted',
        created: 4,
        compactionSeq: 20,
      },
      toolGroup('third-tools', 5),
    ];

    expect(mergeDurableProjection(durable, projected).map((message) => message.id)).toEqual([
      'first-tools',
      'first-boundary',
      'second-tools',
      'second-boundary',
      'third-tools',
    ]);
  });

  it('adopts a recovered server-row compaction by its durable sequence', () => {
    const raw = {
      id: 10,
      sequence: 10,
      role: 'user',
      parts: [{ type: 'text', text: '[Context Compaction]\nsummary' }],
    };
    const durableBoundary = convertServerMessages([raw])[0];
    const recoveredBoundary = convertServerMessages([raw])[0];

    const merged = mergeDurableProjection([durableBoundary], [recoveredBoundary]);
    expect(merged).toHaveLength(1);
    expect(merged[0]).toBe(durableBoundary);
  });

  it('does not reintroduce durable tools while coalescing adjacent pending groups', () => {
    const durable: Message[] = [
      {
        id: 'durable-tools',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [{ id: 'durable-call', name: 'shell', status: 'done' }],
      },
      {
        id: 'durable-assistant',
        role: 'assistant',
        content: 'covered boundary',
        created: 2,
        responseId: 'r1',
        assistantSegmentOrdinal: 0,
        segmentEndSequence: 5,
      },
    ];
    const projected: Message[] = [
      {
        id: 'pending-first',
        role: 'tool-group',
        content: '',
        created: 3,
        responseId: 'r1',
        tools: [{ id: 'pending-first', name: 'grep', status: 'done' }],
      },
      {
        id: 'covered-assistant',
        role: 'assistant',
        content: 'covered boundary',
        created: 4,
        responseId: 'r1',
        assistantSegmentOrdinal: 0,
        segmentEndSequence: 5,
      },
      {
        id: 'pending-second',
        role: 'tool-group',
        content: '',
        created: 5,
        responseId: 'r1',
        tools: [
          { id: 'durable-call', name: 'shell', status: 'running' },
          { id: 'pending-second', name: 'read_file', status: 'running' },
        ],
      },
    ];

    const merged = mergeDurableProjection(durable, projected);
    expect(merged.flatMap((message) => message.tools || []).map((tool) => tool.id)).toEqual([
      'durable-call',
      'pending-first',
      'pending-second',
    ]);
  });

  it('does not move an unrelated durable compaction into a pending live boundary', () => {
    const durable: Message[] = [
      { id: 'question', role: 'user', content: 'question', created: 1 },
      {
        id: 'historical-compaction',
        role: 'compaction',
        content: 'Context compacted',
        created: 2,
        serverSeq: 2,
      },
    ];
    const projected: Message[] = [
      {
        id: 'pending-compaction',
        role: 'compaction-boundary',
        content: 'Context compacted',
        created: 3,
        responseId: 'r1',
        compactionSeq: 10,
      },
    ];

    expect(mergeDurableProjection(durable, projected).map((message) => message.id)).toEqual([
      'question',
      'historical-compaction',
      'pending-compaction',
    ]);
  });

  it('does not deduplicate response-local tool IDs across responses', () => {
    const durable: Message[] = [
      {
        id: 'old-tools',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [{ id: 'call-1', name: 'shell', status: 'done' }],
      },
    ];
    const projected: Message[] = [
      {
        id: 'new-tools',
        role: 'tool-group',
        content: '',
        created: 2,
        responseId: 'r2',
        tools: [{ id: 'call-1', name: 'read_file', status: 'running' }],
      },
    ];

    const merged = mergeDurableProjection(durable, projected);
    expect(merged).toHaveLength(2);
    expect(merged.map((message) => message.responseId)).toEqual(['r1', 'r2']);
  });

  it('coalesces partially durable tool activity without duplicating completed calls', () => {
    const durable: Message[] = [
      {
        id: 'durable-tools',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        status: 'done',
        tools: [
          {
            id: 'c1',
            name: 'shell',
            status: 'done',
            result: '/tmp',
            guardianReviews: [{ outcome: 'approved', message: 'safe' }],
          },
        ],
      },
    ];
    const projected: Message[] = [
      {
        id: 'projected-tools',
        role: 'tool-group',
        content: '',
        created: 2,
        responseId: 'r1',
        status: 'running',
        tools: [
          { id: 'c1', name: 'shell', status: 'running', guardianReviews: undefined },
          { id: 'c2', name: 'read_file', status: 'running' },
        ],
      },
    ];

    const merged = mergeDurableProjection(durable, projected);
    expect(merged).toHaveLength(1);
    expect(merged[0]).toMatchObject({ id: 'durable-tools', status: 'running' });
    expect(merged[0].tools?.map((tool) => tool.id)).toEqual(['c1', 'c2']);
    expect(merged[0].tools?.[0]).toMatchObject({
      status: 'done',
      guardianReviews: [{ outcome: 'approved', message: 'safe' }],
    });
  });

  it('does not reconcile tool groups across a structural boundary', () => {
    const durable: Message[] = [
      {
        id: 'durable-tools',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [{ id: 'c1', name: 'shell', status: 'done' }],
      },
      {
        id: 'durable-answer',
        role: 'assistant',
        content: 'between batches',
        created: 2,
        responseId: 'r1',
        assistantSegmentOrdinal: 1,
      },
    ];
    const projected: Message[] = [
      {
        id: 'projected-tools',
        role: 'tool-group',
        content: '',
        created: 3,
        responseId: 'r1',
        tools: [
          { id: 'c1', name: 'shell', status: 'done' },
          { id: 'c2', name: 'read_file', status: 'running' },
        ],
      },
    ];

    const merged = mergeDurableProjection(durable, projected);
    expect(merged.map((message) => message.role)).toEqual([
      'tool-group',
      'assistant',
      'tool-group',
    ]);
    expect(merged[0].tools?.map((tool) => tool.id)).toEqual(['c1']);
    expect(merged[2].tools?.map((tool) => tool.id)).toEqual(['c2']);
  });
});
