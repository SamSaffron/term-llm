import test from 'node:test';
import assert from 'node:assert/strict';
import { createDemoAPI } from './mock-api.mjs';
const request = (path, body, headers = {}, method = body ? 'POST' : 'GET') => new Request('https://demo.test/demo-api' + path, { method, headers: { 'Content-Type': 'application/json', ...headers }, ...(body ? { body: JSON.stringify(body) } : {}) });
const getSession = async (api, id) => (await api.fetch(request(`/v1/sessions?selected_only=1&selected_session=${id}`))).json();
const tool = name => ({ type: 'function', name: `webmcp__${name}`, description: `${name} on the device`, parameters: { type: 'object', properties: {} } });
const reply = (api, input, tools = []) => api.fetch(request('/v1/responses', {
  input: [{ type: 'message', role: 'user', content: input }], ...(tools.length ? { tools } : {}),
}, { 'X-Term-LLM-Session-ID': api.initialSession }));
const replyState = async (api, id) => (await api.fetch(request(`/v1/responses/${id}`))).json();
const sendResult = (api, id, callId, output) => api.fetch(request(
  `/v1/responses/${id}/client_tool_calls/${encodeURIComponent(callId)}/result`, { output }));
const events = stream => stream.split('\n\n').flatMap(block => {
  const lines = block.split('\n');
  const type = lines.find(line => line.startsWith('event: '))?.slice(7);
  const data = lines.find(line => line.startsWith('data: '))?.slice(6);
  if (!type || !data || data === '[DONE]') return [];
  return [{ type, data: JSON.parse(data) }];
});
const deltas = stream => events(stream)
  .filter(entry => entry.type === 'response.output_text.delta')
  .map(entry => entry.data.delta).join('');

test('native commands explain setup when tools are off, and say when an older app lacks one', async () => {
  const api = createDemoAPI({ delay: 0 });
  const unavailable = await reply(api, 'create playlist');
  const unavailableStream = await unavailable.text();
  assert.match(unavailableStream, /event: response.completed/);
  assert.doesNotMatch(unavailableStream, /response.client_tool.requested/);
  assert.match(deltas(unavailableStream), /Share device tools/);
  assert.match(deltas(unavailableStream), /Settings → Device tools/);
  assert.match(deltas(unavailableStream), /MCP servers/);
  assert.match(deltas(unavailableStream), /create playlist/);

  const oldApp = await reply(api, 'create playlist', [tool('ping'), tool('playlist_create_demo_extra')]);
  const oldStream = await oldApp.text();
  assert.doesNotMatch(oldStream, /response.client_tool.requested/);
  assert.match(deltas(oldStream), /Update the app/);
  assert.match(deltas(oldStream), /playlist_create_demo/);
});

test('create playlist requests the exact offered tool and streams the actual native result', async () => {
  const api = createDemoAPI({ delay: 0 });
  const response = await reply(api, 'create a demo playlist', [tool('playlist_create_demo'), tool('ping')]);
  const id = response.headers.get('X-Response-ID');
  const streamed = response.text();
  const state = await replyState(api, id);
  assert.equal(state.status, 'in_progress');
  assert.equal(state.pending_inline_client_tools.length, 1);
  const call = state.pending_inline_client_tools[0];
  assert.match(call.call_id, /^call_demo_\d+$/);
  assert.equal(call.name, 'webmcp__playlist_create_demo');
  assert.equal(call.arguments, '{}');

  const detail = { id: 'playlist-7', name: 'Fictional Sample Playlist', songs: 5,
    entries: Array.from({ length: 5 }, (_, index) => ({ title: `Sample ${index + 1}` })), sample_notice: 'Sample tracks are fictional.' };
  const delivered = await sendResult(api, id, call.call_id, JSON.stringify(detail));
  assert.equal(delivered.status, 200);
  const stream = await streamed;
  const finalText = deltas(stream);
  assert.match(stream, /event: response.client_tool.requested/);
  assert.match(finalText, /Fictional Sample Playlist/);
  assert.match(finalText, /5 songs/);
  assert.match(finalText, /saved locally/);
  assert.match(finalText, /term-llm’s native Music screen/);
  assert.match(finalText, /includes any songs already added/);
  assert.match(finalText, /Sample tracks are fictional/);
  assert.doesNotMatch(finalText, /could not read|reported an error/);
  assert.equal(events(stream).at(-1).type, 'response.completed');

  const after = await replyState(api, id);
  assert.equal(after.status, 'completed');
  assert.deepEqual(after.pending_inline_client_tools, []);
  const transcript = await getSession(api, api.initialSession);
  assert.match(transcript.selected_transcript.bodies.messages.at(-1).parts[0].text, /Fictional Sample Playlist/);
});

test('renamed or empty existing playlists report their actual name and song count', async () => {
  const api = createDemoAPI({ delay: 0 });
  const response = await reply(api, 'create playlist', [tool('playlist_create_demo')]);
  const id = response.headers.get('X-Response-ID');
  const streamed = response.text();
  const call = (await replyState(api, id)).pending_inline_client_tools[0];
  await sendResult(api, id, call.call_id, JSON.stringify({ id: 'playlist-3', name: 'My Renamed Mix', songs: 0,
    entries: [], sample_notice: 'This is the existing sample playlist.' }));
  const finalText = deltas(await streamed);
  assert.match(finalText, /My Renamed Mix/);
  assert.match(finalText, /no songs/);
  assert.doesNotMatch(finalText, /5 songs|5 sample songs|Fictional Sample Playlist/);
});

test('ping uses the fixed demo message and shows plain-text native output', async () => {
  const api = createDemoAPI({ delay: 0 });
  const response = await reply(api, 'ping', [tool('ping')]);
  const id = response.headers.get('X-Response-ID');
  const streamed = response.text();
  const call = (await replyState(api, id)).pending_inline_client_tools[0];
  assert.equal(call.name, 'webmcp__ping');
  assert.deepEqual(JSON.parse(call.arguments), { message: 'Hello from the demo' });
  const output = 'pong from term-llm iPhone app at 1970-01-01T00:00:00Z\necho: Hello from the demo';
  assert.equal((await sendResult(api, id, call.call_id, output)).status, 200);
  const finalText = deltas(await streamed);
  assert.match(finalText, /pong from term-llm iPhone app/);
  assert.match(finalText, /echo: Hello from the demo/);
  assert.match(finalText, /```text/);
});

test('list playlists and device info are read-only, concise JSON results', async () => {
  const api = createDemoAPI({ delay: 0 });
  const tools = [tool('playlist_list'), tool('device_info')];

  const listed = await reply(api, 'list playlists', tools);
  const listedId = listed.headers.get('X-Response-ID');
  const listedStream = listed.text();
  const listCall = (await replyState(api, listedId)).pending_inline_client_tools[0];
  assert.equal(listCall.name, 'webmcp__playlist_list');
  await sendResult(api, listedId, listCall.call_id, JSON.stringify([{ id: 'p1', name: 'Fictional Sample Playlist', songs: 5 }]));
  const listText = deltas(await listedStream);
  assert.match(listText, /```json/);
  assert.match(listText, /Fictional Sample Playlist/);
  assert.match(listText, /Read-only result from the device tool/);
  assert.match(listText, /Lists playlists saved in term-llm on this device \(including any existing playlists\); does not query Apple Music catalog\./);
  assert.doesNotMatch(listText, /demo playlists only|cannot read your music library|control playback/);

  const info = await reply(api, 'device info', tools);
  const infoId = info.headers.get('X-Response-ID');
  const infoStream = info.text();
  const infoCall = (await replyState(api, infoId)).pending_inline_client_tools[0];
  assert.equal(infoCall.name, 'webmcp__device_info');
  assert.equal(infoCall.arguments, '{}');
  await sendResult(api, infoId, infoCall.call_id, JSON.stringify({ platform: 'iOS', webmcp: true }));
  const infoText = deltas(await infoStream);
  assert.match(infoText, /```json/);
  assert.match(infoText, /"platform": "iOS"/);
  assert.match(infoText, /shared device-tools bridge/);
});

test('native errors become visible failures, never success', async () => {
  const api = createDemoAPI({ delay: 0 });
  const response = await reply(api, 'create playlist', [tool('playlist_create_demo')]);
  const id = response.headers.get('X-Response-ID');
  const streamed = response.text();
  const call = (await replyState(api, id)).pending_inline_client_tools[0];
  await sendResult(api, id, call.call_id, 'Error: Apple Music is not authorized');
  const finalText = deltas(await streamed);
  assert.match(finalText, /device reported an error/);
  assert.match(finalText, /Apple Music is not authorized/);
  assert.match(finalText, /can't confirm/);
  assert.doesNotMatch(finalText, /saved locally|demo playlist `/);
});

test('malformed results are rejected or reported, never shown as success', async () => {
  const api = createDemoAPI({ delay: 0 });
  const response = await reply(api, 'create playlist', [tool('playlist_create_demo')]);
  const id = response.headers.get('X-Response-ID');
  const streamed = response.text();
  const call = (await replyState(api, id)).pending_inline_client_tools[0];

  const wrongType = await sendResult(api, id, call.call_id, 5);
  assert.equal(wrongType.status, 400);
  const empty = await sendResult(api, id, call.call_id, '   ');
  assert.equal(empty.status, 400);
  assert.equal((await replyState(api, id)).pending_inline_client_tools.length, 1);

  await sendResult(api, id, call.call_id, 'not JSON');
  const finalText = deltas(await streamed);
  assert.match(finalText, /could not read/);
  assert.match(finalText, /can't confirm/);
  assert.doesNotMatch(finalText, /saved locally|with \d+ songs|The device reports playlist/);
});

test('repeated and unknown inline results are idempotent or rejected', async () => {
  const api = createDemoAPI({ delay: 0 });
  const response = await reply(api, 'create playlist', [tool('playlist_create_demo')]);
  const id = response.headers.get('X-Response-ID');
  const streamed = response.text();
  const call = (await replyState(api, id)).pending_inline_client_tools[0];

  const mismatched = await api.fetch(request(`/v1/responses/${id}/client_tool_calls/other/result`, { output: '{}' }));
  assert.equal(mismatched.status, 404);
  assert.equal((await replyState(api, id)).pending_inline_client_tools[0].call_id, call.call_id);

  const output = JSON.stringify({ id: 'p9', name: 'Replay-safe Playlist', songs: 5, entries: Array.from({ length: 5 }, () => ({ title: 'Sample', sample: true })) });
  assert.equal((await sendResult(api, id, call.call_id, output)).status, 200);
  await streamed;
  const settled = await replyState(api, id);
  assert.equal(settled.status, 'completed');
  assert.equal((await sendResult(api, id, call.call_id, output)).status, 200);
  const repeated = await replyState(api, id);
  assert.equal(repeated.last_sequence_number, settled.last_sequence_number);
  assert.equal((await sendResult(api, id, 'call_demo_unknown', output)).status, 404);
  assert.equal((await api.fetch(request('/v1/responses/reply-unknown/client_tool_calls/x/result', { output: '{}' }))).status, 404);
});

test('a silent device times out honestly, and late results cannot claim success', async () => {
  const api = createDemoAPI({ delay: 0, toolTimeout: 20 });
  const response = await reply(api, 'create playlist', [tool('playlist_create_demo')]);
  const id = response.headers.get('X-Response-ID');
  const stream = await response.text();
  const finalText = deltas(stream);
  assert.match(finalText, /did not answer/);
  assert.match(finalText, /20 ms/);
  assert.match(finalText, /nothing is reported as successful/i);
  assert.doesNotMatch(finalText, /saved locally|sample songs/);
  assert.equal(events(stream).at(-1).type, 'response.failed');
  assert.match(JSON.stringify(events(stream).at(-1).data), /did not answer/);

  const timedOut = await replyState(api, id);
  assert.equal(timedOut.status, 'failed');
  assert.deepEqual(timedOut.pending_inline_client_tools, []);
  const late = await sendResult(api, id, 'call_demo_1', JSON.stringify({ name: 'Late Mix', songs: 5 }));
  assert.equal(late.status, 409);
  assert.equal((await replyState(api, id)).last_sequence_number, timedOut.last_sequence_number);
});

test('cancelling a pending native call warns and never completes, late results are refused', async () => {
  const api = createDemoAPI({ delay: 0, toolTimeout: 10_000 });
  const response = await reply(api, 'create playlist', [tool('playlist_create_demo')]);
  const id = response.headers.get('X-Response-ID');
  const streamed = response.text();
  const call = (await replyState(api, id)).pending_inline_client_tools[0];
  assert.equal((await api.fetch(request(`/v1/responses/${id}/cancel`, {}))).status, 200);
  const stream = await streamed;
  const finalText = deltas(stream);
  assert.match(finalText, /Stopped before the device answered/);
  assert.match(finalText, /cannot be undone/);
  assert.doesNotMatch(finalText, /demo playlist `/);
  assert.doesNotMatch(stream, /response.completed/);
  assert.equal(events(stream).at(-1).type, 'response.cancelled');
  assert.equal((await replyState(api, id)).status, 'cancelled');

  const late = await sendResult(api, id, call.call_id, JSON.stringify({ name: 'Stopped Mix', songs: 5 }));
  assert.equal(late.status, 409);
  assert.match((await late.json()).error.message, /may still have happened/);
});

test('welcome and help are honest, list the commands, and never call tools on startup', async () => {
  const api = createDemoAPI({ delay: 0, toolTimeout: 5 });
  const initial = await getSession(api, api.initialSession);
  assert.equal(initial.selected_transcript.bodies.messages.length, 1);
  const welcome = initial.selected_transcript.bodies.messages[0].parts[0].text;
  assert.match(welcome, /scripted/);
  assert.match(welcome, /no AI model/i);
  assert.match(welcome, /persist after you reload/i);
  assert.match(welcome, /fictional and not playable/);
  assert.match(welcome, /Share device tools/);
  assert.match(welcome, /♪/);
  assert.match(welcome, /create playlist/);
  await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal((await getSession(api, api.initialSession)).selected_transcript.bodies.messages.length, 1);
  assert.equal((await api.fetch(request('/v1/responses/reply-1'))).status, 404);

  const help = await reply(api, 'help', [tool('playlist_create_demo'), tool('ping')]);
  const helpStream = await help.text();
  assert.doesNotMatch(helpStream, /response.client_tool.requested/);
  const helpText = deltas(helpStream);
  for (const command of ['create playlist', 'list playlists', 'ping', 'device info'])
    assert.match(helpText, new RegExp(command));
  assert.match(helpText, /scripted/);
  assert.match(helpText, /Share device tools/);
  assert.match(helpText, /♪/);
});

test('scripted replies stream through the real frontend API contract and retain transcript', async () => {
  const api = createDemoAPI({ delay: 0 });
  const initial = await getSession(api, api.initialSession);
  assert.match(initial.selected_transcript.bodies.messages[0].parts[0].text, /scripted/);
  const response = await api.fetch(request('/v1/responses', { input: [{ type: 'message', role: 'user', content: 'hello' }] }, { 'X-Term-LLM-Session-ID': api.initialSession }));
  assert.equal(response.headers.get('X-Session-ID'), api.initialSession);
  const stream = await response.text();
  assert.match(stream, /event: response.created/);
  assert.match(stream, /event: response.output_text.delta/);
  assert.match(stream, /event: response.completed/);
  assert.match(stream, /data: \[DONE\]/);
  const session = await getSession(api, api.initialSession);
  assert.equal(session.selected_transcript.bodies.messages[1].parts[0].text, 'hello');
  assert.equal(session.selected_transcript.bodies.messages[2].response_id, response.headers.get('X-Response-ID'));
  assert.match(session.selected_transcript.bodies.messages[2].parts[0].text, /scripted UI demo/);
});

test('new chats, rename, delete, and independent page lifetimes', async () => {
  const api = createDemoAPI();
  const { session } = await (await api.fetch(request('/v1/sessions', {}))).json();
  await api.fetch(request(`/v1/sessions/${session.id}`, { title: 'Renamed' }, {}, 'PATCH'));
  assert.equal((await getSession(api, session.id)).selected_session.title, 'Renamed');
  const fresh = createDemoAPI();
  assert.equal((await getSession(fresh, session.id)).selected_session, null);
  await api.fetch(request(`/v1/sessions/${session.id}`, null, {}, 'DELETE'));
  assert.equal((await getSession(api, session.id)).selected_session, null);
});

test('stop cancels an in-progress canned reply without claiming completion', async () => {
  const api = createDemoAPI({ delay: 20 });
  const response = await api.fetch(request('/v1/responses', { input: 'hello' }, { 'X-Term-LLM-Session-ID': api.initialSession }));
  const id = response.headers.get('X-Response-ID');
  await api.fetch(request(`/v1/responses/${id}/cancel`, {}));
  const stream = await response.text();
  assert.match(stream, /response.cancelled/);
  assert.doesNotMatch(stream, /response.completed/);
  assert.equal((await (await api.fetch(request(`/v1/responses/${id}`))).json()).status, 'cancelled');
});

test('unsupported execution is disabled, not faked', async () => {
  const api = createDemoAPI();
  const capabilities = await (await api.fetch(request('/v1/capabilities'))).json();
  assert.equal(capabilities.shell.enabled, false);
  assert.equal((await api.fetch(request('/v1/shell', { command: 'uname' }))).status, 400);
});


test('incomplete or inconsistent playlist results never claim persistence', async () => {
  for (const detail of [
    { name: 'Missing identity' },
    { id: 'p1', name: 'Negative count', songs: -1, entries: [] },
    { id: 'p1', name: 'Fractional count', songs: 0.5, entries: [] },
    { id: 'p1', name: 'Missing entries', songs: 5 },
    { id: 'p1', name: 'Mismatched count', songs: 5, entries: [] },
  ]) {
    const api = createDemoAPI({ delay: 0 });
    const response = await reply(api, 'create playlist', [tool('playlist_create_demo')]);
    const id = response.headers.get('X-Response-ID');
    const streamed = response.text();
    const call = (await replyState(api, id)).pending_inline_client_tools[0];
    await sendResult(api, id, call.call_id, JSON.stringify(detail));
    const result = deltas(await streamed);
    assert.match(result, /could not read/);
    assert.doesNotMatch(result, /saved locally|The device reports playlist/);
  }
});

test('sample and mixed playlist counts are described without inventing sample metadata', async () => {
  for (const sample of [true, false]) {
    const api = createDemoAPI({ delay: 0 });
    const response = await reply(api, 'Create a playlist!', [tool('playlist_create_demo')]);
    const id = response.headers.get('X-Response-ID');
    const streamed = response.text();
    const call = (await replyState(api, id)).pending_inline_client_tools[0];
    assert.equal(call.name, 'webmcp__playlist_create_demo');
    await sendResult(api, id, call.call_id, JSON.stringify({ id: 'p1', name: 'Edited Playlist', songs: 1,
      entries: [{ title: 'One song', sample }] }));
    const result = deltas(await streamed);
    assert.match(result, sample ? /1 sample song/ : /1 song/);
    if (!sample) assert.doesNotMatch(result, /1 sample song/);
  }
});

test('native requests require function definitions and never forward arbitrary commands', async () => {
  const api = createDemoAPI({ delay: 0 });
  const nonFunction = await reply(api, 'create playlist', [{ type: 'custom', name: 'webmcp__playlist_create_demo' }]);
  assert.doesNotMatch(await nonFunction.text(), /response.client_tool.requested/);
  for (const command of ['delete all playlists', 'create playlist named Surprise', 'play music', 'show my location']) {
    const response = await reply(api, command, [tool('playlist_create_demo'), tool('playlist_delete'), tool('music_play'), tool('location_current')]);
    assert.doesNotMatch(await response.text(), /response.client_tool.requested/);
  }
});
