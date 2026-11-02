import { describe, expect, it } from 'vitest';
import {
  DEFAULT_THINKING,
  cycleThinking,
  effectiveDisplay,
  historyExpanded,
  historyMode,
  historyVisible,
  parseThinkingDisplay,
  statusEnabled,
  thinkingCompletions,
  thinkingStatus,
} from './thinking';
import { applyCompletion, composerCompletions } from './completions';
import { initialProjection, reduceResponse } from './response';
import { convertServerMessages, indexTranscriptTurns, mergeDurableProjection } from './transcript';
import { responseActivity } from './activity';
import type { ActiveRun, Message } from './types';

const thought: Message = {
  id: 'thought',
  role: 'reasoning',
  content: 'A  summary\nwith whitespace',
  reasoningKind: 'summary',
  reasoningTitle: 'Checking the path',
  created: 1,
  responseId: 'r',
};
const run: ActiveRun = {
  responseId: 'r',
  sessionId: 's',
  epoch: 1,
  status: 'streaming',
  lastSequence: 0,
  startedRev: 0,
  reconnects: 0,
};

describe('thinking policy (Go interactive policy parity)', () => {
  it('defaults web thinking to off and shows safe provider thinking only when opted in', () => {
    expect(effectiveDisplay(DEFAULT_THINKING)).toBe('off');
    for (const kind of ['summary', 'raw']) {
      expect(historyVisible(kind, DEFAULT_THINKING)).toBe(false);
      expect(historyVisible(kind, { ...DEFAULT_THINKING, display: 'collapsed' })).toBe(true);
      expect(historyExpanded(kind, { ...DEFAULT_THINKING, display: 'collapsed' })).toBe(false);
    }
    for (const kind of ['unknown', 'encrypted', ''])
      expect(historyVisible(kind, { ...DEFAULT_THINKING, display: 'collapsed' })).toBe(false);
  });
  it('raw display needs its gate, but provider thinking remains visible', () => {
    const blocked = { ...DEFAULT_THINKING, display: 'raw' };
    expect(effectiveDisplay(blocked)).toBe('collapsed');
    expect(historyVisible('raw', blocked)).toBe(true);
    expect(effectiveDisplay({ ...blocked, raw: true })).toBe('raw');
    expect(historyExpanded('raw', { ...blocked, raw: true })).toBe(true);
  });
  it.each(['none', 'transcript_only'])('hides history=%s', (history) => {
    expect(historyVisible('summary', { ...DEFAULT_THINKING, history })).toBe(false);
  });
  it.each(['off', 'status'])('hides display=%s', (display) => {
    expect(historyMode({ ...DEFAULT_THINKING, display })).toBe('none');
  });
  it('normalises modes and falls back just as Go does', () => {
    for (const display of ['', 'auto', 'invalid'])
      expect(effectiveDisplay({ ...DEFAULT_THINKING, display })).toBe('collapsed');
    expect(parseThinkingDisplay(' AUTO ')).toBe('collapsed');
    expect(parseThinkingDisplay('bad')).toBeNull();
    expect(
      ['collapsed', 'expanded', 'off'].map((mode) =>
        cycleThinking(mode as 'collapsed' | 'expanded' | 'off'),
      ),
    ).toEqual(['expanded', 'off', 'collapsed']);
    for (const display of ['expanded', 'raw'])
      expect(historyMode({ ...DEFAULT_THINKING, display, raw: true, history: '' })).toBe(
        'expanded',
      );
    expect(historyExpanded('summary', { ...DEFAULT_THINKING, display: 'expanded' })).toBe(true);
  });
  it('uses status titles, generic text and rune-capped whitespace-collapsed summaries', () => {
    expect(thinkingStatus(thought, { ...DEFAULT_THINKING, display: 'collapsed' })).toBe(
      'Checking the path',
    );
    for (const cfg of [{ display: 'off' }, { status: 'none' }, { status: 'generic' }])
      expect(thinkingStatus(thought, { ...DEFAULT_THINKING, ...cfg })).toBe('Thinking');
    expect(statusEnabled({ ...DEFAULT_THINKING, display: 'off' })).toBe(false);
    expect(
      thinkingStatus(
        { ...thought, reasoningTitle: '' },
        { ...DEFAULT_THINKING, display: 'status', status: 'summary' },
      ),
    ).toBe('A summary with whitespace');
    expect(
      Array.from(
        thinkingStatus(
          { ...thought, reasoningTitle: '', content: '🙂'.repeat(90) },
          { ...DEFAULT_THINKING, display: 'status', status: 'summary' },
        ),
      ),
    ).toHaveLength(80);
  });
});

describe('thinking completions', () => {
  it.each(['/thinking', '/reasoning'])(
    'offers modes, badges, safe replacement for %s',
    (command) => {
      const entries = thinkingCompletions(command, 'collapsed', false)!;
      expect(entries.map((e) => e.value)).toEqual([
        'off',
        'status',
        'collapsed',
        'expanded',
        'raw',
      ]);
      expect(entries.find((e) => e.current)?.value).toBe('collapsed');
      expect(entries.at(-1)?.unavailable).toBe(true);
      expect(applyCompletion(command, entries[3])).toBe(`${command} expanded `);
      const partial = thinkingCompletions(`${command} ex`, 'collapsed', true)!;
      expect(partial).toHaveLength(1);
      expect(applyCompletion(`${command} ex`, partial[0])).toBe(`${command} expanded `);
      expect(composerCompletions(`${command} `, [], [], true).length).toBe(5);
    },
  );
});

describe('reasoning projection', () => {
  it('appends by index, replaces on title closure, opens a new row and discards attempts', () => {
    let projection = initialProjection(run);
    let sequence = 0;
    const send = (rest: Record<string, unknown>) => {
      projection = reduceResponse(projection, {
        type: 'response.reasoning.delta',
        response_id: 'r',
        run_epoch: 1,
        sequence_number: ++sequence,
        kind: 'summary',
        ...rest,
      });
    };
    send({ reasoning_index: 0, delta: '**Checking' });
    send({ reasoning_index: 0, title: 'Title', text: 'body' });
    send({ reasoning_index: 0, title: 'Title', delta: ' tail' });
    expect(projection.messages).toHaveLength(1);
    expect(projection.messages[0]).toMatchObject({
      content: 'body tail',
      reasoningTitle: 'Title',
      reasoningIndex: 0,
    });
    send({ reasoning_index: 1, kind: 'raw', delta: 'next' });
    expect(projection.messages).toHaveLength(2);
    projection = reduceResponse(projection, {
      type: 'response.attempt.discard',
      response_id: 'r',
      run_epoch: 1,
      sequence_number: ++sequence,
    });
    expect(projection.messages).toHaveLength(0);
  });
  it('preserves durable order and source metadata without including thoughts in response copy', () => {
    const rows = convertServerMessages([
      {
        id: 12,
        role: 'assistant',
        response_id: 'r',
        sequence: 4,
        parts: [
          { type: 'tool_call', tool_name: 'read_file', tool_call_id: 'a' },
          { type: 'reasoning', text: 'body', reasoning_kind: 'summary', reasoning_title: 'Title' },
          { type: 'text', text: 'answer' },
        ],
      },
    ]);
    expect(rows.map((m) => m.role)).toEqual(['tool-group', 'reasoning', 'assistant']);
    expect(rows[1]).toMatchObject({
      responseId: 'r',
      serverSeq: 4,
      reasoningKind: 'summary',
      reasoningTitle: 'Title',
    });
    const contexts = indexTranscriptTurns(rows, (message) => message.content);
    expect(contexts.get(rows[1])?.copyTarget).toBe(false);
    expect(contexts.get(rows[2])?.responseText).toBe('answer');
  });
  it('keeps the timestamp of an answer whose thought comes first', () => {
    const at = '2026-11-01T09:00:00Z';
    const rows = convertServerMessages([
      {
        id: 8,
        role: 'assistant',
        created_at: at,
        parts: [
          { type: 'reasoning', text: 'plan', reasoning_kind: 'summary' },
          { type: 'text', text: 'answer' },
          { type: 'tool_call', tool_name: 'read_file', tool_call_id: 't' },
          { type: 'text', text: 'later segment' },
        ],
      },
    ]);
    const answers = rows.filter((m) => m.role === 'assistant');
    expect(answers.map((m) => m.created)).toEqual([Date.parse(at), 0]);
  });
  it('puts provider tool activity between the first thought and its answer, as the TUI does', () => {
    const rows = convertServerMessages([
      {
        id: 7,
        role: 'assistant',
        response_id: 'r',
        parts: [
          {
            type: 'reasoning',
            text: 'search',
            reasoning_kind: 'summary',
            reasoning_title: 'Search',
          },
          { type: 'text', text: 'answer' },
          { type: 'tool_activity', tool_name: 'web_search', tool_call_id: 'ws' },
        ],
      },
    ]);
    expect(rows.map((m) => m.role)).toEqual(['reasoning', 'tool-group', 'assistant']);
  });
  it('shows a saved turn’s thought once when history is merged into a running response', () => {
    const durable = convertServerMessages([
      { id: 1, role: 'user', parts: [{ type: 'text', text: 'go' }] },
      {
        id: 2,
        role: 'assistant',
        response_id: 'r',
        assistant_segment_ordinal: 0,
        parts: [
          { type: 'reasoning', text: 'plan', reasoning_kind: 'summary', reasoning_title: 'Plan' },
          { type: 'text', text: 'first answer' },
        ],
      },
      // An inline tool snapshot is saved before its turn's reasoning is attached.
      {
        id: 3,
        role: 'assistant',
        response_id: 'r',
        assistant_segment_ordinal: 1,
        parts: [{ type: 'tool_call', tool_name: 'read_file', tool_call_id: 'a' }],
      },
    ]);
    let projection = initialProjection(run);
    let sequence = 0;
    const send = (type: string, rest: Record<string, unknown>) => {
      projection = reduceResponse(projection, {
        type,
        response_id: 'r',
        run_epoch: 1,
        sequence_number: ++sequence,
        ...rest,
      });
    };
    const think = (index: number, delta: string) =>
      send('response.reasoning.delta', { reasoning_index: index, kind: 'summary', delta });
    think(0, 'plan');
    send('response.output_text.delta', { assistant_segment_ordinal: 0, delta: 'first answer' });
    think(1, 'read the file');
    send('response.output_item.added', {
      item: { type: 'function_call', call_id: 'a', name: 'read_file' },
    });
    think(2, 'next');
    const merged = mergeDurableProjection(durable, projection.messages);
    expect(merged.filter((m) => m.role === 'reasoning').map((m) => m.content)).toEqual([
      'plan',
      'read the file',
      'next',
    ]);
  });
  it('keeps post-tool thinking when only an earlier burst has been saved', () => {
    const durable = convertServerMessages([
      {
        id: 2,
        role: 'assistant',
        response_id: 'r',
        assistant_segment_ordinal: 0,
        parts: [
          { type: 'reasoning', text: 'before tool', reasoning_kind: 'raw' },
          { type: 'tool_call', tool_name: 'read_file', tool_call_id: 'a' },
          { type: 'tool_call', tool_name: 'read_file', tool_call_id: 'b' },
        ],
      },
    ]);
    let projection = initialProjection(run);
    let sequence = 0;
    const send = (type: string, rest: Record<string, unknown>) => {
      projection = reduceResponse(projection, {
        type,
        response_id: 'r',
        run_epoch: 1,
        sequence_number: ++sequence,
        ...rest,
      });
    };
    send('response.reasoning.delta', { reasoning_index: 0, kind: 'raw', delta: 'before tool' });
    send('response.output_item.added', {
      item: { type: 'function_call', call_id: 'a', name: 'read_file' },
    });
    send('response.reasoning.delta', { reasoning_index: 1, kind: 'raw', delta: 'after tool' });
    send('response.output_item.added', {
      item: { type: 'function_call', call_id: 'b', name: 'read_file' },
    });
    const merged = mergeDurableProjection(durable, projection.messages);
    expect(
      merged.map((m) =>
        m.role === 'tool-group' ? m.tools?.map((tool) => tool.id).join(',') : m.content,
      ),
    ).toEqual(['before tool', 'a', 'after tool', 'b']);
  });
  it('adopts each saved burst in order, including a final reasoning-only burst', () => {
    const durable = convertServerMessages([
      {
        id: 2,
        role: 'assistant',
        response_id: 'r',
        parts: [
          { type: 'reasoning', text: 'check', reasoning_kind: 'raw' },
          { type: 'tool_call', tool_name: 'read_file', tool_call_id: 'a' },
          { type: 'reasoning', text: 'check', reasoning_kind: 'raw' },
          { type: 'tool_call', tool_name: 'read_file', tool_call_id: 'b' },
          { type: 'reasoning', text: 'done', reasoning_kind: 'raw' },
        ],
      },
    ]);
    let projection = initialProjection(run);
    let sequence = 0;
    const send = (type: string, rest: Record<string, unknown>) => {
      projection = reduceResponse(projection, {
        type,
        response_id: 'r',
        run_epoch: 1,
        sequence_number: ++sequence,
        ...rest,
      });
    };
    for (const [index, callID] of ['a', 'b'].entries()) {
      send('response.reasoning.delta', { reasoning_index: index, kind: 'raw', delta: 'check' });
      send('response.output_item.added', {
        item: { type: 'function_call', call_id: callID, name: 'read_file' },
      });
    }
    send('response.reasoning.delta', { reasoning_index: 2, kind: 'raw', delta: 'done' });
    const merged = mergeDurableProjection(durable, projection.messages);
    expect(merged.map((m) => m.role)).toEqual([
      'reasoning',
      'tool-group',
      'reasoning',
      'tool-group',
      'reasoning',
    ]);
    expect(merged.filter((m) => m.role === 'reasoning').map((m) => m.content)).toEqual([
      'check',
      'check',
      'done',
    ]);
  });
  it.each([false, true])(
    'matches repeated thought boundaries without duplicating text (stream grows=%s)',
    (growing) => {
      const saved = convertServerMessages([
        {
          id: 12,
          role: 'assistant',
          response_id: 'r',
          assistant_segment_ordinal: 0,
          parts: [
            { type: 'reasoning', text: 'repeat', reasoning_kind: 'summary' },
            { type: 'text', text: 'A' },
            { type: 'reasoning', text: 'repeat', reasoning_kind: 'summary' },
            { type: 'text', text: 'A' },
          ],
        },
      ]);
      const projected = saved.map((message, index) => ({
        ...message,
        id: `live-${index}`,
        durableRowId: undefined,
        assistantSegmentOrdinal: message.role === 'assistant' ? (index - 1) / 2 : undefined,
        content: growing && index === 3 ? 'A more' : message.content,
      }));
      const merged = mergeDurableProjection(saved, projected);
      expect(merged.map((message) => [message.role, message.content])).toEqual([
        ['reasoning', 'repeat'],
        ['assistant', 'A'],
        ['reasoning', 'repeat'],
        ['assistant', growing ? 'A more' : 'A'],
      ]);
    },
  );
  it('prioritises stopping, retry, live thought, plan, tools and working', () => {
    const projection = { messages: [thought], retry: undefined };
    const plan = { plan: [{ step: 'Plan step', status: 'in_progress' as const }] };
    expect(
      responseActivity(projection, plan, 'streaming', {
        ...DEFAULT_THINKING,
        display: 'collapsed',
      }),
    ).toEqual({
      text: 'Checking the path',
      kind: 'thinking',
    });
    expect(responseActivity(projection, plan, 'cancelling').kind).toBe('stopping');
    expect(
      responseActivity(
        { ...projection, retry: { attempt: 2, delayMs: 1, error: 'busy' } },
        plan,
        'streaming',
      ).kind,
    ).toBe('retrying');
    const tools: Message = {
      id: 'tools',
      role: 'tool-group',
      content: '',
      created: 1,
      tools: [{ id: 'tool', name: 'read_file', status: 'running' }],
    };
    expect(responseActivity({ messages: [tools, thought] }, plan, 'streaming').kind).toBe('plan');
    expect(responseActivity({ messages: [tools, thought] }, null, 'streaming').kind).toBe('tool');
    expect(
      responseActivity(projection, null, 'streaming', { ...DEFAULT_THINKING, display: 'off' }).text,
    ).toBe('Thinking');
  });
});
