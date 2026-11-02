import { describe, expect, it } from 'vitest';
import { thinkingTranscript } from './thinking-transcript';
import { DEFAULT_THINKING } from './thinking';
import type { Message } from './types';

const thought: Message = {
  id: 'thought',
  role: 'reasoning',
  content: 'plan',
  reasoningKind: 'summary',
  created: 1,
};
const group = (id: string, running = false): Message => ({
  id,
  role: 'tool-group',
  content: '',
  created: 1,
  responseId: 'r',
  toolGroupClosed: true,
  tools: [{ id, name: 'read_file', status: running ? 'running' : 'done' }],
});

describe('thinking-aware transcript grouping', () => {
  it.each(['off', 'status'])(
    'joins groups across hidden thinking in %s without modifying source',
    (display) => {
      const messages = [
        group('one'),
        thought,
        group('two', true),
        { ...thought, id: 'next' },
        group('three'),
      ];
      const result = thinkingTranscript(messages, { ...DEFAULT_THINKING, display });
      expect(result).toHaveLength(1);
      expect(result[0].tools?.map((tool) => tool.id)).toEqual(['one', 'two', 'three']);
      expect(result[0].status).toBe('running');
      expect(result[0].id).toBe('one');
      expect(messages).toHaveLength(5);
      expect(messages[0].tools).toHaveLength(1);
    },
  );
  it.each(['collapsed', 'expanded', 'raw'])(
    'preserves visible thought boundaries in %s',
    (display) => {
      const messages = [group('one'), thought, group('two')];
      expect(thinkingTranscript(messages, { ...DEFAULT_THINKING, display, raw: true })).toBe(
        messages,
      );
    },
  );
  it.each(['none', 'transcript_only'])(
    'joins when history=%s even if display is expanded',
    (history) => {
      expect(
        thinkingTranscript([group('one'), thought, group('two')], {
          ...DEFAULT_THINKING,
          display: 'expanded',
          history,
        }),
      ).toHaveLength(1);
    },
  );
  it.each(['unknown', 'encrypted', ''])(
    'ignores non-visible kind %s as a tool boundary',
    (reasoningKind) => {
      expect(
        thinkingTranscript([group('one'), { ...thought, reasoningKind }, group('two')], {
          ...DEFAULT_THINKING,
          display: 'collapsed',
        }),
      ).toHaveLength(1);
    },
  );
  it('does not treat an empty thought as visible', () => {
    expect(
      thinkingTranscript([group('one'), { ...thought, content: '' }, group('two')], {
        ...DEFAULT_THINKING,
        display: 'collapsed',
      }),
    ).toHaveLength(1);
  });
  it.each(['assistant', 'user', 'model-swap', 'compaction-boundary'] as const)(
    'retains %s boundaries',
    (role) => {
      const messages = [
        group('one'),
        thought,
        { id: 'boundary', role, content: 'boundary', created: 2 },
        group('two'),
      ];
      expect(thinkingTranscript(messages, DEFAULT_THINKING).map((message) => message.role)).toEqual(
        ['tool-group', role, 'tool-group'],
      );
    },
  );
  it('never joins groups across responses, or unrelated adjacent closed groups', () => {
    const first = group('one');
    const second = { ...group('two'), responseId: 'other' };
    expect(thinkingTranscript([first, thought, second], DEFAULT_THINKING)).toEqual([first, second]);
    const messages = [first, group('two')];
    expect(thinkingTranscript(messages, DEFAULT_THINKING)).toBe(messages);
  });
  it('preserves source identities and tool details while deduplicating tools', () => {
    const first = {
      ...group('one'),
      durableRowId: 1,
      durableSourceRowIds: [1],
      durableSourceHead: { id: 1 },
    };
    const second = {
      ...group('two'),
      durableRowId: 2,
      durableSourceRowIds: [2],
      durableSourceHead: { id: 2 },
      tools: [{ ...first.tools![0], result: 'updated' }, ...group('two').tools!],
    };
    const result = thinkingTranscript([first, thought, second], DEFAULT_THINKING);
    expect(result[0]).toMatchObject({
      id: 'one',
      durableSourceRowIds: [1, 2],
      durableSourceHead: { id: 2 },
    });
    expect(result[0].tools?.map((tool) => tool.id)).toEqual(['one', 'two']);
    expect(result[0].tools?.[0].result).toBe('updated');
    expect(first.tools![0].result).toBeUndefined();
  });
});
