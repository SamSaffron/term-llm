// In-browser API double for the stock frontend. Replies are scripted, not AI.
const MODEL = 'scripted-demo';
const encoder = new TextEncoder();
const json = (body, status = 200) => Response.json(body, { status });
const text = value => typeof value === 'string' ? value : Array.isArray(value)
  ? value.map(part => text(part.content ?? part.text ?? '')).join('\n') : '';
const replies = [
  'This is a **scripted UI demo**, not an AI response.\n\nYou can try the normal chat interface: send messages, create another chat, rename it, and switch between conversations. Everything stays in this tab and disappears on reload.',
  'Here is some sample Markdown:\n\n- **Bold** and *italic* text\n- `inline code`\n- A code block you can copy:\n\n```ruby\nputs "Hello from term-llm!"\n```\n\nThis is illustrative text only; no code was executed.',
  'A sample comparison table:\n\n| Feature | This demo |\n| --- | --- |\n| Chat interface | Real term-llm frontend |\n| Replies | Scripted examples |\n| Model or API key | Not needed |\n| Storage | This tab only |\n\nUse **New chat** to explore another conversation.',
];

export function createDemoAPI({ delay = 15 } = {}) {
  const sessions = new Map();
  const responses = new Map();
  let nextSession = 0, nextMessage = 0, nextResponse = 0;
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
  welcome.messages.push(message('assistant', 'Welcome to the **term-llm UI demo**. Replies here are scripted; no AI model, API key, or backend is running.\n\nSend a message to try streaming, Markdown, and the chat controls. Reloading clears your demo conversations.'));

  function streamReply(request, body) {
    const session = sessions.get(request.headers.get('X-Term-LLM-Session-ID')) ?? createSession();
    const input = text(body.input).trim();
    if (!input) return json({ error: { message: 'Enter a message to try the demo.' } }, 400);
    if (session.active_response_id) return json({ error: { message: 'This chat already has a reply in progress.' } }, 409);
    session.messages.push({ ...message('user', input), client_message_id: body.client_message_id });
    if (session.title === 'New chat') session.name = session.title = input.slice(0, 60);
    const answer = message('assistant', '');
    session.messages.push(answer);
    const content = replies[(session.messages.filter(m => m.role === 'user').length - 1) % replies.length];
    const id = `reply-${++nextResponse}`;
    answer.response_id = id;
    const record = { id, session_id: session.id, status: 'in_progress', run_epoch: 1, last_sequence_number: 0, events: [] };
    responses.set(id, record);
    session.active_response_id = session.last_response_id = id;
    let controller, timer, finished = false, offset = 0;
    const event = (name, data) => {
      const payload = { ...data, response_id: id, run_epoch: 1, sequence_number: ++record.last_sequence_number };
      const chunk = `event: ${name}\ndata: ${JSON.stringify(payload)}\n\n`;
      record.events.push(chunk);
      controller.enqueue(encoder.encode(chunk));
    };
    const finish = (cancelled = false, readable = true) => {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      request.signal.removeEventListener('abort', abort);
      record.status = cancelled ? 'cancelled' : 'completed';
      session.active_response_id = '';
      session.updated_at = session.last_message_at = now();
      session.transcript_rev++;
      if (readable) {
        event(`response.${record.status}`, { ended_at: Date.now(), response: { id, status: record.status, model: MODEL, provider: 'demo' } });
        controller.enqueue(encoder.encode('data: [DONE]\n\n'));
        controller.close();
      }
    };
    const abort = () => finish(true);
    const stream = new ReadableStream({
      start(value) {
        controller = value;
        event('response.created', { started_at: Date.now(), response: { id, object: 'response', status: 'in_progress', model: MODEL, provider: 'demo' } });
        record.cancel = abort;
        request.signal.addEventListener('abort', abort, { once: true });
        if (request.signal.aborted) { finish(true); return; }
        const pump = () => {
          if (finished) return;
          const chunk = content.slice(offset, offset += 16);
          answer.parts[0].text += chunk;
          event('response.output_text.delta', { delta: chunk });
          if (offset >= content.length) finish();
          else timer = setTimeout(pump, delay);
        };
        timer = setTimeout(pump, delay);
      },
      cancel() { finish(true, false); },
    });
    return new Response(stream, { headers: { 'Content-Type': 'text/event-stream', 'X-Session-ID': session.id,
      'X-Session-Number': String(session.number), 'X-Response-ID': id, 'X-Term-LLM-Response-Status': 'in_progress' } });
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
    if (path.startsWith('/v1/responses/')) {
      const [, , , id, action] = path.split('/');
      const record = responses.get(id);
      if (!record) return json({ error: { message: 'Demo reply not found' } }, 404);
      if (action === 'cancel') { record.cancel?.(); return json({ ok: true }); }
      if (action === 'events') return new Response(record.events.join('') + 'data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
      const { events, cancel, ...state } = record;
      return json(state);
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
