import { describe, expect, it, vi } from 'vitest';
import {
  convertServerMessages,
  indexTranscriptTurns,
  mergeDurableProjection,
  sanitizeContextUsage,
  sanitizeSession,
  olderTranscriptAnchors,
  windowTranscript,
} from './transcript';
import { initialProjection, reduceResponse } from './response';
import type { Message } from './types';

describe('transcript domain', () => {
  it('restores completed spawn tool-call totals from validated history projection', () => {
    const messages = convertServerMessages([
      {
        id: 1,
        role: 'assistant',
        parts: [{ type: 'tool_call', tool_call_id: 'spawn-1', tool_name: 'spawn_agent' }],
      },
      {
        id: 2,
        role: 'tool',
        parts: [
          {
            type: 'tool_result',
            tool_call_id: 'spawn-1',
            tool_name: 'spawn_agent',
            spawn_agent: { agent_name: 'developer', session_id: 'child' },
            spawn_agent_tool_calls: 12,
          },
        ],
      },
    ]);
    expect(messages.flatMap((message) => message.tools || [])[0].subagentProgress).toEqual({
      seq: 0,
      state: 'completed',
      callsStarted: 12,
      callsActive: 0,
    });
  });

  function inlineTranscript(texts = ['before', 'between', 'after'], responseId = 'inline') {
    const parts = texts.flatMap((content, index) => [
      { type: 'text', text: content },
      { type: 'tool_call', tool_call_id: `call-${index}`, tool_name: 'shell' },
    ]);
    const durable = convertServerMessages([
      {
        id: 42,
        role: 'assistant',
        response_id: responseId,
        assistant_segment_ordinal: 0,
        segment_start_sequence: 1,
        segment_end_sequence: 1,
        parts,
      },
    ]);
    let projection = initialProjection({
      responseId,
      sessionId: 'session',
      epoch: 1,
      status: 'streaming',
      lastSequence: 0,
      startedRev: 0,
      reconnects: 0,
    });
    let sequence = 0;
    const emit = (type: string, payload: Record<string, unknown>) => {
      projection = reduceResponse(projection, {
        type,
        response_id: responseId,
        run_epoch: 1,
        sequence_number: ++sequence,
        ...payload,
      });
    };
    texts.forEach((content, index) => {
      if (index) emit('response.output_text.new_segment', { assistant_segment_ordinal: index });
      emit('response.output_text.delta', { assistant_segment_ordinal: index, delta: content });
      emit('response.output_item.added', {
        item: { type: 'function_call', call_id: `call-${index}`, name: 'shell' },
      });
      emit('response.tool_exec.end', { call_id: `call-${index}`, output: 'ok' });
    });
    return { durable, projected: projection.messages };
  }

  it.each([
    ['before', 'between', 'after'],
    ['repeated', 'repeated', 'repeated'],
  ])('merges inline text/tool parts without repeating commentary (%j)', (...texts) => {
    const { durable, projected } = inlineTranscript(texts);
    const merged = mergeDurableProjection(durable, projected);
    expect(merged.map((message) => message.role)).toEqual(durable.map((message) => message.role));
    expect(
      merged.filter((message) => message.role === 'assistant').map((message) => message.content),
    ).toEqual(texts);
    expect(merged.flatMap((message) => message.tools || []).map((tool) => tool.id)).toEqual([
      'call-0',
      'call-1',
      'call-2',
    ]);
    expect(
      merged.filter((message) => message.role === 'assistant').map((message) => message.id),
    ).toEqual(
      durable.filter((message) => message.role === 'assistant').map((message) => message.id),
    );
    // Snapshot recovery changes presentation IDs, not response/tool identity.
    expect(
      mergeDurableProjection(
        durable,
        projected.map((message, index) => ({
          ...message,
          id: `recovered-${index}`,
        })),
      ),
    ).toEqual(merged);
  });

  it.each(['live ahead', 'durable ahead'])(
    'keeps the newest anchored inline text: %s',
    (variant) => {
      const { durable, projected } = inlineTranscript();
      const saved = durable.find((message) => message.content === 'between')!;
      const live = projected.find((message) => message.content === 'between')!;
      if (variant === 'live ahead') live.content += ' suffix';
      else saved.content += ' suffix';
      const merged = mergeDurableProjection(durable, projected);
      expect(
        merged.filter((message) => message.role === 'assistant').map((message) => message.content),
      ).toEqual(['before', 'between suffix', 'after']);
      expect(merged.find((message) => message.content === 'between suffix')?.id).toBe(saved.id);
    },
  );

  it('keeps an unsaved inline tail after its durable tool boundary', () => {
    const { durable, projected } = inlineTranscript();
    const merged = mergeDurableProjection(durable.slice(0, 4), projected);
    expect(
      merged.filter((message) => message.role === 'assistant').map((message) => message.content),
    ).toEqual(['before', 'between', 'after']);
    expect(merged.map((message) => message.role)).toEqual(durable.map((message) => message.role));
  });

  it('does not collide with later provider turns whose ordinals overlap inline segments', () => {
    const { durable, projected } = inlineTranscript();
    const marker: Message = {
      id: 'switch',
      role: 'model-swap',
      content: 'switched',
      created: 1,
      boundaryId: 'boundary',
    };
    const later: Message = {
      id: 'later',
      role: 'assistant',
      content: 'next provider turn',
      created: 2,
      responseId: 'inline',
      assistantSegmentOrdinal: 1,
      segmentEndSequence: 30,
    };
    const merged = mergeDurableProjection(
      [...durable, marker, later],
      [...projected, marker, { ...later, id: 'live-later', assistantSegmentOrdinal: 3 }],
    );
    expect(merged.filter((message) => message.content === 'between')).toHaveLength(1);
    expect(merged.filter((message) => message.content === 'next provider turn')).toHaveLength(1);
    expect(merged.findIndex((message) => message.id === 'later')).toBeGreaterThan(
      merged.indexOf(marker),
    );
  });

  it.each(['user', 'compaction-boundary'] as const)(
    'matches inline continuations after %s',
    (role) => {
      const { durable, projected } = inlineTranscript();
      const boundary: Message = {
        id: 'boundary',
        role,
        content: 'boundary',
        created: 1,
        ...(role === 'user' ? { clientMessageId: 'steer-1' } : { compactionSeq: 12 }),
      };
      const saved: Message = {
        id: 'saved-tail',
        role: 'assistant',
        content: 'continued',
        created: 1,
        responseId: 'inline',
        assistantSegmentOrdinal: 1,
      };
      const merged = mergeDurableProjection(
        [...durable, boundary, saved],
        [...projected, boundary, { ...saved, id: 'live-tail', assistantSegmentOrdinal: 4 }],
      );
      expect(
        merged.filter((message) => message.role === 'assistant').map((message) => message.content),
      ).toEqual(['before', 'between', 'after', 'continued']);
    },
  );

  it('matches text after tool-only provider turns without assuming contiguous ordinals', () => {
    const { durable, projected } = inlineTranscript();
    // A tool-only prefix does not consume a text segment, but provider turn
    // indices can still advance. Adjacent call identity survives either form.
    const saved = durable.slice(1);
    const live = projected
      .slice(1)
      .map((message) =>
        message.role === 'assistant'
          ? { ...message, assistantSegmentOrdinal: (message.assistantSegmentOrdinal || 0) + 7 }
          : message,
      );
    const merged = mergeDurableProjection(saved, live);
    expect(merged.map((message) => message.role)).toEqual(saved.map((message) => message.role));
    expect(
      merged.filter((message) => message.role === 'assistant').map((message) => message.content),
    ).toEqual(['between', 'after']);
  });

  it('scopes inline tool boundaries to their response even when call IDs repeat', () => {
    const first = inlineTranscript(undefined, 'first');
    const second = inlineTranscript(undefined, 'second');
    const merged = mergeDurableProjection([...first.durable, ...second.durable], second.projected);
    expect(merged.filter((message) => message.role === 'assistant')).toHaveLength(6);
    expect(merged.filter((message) => message.content === 'between')).toHaveLength(2);
  });

  it('preserves per-part times and leaves unknown legacy segment times unset', () => {
    const start = 1_800_000_000_000;
    const parts = [
      { type: 'text', text: 'before', created_at: start },
      { type: 'tool_call', tool_call_id: 'a', tool_name: 'shell', created_at: start + 1000 },
      { type: 'text', text: 'final', created_at: start + 600_000 },
    ];
    const saved = { id: 1, role: 'assistant', created_at: start, parts };
    expect(convertServerMessages([saved]).map((message) => message.created)).toEqual([
      start,
      start + 1000,
      start + 600_000,
    ]);
    expect(
      convertServerMessages([
        { ...saved, parts: parts.map(({ created_at: _time, ...part }) => part) },
      ]).map((message) => message.created),
    ).toEqual([start, 0, 0]);
  });

  it('does not replace live segment times with the inline row start during handoff', () => {
    const { durable, projected } = inlineTranscript();
    const times = [1_800_000_000_000, 1_800_000_060_000, 1_800_000_600_000];
    let index = 0;
    for (const message of projected)
      if (message.role === 'assistant') message.created = times[index++];
    const merged = mergeDurableProjection(durable, projected);
    expect(
      merged.filter((message) => message.role === 'assistant').map((message) => message.created),
    ).toEqual(times);
    const saved = durable.find((message) => message.content === 'after')!;
    saved.created = times[2] - 500;
    saved.segmentCreatedAt = saved.created;
    expect(
      mergeDurableProjection(durable, projected).find((message) => message.content === 'after')
        ?.created,
    ).toBe(times[2] - 500);
  });

  it('matches a legacy result without a response ID only to an unambiguous call in its turn', () => {
    const converted = convertServerMessages([
      {
        id: 1,
        role: 'assistant',
        response_id: 'r1',
        parts: [
          { type: 'tool_call', tool_call_id: 'a', tool_name: 'shell' },
          { type: 'text', text: 'final' },
        ],
      },
      {
        id: 2,
        role: 'tool',
        parts: [
          { type: 'tool_result', tool_call_id: 'a', tool_name: 'shell', output: 'legacy result' },
        ],
      },
    ]);
    expect(converted.map((message) => message.role)).toEqual(['tool-group', 'assistant']);
    expect(converted[0].tools?.[0].result).toBe('legacy result');
  });

  it.each([
    { guardian_reviews: [{ outcome: 'approved', message: 'safe', model: 'guardian' }] },
    { images: ['/image.png'] },
    { media: [{ url: '/video.mp4', type: 'video/mp4' }] },
    { tool_error: true },
  ])('attaches delayed inline results to their original calls (%j)', (metadata) => {
    const converted = convertServerMessages([
      {
        id: 1,
        role: 'assistant',
        response_id: 'r1',
        parts: [
          { type: 'text', text: 'before' },
          { type: 'tool_call', tool_call_id: 'a', tool_name: 'shell' },
          { type: 'text', text: 'between' },
          { type: 'tool_call', tool_call_id: 'b', tool_name: 'shell' },
          { type: 'text', text: 'final answer' },
        ],
      },
      ...['a', 'b'].map((id, index) => ({
        id: index + 2,
        role: 'tool',
        response_id: 'r1',
        parts: [
          {
            type: 'tool_result',
            tool_call_id: id,
            tool_name: 'shell',
            output: `result ${id}`,
            ...metadata,
          },
        ],
      })),
    ]);
    expect(converted.map((message) => message.role)).toEqual([
      'assistant',
      'tool-group',
      'assistant',
      'tool-group',
      'assistant',
    ]);
    expect(converted.at(-1)?.content).toBe('final answer');
    const tools = converted.flatMap((message) => message.tools || []);
    expect(tools.map((tool) => [tool.id, tool.result])).toEqual([
      ['a', 'result a'],
      ['b', 'result b'],
    ]);
    expect(tools[0].status).toBe('tool_error' in metadata ? 'error' : 'done');
    if ('guardian_reviews' in metadata) expect(tools[0].guardianReviews).toHaveLength(1);
    if ('images' in metadata) expect(tools[0].images).toEqual(['/image.png']);
    if ('media' in metadata) expect(tools[0].media?.[0].url).toBe('/video.mp4');
  });

  it('keeps a completed answer after 58 reviewed shell results without creating another group', () => {
    const calls = Array.from({ length: 58 }, (_, index) => `shell-${index}`);
    const converted = convertServerMessages([
      {
        id: 1,
        role: 'assistant',
        response_id: 'r1',
        parts: [
          ...calls.map((id) => ({ type: 'tool_call', tool_call_id: id, tool_name: 'shell' })),
          { type: 'text', text: 'final answer' },
        ],
      },
      ...calls.map((id, index) => ({
        id: index + 2,
        role: 'tool',
        response_id: 'r1',
        parts: [
          {
            type: 'tool_result',
            tool_call_id: id,
            tool_name: 'shell',
            guardian_reviews: [{ outcome: 'approved', message: 'safe', model: 'guardian' }],
          },
        ],
      })),
    ]);
    expect(converted.map((message) => message.role)).toEqual(['tool-group', 'assistant']);
    expect(converted[0].tools).toHaveLength(58);
    expect(converted[0].tools?.every((tool) => tool.guardianReviews?.length === 1)).toBe(true);
    expect(converted[1].content).toBe('final answer');
  });

  it('does not attach delayed results to the same call ID from another response', () => {
    const converted = convertServerMessages([
      {
        id: 1,
        role: 'assistant',
        response_id: 'first',
        parts: [
          { type: 'tool_call', tool_call_id: 'shared', tool_name: 'shell' },
          { type: 'text', text: 'first answer' },
        ],
      },
      {
        id: 2,
        role: 'assistant',
        response_id: 'second',
        parts: [{ type: 'tool_call', tool_call_id: 'shared', tool_name: 'shell' }],
      },
      {
        id: 3,
        role: 'tool',
        response_id: 'first',
        parts: [
          {
            type: 'tool_result',
            tool_call_id: 'shared',
            tool_name: 'shell',
            output: 'first result',
          },
        ],
      },
    ]);
    expect(converted[0].tools?.[0].result).toBe('first result');
    expect(converted[2].tools?.[0].result).toBeUndefined();
  });

  it('preserves live shell execution and timing over assumed-complete history', () => {
    const durable = convertServerMessages([
      {
        id: 1,
        role: 'assistant',
        response_id: 'r1',
        parts: [{ type: 'tool_call', tool_call_id: 'shell-1', tool_name: 'shell' }],
      },
    ]);
    expect(durable[0].tools?.[0].status).toBe('done');
    const live: Message = {
      id: 'live',
      role: 'tool-group',
      responseId: 'r1',
      content: '',
      created: 1,
      tools: [{ id: 'shell-1', name: 'shell', status: 'running', startedAt: 10_000 }],
    };
    const running = mergeDurableProjection(durable, [live]);
    expect(running).toHaveLength(1);
    expect(running[0]).toMatchObject({ status: 'running', tools: live.tools });
    const finished: Message = {
      ...live,
      tools: [
        {
          ...live.tools![0],
          status: 'done',
          resultStatus: 'success',
          endedAt: 30_000,
          durationMs: 20_000,
        },
      ],
    };
    expect(mergeDurableProjection(durable, [finished])[0]).toMatchObject({
      status: 'done',
      tools: finished.tools,
    });
    expect(durable[0].tools?.[0]).not.toHaveProperty('startedAt');
    expect(durable[0].tools?.[0].status).toBe('done');
  });

  it.each(['done', 'error', 'cancelled'] as const)(
    'does not regress an explicit durable %s result to running',
    (status) => {
      const durable: Message = {
        id: 'durable',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [
          {
            id: 'shell-1',
            name: 'shell',
            status,
            resultStatus: status === 'error' ? 'error' : 'success',
            endedAt: 30_000,
          },
        ],
      };
      const live: Message = {
        ...durable,
        id: 'live',
        tools: [{ id: 'shell-1', name: 'shell', status: 'running', startedAt: 10_000 }],
      };
      expect(mergeDurableProjection([durable], [live])[0]).toBe(durable);
    },
  );
  it.each([undefined, 20_000])(
    'only overlays timing onto matching explicit terminal results (duration: %s)',
    (durationMs) => {
      const durable: Message = {
        id: 'durable',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [
          {
            id: 'shell-1',
            name: 'shell',
            status: 'done',
            resultStatus: 'success',
            arguments: '{"command":"echo complete"}',
            result: 'complete output',
            media: [{ url: '/ui/media/result.png', type: 'image/png', caption: 'Saved caption' }],
            durationMs: 5_000,
          },
        ],
      };
      const live: Message = {
        ...durable,
        id: 'live',
        tools: [
          {
            id: 'shell-1',
            name: 'shell',
            status: 'done',
            resultStatus: 'success',
            arguments: '',
            result: '',
            media: [{ url: '/media/result.png', type: 'image/png' }],
            startedAt: 10_000,
            endedAt: 30_000,
            durationMs,
          },
        ],
      };
      const merged = mergeDurableProjection([durable], [live]);
      expect(merged[0].tools?.[0]).toEqual({
        ...durable.tools![0],
        startedAt: 10_000,
        endedAt: 30_000,
        durationMs: durationMs ?? 5_000,
      });
      expect(durable.tools![0]).not.toHaveProperty('startedAt');
    },
  );

  it('does not overwrite saved arguments with a running projection', () => {
    const durable: Message = {
      id: 'durable',
      role: 'tool-group',
      content: '',
      created: 1,
      responseId: 'r1',
      tools: [
        { id: 'shell-1', name: 'shell', status: 'done', arguments: '{"command":"sleep 20"}' },
      ],
    };
    const live: Message = {
      ...durable,
      id: 'live',
      tools: [
        {
          id: 'shell-1',
          name: 'shell',
          status: 'running',
          startedAt: 10_000,
          arguments: '{"command":',
        },
      ],
    };
    expect(mergeDurableProjection([durable], [live])[0]).toMatchObject({
      status: 'running',
      tools: [{ ...durable.tools![0], status: 'running', startedAt: 10_000 }],
    });
  });

  it.each(['running', 'error'] as const)(
    'preserves a durable result-only completion over live %s state',
    (status) => {
      const durable: Message = {
        id: 'durable',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [{ id: 'shell-1', name: 'shell', status: 'done', result: '' }],
      };
      const live: Message = {
        ...durable,
        id: 'live',
        tools: [
          {
            id: 'shell-1',
            name: 'shell',
            status,
            startedAt: 10_000,
            ...(status === 'error' ? { endedAt: 30_000, resultStatus: 'error' as const } : {}),
          },
        ],
      };
      expect(mergeDurableProjection([durable], [live])[0]).toBe(durable);
    },
  );

  it('preserves untouched tool identities and applies terminal overlays without mutating durable input', () => {
    const completed: Message = {
      id: 'complete',
      role: 'tool-group',
      content: '',
      created: 1,
      tools: [{ id: 'done', name: 'read_file', status: 'done' }],
    };
    const running: Message = {
      id: 'running',
      role: 'tool-group',
      responseId: 'r1',
      content: '',
      created: 2,
      tools: [
        { id: 'a', name: 'shell', status: 'running' },
        { id: 'b', name: 'shell', status: 'running' },
      ],
    };
    for (const message of [completed, running]) {
      message.tools!.forEach(Object.freeze);
      Object.freeze(message.tools);
      Object.freeze(message);
    }
    expect(mergeDurableProjection([completed, running], [])).toEqual([completed, running]);
    expect(mergeDurableProjection([completed, running], [])[1]).toBe(running);
    const live: Message = {
      ...running,
      tools: [
        { id: 'a', name: 'shell', status: 'done', endedAt: 10, result: 'ok' },
        { id: 'b', name: 'shell', status: 'error', endedAt: 11, result: 'failed' },
      ],
    };
    const merged = mergeDurableProjection([completed, running], [live]);
    expect(merged[0]).toBe(completed);
    expect(merged[0].tools![0]).toBe(completed.tools![0]);
    expect(merged[1].status).toBe('done');
    expect(merged[1].tools).toEqual(live.tools);
    expect(running.tools!.every((tool) => tool.status === 'running')).toBe(true);
  });

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

  it('preserves structured durable model-switch metadata', () => {
    const messages = convertServerMessages([
      {
        id: 7,
        sequence: 7,
        role: 'event',
        parts: [
          {
            type: 'model_swap',
            text: 'legacy display text',
            model_swap: {
              boundary_id: 'r1:model-switch:2',
              from_provider: 'chatgpt',
              from_model: 'gpt-5.6-luna-high',
              from_effort: 'high',
              to_provider: 'chatgpt',
              to_model: 'gpt-5.6-sol-high',
              to_effort: 'medium',
              status: 'succeeded',
            },
          },
        ],
      },
    ]);

    expect(messages[0]).toMatchObject({
      role: 'model-swap',
      boundaryId: 'r1:model-switch:2',
      fromModel: 'gpt-5.6-luna-high',
      fromEffort: 'high',
      toModel: 'gpt-5.6-sol-high',
      toEffort: 'medium',
    });
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
      durationMs: 250,
      subagent: { agentName: 'reviewer', output: 'No issues found.', durationMs: 250 },
    });
  });

  it('keeps durable ask_user answers on their matching tool calls', () => {
    const messages = convertServerMessages([
      {
        id: 1,
        sequence: 0,
        role: 'assistant',
        parts: [
          {
            type: 'tool_call',
            tool_call_id: 'ask-1',
            tool_name: 'ask_user',
            tool_arguments:
              '{"questions":[{"header":"Frontend","question":"Which renderer?","options":[]}]}',
          },
          {
            type: 'tool_call',
            tool_call_id: 'ask-2',
            tool_name: 'ask_user',
            tool_arguments:
              '{"questions":[{"header":"Theme","question":"Which theme?","options":[]}]}',
          },
        ],
      },
      {
        id: 2,
        sequence: 1,
        role: 'tool',
        parts: [
          {
            type: 'tool_result',
            tool_call_id: 'ask-2',
            tool_name: 'ask_user',
            ask_user_summary: 'Theme: Dark',
          },
          {
            type: 'tool_result',
            tool_call_id: 'ask-1',
            tool_name: 'ask_user',
            ask_user_summary: 'Frontend: Vendor xterm.js',
          },
        ],
      },
    ]);

    expect(messages).toHaveLength(1);
    expect(messages[0].role).toBe('tool-group');
    expect(messages[0].tools).toEqual([
      expect.objectContaining({ id: 'ask-1', askUserAnswer: 'Frontend: Vendor xterm.js' }),
      expect.objectContaining({ id: 'ask-2', askUserAnswer: 'Theme: Dark' }),
    ]);
    expect(messages.some((message) => message.role === 'user')).toBe(false);
  });

  it('represents an orphaned ask_user result as a tool instead of a user turn', () => {
    const messages = convertServerMessages([
      {
        id: 3,
        sequence: 2,
        role: 'tool',
        parts: [
          {
            type: 'tool_result',
            tool_call_id: 'ask-orphan',
            tool_name: 'ask_user',
            ask_user_summary: 'Choice: Continue',
          },
        ],
      },
    ]);

    expect(messages).toHaveLength(1);
    expect(messages[0]).toMatchObject({
      role: 'tool-group',
      tools: [
        {
          id: 'ask-orphan',
          name: 'ask_user',
          status: 'done',
          askUserAnswer: 'Choice: Continue',
        },
      ],
    });
  });

  it('sanitizes server session defaults and seconds timestamps', () => {
    const session = sanitizeSession({
      id: 's1',
      created_at: 1_700_000_000,
      title: '',
      pinned: 1,
      file_change_summary: { file_count: 2, adds: 7, dels: 3, git: true },
      context_usage: {
        used_tokens: 135_000,
        input_limit: 372_000,
        cached_input_tokens: 51_900_000,
        estimated: true,
      },
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
      contextUsage: {
        usedTokens: 135_000,
        inputLimit: 372_000,
        cachedInputTokens: 51_900_000,
        estimated: true,
      },
    });
    expect(session).not.toHaveProperty('approvalDefaultMode');
    expect(
      sanitizeSession({
        id: 's2',
        approval_default_mode: 'auto',
        approval_requested_mode: 'auto',
        approval_effective_mode: 'prompt',
        guardian_auto_suspended: true,
      }),
    ).toMatchObject({
      approvalDefaultMode: 'auto',
      approvalRequestedMode: 'auto',
      approvalEffectiveMode: 'prompt',
      guardianAutoSuspended: true,
    });
  });

  it('sanitizes context usage without turning missing values into zero', () => {
    expect(sanitizeContextUsage(null)).toBeUndefined();
    expect(sanitizeContextUsage({})).toBeUndefined();
    expect(
      sanitizeContextUsage({
        used_tokens: null,
        input_limit: null,
        cached_input_tokens: null,
      }),
    ).toBeUndefined();
    expect(
      sanitizeContextUsage({
        usedTokens: -3.8,
        inputLimit: 0,
        cachedInputTokens: 4.9,
        estimated: false,
      }),
    ).toEqual({ usedTokens: 0, cachedInputTokens: 4, estimated: false });
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

  it('retains earlier turn anchors from the full index, including a leading assistant turn', () => {
    expect(
      olderTranscriptAnchors({
        index: { rows: { ids: [1, 2, 3, 4, 5, 6], roles: 'auatua' } },
        bodies: { messages: [{ id: 5 }, { id: 6 }] },
      }),
    ).toEqual([1, 2]);
    expect(olderTranscriptAnchors({ bodies: { messages: [{ id: 5 }] } })).toEqual([]);
    expect(
      olderTranscriptAnchors({
        index: { rows: { ids: [1, 2], roles: 'ua' } },
        bodies: { messages: [] },
      }),
    ).toEqual([1]);
  });

  it('does not leave a zero-message gap after every turn is revealed', () => {
    const messages = Array.from({ length: 200 }, (_, index): Message => ({
      id: String(index),
      role: index % 2 === 0 ? 'user' : 'assistant',
      content: String(index),
      created: index,
    }));
    expect(windowTranscript(messages, 100)).toEqual([{ type: 'messages', key: 'all', messages }]);
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

  it('preserves live terminal tool state over an earlier durable running placeholder', () => {
    const durable: Message[] = [
      {
        id: 'durable-tools',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        status: 'running',
        tools: [
          { id: 'spawn-a', name: 'spawn_agent', status: 'running', startedAt: 100 },
          { id: 'spawn-b', name: 'spawn_agent', status: 'running', startedAt: 200 },
        ],
      },
    ];
    const projected: Message[] = [
      {
        id: 'live-tools',
        role: 'tool-group',
        content: '',
        created: 2,
        responseId: 'r1',
        status: 'running',
        tools: [
          {
            id: 'spawn-a',
            name: 'spawn_agent',
            status: 'done',
            resultStatus: 'success',
            startedAt: 100,
            endedAt: 1_100,
            durationMs: 1_000,
          },
          { id: 'spawn-b', name: 'spawn_agent', status: 'running', startedAt: 200 },
        ],
      },
    ];

    const merged = mergeDurableProjection(durable, projected);

    expect(merged).toHaveLength(1);
    expect(merged[0].id).toBe('durable-tools');
    expect(merged[0].tools?.[0]).toMatchObject({
      id: 'spawn-a',
      status: 'done',
      resultStatus: 'success',
      endedAt: 1_100,
      durationMs: 1_000,
    });
    expect(merged[0].tools?.[1]).toMatchObject({ id: 'spawn-b', status: 'running' });
    expect(durable[0].tools?.[0].status).toBe('running');
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

  it('keeps a model boundary fixed while adjacent tool groups become durable', () => {
    const projected: Message[] = [
      {
        id: 'before-live',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [{ id: 'before', name: 'shell', status: 'done' }],
      },
      {
        id: 'r1:model-switch:1',
        role: 'model-swap',
        content: '',
        created: 2,
        responseId: 'r1',
        boundaryId: 'r1:model-switch:1',
        fromModel: 'gpt-5.6-luna-high',
        toModel: 'gpt-5.6-sol-high',
      },
      {
        id: 'after-live',
        role: 'tool-group',
        content: '',
        created: 3,
        responseId: 'r1',
        tools: [{ id: 'after', name: 'read_file', status: 'running' }],
      },
    ];
    const partialDurable: Message[] = [
      {
        id: 'before-durable',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [{ id: 'before', name: 'shell', status: 'done' }],
      },
    ];
    const combinedDurable: Message[] = [
      {
        id: 'combined-durable',
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r1',
        tools: [
          { id: 'before', name: 'shell', status: 'done' },
          { id: 'after', name: 'read_file', status: 'done' },
        ],
      },
    ];
    const shape = (messages: Message[]) =>
      messages.map((message) =>
        message.role === 'tool-group'
          ? message.tools?.map((tool) => tool.id).join(',')
          : message.role,
      );

    expect(shape(mergeDurableProjection([], projected))).toEqual(['before', 'model-swap', 'after']);
    expect(shape(mergeDurableProjection(partialDurable, projected))).toEqual([
      'before',
      'model-swap',
      'after',
    ]);
    expect(shape(mergeDurableProjection(combinedDurable, projected))).toEqual([
      'before',
      'model-swap',
      'after',
    ]);
  });

  it('keeps pending pre-switch tools ahead of an unanchored model boundary', () => {
    const durable: Message[] = [
      {
        id: 'durable-answer',
        role: 'assistant',
        content: 'working',
        created: 1,
        responseId: 'r1',
        assistantSegmentOrdinal: 0,
      },
    ];
    const projected: Message[] = [
      { ...durable[0], id: 'live-answer' },
      {
        id: 'before',
        role: 'tool-group',
        content: '',
        created: 2,
        responseId: 'r1',
        tools: [{ id: 'before', name: 'shell', status: 'done' }],
      },
      {
        id: 'switch',
        role: 'model-swap',
        content: '',
        created: 3,
        boundaryId: 'switch',
        fromModel: 'old',
        toModel: 'new',
      },
      {
        id: 'after',
        role: 'tool-group',
        content: '',
        created: 4,
        responseId: 'r1',
        tools: [{ id: 'after', name: 'read_file', status: 'running' }],
      },
    ];

    expect(mergeDurableProjection(durable, projected).map((message) => message.id)).toEqual([
      'durable-answer',
      'before',
      'switch',
      'after',
    ]);
  });

  it('adopts a durable model boundary by exact identity without duplication', () => {
    const durable: Message[] = [
      {
        id: 'durable-switch',
        role: 'model-swap',
        content: '',
        created: 1,
        boundaryId: 'r1:model-switch:1',
        fromModel: 'old',
        toModel: 'new',
      },
    ];
    const projected: Message[] = [
      {
        ...durable[0],
        id: 'live-switch',
        responseId: 'r1',
      },
    ];

    expect(mergeDurableProjection(durable, projected)).toEqual(durable);
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

  it('projects durable typed media and rebases its URL', () => {
    const messages = convertServerMessages(
      [
        {
          id: 1,
          role: 'assistant',
          parts: [{ type: 'tool_call', tool_call_id: 'm1', tool_name: 'show_media' }],
        },
        {
          id: 2,
          role: 'tool',
          parts: [
            {
              type: 'tool_result',
              tool_call_id: 'm1',
              tool_name: 'show_media',
              media: [
                {
                  reference: '0123456789abcdef0123456789abcdef',
                  url: '/media/hash.mp4',
                  type: 'video/mp4',
                  name: 'demo.mp4',
                },
              ],
            },
          ],
        },
      ],
      { rebaseAssetURL: (value) => `/node/demo${value}` },
    );
    expect(messages[0].tools?.[0].media).toEqual([
      {
        reference: '0123456789abcdef0123456789abcdef',
        url: '/node/demo/media/hash.mp4',
        type: 'video/mp4',
        name: 'demo.mp4',
      },
    ]);
  });
});
