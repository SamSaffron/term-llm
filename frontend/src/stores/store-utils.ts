import { APIError } from '../api/client';
import type { AppConfig } from '../app/config';
import { rebaseHubAssetURL } from '../app/config';
import type { ApprovalPrompt, AskUserPrompt, MCPServer, Session } from '../domain/types';
import { sanitizeSession } from '../domain/transcript';

export const uuid = (): string =>
  globalThis.crypto?.randomUUID?.() || Math.random().toString(36).slice(2);

export const array = (value: unknown): Record<string, unknown>[] =>
  Array.isArray(value)
    ? value.filter(
        (entry): entry is Record<string, unknown> => Boolean(entry) && typeof entry === 'object',
      )
    : [];

export const recordValue = (value: unknown): Record<string, unknown> | null =>
  Boolean(value) && typeof value === 'object' ? (value as Record<string, unknown>) : null;

export const listFrom = (
  value: Record<string, unknown>,
  ...keys: string[]
): Record<string, unknown>[] => {
  for (const key of keys) if (Array.isArray(value[key])) return array(value[key]);
  return [];
};

export const compareSessionsByActivity = (left: Session, right: Session): number =>
  Number(right.pinned) - Number(left.pinned) ||
  (right.lastMessageAt || right.created) - (left.lastMessageAt || left.created) ||
  (right.number || 0) - (left.number || 0);

export const sessionFrom = (config: AppConfig, value: Record<string, unknown>): Session =>
  sanitizeSession(value, { rebaseAssetURL: (url) => rebaseHubAssetURL(config, url) });

export const normalizeMCPState = (value: unknown): { servers: MCPServer[]; enabled: string[] } => {
  const source = recordValue(value) || {};
  const enabled = Array.isArray(source.enabled)
    ? source.enabled
        .map(String)
        .map((name) => name.trim())
        .filter(Boolean)
    : [];
  const enabledSet = new Set(enabled);
  const servers = array(source.servers)
    .map((server): MCPServer | null => {
      const name = String(server.name || '').trim();
      if (!name) return null;
      const serverEnabled = enabledSet.has(name) || Boolean(server.enabled);
      const count = (field: string): number => {
        const parsed = Number(server[field]);
        return Number.isFinite(parsed) ? Math.max(0, parsed) : 0;
      };
      return {
        name,
        configured: server.configured !== false,
        enabled: serverEnabled,
        status: String(server.status || (serverEnabled ? 'ready' : 'stopped')).trim() || 'stopped',
        error: String(server.error || '').trim(),
        refreshWarning: String(server.refresh_warning || '').trim(),
        tools: count('tools'),
        active: count('active'),
        deferred: count('deferred'),
        loadingMode: String(server.loading_mode || '').trim(),
        authState: String(server.auth_state || 'not_needed').trim(),
        authIssuer: String(server.auth_issuer || '').trim(),
        authScopes: Array.isArray(server.auth_scopes) ? server.auth_scopes.map(String) : [],
        authExpiresAt: String(server.auth_expires_at || '').trim(),
        canSignIn: Boolean(server.can_sign_in),
        canSignOut: Boolean(server.can_sign_out),
      };
    })
    .filter((server): server is MCPServer => server !== null);
  return {
    servers,
    enabled: enabled.length
      ? [...new Set(enabled)]
      : servers.filter((server) => server.enabled).map((server) => server.name),
  };
};

export const askUserPrompt = (value: unknown, sessionId: string): AskUserPrompt | null => {
  const source = recordValue(value);
  if (!source || !Array.isArray(source.questions) || !source.questions.length) return null;
  const callId = String(source.callId || source.call_id || '');
  if (!callId) return null;
  return {
    sessionId: String(source.sessionId || source.session_id || sessionId),
    callId,
    questions: source.questions as AskUserPrompt['questions'],
  };
};

export const approvalPrompt = (value: unknown, sessionId: string): ApprovalPrompt | null => {
  const source = recordValue(value);
  if (!source) return null;
  const id = String(source.id || source.approval_id || '');
  const options = Array.isArray(source.options)
    ? (source.options as ApprovalPrompt['options'])
    : [];
  if (!id || !options?.length) return null;
  return {
    ...source,
    sessionId: String(source.sessionId || source.session_id || sessionId),
    id,
    options,
    resumeAutoAvailable: Boolean(source.resumeAutoAvailable || source.resume_auto_available),
  } as ApprovalPrompt;
};

export const worktreeErrorMessage = (error: unknown): string => {
  const code = error instanceof APIError ? error.type : '';
  const messages: Record<string, string> = {
    project_required: 'Choose a project before selecting a worktree.',
    worktrees_unavailable: 'Worktrees are unavailable for this project.',
    project_not_found: 'This project no longer exists.',
    project_archived: 'Restore this project before creating a worktree.',
    projects_disabled: 'Worktrees are unavailable in no-project mode.',
  };
  return messages[code] || (error instanceof Error ? error.message : 'Worktree action failed.');
};
