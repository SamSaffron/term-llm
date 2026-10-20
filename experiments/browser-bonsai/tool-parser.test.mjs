import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseToolResponse } from './web/tool-parser.mjs';
const tools = [{ type: 'function', function: { name: 'read_file', parameters: { type: 'object', properties: { path: { type: 'string' }, start_line: { type: 'integer' } } } } }];
test('Qwen3.5 XML calls preserve string arguments and type numeric parameters', () => {
  const result = parseToolResponse('Reading it.\n<tool_call>\n<function=read_file>\n<parameter=path>notes.txt</parameter>\n<parameter=start_line>1</parameter>\n</function>\n</tool_call>', tools);
  assert.equal(result.content, 'Reading it.');
  assert.deepEqual(JSON.parse(result.tool_calls[0].function.arguments), { path: 'notes.txt', start_line: 1 });
});
test('JSON-style native calls are supported', () => {
  assert.equal(parseToolResponse('<tool_call>{"name":"read_file","arguments":{"path":"hi"}}</tool_call>', tools).tool_calls[0].function.name, 'read_file');
});
test('truncated and undeclared calls fail explicitly', () => {
  assert.throws(() => parseToolResponse('<tool_call>{"name":"shell","arguments":{}}</tool_call>', tools), /undeclared/);
  assert.throws(() => parseToolResponse('<tool_call><function=read_file>', tools), /Incomplete/);
  assert.throws(() => parseToolResponse('<tool_call><function=read_file><parameter=start_line>banana</parameter></function></tool_call>', tools), /Invalid integer/);
});
