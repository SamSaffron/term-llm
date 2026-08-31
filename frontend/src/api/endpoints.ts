import type { APIClient } from './client';
import type { Goal, MCPOAuthFlow, MCPResponse } from '../domain/types';
import type { MentionSearchResponse } from '../domain/completions';

const encoded = (value: string): string => encodeURIComponent(value);
const sessionHeaders = (id: string): Record<string, string> => ({ 'X-Term-LLM-Session-ID': id });
// Review data may come from the session's node rather than the shell host. The
// node's embedded UI hash is not authoritative for the browser's shell assets.
const sessionReviewRead = { auth: 'session', versionCheck: false } as const;
export const endpoints = (api: APIClient) => ({
  capabilities: () => api.get<Record<string, unknown>>('/v1/capabilities'),
  providers: () => api.get<Record<string, unknown>>('/v1/providers'),
  models: (provider = '', signal?: AbortSignal) =>
    api.get<Record<string, unknown>>(
      `/v1/models${provider ? `?provider=${encoded(provider)}` : ''}`,
      signal,
    ),
  mentionSearch: (body: unknown, sessionId = '', signal?: AbortSignal) =>
    api.json<MentionSearchResponse>(
      '/v1/mentions/search',
      {
        method: 'POST',
        signal,
        headers: sessionId ? { 'X-Term-LLM-Session-ID': sessionId } : undefined,
        body: JSON.stringify(body),
      },
      { policy: 'safe-read', auth: 'session' },
    ),
  sidebar: (hidden: boolean) =>
    api.get<Record<string, unknown>>(
      `/v1/sidebar?per_project=12&include_archived_projects=1&include_archived_sessions=${hidden ? '1' : '0'}`,
    ),
  recentSessions: (cursor: string, hidden: boolean) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions?scope=all${cursor ? `&cursor=${encoded(cursor)}` : ''}&limit=30&include_archived=${hidden ? '1' : '0'}`,
    ),
  projectSessions: (projectId: string, cursor: string, hidden: boolean) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions?project_id=${encoded(projectId)}&cursor=${encoded(cursor)}&limit=12&include_archived=${hidden ? '1' : '0'}`,
    ),
  noProjectSessions: (cursor: string, hidden: boolean) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions?no_project=1&cursor=${encoded(cursor)}&limit=30&include_archived=${hidden ? '1' : '0'}`,
    ),
  sessionStatus: async (
    selected = '',
    hidden = false,
    categories: string[] = ['all'],
    etag = '',
  ): Promise<Record<string, unknown>> => {
    const params = new URLSearchParams();
    if (selected) params.set('selected_session', selected);
    if (hidden) params.set('include_archived', '1');
    if (!categories.includes('all')) params.set('categories', categories.join(','));
    const response = await api.request(
      `/v1/sessions/status${params.size ? `?${params}` : ''}`,
      { headers: etag ? { 'If-None-Match': etag } : undefined },
      { policy: 'safe-read' },
    );
    if (response.status === 304)
      return { __notModified: true, __etag: response.headers.get('ETag') || etag };
    if (!response.ok) {
      const body = await response.text();
      throw new Error(body || `Status request returned ${response.status}`);
    }
    return {
      ...((await response.json()) as Record<string, unknown>),
      __etag: response.headers.get('ETag') || '',
    };
  },
  searchSessions: (
    query: string,
    hidden = false,
    categories: string[] = ['all'],
    signal?: AbortSignal,
  ) => {
    const params = new URLSearchParams({ q: query, limit: '30' });
    if (hidden) params.set('include_archived', '1');
    if (!categories.includes('all')) params.set('categories', categories.join(','));
    return api.get<Record<string, unknown>>(`/v1/sessions/search?${params}`, signal);
  },
  sessions: (query = '') =>
    api.get<Record<string, unknown>>(`/v1/sessions${query ? `?${query}` : ''}`),
  selectedSession: (id: string) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions?selected_only=1&include_transcript=1&include_widget_status=1&selected_session=${encoded(id)}`,
    ),
  sessionState: (id: string, signal?: AbortSignal) =>
    api.get<Record<string, unknown>>(`/v1/sessions/${encoded(id)}/state`, signal),
  createResponse: (
    body: unknown,
    sessionId: string,
    requestId: string,
    signal?: AbortSignal,
    notificationSubscriptionId = '',
  ) => {
    const streaming = Boolean(
      body && typeof body === 'object' && (body as Record<string, unknown>).stream,
    );
    const draft = sessionId.startsWith('draft_');
    return api.request(
      '/v1/responses',
      {
        method: 'POST',
        signal,
        headers: {
          ...(sessionId && !draft ? { 'X-Term-LLM-Session-ID': sessionId } : {}),
          ...(draft ? { 'X-Term-LLM-Draft-ID': sessionId } : {}),
          ...(notificationSubscriptionId
            ? { 'X-Term-LLM-Push-Subscription-ID': notificationSubscriptionId }
            : {}),
          'Idempotency-Key': requestId,
          'X-Term-LLM-Request-ID': requestId,
        },
        body: JSON.stringify(body),
      },
      streaming
        ? { policy: 'idempotent-mutation', retries: 2, timeoutMs: 0, auth: 'session' }
        : { policy: 'mutation', retries: 0, timeoutMs: 0, auth: 'session' },
    );
  },
  response: (id: string, signal?: AbortSignal) =>
    api.get<Record<string, unknown>>(`/v1/responses/${encoded(id)}`, signal),
  responseEvents: (id: string, after: number, signal: AbortSignal) =>
    api.request(
      `/v1/responses/${encoded(id)}/events?after=${after}`,
      { signal, headers: { Accept: 'text/event-stream' } },
      { policy: 'stream', retries: 0, timeoutMs: 0, auth: 'session' },
    ),
  serverEventStream: (after: number | null, channels: string[], signal: AbortSignal) => {
    const params = new URLSearchParams();
    if (after !== null) params.set('after', String(after));
    if (channels.length) params.set('channels', channels.join(','));
    return api.request(
      `/v1/events${params.size ? `?${params}` : ''}`,
      { signal, headers: { Accept: 'text/event-stream' } },
      { policy: 'stream', retries: 0, timeoutMs: 0, auth: 'session' },
    );
  },
  serverEventPoll: (after: number | null, channels: string[], signal: AbortSignal) => {
    const params = new URLSearchParams();
    if (after !== null) {
      params.set('after', String(after));
      params.set('wait_ms', '25000');
      params.set('limit', '100');
    }
    if (channels.length) params.set('channels', channels.join(','));
    return api.request(
      `/v1/events/poll${params.size ? `?${params}` : ''}`,
      { signal, headers: { Accept: 'application/json' } },
      { policy: 'safe-read', retries: 0, timeoutMs: 35_000, auth: 'session' },
    );
  },
  cancelResponse: (id: string) =>
    api.post<Record<string, unknown>>(
      `/v1/responses/${encoded(id)}/cancel`,
      {},
      'idempotent-mutation',
      { 'Idempotency-Key': `cancel_${id}` },
    ),
  interrupt: (sessionId: string, body: unknown, interjectionId: string) =>
    api.json(
      `/v1/sessions/${encoded(sessionId)}/interrupt`,
      {
        method: 'POST',
        headers: { 'Idempotency-Key': `interrupt_${interjectionId}` },
        body: JSON.stringify(body),
      },
      { policy: 'idempotent-mutation', auth: 'session' },
    ),
  deleteInterrupt: (sessionId: string, interjectionId: string) =>
    api.delete(`/v1/sessions/${encoded(sessionId)}/interjections/${encoded(interjectionId)}`),
  patchSession: (id: string, body: unknown) => api.patch(`/v1/sessions/${encoded(id)}`, body),
  refineTitle: (id: string) =>
    api.post<Record<string, unknown>>(`/v1/sessions/${encoded(id)}/title/refine`, {
      preview: true,
    }),
  projectAssignment: (id: string, signal?: AbortSignal) =>
    api.get<Record<string, unknown>>(`/v1/sessions/${encoded(id)}/project`, signal),
  setProject: (id: string, body: Record<string, unknown>) =>
    api.post<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/project`,
      body,
      'idempotent-mutation',
      { 'Idempotency-Key': `project_${id}_${JSON.stringify(body)}` },
    ),
  patchProject: (id: string, body: unknown) => api.patch(`/v1/projects/${encoded(id)}`, body),
  projectDirectories: (path = '', showHidden = false, signal?: AbortSignal) => {
    const params = new URLSearchParams();
    if (path) params.set('path', path);
    if (showHidden) params.set('show_hidden', '1');
    return api.get<Record<string, unknown>>(
      `/v1/project-directories${params.size ? `?${params}` : ''}`,
      signal,
    );
  },
  createProject: (body: unknown, dryRun = false) =>
    api.post<Record<string, unknown>>(`/v1/projects${dryRun ? '?dry_run=1' : ''}`, body),
  runtime: (id: string, operation: string, body: unknown) =>
    api.post(`/v1/sessions/${encoded(id)}/runtime/${operation}`, body, 'idempotent-mutation', {
      'Idempotency-Key': `runtime_${id}_${operation}_${JSON.stringify(body)}`,
    }),
  compact: (id: string) => api.post(`/v1/sessions/${encoded(id)}/runtime/compact`, {}),
  mutateTranscript: (id: string, operation: 'undo' | 'redo', body: unknown) =>
    api.post<Record<string, unknown>>(`/v1/sessions/${encoded(id)}/runtime/${operation}`, body),
  goal: (id: string, body: Goal | { action: string }) =>
    api.post(`/v1/sessions/${encoded(id)}/runtime/goal`, body, 'idempotent-mutation', {
      'Idempotency-Key': `goal_${id}_${JSON.stringify(body)}`,
    }),
  getMCP: (id: string) => api.get<MCPResponse>(`/v1/sessions/${encoded(id)}/mcp`),
  setMCP: (id: string, enabled: string[]) =>
    api.patch<MCPResponse>(`/v1/sessions/${encoded(id)}/mcp`, { enabled }),
  startMCPOAuth: (id: string, server: string, force = false) =>
    api.post<MCPOAuthFlow>(`/v1/sessions/${encoded(id)}/mcp/${encoded(server)}/oauth/start`, {
      force,
    }),
  cancelMCPOAuth: (id: string, server: string, flowId: string) =>
    api.post(`/v1/sessions/${encoded(id)}/mcp/${encoded(server)}/oauth/cancel`, {
      flow_id: flowId,
    }),
  logoutMCPOAuth: (id: string, server: string) =>
    api.delete(`/v1/sessions/${encoded(id)}/mcp/${encoded(server)}/oauth`),
  getMCPOAuthFlow: (flowId: string) =>
    api.get<MCPOAuthFlow>(`/v1/mcp/oauth/flows/${encoded(flowId)}`),
  askUser: (id: string, body: unknown, operationId: string) =>
    api.post(`/v1/sessions/${encoded(id)}/ask_user`, body, 'idempotent-mutation', {
      'Idempotency-Key': `ask_user_${operationId}`,
    }),
  approval: (id: string, body: unknown, operationId: string) =>
    api.post(`/v1/sessions/${encoded(id)}/approval`, body, 'idempotent-mutation', {
      'Idempotency-Key': `approval_${operationId}`,
    }),
  sideQuestionState: (id: string) =>
    api.get<Record<string, unknown>>(`/api/sessions/${encoded(id)}/side-question`),
  startSideQuestion: (id: string, question: string) =>
    api.request(
      `/api/sessions/${encoded(id)}/side-question`,
      { method: 'POST', body: JSON.stringify({ question }) },
      { policy: 'mutation', timeoutMs: 0, auth: 'session' },
    ),
  cancelSideQuestion: (id: string) =>
    api.request(
      `/api/sessions/${encoded(id)}/side-question/active`,
      { method: 'DELETE' },
      'idempotent-mutation',
    ),
  tree: (id: string, signal?: AbortSignal, includeBranchPoints = false) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/tree${includeBranchPoints ? '?include_branch_points=1' : ''}`,
      signal,
    ),
  branch: (id: string, body: unknown) =>
    api.post<Record<string, unknown>>(`/v1/sessions/${encoded(id)}/branches`, body),
  pathNotes: (id: string, body: unknown) =>
    api.json<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/path-notes`,
      { method: 'POST', body: JSON.stringify(body) },
      { policy: 'mutation', timeoutMs: 150_000 },
    ),
  skills: (id: string) =>
    api.json<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/skills`,
      { headers: sessionHeaders(id) },
      { policy: 'safe-read', auth: 'session' },
    ),
  invokeSkill: (id: string, body: unknown, key: string) =>
    api.post<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/skills/invoke`,
      body,
      'idempotent-mutation',
      { ...sessionHeaders(id), 'Idempotency-Key': `skill_${key}` },
    ),
  skillRun: (id: string, runId: string) =>
    api.json<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/skill-runs/${encoded(runId)}`,
      { headers: sessionHeaders(id) },
      { policy: 'safe-read', auth: 'session' },
    ),
  cancelSkillRun: (id: string, runId: string) =>
    api.json(
      `/v1/sessions/${encoded(id)}/skill-runs/${encoded(runId)}`,
      { method: 'DELETE', headers: sessionHeaders(id) },
      { policy: 'idempotent-mutation', auth: 'session' },
    ),
  fileChanges: (id: string, scope: string) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/file-changes${scope ? `?scope=${encoded(scope)}` : ''}`,
      undefined,
      sessionReviewRead,
    ),
  fileDiff: (id: string, path: string, scope: string, context = 0, snapshotSeq = 0) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/file-changes/diff?path=${encoded(path)}&scope=${encoded(scope)}${context ? `&context=${context}` : ''}${snapshotSeq ? `&snapshot_seq=${snapshotSeq}` : ''}`,
      undefined,
      sessionReviewRead,
    ),
  fileContentURL: (
    id: string,
    path: string,
    scope: string,
    side: 'before' | 'after',
    snapshotSeq = 0,
  ) =>
    api.url(
      `/v1/sessions/${encoded(id)}/file-changes/content?path=${encoded(path)}&scope=${encoded(scope)}&side=${side}${snapshotSeq ? `&snapshot_seq=${snapshotSeq}` : ''}`,
    ),
  fileText: (
    id: string,
    path: string,
    scope: string,
    side: 'before' | 'after',
    snapshotSeq = 0,
    signal?: AbortSignal,
  ) =>
    import('./file-text').then(({ fileText }) =>
      fileText(api, id, path, scope, side, snapshotSeq, signal),
    ),
  diffComments: (id: string) =>
    api.get<Record<string, unknown>>(
      `/v1/sessions/${encoded(id)}/diff-comments`,
      undefined,
      sessionReviewRead,
    ),
  legacyWorktrees: () => api.get<Record<string, unknown>>('/v1/worktrees'),
  projectWorktrees: (id: string) =>
    api.get<Record<string, unknown>>(`/v1/projects/${encoded(id)}/worktrees`),
  createProjectWorktree: (id: string, body: unknown) =>
    api.post<Record<string, unknown>>(`/v1/projects/${encoded(id)}/worktrees`, body),
  switchWorktree: (id: string, dir: string, sessionId: string) =>
    api.post<Record<string, unknown>>(
      `/v1/projects/${encoded(id)}/worktrees/switch`,
      { dir },
      'mutation',
      sessionHeaders(sessionId),
    ),
  worktreeDiff: (id: string, dir: string) =>
    api.get<Record<string, unknown>>(
      `/v1/projects/${encoded(id)}/worktrees/diff?dir=${encoded(dir)}`,
    ),
  mergeWorktree: (id: string, dir: string, sessionId = '', force = false) =>
    api.post<Record<string, unknown>>(
      `/v1/projects/${encoded(id)}/worktrees/merge`,
      { dir, ...(force ? { force: true } : {}) },
      'mutation',
      sessionId ? sessionHeaders(sessionId) : undefined,
    ),
  assistedMergeWorktree: (id: string, dir: string, sessionId: string) =>
    api.post<Record<string, unknown>>(
      `/v1/projects/${encoded(id)}/worktrees/assisted-merge`,
      { dir },
      'mutation',
      sessionHeaders(sessionId),
    ),
  promoteWorktree: (id: string, dir: string, branch: string, sessionId = '') =>
    api.post<Record<string, unknown>>(
      `/v1/projects/${encoded(id)}/worktrees/promote`,
      { dir, branch },
      'mutation',
      sessionId ? sessionHeaders(sessionId) : undefined,
    ),
  removeWorktree: (id: string, dir: string, force = false) =>
    api.delete<Record<string, unknown>>(
      `/v1/projects/${encoded(id)}/worktrees?dir=${encoded(dir)}${force ? '&force=1' : ''}`,
    ),
  transcribe: (
    body: FormData,
    controls?: { signal?: AbortSignal; onProgress?: (loaded: number, total?: number) => void },
  ) => api.upload<Record<string, unknown>>('/v1/transcribe', body, controls),
  pushSubscribe: (body: unknown) =>
    api.post<{ id: string; state: 'active' | 'stale'; vapid_key_id: string }>(
      '/v1/push/subscribe',
      body,
      'idempotent-mutation',
      { 'Idempotency-Key': 'push_subscription' },
    ),
  pushUnsubscribe: (body: { id?: string; endpoint?: string }) =>
    api.json(
      '/v1/push/subscribe',
      { method: 'DELETE', body: JSON.stringify(body) },
      { policy: 'mutation' },
    ),
  widgetStatus: () =>
    api.json<Record<string, unknown>>(
      '/admin/widgets/status',
      {},
      { policy: 'safe-read', auth: 'ignore' },
    ),
  stopWidget: (mount: string) =>
    api.post<Record<string, unknown>>(`/admin/widgets/${encoded(mount)}/stop`, {}, 'mutation'),
  hubNodes: (absoluteURL: string, signal?: AbortSignal) =>
    api.json<Record<string, unknown>>(
      absoluteURL,
      { signal },
      { policy: 'safe-read', auth: 'ignore', retries: 0, timeoutMs: 10_000 },
    ),
});

export type Endpoints = ReturnType<typeof endpoints>;
