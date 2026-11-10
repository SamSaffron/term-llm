import { describe, expect, it } from 'vitest';
import { codeModeDisplayTool } from './code-mode';
import type { ToolCall } from './types';

describe('code-mode visible result', () => {
  const output = [{ kind: 'text', value: 'selected result' }];
  const envelope = {
    status: 'completed',
    cell_id: 'cell',
    state_committed: true,
    state: { cursor: 42 },
    output,
    calls: [{ name: 'computer__click' }],
  };
  const result = JSON.stringify(envelope);
  it('omits only the trailing machine envelope and keeps the original audit data intact', () => {
    const tool: ToolCall = {
      id: 'cell',
      name: 'code_mode',
      status: 'done',
      result,
      contentParts: [
        { type: 'text', text: 'selected result' },
        { type: 'image', url: '/frame.png' },
        { type: 'text', text: '\n' + result },
      ],
    };
    const display = codeModeDisplayTool(tool);
    expect(display.contentParts).toEqual(tool.contentParts?.slice(0, 2));
    expect(display.result).toBeUndefined();
    expect(tool.contentParts).toHaveLength(3);
    expect(tool.result).toBe(result);
  });
  it('handles live ordered output without a legacy result field', () => {
    const tool: ToolCall = {
      id: 'cell',
      name: 'code_mode',
      status: 'done',
      contentParts: [
        { type: 'text', text: 'selected result' },
        { type: 'text', text: '\n' + result },
      ],
    };
    expect(codeModeDisplayTool(tool).contentParts).toEqual([
      { type: 'text', text: 'selected result' },
    ]);
  });
  it('preserves deliberately emitted JSON even when it is identical to the envelope', () => {
    const tool: ToolCall = {
      id: 'cell',
      name: 'code_mode',
      status: 'done',
      result,
      contentParts: [
        { type: 'text', text: result },
        { type: 'text', text: '\n' + result },
      ],
    };
    expect(codeModeDisplayTool(tool).contentParts).toEqual([{ type: 'text', text: result }]);
  });
  it('uses emitted output for older results without content parts, not state or call traces', () => {
    expect(
      codeModeDisplayTool({ id: 'cell', name: 'code_mode', status: 'done', result }).contentParts,
    ).toEqual([{ type: 'text', text: 'selected result' }]);
  });
  it('keeps failures visible without exposing the audit envelope', () => {
    const failed = JSON.stringify({ ...envelope, status: 'failed', error: 'deadline exceeded' });
    expect(
      codeModeDisplayTool({ id: 'cell', name: 'code_mode', status: 'error', result: failed })
        .result,
    ).toBe('deadline exceeded');
  });
  it.each(['spawn_script', 'wait_script', 'cancel_script'])(
    'projects %s observations using both call and cell identities',
    (name) => {
      const modern = JSON.stringify({
        ...envelope,
        cell_id: 'original-cell',
        tool_call_id: 'observation',
      });
      const tool: ToolCall = {
        id: 'observation',
        name,
        status: 'done',
        result: modern,
        contentParts: [
          { type: 'text', text: 'selected result' },
          { type: 'text', text: modern },
        ],
      };
      expect(codeModeDisplayTool(tool).contentParts).toEqual([
        { type: 'text', text: 'selected result' },
      ]);
      expect(codeModeDisplayTool(tool).result).toBeUndefined();
      expect(tool.result).toBe(modern);
      expect(codeModeDisplayTool({ ...tool, id: 'foreign-call' })).toEqual({
        ...tool,
        id: 'foreign-call',
      });
    },
  );
  it.each([
    ['running', 'Script is still running.'],
    ['yielded', 'Script is still running.'],
    ['cancelling', 'Script cancellation is pending.'],
    ['cancelled', 'Script was cancelled.'],
  ])('keeps the %s lifecycle status visible without machine JSON', (status, text) => {
    const raw = JSON.stringify({ ...envelope, tool_call_id: 'observation', status });
    const display = codeModeDisplayTool({
      id: 'observation',
      name: 'wait_script',
      status: 'done',
      result: raw,
    });
    expect(display.contentParts?.at(-1)).toEqual({ type: 'text', text });
    expect(display.result).toBeUndefined();
  });
  it('keeps output-limit notices visible', () => {
    const raw = JSON.stringify({
      ...envelope,
      tool_call_id: 'observation',
      has_more_output: true,
      output_truncated: true,
    });
    const display = codeModeDisplayTool({
      id: 'observation',
      name: 'wait_script',
      status: 'done',
      result: raw,
    });
    expect(display.contentParts?.slice(-2)).toEqual([
      { type: 'text', text: 'More output is available.' },
      { type: 'text', text: 'Output was truncated.' },
    ]);
  });
  it('does not reinterpret ordinary results or a mismatched cell identity', () => {
    const tool: ToolCall = { id: 'another', name: 'code_mode', status: 'done', result };
    expect(codeModeDisplayTool(tool)).toBe(tool);
    expect(codeModeDisplayTool({ ...tool, name: 'read_file' }).result).toBe(result);
  });
});
