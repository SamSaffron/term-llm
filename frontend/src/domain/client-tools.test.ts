import { describe, expect, it } from 'vitest';
import {
  CLIENT_TOOL_PREFIX,
  clientToolDefinitions,
  isValidClientToolName,
  parseToolArguments,
  patchToolCalls,
  pendingClientCalls,
  stringifyToolResult,
  toolOutputItem,
  type ClientTool,
} from './client-tools';
import type { Message, ToolCall } from './types';

const tool = (name: string, extra: Partial<ClientTool> = {}): ClientTool => ({
  name,
  title: '',
  description: `${name} tool`,
  inputSchema: { type: 'object', properties: {} },
  readOnly: false,
  ...extra,
});

const group = (responseId: string, tools: Partial<ToolCall>[], id = tools[0]?.id): Message => ({
  id: `${responseId}:tools:${id}`,
  role: 'tool-group',
  content: '',
  created: 1,
  responseId,
  tools: tools.map((entry) => ({ status: 'done', name: 'tool', id: 'x', ...entry }) as ToolCall),
});

const text = (responseId: string, content: string): Message => ({
  id: `${responseId}:assistant:${content}`,
  role: 'assistant',
  content,
  created: 1,
  responseId,
});

describe('client tool definitions', () => {
  it('namespaces page tools and says where they run', () => {
    expect(
      clientToolDefinitions([tool('ping', { inputSchema: { type: 'object', x: 1 } })], 'iPhone'),
    ).toEqual([
      {
        type: 'function',
        name: `${CLIENT_TOOL_PREFIX}ping`,
        description: "ping tool Runs on the user's iPhone.",
        parameters: { type: 'object', x: 1 },
      },
    ]);
    expect(clientToolDefinitions([tool('ping')], '')[0].description).toBe('ping tool');
  });

  it('skips names providers reject and duplicates', () => {
    const names = clientToolDefinitions(
      [tool('ok'), tool('has space'), tool(''), tool('x'.repeat(57)), tool('ok'), tool('a-b_9')],
      '',
    ).map((definition) => definition.name);
    expect(names).toEqual([`${CLIENT_TOOL_PREFIX}ok`, `${CLIENT_TOOL_PREFIX}a-b_9`]);
    expect(isValidClientToolName('x'.repeat(56))).toBe(true);
  });

  it('falls back to the title or name when there is no description', () => {
    const [titled, bare] = clientToolDefinitions(
      [tool('a', { description: '', title: 'Alpha' }), tool('b', { description: '' })],
      '',
    );
    expect(titled.description).toBe('Alpha');
    expect(bare.description).toBe('b');
  });
});

describe('pendingClientCalls', () => {
  const ping = { id: 'call_1', name: `${CLIENT_TOOL_PREFIX}ping`, arguments: '{"message":"hi"}' };

  it('returns the client calls that ended the response', () => {
    const messages = [text('r1', 'checking'), group('r1', [ping])];
    expect(pendingClientCalls(messages, 'r1')).toEqual([
      { callId: 'call_1', name: 'ping', arguments: '{"message":"hi"}' },
    ]);
  });

  it('ignores client calls made alongside server tools, which the server drops', () => {
    const mixed = group('r1', [{ id: 'call_s', name: 'shell' }, ping]);
    const later = group('r1', [{ id: 'call_2', name: `${CLIENT_TOOL_PREFIX}info` }]);
    expect(pendingClientCalls([mixed], 'r1')).toEqual([]);
    expect(pendingClientCalls([mixed, text('r1', 'more'), later], 'r1')).toEqual([
      { callId: 'call_2', name: 'info', arguments: '{}' },
    ]);
  });

  it('only looks at the given response', () => {
    expect(pendingClientCalls([group('r0', [ping])], 'r1')).toEqual([]);
    expect(pendingClientCalls([group('r1', [{ id: 'call_s', name: 'shell' }])], 'r1')).toEqual([]);
  });
});

describe('client tool helpers', () => {
  it('parses object arguments only', () => {
    expect(parseToolArguments('')).toEqual({});
    expect(parseToolArguments(' {"a":1} ')).toEqual({ a: 1 });
    expect(() => parseToolArguments('[1]')).toThrow('JSON object');
    expect(() => parseToolArguments('null')).toThrow('JSON object');
    expect(() => parseToolArguments('{')).toThrow();
  });

  it('turns results into model-readable text', () => {
    expect(stringifyToolResult('pong')).toBe('pong');
    expect(stringifyToolResult({ ok: true })).toBe('{"ok":true}');
    expect(stringifyToolResult(undefined)).toBe('');
    expect(stringifyToolResult(null)).toBe('');
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    expect(stringifyToolResult(cyclic)).toBe('[object Object]');
    expect(stringifyToolResult(Symbol('tag'))).toBe('Symbol(tag)');
  });

  it('builds function_call_output items', () => {
    expect(toolOutputItem('call_1', 'pong')).toEqual({
      type: 'function_call_output',
      call_id: 'call_1',
      output: 'pong',
    });
  });

  it('patches only the listed tool calls', () => {
    const messages = [
      text('r1', 'hi'),
      group('r1', [
        { id: 'a', startedAt: 10 },
        { id: 'b', startedAt: 20 },
      ]),
    ];
    const patched = patchToolCalls(messages, new Set(['b']), (entry) => ({
      status: 'error',
      durationMs: 30 - (entry.startedAt || 0),
    }));
    expect(patched[0]).toBe(messages[0]);
    expect(patched[1].tools).toEqual([
      expect.objectContaining({ id: 'a', status: 'done' }),
      expect.objectContaining({ id: 'b', status: 'error', durationMs: 10 }),
    ]);
    expect(patchToolCalls(messages, new Set(['zzz']), { status: 'error' })[1]).toBe(messages[1]);
  });
});
