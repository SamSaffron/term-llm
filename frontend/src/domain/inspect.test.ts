import { describe, expect, it } from 'vitest';
import { SLASH_COMMANDS } from './completions';
import {
  activeInspectModel,
  formatInspectModel,
  sameInspectModel,
  INSPECT_CATEGORIES,
  filterInspectBlocks,
  inspectBodyKey,
  inspectTailStarts,
  inspectTurnStarts,
  inspectWindow,
  type InspectBlock,
} from './inspect';

const block = (
  id: string,
  kind: InspectBlock['kind'],
  extra: Partial<InspectBlock> = {},
): InspectBlock => ({ id, kind, ...extra });

describe('inspect projection helpers', () => {
  it('registers a streaming-safe inspect command', () => {
    expect(SLASH_COMMANDS.find((entry) => entry.command === '/inspect')).toMatchObject({
      streamingSafe: true,
    });
  });

  it('applies tool and reasoning filters to compaction tails as well', () => {
    const tail = block('m1.p0.tool_result', 'tool_result', { compaction_tail: true });
    expect(filterInspectBlocks([tail], new Set(['tools']))).toEqual([]);
    expect(filterInspectBlocks([tail], new Set(['compaction']))).toEqual([]);
  });
  it('toggles hidden categories without hiding ordinary conversation rows', () => {
    const blocks = [
      block('m1.p0.system', 'system'),
      block('m2.p0.developer', 'developer'),
      block('m3.p0.goal_steering', 'goal_steering'),
      block('m4.p0.path_note', 'path_note'),
      block('m5.p0.reasoning', 'reasoning'),
      block('m6.p0.tool_call', 'tool_call'),
      block('m7.p0.tool_result', 'tool_result'),
      block('m8.p0.compaction_summary', 'compaction_summary'),
      block('m9.p0.assistant_text', 'assistant_text', { compaction_tail: true }),
      block('m10.p0.user', 'user'),
      block('m11.p0.assistant_text', 'assistant_text'),
    ];
    expect(filterInspectBlocks(blocks, new Set())).toEqual(blocks);
    expect(
      filterInspectBlocks(blocks, new Set(['system', 'reasoning', 'tools', 'compaction'])),
    ).toEqual(blocks.slice(-2));
    expect(filterInspectBlocks(blocks, new Set(['reasoning']))).not.toContain(blocks[4]);
  });

  it('invalidates bodies when their full size changes, not when previews change', () => {
    expect(inspectBodyKey({ id: 'm1.p0.user', total_bytes: 3 })).toBe('m1.p0.user:3');
    expect(inspectBodyKey({ id: 'm1.p0.user', total_bytes: 4 })).not.toBe(
      inspectBodyKey({ id: 'm1.p0.user', total_bytes: 3 }),
    );
    expect(inspectBodyKey({ id: 'runtime.system_prompt' })).toBe('runtime.system_prompt:0');
  });

  it('windows the latest 200 blocks and reveals earlier blocks without reordering', () => {
    const blocks = Array.from({ length: 450 }, (_, index) => block(`m${index}.p0.user`, 'user'));
    expect(inspectWindow(blocks)).toEqual({ blocks: blocks.slice(250), earlier: 250 });
    expect(inspectWindow(blocks, 400)).toEqual({ blocks: blocks.slice(50), earlier: 50 });
    expect(inspectWindow(blocks, 600)).toEqual({ blocks, earlier: 0 });
    expect(inspectWindow([])).toEqual({ blocks: [], earlier: 0 });
  });

  it('separates user messages, not each part, and marks contiguous compaction tails', () => {
    const blocks = [
      block('m1.p0.user', 'user', { message_id: '1' }),
      block('m1.p1.user', 'user', { message_id: '1' }),
      block('m2.p0.assistant_text', 'assistant_text'),
      block('i3.p0.user', 'user'),
      block('m4.p0.tool_result', 'tool_result', {
        compaction_tail: true,
        compaction_summary_id: 'm2.compaction_summary',
      }),
      block('m5.p0.assistant_text', 'assistant_text', {
        compaction_tail: true,
        compaction_summary_id: 'm2.compaction_summary',
      }),
      block('m6.p0.user', 'user', {
        compaction_tail: true,
        compaction_summary_id: 'm6.compaction_summary',
      }),
    ];
    expect([...inspectTurnStarts(blocks)]).toEqual(['m1.p0.user', 'i3.p0.user', 'm6.p0.user']);
    expect([...inspectTailStarts(blocks)]).toEqual(['m4.p0.tool_result', 'm6.p0.user']);
  });
});

describe('inspect model timeline', () => {
  const initial = { provider: 'openai', model: 'gpt-5', effort: 'high' };
  const target = { provider: 'claude-bin', model: 'opus', effort: 'max', agent: 'reviewer' };
  const change = (status = 'succeeded') =>
    block('m2.p0.model_change', 'model_change', {
      compaction_tail: true,
      model_change: { from: initial, to: target, strategy: 'handover', status },
    });

  it('keeps change points visible under every filter combination, even in tails', () => {
    const marker = change();
    const handover = block('m3.p0.handover_document', 'handover_document');
    for (let mask = 0; mask < 16; mask++) {
      const hidden = new Set(INSPECT_CATEGORIES.filter((_, i) => mask & (1 << i)).map((c) => c.id));
      const result = filterInspectBlocks([marker, handover], hidden);
      expect(result).toContain(marker);
      expect(result.includes(handover)).toBe(!hidden.has('system'));
    }
  });

  it.each([
    ['succeeded', target],
    ['failed', initial],
    ['started', target],
    ['', target],
  ] as const)('tracks %s changes preceding the visible window', (status, expected) => {
    const marker = change(status);
    const blocks = [block('m1.p0.user', 'user'), marker, block('m3.p0.user', 'user')];
    expect(activeInspectModel(initial, blocks, blocks[0].id)).toEqual(initial);
    expect(activeInspectModel(initial, blocks, marker.id)).toEqual(initial);
    expect(activeInspectModel(initial, blocks, blocks[2].id)).toEqual(expected);
  });

  it('tracks multiple changes and preserves missing historical fields instead of guessing', () => {
    const unknown = block('m4.p0.model_change', 'model_change', {
      model_change: { from: target, to: {}, status: 'succeeded' },
    });
    expect(activeInspectModel(initial, [change(), unknown], undefined)).toEqual({});
    expect(activeInspectModel(initial, [], undefined)).toEqual(initial);
  });

  it('formats unknown models, default effort, mode and agent, and compares model fields', () => {
    expect(formatInspectModel({})).toBe('unknown:unknown · Thinking: default');
    expect(formatInspectModel({ ...initial, reasoning_mode: 'pro', agent: 'dev' })).toBe(
      'openai:gpt-5 · Thinking: high · pro · @dev',
    );
    expect(sameInspectModel({}, { effort: '' })).toBe(true);
    expect(sameInspectModel(initial, { ...initial, effort: 'low' })).toBe(false);
    expect(sameInspectModel(initial, { ...initial, agent: 'dev' })).toBe(false);
  });
});
