import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  CONTEXT,
  MODEL_FILE,
  MODEL_REVISION,
  MODEL_URL,
  MODEL_ORIGIN_URL,
  MODEL_CACHE_KEY,
  adaptCache,
  adaptProgress,
  createBonsaiSession,
  normalizeRequest,
  toChatMessages,
} from './web/inference.mjs';

const tools = [{
  type: 'function',
  function: {
    name: 'read_file',
    description: 'Read a file',
    parameters: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'] },
  },
}];

function fakeEngine({ chunks = ['Hello'], reason = 'eos', promptTokens = 12, newTokens = 2, contextLength = CONTEXT, promptLength = 8 } = {}) {
  const calls = { render: [], chat: [], dispose: 0 };
  return {
    calls,
    info: { contextLength },
    renderPrompt(messages, opts) {
      calls.render.push({ messages, opts });
      return 'x'.repeat(promptLength);
    },
    encode(text) {
      return Array.from({ length: text.length }, (_, index) => index);
    },
    async *chat(messages, opts) {
      calls.chat.push({ messages, opts });
      for (const text of chunks) {
        if (opts.signal?.aborted) throw new DOMException('Aborted', 'AbortError');
        yield { type: 'token', phase: 'answer', token: 0, text };
      }
      yield { type: 'done', reason, stats: { promptTokens, newTokens } };
    },
    dispose() {
      calls.dispose += 1;
    },
  };
}

test('request normalization caps output and rejects forced tool calls', () => {
  const messages = [{ role: 'user', content: 'hi' }];
  assert.throws(() => normalizeRequest({ messages, tool_choice: 'required' }), /forced calls/);
  const none = normalizeRequest({ messages, tools, tool_choice: 'none' });
  assert.deepEqual(none.tools, []);
  assert.equal(normalizeRequest({ messages, max_tokens: 99_999 }).maxTokens, 2048);
  assert.equal(normalizeRequest({ messages, max_completion_tokens: 'nope' }).maxTokens, 1024);
});

test('OpenAI messages become Talk chat messages with structured tool turns', () => {
  const converted = toChatMessages([
    { role: 'system', content: 'sys' },
    {
      role: 'assistant',
      content: null,
      tool_calls: [{ id: 'call_1', type: 'function', function: { name: 'read_file', arguments: '{"path":"README.md"}' } }],
    },
    { role: 'tool', content: [{ type: 'text', text: 'file body' }] },
    { role: 'user', content: 'go on' },
  ]);
  assert.deepEqual(converted[1], {
    role: 'assistant',
    content: '',
    toolCalls: [{ name: 'read_file', arguments: { path: 'README.md' } }],
  });
  assert.equal(converted[2].content, 'file body');
  assert.throws(
    () => toChatMessages([{ role: 'user', content: [{ type: 'image_url', image_url: { url: 'x' } }] }]),
    /text messages only/,
  );
  assert.throws(
    () => toChatMessages([{ role: 'assistant', content: '', tool_calls: [{ function: { name: 'x', arguments: '{' } }] }]),
    /Invalid JSON arguments/,
  );
});

test('complete streams each native answer chunk once and returns the final response', async () => {
  const engine = fakeEngine({ chunks: ['Hello', ' world'] });
  const session = createBonsaiSession(engine, { contextLength: 128 });
  const streamed = [];
  const result = await session.complete(
    { messages: [{ role: 'user', content: 'hi' }], max_tokens: 8 },
    { onText: text => streamed.push(text) },
  );

  assert.deepEqual(streamed, ['Hello', ' world']);
  assert.equal(result.choices[0].message.content, 'Hello world');
  assert.equal(result.choices[0].finish_reason, 'stop');
  assert.equal(result.usage.prompt_tokens, 12);
  assert.equal(result.usage.completion_tokens, 2);
  assert.equal(result.usage.total_tokens, 14);
  assert.equal(engine.calls.chat[0].opts.thinking, false);
  assert.deepEqual(engine.calls.chat[0].opts.tools, []);
});

test('complete parses XML tool calls while streaming the raw token chunks', async () => {
  const chunks = [
    'Reading it.\n',
    '<tool_call>\n<function=read_file>\n<parameter=path>README.md</parameter>\n</function>\n</tool_call>',
  ];
  const engine = fakeEngine({ chunks, newTokens: 9 });
  const session = createBonsaiSession(engine, { contextLength: 128 });
  const streamed = [];
  const result = await session.complete(
    { messages: [{ role: 'user', content: 'read it' }], tools, max_tokens: 32 },
    { onText: text => streamed.push(text) },
  );

  assert.deepEqual(streamed, chunks);
  assert.equal(result.choices[0].finish_reason, 'tool_calls');
  assert.equal(result.choices[0].message.content, 'Reading it.');
  assert.deepEqual(JSON.parse(result.choices[0].message.tool_calls[0].function.arguments), { path: 'README.md' });
  assert.deepEqual(engine.calls.chat[0].opts.tools, tools);
});

test('complete rejects a prompt plus output budget that exceeds the context', async () => {
  const engine = fakeEngine({ contextLength: 10, promptLength: 8 });
  const session = createBonsaiSession(engine, { contextLength: 10 });
  await assert.rejects(
    session.complete({ messages: [{ role: 'user', content: 'hi' }], max_tokens: 4 }),
    /exceeds this demo's 10-token context/,
  );
});

test('complete forwards an abort before and during generation', async () => {
  const engine = fakeEngine({ chunks: ['one', 'two'] });
  const session = createBonsaiSession(engine, { contextLength: 128 });
  const stopped = new AbortController();
  stopped.abort();
  await assert.rejects(
    session.complete({ messages: [{ role: 'user', content: 'hi' }] }, { signal: stopped.signal }),
    { name: 'AbortError' },
  );

  const during = new AbortController();
  await assert.rejects(session.complete(
    { messages: [{ role: 'user', content: 'hi' }], max_tokens: 8 },
    { signal: during.signal, onText: () => during.abort() },
  ), { name: 'AbortError' });
});

test('dispose releases the engine and refuses further work', async () => {
  const engine = fakeEngine();
  const session = createBonsaiSession(engine, { contextLength: 128 });
  await session.dispose();
  assert.equal(engine.calls.dispose, 1);
  await assert.rejects(
    session.complete({ messages: [{ role: 'user', content: 'hi' }] }),
    /disposed/,
  );
});

test('progress events map to the demo gate shape', () => {
  assert.deepEqual(
    adaptProgress({ stage: 'weights', loaded: 50, total: 200, tensor: 'blk.0' }),
    { status: 'Downloading weights', file: '50 B / 200 B · blk.0', progress: 25 },
  );
  assert.deepEqual(
    adaptProgress({ stage: 'ready', message: 'ok' }),
    { status: 'Model ready', file: 'ok', progress: 100 },
  );
  const mixed = adaptCache({ total: 100, fromCache: 25, fromNetwork: 75 });
  assert.equal(mixed.file, '75 B downloaded · 25 B from cache · 100 B total');
  assert.equal(mixed.progress, 100);
  assert.equal(adaptCache({ total: 100, fromCache: 0, fromNetwork: 50 }).progress, 50);
});

test('the model pin stays on the official 2B PTQ1_0 file', () => {
  assert.equal(MODEL_FILE, 'Ternary-Bonsai-2-2B-PTQ1_0.gguf');
  assert.ok(MODEL_ORIGIN_URL.includes(MODEL_REVISION));
  assert.ok(MODEL_ORIGIN_URL.endsWith(`/${MODEL_FILE}`));
  assert.equal(MODEL_CACHE_KEY, MODEL_ORIGIN_URL);
  assert.equal(MODEL_URL, 'https://demo.term-llm.com/models/bonsai2-2b-ptq1-36061ad406df.bin');
});
