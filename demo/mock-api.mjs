// In-browser API double for the stock frontend. Replies are scripted, not AI.
const MODEL = 'scripted-demo';
const encoder = new TextEncoder();
const json = (body, status = 200) => Response.json(body, { status });
const text = value => typeof value === 'string' ? value : Array.isArray(value)
  ? value.map(part => text(part.content ?? part.text ?? '')).join('\n') : '';

// Inline native (WebMCP) tools use the stock client-tool protocol. The page
// offers `webmcp__NAME` functions; a response requests one and stays open until
// the page POSTs that call's result.
const CLIENT_TOOL_PREFIX = 'webmcp__';
const NATIVE_COMMANDS = [
  { tool: 'playlist_create_demo', example: 'create playlist', arguments: '{}',
    aliases: ['create playlist', 'create a playlist', 'create demo playlist', 'create a demo playlist'] },
  { tool: 'playlist_list', example: 'list playlists', arguments: '{}',
    aliases: ['list playlists'] },
  { tool: 'ping', example: 'ping', arguments: JSON.stringify({ message: 'Hello from the demo' }),
    aliases: ['ping'] },
  { tool: 'device_info', example: 'device info', arguments: '{}', aliases: ['device info'] },
];
const SETUP_STEPS = [
  'In the term-llm app on iPhone or iPad, pull down near the top of the screen for **app controls**.',
  'Open **Settings → Device tools**, turn on **Share device tools with this server**, then tap **Done**.',
  'Return to this chat, tap **+ → MCP servers**, and enable **iPhone/iPad (WebMCP from this page)**.',
].map((step, index) => `${index + 1}. ${step}`).join('\n');
const HELP_COMMAND = [
  '**Scripted demo, real native actions.** Replies here are scripted; no AI model, API key, or backend is running. Native device actions you trigger below really run on your iPhone or iPad and can persist after you reload this chat. Sharing device tools grants this page the device-tools access the app exposes; the demo only issues the fixed read-only calls and sample creation listed here. Sample entries the demo creates are fictional and not playable.',
  '',
  'Commands:',
  '- `create playlist` or `create a demo playlist` — creates the idempotent demo playlist through device tools.',
  '- `list playlists` — lists playlists saved in term-llm on this device, including any existing playlists; it does not query the Apple Music catalog.',
  '- `ping` — checks that the device tool bridge answers.',
  '- `device info` — shows read-only device tool information.',
  '',
  'To enable native actions:',
  SETUP_STEPS,
  '',
  'Open term-llm’s native Music screen from the **♪** control in the pull-down app controls. A playlist created here is saved locally on the device; its sample entries are fictional and not playable.',
].join('\n');
const MAX_RESULT_CHARS = 4_000;
const MAX_TOOL_OUTPUT_CHARS = 100_000;
const replies = [
  'This is a **scripted UI demo**, not an AI response.\n\nYou can try the normal chat interface: send messages, create another chat, rename it, and switch between conversations. Chat history stays in this tab and disappears on reload; native actions you trigger with the demo commands are real and can persist after reload.',
  'Here is some sample Markdown:\n\n- **Bold** and *italic* text\n- `inline code`\n- A code block you can copy:\n\n```ruby\nputs "Hello from term-llm!"\n```\n\nThis is illustrative text only; no code was executed.',
  'A sample comparison table:\n\n| Feature | This demo |\n| --- | --- |\n| Chat interface | Real term-llm frontend |\n| Replies | Scripted examples |\n| Model or API key | Not needed |\n| Native device actions | Real, via device tools |\n| Storage | This tab only (native actions persist) |\n\nUse **New chat** to explore another conversation.',
];

const normalizeCommand = value => value.trim().toLowerCase().replace(/\s+/g, ' ');
const commandFor = input => {
  const raw = normalizeCommand(input);
  if (raw === '?' || raw === '/help' || raw === '/commands') return { help: true };
  const normalized = raw.replace(/[.!?]+$/, '');
  if (normalized === 'help' || normalized === 'commands') return { help: true };
  const command = NATIVE_COMMANDS.find(entry => entry.aliases.includes(normalized));
  return command ? { command } : null;
};
const clamp = (value, limit = MAX_RESULT_CHARS) => value.length <= limit
  ? value : `${value.slice(0, limit)}\n\n… truncated ${value.length - limit} characters`;
const oneLine = (value, limit = 500) => {
  const line = value.replace(/\s+/g, ' ').trim();
  return line.length <= limit ? line : `${line.slice(0, limit)}…`;
};
const jsonBlock = value => {
  let rendered;
  try {
    rendered = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  } catch {
    rendered = String(value);
  }
  return `\`\`\`json\n${clamp(rendered ?? String(value), 2_500)}\n\`\`\``;
};
const plainBlock = value => `\`\`\`text\n${clamp(value, 1_000)}\n\`\`\``;
const malformedOutput = reason =>
  `The device returned a result this demo could not read (${reason}), so I can't confirm the action and nothing is reported as successful.\n\nIf you triggered a native action, check the device directly. The demo cannot tell whether it completed.`;
const failedOutput = detail =>
  `The device reported an error: ${oneLine(detail)}\n\nI can't confirm that anything changed. If this was a playlist action, check term-llm’s native Music screen directly; the demo cannot tell whether part of it completed.`;

function interpretToolResult(tool, rawOutput) {
  const output = rawOutput.slice(0, MAX_TOOL_OUTPUT_CHARS).trim();
  if (!output) return malformedOutput('the reply was empty');
  if (/^Error:/i.test(output)) return failedOutput(output);
  if (tool === 'playlist_create_demo') {
    let detail;
    try { detail = JSON.parse(output); } catch { return malformedOutput('the reply was not JSON'); }
    if (!detail || typeof detail !== 'object' || Array.isArray(detail))
      return malformedOutput('the reply was not a playlist object');
    if (typeof detail.error === 'string' && detail.error.trim())
      return failedOutput(detail.error);
    const name = typeof detail.name === 'string' ? detail.name.trim() : '';
    if (!name) return malformedOutput('the reply had no playlist name');
    if (typeof detail.id !== 'string' || !detail.id.trim())
      return malformedOutput('the reply had no playlist id');
    const songs = detail.songs;
    const entries = detail.entries;
    if (!Number.isInteger(songs) || songs < 0 || !Array.isArray(entries) || entries.length !== songs)
      return malformedOutput('the playlist song count and entries did not match');
    const everyEntrySample = entries.length > 0 && entries.every(entry => entry && entry.sample === true);
    const notice = typeof detail.sample_notice === 'string' ? detail.sample_notice.trim()
      : typeof detail.explanation === 'string' ? detail.explanation.trim() : '';
    const count = songs === 0 ? ' with no songs'
      : ` with ${songs} ${everyEntrySample ? 'sample ' : ''}song${songs === 1 ? '' : 's'}`;
    return [
      `The device reports playlist \`${oneLine(name, 120)}\`${count}.`,
      '',
      'The playlist is saved locally on this device and remains after you reload this chat. Check term-llm’s native Music screen; the count comes from the device and includes any songs already added to the playlist.',
      ...(notice ? ['', clamp(notice)] : []),
    ].join('\n');
  }
  if (tool === 'playlist_list') {
    let detail;
    try { detail = JSON.parse(output); } catch { return malformedOutput('the reply was not JSON'); }
    const playlists = Array.isArray(detail) ? detail
      : detail && typeof detail === 'object' && Array.isArray(detail.playlists)
        ? detail.playlists : null;
    if (!playlists) return malformedOutput('the reply was not a playlist list');
    return [
      playlists.length ? 'Playlists saved in term-llm on this device (including any existing playlists):' : 'No term-llm playlists are saved on this device yet.',
      '',
      jsonBlock(playlists),
      '',
      'Read-only result from the device tool. Lists playlists saved in term-llm on this device (including any existing playlists); does not query Apple Music catalog.',
    ].join('\n');
  }
  if (tool === 'ping') {
    let detail;
    try { detail = JSON.parse(output); } catch { detail = undefined; }
    if (detail !== undefined && detail !== null && typeof detail === 'object')
      return `The device tool bridge answered:\n\n${jsonBlock(detail)}`;
    return `The device tool bridge answered:\n\n${plainBlock(output)}`;
  }
  if (tool === 'device_info') {
    let detail;
    try { detail = JSON.parse(output); } catch { return malformedOutput('the reply was not JSON'); }
    if (!detail || typeof detail !== 'object' || Array.isArray(detail))
      return malformedOutput('the reply was not an object');
    return [
      'Read-only device tool information from the shared device-tools bridge:',
      '',
      jsonBlock(detail),
    ].join('\n');
  }
  return malformedOutput('the demo does not know this tool');
}

export function createDemoAPI({ delay = 15, toolTimeout = 20_000 } = {}) {
  const sessions = new Map();
  const responses = new Map();
  let nextSession = 0, nextMessage = 0, nextResponse = 0, nextCall = 0;
  const timeoutMs = Number.isFinite(toolTimeout) && toolTimeout >= 0 ? toolTimeout : 20_000;
  const now = () => new Date().toISOString();
  const message = (role, value) => ({ id: ++nextMessage, role, created_at: now(), parts: [{ type: 'text', text: value }] });
  const createSession = () => {
    const id = `demo-${++nextSession}`;
    const session = { id, number: nextSession, title: 'New chat', name: 'New chat', mode: 'chat', origin: 'web',
      provider: 'demo', provider_key: 'demo', model: MODEL, active_provider: 'demo', active_model: MODEL,
      created_at: now(), updated_at: now(), last_message_at: now(), pinned: false, archived: false,
      transcript_rev: 1, messages: [], active_response_id: '', last_response_id: '' };
    sessions.set(id, session);
    return session;
  };
  const summary = session => {
    const { messages, ...rest } = session;
    return { ...rest, short_title: session.title, message_count: messages.length };
  };
  const transcript = session => ({ rev: session.transcript_rev, messages: session.messages });
  const selected = session => ({ selected_session: session ? summary(session) : null,
    selected_transcript: session ? { rev: session.transcript_rev,
      index: { rows: { ids: session.messages.map(m => m.id), roles: session.messages.map(m => m.role === 'user' ? 'u' : 'a').join('') } },
      bodies: transcript(session) } : null });
  const welcome = createSession();
  welcome.title = welcome.name = 'Welcome · UI demo';
  welcome.messages.push(message('assistant', 'Welcome to the **term-llm UI demo**. Replies here are scripted; no AI model, API key, or backend is running.\n\nNative device actions you trigger with the demo commands are real and can persist after you reload this chat. Sharing device tools grants this page the device-tools access the app exposes; the demo only issues the fixed commands below. Sample entries the demo creates are fictional and not playable.\n\nTry `create playlist`, `list playlists`, `ping`, or `device info`. For setup, pull down near the top for app controls → **Settings → Device tools** and enable **Share device tools with this server**; then use **+ → MCP servers** to enable **iPhone/iPad (WebMCP from this page)**. Open term-llm’s native Music screen from the **♪** control in that pull-down.\n\nSend a message to try streaming and the chat controls. Reloading clears the demo chat history (not native actions).'));

  function streamReply(request, body) {
    const session = sessions.get(request.headers.get('X-Term-LLM-Session-ID')) ?? createSession();
    const input = text(body.input).trim();
    if (!input) return json({ error: { message: 'Enter a message to try the demo.' } }, 400);
    if (session.active_response_id) return json({ error: { message: 'This chat already has a reply in progress.' } }, 409);
    const offeredNames = new Set((Array.isArray(body.tools) ? body.tools : [])
      .filter(tool => tool && tool.type === 'function' && typeof tool.name === 'string')
      .map(tool => tool.name));
    const deviceToolsOffered = [...offeredNames].some(name => name.startsWith(CLIENT_TOOL_PREFIX));
    session.messages.push({ ...message('user', input), client_message_id: body.client_message_id });
    if (session.title === 'New chat') session.name = session.title = input.slice(0, 60);
    const answer = message('assistant', '');
    session.messages.push(answer);
    const id = `reply-${++nextResponse}`;
    answer.response_id = id;
    const record = { id, session_id: session.id, status: 'in_progress', accepting: true,
      run_epoch: 1, last_sequence_number: 0, started_at: Date.now(), events: [], calls: new Map(),
      pending: null, toolTimer: null, error: '' };
    responses.set(id, record);
    session.active_response_id = session.last_response_id = id;
    const command = commandFor(input);
    let controller, closed = false, finished = false, timer, pumpOffset = 0;
    const event = (name, data) => {
      const payload = { ...data, response_id: id, run_epoch: 1, sequence_number: ++record.last_sequence_number };
      const chunk = `event: ${name}\ndata: ${JSON.stringify(payload)}\n\n`;
      record.events.push({ sequence: payload.sequence_number, chunk });
      if (!closed && controller) {
        try { controller.enqueue(encoder.encode(chunk)); } catch { /* stream closed */ }
      }
      return payload.sequence_number;
    };
    const append = value => {
      if (!value) return;
      answer.parts[0].text += value;
      event('response.output_text.delta', { delta: value });
    };
    const done = () => {
      if (!closed) {
        try {
          controller.enqueue(encoder.encode('data: [DONE]\n\n'));
          controller.close();
        } catch { /* already closed */ }
        closed = true;
      }
    };
    const finish = (status = 'completed', { readable = true, error = '' } = {}) => {
      if (finished) return;
      finished = true;
      record.accepting = false;
      record.pending = null;
      clearTimeout(timer);
      clearTimeout(record.toolTimer);
      request.signal.removeEventListener('abort', abort);
      record.status = status;
      record.ended_at = Date.now();
      if (error) record.error = error;
      session.active_response_id = '';
      session.updated_at = session.last_message_at = now();
      session.transcript_rev++;
      if (!readable) return;
      const payload = { ended_at: Date.now(), response: { id, status, model: MODEL, provider: 'demo' } };
      if (error && status === 'failed') payload.error = { message: error };
      event(`response.${status}`, payload);
      done();
    };
    const appendWarning = value => {
      if (!finished && !closed) append(value);
    };
    const cancel = () => {
      if (finished || record.status !== 'in_progress') return;
      const pending = record.pending;
      record.pending = null;
      record.accepting = false;
      if (pending) appendWarning(`\n\nStopped before the device answered \`${pending.tool}\`. If the device already ran it, that native action may still have happened and cannot be undone from this demo. Reload and check term-llm’s native Music screen.`);
      finish('cancelled');
    };
    record.cancel = cancel;
    const abort = () => cancel();
    const pump = (value, then) => {
      const step = () => {
        if (finished || closed) return;
        append(value.slice(pumpOffset, pumpOffset + 16));
        pumpOffset += 16;
        if (pumpOffset >= value.length) then();
        else timer = setTimeout(step, delay);
      };
      if (!value) { then(); return; }
      step();
    };
    record.deliver = (name, output) => {
      const tool = name.startsWith(CLIENT_TOOL_PREFIX) ? name.slice(CLIENT_TOOL_PREFIX.length) : name;
      pump(interpretToolResult(tool, output), () => finish('completed'));
    };
    const guide = command?.command && !offeredNames.has(CLIENT_TOOL_PREFIX + command.command.tool)
      ? deviceToolsOffered
        ? `Device tools are connected, but this app version does not provide \`${command.command.tool}\`. Update the app if device tools are enabled, then send \`${command.command.example}\` again.`
        : `I can't run that native action here yet. This page is not sharing the device's tools with the chat.\n\n${SETUP_STEPS}\n\nThen send \`${command.command.example}\` again.`
      : '';
    const stream = new ReadableStream({
      start(value) {
        controller = value;
        event('response.created', { started_at: Date.now(), response: { id, object: 'response', status: 'in_progress', model: MODEL, provider: 'demo' } });
        request.signal.addEventListener('abort', abort, { once: true });
        if (request.signal.aborted) { finish('cancelled'); return; }
        if (command?.help) { pump(HELP_COMMAND, () => finish()); return; }
        if (guide) { pump(guide, () => finish()); return; }
        if (command?.command) {
          const callId = `call_demo_${++nextCall}`;
          const name = CLIENT_TOOL_PREFIX + command.command.tool;
          record.pending = { call_id: callId, name, arguments: command.command.arguments, tool: command.command.tool };
          event('response.client_tool.requested', { call_id: callId, name, arguments: command.command.arguments });
          record.toolTimer = setTimeout(() => {
            if (finished || record.status !== 'in_progress' || record.pending?.call_id !== callId) return;
            const tool = record.pending.tool;
            record.pending = null;
            record.accepting = false;
            pump(`The device did not answer \`${tool}\` within ${timeoutMs} ms, so I can't confirm that it ran and nothing is reported as successful. If you later see a change on the device, it came from the native app; this chat will not claim it.`, () =>
              finish('failed', { error: `The device did not answer ${tool} within ${timeoutMs} ms.` }));
          }, timeoutMs);
          return;
        }
        const content = replies[(session.messages.filter(m => m.role === 'user').length - 1) % replies.length];
        pump(content, () => finish());
      },
      cancel() { finish('cancelled', { readable: false }); },
    });
    return new Response(stream, { headers: { 'Content-Type': 'text/event-stream', 'X-Session-ID': session.id,
      'X-Session-Number': String(session.number), 'X-Response-ID': id, 'X-Term-LLM-Response-Status': 'in_progress' } });
  }

  function responseState(record) {
    return { id: record.id, session_id: record.session_id, status: record.status,
      run_epoch: record.run_epoch, last_sequence_number: record.last_sequence_number,
      started_at: record.started_at, ...(record.ended_at ? { ended_at: record.ended_at } : {}),
      ...(record.error ? { error: { message: record.error } } : {}),
      pending_inline_client_tools: record.status === 'in_progress' && record.pending
        ? [{ call_id: record.pending.call_id, name: record.pending.name, arguments: record.pending.arguments }] : [],
      recovery: { sequence_number: record.last_sequence_number, messages: [] } };
  }

  function inlineResult(record, callId, body) {
    if (record.status === 'in_progress' && record.accepting && record.pending?.call_id === callId) {
      if (typeof body.output !== 'string')
        return json({ error: { message: 'The client tool result must include a string output.' } }, 400);
      if (!body.output.trim())
        return json({ error: { message: 'The client tool result output must not be empty.' } }, 400);
      const pendingName = record.pending.name;
      record.accepting = false;
      record.pending = null;
      clearTimeout(record.toolTimer);
      record.calls.set(callId, true);
      return { name: pendingName, output: body.output };
    }
    if (record.calls.has(callId)) return json({ ok: true });
    if (record.status === 'cancelled')
      return json({ error: { message: 'This response was stopped before the device result arrived. The native action may still have happened and cannot be undone from this demo.' } }, 409);
    if (record.status === 'failed')
      return json({ error: { message: 'The demo stopped waiting for this device result. The native action may still have happened; reload and check the device.' } }, 409);
    return json({ error: { message: 'No pending client tool call matches this response and call id.' } }, 404);
  }

  async function fetch(request) {
    const url = new URL(request.url);
    const path = url.pathname.replace(/^\/demo-api/, '');
    const method = request.method;
    if (request.signal.aborted) throw new DOMException('Aborted', 'AbortError');
    const body = method === 'GET' || method === 'HEAD' ? {} : await request.json().catch(() => ({}));
    if (path === '/v1/capabilities') return json({ projects: { enabled: false }, worktrees: { enabled: false }, shell: { enabled: false }, live: { enabled: false } });
    if (path === '/v1/providers') return json({ data: [{ name: 'demo', display_name: 'UI demo', configured: true, available: true, is_default: true, default_model: MODEL, models: [MODEL] }] });
    if (path === '/v1/models') return json({ data: [{ id: MODEL, name: 'Scripted replies · no AI', display_name: 'Scripted replies · no AI', owned_by: 'demo', provider: 'demo' }] });
    if (path === '/v1/responses' && method === 'POST') return streamReply(request, body);
    const callMatch = path.match(/^\/v1\/responses\/([^/]+)\/client_tool_calls\/([^/]+)\/result$/);
    if (callMatch) {
      if (method !== 'POST') return json({ error: { message: 'Use POST for client tool results.' } }, 405);
      const record = responses.get(decodeURIComponent(callMatch[1]));
      if (!record) return json({ error: { message: 'Demo reply not found' } }, 404);
      const callId = decodeURIComponent(callMatch[2]);
      const result = inlineResult(record, callId, body);
      if (result instanceof Response) return result;
      record.deliver(result.name, result.output);
      return json({ ok: true });
    }
    if (path.startsWith('/v1/responses/')) {
      const [, , , id, action] = path.split('/');
      const record = responses.get(decodeURIComponent(id));
      if (!record) return json({ error: { message: 'Demo reply not found' } }, 404);
      if (action === 'cancel') { record.cancel?.(); return json({ ok: true }); }
      if (action === 'events') {
        const after = Number(url.searchParams.get('after')) || 0;
        const chunks = record.events.filter(entry => entry.sequence > after).map(entry => entry.chunk).join('');
        return new Response(chunks + 'data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
      }
      return json(responseState(record));
    }
    if (path === '/v1/sessions' && method === 'POST') return json({ session: summary(createSession()) });
    if (path === '/v1/sessions' && url.searchParams.get('selected_only') === '1') return json(selected(sessions.get(url.searchParams.get('selected_session'))));
    if (path === '/v1/sessions' || path === '/v1/sidebar' || path === '/v1/sessions/search') {
      const query = (url.searchParams.get('q') ?? '').toLowerCase();
      const items = [...sessions.values()].filter(s => s.title.toLowerCase().includes(query)).reverse().map(summary);
      return json({ sessions: items, recent_sessions: items, next_cursor: '' });
    }
    if (path === '/v1/sessions/status') return json({ sessions: [...sessions.values()].map(summary) });
    if (path === '/v1/sessions/pinned-order') return json({ ok: true });
    if (path.startsWith('/v1/sessions/')) {
      const [, , , id, ...segments] = path.split('/');
      const session = sessions.get(decodeURIComponent(id));
      if (!session) return json({ error: { message: 'Demo chat not found' } }, 404);
      const action = segments.join('/');
      if (!action && method === 'DELETE') { responses.get(session.active_response_id)?.cancel?.(); sessions.delete(session.id); return new Response(null, { status: 204 }); }
      if (!action && (method === 'PATCH' || method === 'PUT')) {
        for (const key of ['title', 'name', 'pinned', 'archived']) if (body[key] !== undefined) session[key] = body[key];
        if (body.title !== undefined) session.name = body.title;
        return json({ session: summary(session) });
      }
      if (action === 'state' || !action) return json({ session: summary(session) });
      if (action === 'transcript/index') return json({ rows: { ids: session.messages.map(m => m.id), roles: session.messages.map(m => m.role === 'user' ? 'u' : 'a').join('') }, rev: session.transcript_rev });
      if (action.startsWith('transcript')) return json(transcript(session));
      if (action === 'title') return json({ title: session.title });
      if (action === 'stats') return json({ stats: {} });
      if (action === 'children') return json({ children: [] });
      if (action === 'file-changes') return json({ files: [] });
      if (action === 'steering') return json({ messages: [] });
      if (action === 'attention/seen') return json({ attention_unseen: false });
      if (action === 'state' || action === 'policy') return json({ session: summary(session) });
    }
    if (path === '/v1/events') return new Response('data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
    if (path === '/v1/events/poll') return json({ events: [], next_cursor: 0 });
    if (path === '/v1/agents') return json({ agents: [] });
    if (path === '/v1/sharing/capabilities') return json({ enabled: false });
    if (method === 'GET') return json({});
    return json({ error: { message: 'This feature is not available in the scripted UI demo.' } }, 400);
  }
  return { fetch, initialSession: welcome.id };
}
