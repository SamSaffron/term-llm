import { describe, expect, it } from 'vitest';
import {
  convertServerMessages,
  mergeDurableProjection,
  transcriptMutationHeadID,
  type ServerMessage,
} from './transcript';
import type { Message } from './types';

const assistant = (id: number, sequence?: number): ServerMessage => ({
  id,
  sequence,
  role: 'assistant',
  response_id: 'response-1',
  parts: [{ type: 'text', text: 'Answer' }],
});

const skill = (id: number, sequence: number | undefined, status: string): ServerMessage => ({
  id,
  sequence,
  role: 'event',
  parts: [
    {
      type: 'skill_activation',
      skill_activation: { run_id: 'skill-1', name: 'review', status },
    },
  ],
});

describe('transcript mutation head', () => {
  it('uses a coalesced skill completion after the visually trailing assistant', () => {
    const messages = convertServerMessages([
      skill(90, 0, 'running'),
      assistant(100, 1),
      skill(20, 2, 'completed'),
    ]);

    expect(messages.map((message) => message.role)).toEqual(['skill-run', 'assistant']);
    expect(messages[0]).toMatchObject({ status: 'completed', serverSeq: 2 });
    expect(transcriptMutationHeadID(messages)).toBe(20);
    // Event rows must not become branch/share continuation anchors.
    expect(messages[0].durableRowId).toBeUndefined();
    expect(messages[1].durableRowId).toBe(100);
  });

  it('uses an inline tool result attached before the visually trailing final answer', () => {
    const messages = convertServerMessages([
      {
        id: 200,
        sequence: 3,
        role: 'assistant',
        response_id: 'response-1',
        parts: [
          { type: 'function_call', call_id: 'call-1', name: 'shell', arguments: '{}' },
          { type: 'text', text: 'Answer' },
        ],
      },
      {
        id: 30,
        sequence: 4,
        role: 'tool',
        response_id: 'response-1',
        parts: [{ type: 'tool_result', call_id: 'call-1', name: 'shell', output: 'done' }],
      },
    ]);

    expect(messages.map((message) => message.role)).toEqual(['tool-group', 'assistant']);
    expect(messages[0].tools?.[0]).toMatchObject({ result: 'done', status: 'done' });
    expect(messages[0].durableSourceHead).toMatchObject({ id: 30, sequence: 4 });
    expect(transcriptMutationHeadID(messages)).toBe(30);
    // Keep the existing branch anchors and display ordering sequence unchanged.
    expect(messages.map((message) => message.durableRowId)).toEqual([200, 200]);
    expect(messages.map((message) => message.serverSeq)).toEqual([3, 3]);
  });

  it('orders by sequence before ID, with ID only breaking equal-sequence ties', () => {
    const messages = convertServerMessages([
      assistant(20, 6),
      assistant(10, 6),
      assistant(1000, 5),
    ]);

    expect(transcriptMutationHeadID(messages)).toBe(20);
    expect(transcriptMutationHeadID([...messages].reverse())).toBe(20);
  });

  it.each<ServerMessage>([
    { role: 'assistant', transcriptEmptyBody: true },
    {
      role: 'tool',
      parts: [{ type: 'tool_result', call_id: 'unloaded-call', output: 'omitted' }],
    },
    { role: 'assistant', parts: [] },
    { role: 'assistant', compaction_tail: true, parts: [{ type: 'text', text: 'replay' }] },
  ])('retains a hidden non-internal source head: %j', (hidden) => {
    const messages = convertServerMessages([
      assistant(100, 0),
      { ...hidden, id: 5, sequence: 1 },
      { id: 999, sequence: 2, role: 'system', parts: [{ type: 'text', text: 'private' }] },
      { id: 1000, sequence: 3, role: 'developer', parts: [{ type: 'text', text: 'private' }] },
    ]);

    expect(messages).toHaveLength(1);
    expect(transcriptMutationHeadID(messages)).toBe(5);
    expect(messages[0].durableRowId).toBe(100);
    expect(messages[0].serverSeq).toBe(0);
  });

  it('retains the raw head when compaction fingerprint suppression removes its visual row', () => {
    const messages = convertServerMessages([
      assistant(100, 0),
      {
        id: 50,
        sequence: 1,
        role: 'user',
        parts: [{ type: 'text', text: '[Context Compaction]\nSummary' }],
      },
      assistant(10, 2),
    ]);

    expect(messages.map((message) => message.role)).toEqual(['assistant', 'compaction']);
    expect(transcriptMutationHeadID(messages)).toBe(10);
    expect(messages.map((message) => message.durableRowId)).toEqual([100, undefined]);
  });

  it('does not replace durable head sequence with live assistant projection metadata', () => {
    const durable = convertServerMessages([
      assistant(100, 0),
      { id: 5, sequence: 1, role: 'tool', parts: [] },
    ]);
    const projected: Message[] = [
      {
        id: 'live-answer',
        role: 'assistant',
        responseId: 'response-1',
        content: 'Answer extended',
        created: 1,
        segmentEndSequence: 100,
        serverSeq: 999,
        durableSourceHead: undefined,
      },
    ];

    const merged = mergeDurableProjection(durable, projected);
    expect(merged).toHaveLength(1);
    expect(merged[0].content).toBe('Answer extended');
    expect(merged[0].durableRowId).toBe(100);
    expect(transcriptMutationHeadID(merged)).toBe(5);
  });

  it('preserves source heads when a tool projection extends a durable group', () => {
    const durable = convertServerMessages([
      {
        id: 42,
        sequence: 2,
        role: 'assistant',
        response_id: 'response-1',
        parts: [{ type: 'tool_call', tool_call_id: 'saved', tool_name: 'shell' }],
      },
    ]);
    const projected: Message[] = [
      {
        id: 'live-tools',
        role: 'tool-group',
        responseId: 'response-1',
        content: '',
        created: 1,
        serverSeq: 999,
        durableSourceHead: undefined,
        tools: [{ id: 'pending', name: 'shell', status: 'running' }],
      },
    ];

    const merged = mergeDurableProjection(durable, projected);
    expect(merged).toHaveLength(1);
    expect(merged[0].tools).toHaveLength(2);
    expect(merged[0].durableRowId).toBe(42);
    expect(transcriptMutationHeadID(merged)).toBe(42);
  });

  it.each(['converted', 'legacy'])(
    'preserves %s source heads when model boundaries split a durable tool group',
    (source) => {
      const converted = convertServerMessages([
        { id: 100, sequence: 0, role: 'user', parts: [{ type: 'text', text: 'Question' }] },
        {
          id: 42,
          sequence: 2,
          role: 'assistant',
          response_id: 'response-1',
          parts: [
            { type: 'tool_call', tool_call_id: 'before', tool_name: 'shell' },
            { type: 'tool_call', tool_call_id: 'after', tool_name: 'shell' },
          ],
        },
      ]);
      const projected: Message[] = [
        {
          id: 'before',
          role: 'tool-group',
          responseId: 'response-1',
          content: '',
          created: 1,
          tools: [{ id: 'before', name: 'shell', status: 'done' }],
        },
        { id: 'swap', role: 'model-swap', content: 'model changed', created: 2 },
        {
          id: 'after',
          role: 'tool-group',
          responseId: 'response-1',
          content: '',
          created: 3,
          tools: [{ id: 'after', name: 'shell', status: 'done' }],
        },
      ];

      const durable =
        source === 'converted'
          ? converted
          : converted.map((message) => ({ ...message, durableSourceHead: undefined }));
      const merged = mergeDurableProjection(durable, projected);
      expect(merged.map((message) => message.role)).toEqual([
        'user',
        'tool-group',
        'model-swap',
        'tool-group',
      ]);
      const split = merged.filter((message) => message.role === 'tool-group');
      expect(split.every((message) => message.serverSeq === undefined)).toBe(true);
      expect(split.every((message) => message.durableRowId === undefined)).toBe(true);
      expect(split.map((message) => message.durableSourceHead)).toEqual([
        { id: 42, sequence: 2 },
        { id: 42, sequence: 2 },
      ]);
      expect(merged[0].durableRowId).toBe(100);
      expect(transcriptMutationHeadID(merged)).toBe(42);
      expect(transcriptMutationHeadID([...merged].reverse())).toBe(42);
    },
  );

  it.each([
    { start: 0, answer: 1, completed: undefined, expected: 20 },
    { start: undefined, answer: 1, completed: undefined, expected: 20 },
    { start: undefined, answer: undefined, completed: undefined, expected: 20 },
    { start: undefined, answer: undefined, completed: 2, expected: 20 },
  ])('uses raw source order for coalesced mixed-sequence skills: %j', (sequences) => {
    const messages = convertServerMessages([
      skill(90, sequences.start, 'running'),
      assistant(100, sequences.answer),
      skill(20, sequences.completed, 'completed'),
    ]);

    expect(messages.map((message) => message.role)).toEqual(['skill-run', 'assistant']);
    expect(transcriptMutationHeadID(messages)).toBe(sequences.expected);
    expect(transcriptMutationHeadID([...messages].reverse())).toBe(sequences.expected);
    expect(messages.map((message) => message.durableRowId)).toEqual([undefined, 100]);
  });

  it('does not let an earlier unsequenced source override the raw sequenced head', () => {
    const messages = convertServerMessages([skill(90, undefined, 'running'), assistant(20, 2)]);

    expect(transcriptMutationHeadID(messages)).toBe(20);
    expect(transcriptMutationHeadID([...messages].reverse())).toBe(20);
  });

  it('uses a later unsequenced inline result without relying on visual order', () => {
    const messages = convertServerMessages([
      {
        id: 200,
        sequence: 3,
        role: 'assistant',
        parts: [
          { type: 'function_call', call_id: 'call-1', name: 'shell' },
          { type: 'text', text: 'Answer' },
        ],
      },
      {
        id: 30,
        role: 'tool',
        parts: [{ type: 'tool_result', call_id: 'call-1', name: 'shell', output: 'done' }],
      },
    ]);

    expect(messages.map((message) => message.role)).toEqual(['tool-group', 'assistant']);
    expect(transcriptMutationHeadID(messages)).toBe(30);
    expect(transcriptMutationHeadID([...messages].reverse())).toBe(30);
    expect(messages.map((message) => message.durableRowId)).toEqual([200, 200]);
  });

  it('uses sequence metadata on legacy visual rows and ignores unsaved projections', () => {
    const messages: Message[] = [
      {
        id: 'newest',
        role: 'assistant',
        content: '',
        created: 1,
        durableRowId: 20,
        serverSeq: 6,
      },
      {
        id: 'older',
        role: 'error',
        content: '',
        created: 2,
        durableSourceRowIds: ['1000'],
        serverSeq: 5,
      },
      { id: 'live', role: 'assistant', content: '', created: 3, serverSeq: 999 },
    ];

    expect(transcriptMutationHeadID(messages)).toBe(20);
    expect(transcriptMutationHeadID([...messages].reverse())).toBe(20);
  });

  it('supports legacy source IDs and branch-only rows without taking a numeric maximum', () => {
    const messages: Message[] = [
      { id: 'earlier', role: 'assistant', content: '', created: 1, durableRowId: 100 },
      {
        id: 'later',
        role: 'error',
        content: '',
        created: 2,
        durableSourceRowIds: ['80', '10', '', 'bad', 0, -1, 1.5],
      },
    ];
    expect(transcriptMutationHeadID(messages)).toBe(10);
    expect(transcriptMutationHeadID(messages.slice(0, 1))).toBe(100);
    expect(transcriptMutationHeadID([])).toBe(0);
    expect(convertServerMessages([{ id: 1, sequence: 0, role: 'system' }])).toEqual([]);
  });
});
