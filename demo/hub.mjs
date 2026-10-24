// Fixed, build-configured Hub inventory. This is metadata, not a live agent.
export const DEMO_ORIGIN = 'https://demo.term-llm.com';
export const NODE_PATH = '/node/demo/';
export const SEED_TIME = '2026-01-01T00:00:00.000Z';
export const scriptedSessions = [
  { id: 'demo-1', number: 1, title: 'Welcome · UI demo' },
  { id: 'demo-2', number: 2, title: 'Static Hub · scripted tour',
    content: 'This is the **static Hub demo** conversation. The Hub lists one DEMO node and two fixed scripted sessions. No agent, database, or inference service is running.\n\nSend `help` for the native device-tool walkthrough, or any message to try a scripted reply. Browser chat changes stay in this tab and reset on reload; Hub metadata never changes. Explicit native device actions can persist on your device.' },
];

// Native Hub navigation only allows a node root (?new=1) or /chat/<selector>.
// Resolve paths before the ordinary frontend reads its query-string session.
export function sessionForRoute(pathname) {
  if (pathname === NODE_PATH) return scriptedSessions[0].id;
  const match = pathname.match(/^\/node\/demo\/chat\/([a-zA-Z0-9_-]+)$/);
  return match ? scriptedSessions.find(s => String(s.number) === match[1] || s.id === match[1])?.id : undefined;
}

export const hubPageConfig = {
  page: 'dashboard', authMode: 'none', basePath: '', canAddNodes: false,
  passkeyAuth: false, cacheDisplayAllowed: false,
};
const recent = scriptedSessions.map(s => ({
  id: s.id, number: s.number, short_title: s.title, long_title: '', pinned: false,
  active_run: false, interaction_required: false, pending_interaction_count: 0,
  pending_interaction_kinds: [], last_message_at: Date.parse(SEED_TIME), message_count: 1,
  resume_path: `${NODE_PATH}chat/${s.number}`,
}));
export const hubAssets = {
  '/healthz': { status: 'ok', role: 'hub' },
  '/api/nodes': { nodes: [{
    id: 'demo', name: 'DEMO · scripted UI (no AI)', source: 'config', connection: 'direct',
    url: DEMO_ORIGIN, base_path: NODE_PATH.slice(0, -1), proxy_path: NODE_PATH,
    new_session_path: NODE_PATH + '?new=1', has_token: false,
    status: { reachable: true, state: 'ok', latency_ms: 0, version: 'static-demo',
      agent: 'Scripted demo · no live agent', capabilities: [] },
    sessions: { count_label: '2 scripted sessions · resets on reload', has_more: false,
      active_count: 0, input_required_count: 0, unseen_count: 0, attention_capability: 'unavailable',
      active: [], recent, resume_path: recent[0].resume_path }, diagnostics: [],
  }] },
  '/api/attention': { total_running: 0, total_input_required: 0, total_unseen: 0,
    nodes: [], input_required: [], inbox: [], has_more: false },
  '/api/delegations': { delegations: [] },
  '/api/registration-info': { enabled: false, token_configured: false, can_persist_nodes: false },
};
