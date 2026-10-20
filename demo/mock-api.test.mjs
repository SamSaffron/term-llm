import test from 'node:test';
import assert from 'node:assert/strict';
import { createDemoAPI } from './mock-api.mjs';
const request = (path, body, headers = {}, method = body ? 'POST' : 'GET') => new Request('https://demo.test/demo-api' + path, { method, headers: { 'Content-Type': 'application/json', ...headers }, ...(body ? { body: JSON.stringify(body) } : {}) });
const getSession = async (api, id) => (await api.fetch(request(`/v1/sessions?selected_only=1&selected_session=${id}`))).json();

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
