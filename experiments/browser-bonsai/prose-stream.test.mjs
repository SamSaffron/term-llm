import assert from 'node:assert/strict';
import { test } from 'node:test';
import { proseStream } from './web/prose-stream.mjs';
test('prose streams while split tool blocks stay private', () => {
  const chunks = [];
  const stream = proseStream(chunk => chunks.push(chunk));
  for (const chunk of ['I will read it.\n<to', 'ol_call><function=read_file>', '<parameter=path>README.md</parameter>', '</function></tool_', 'call>\nDone.']) stream.push(chunk);
  stream.end();
  assert.equal(chunks.join(''), 'I will read it.\n\nDone.');
});
test('ordinary less-than signs are not swallowed', () => {
  const chunks = [];
  const stream = proseStream(chunk => chunks.push(chunk));
  stream.push('1 < 2.'); stream.end();
  assert.equal(chunks.join(''), '1 < 2.');
});
