import type { HubNode, HubNodeSession } from './types';

const minute = 60_000;
const hour = 60 * minute;
const day = 24 * hour;

export function relativeSessionTime(value: number | string | undefined, now = Date.now()): string {
  const timestamp = typeof value === 'string' ? Date.parse(value) : Number(value);
  if (!Number.isFinite(timestamp) || timestamp <= 0) return '';
  const diff = Math.max(0, now - timestamp);
  if (diff < minute) return 'just now';
  if (diff < hour) return `${Math.floor(diff / minute)}m ago`;
  if (diff < day) return `${Math.floor(diff / hour)}h ago`;
  if (diff < 7 * day) return `${Math.floor(diff / day)}d ago`;
  return new Date(timestamp).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

export function countLabel(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}

export function sessionMessageCountLabel(count: number | undefined): string {
  const value = Number(count);
  return Number.isFinite(value) && value > 0 ? countLabel(value, 'message') : '';
}

export function sessionMetaText(
  session: HubNodeSession,
  options: { active?: boolean; now?: number } = {},
): string {
  const parts: string[] = [];
  if (session.interaction_required) {
    const count = Number(session.pending_interaction_count) || 1;
    parts.push(count === 1 ? 'waiting for input' : `${count} decisions waiting`);
  } else if (options.active || session.active_run) {
    parts.push('active now');
  }
  const relative = relativeSessionTime(session.last_message_at, options.now);
  if (relative) parts.push(relative);
  const messages = sessionMessageCountLabel(session.message_count);
  if (messages) parts.push(messages);
  return parts.join(' · ');
}

export function nodeResumePath(node: HubNode): string {
  const sessions = node.sessions;
  return (
    sessions?.resume_path ||
    sessions?.active?.[0]?.resume_path ||
    sessions?.recent?.[0]?.resume_path ||
    ''
  );
}

export function shellQuote(value: string): string {
  return `'${String(value).replaceAll("'", "'\\''")}'`;
}

export function currentHubURL(location: Pick<Location, 'origin'>, basePath: string): string {
  return `${location.origin}${basePath || ''}/`;
}

export function buildRegistrationCommand(hubURL: string): string {
  return `export HUB_URL=${shellQuote(hubURL)}
export HUB_REGISTRATION_TOKEN="<copy registration token from above>"
export NODE_TOKEN="$(openssl rand -hex 32)"
export NODE_ID="docker-$(hostname)"

# Add --agent, --port, or --base-path if this node needs them.
term-llm serve web \\
  --token "$NODE_TOKEN" \\
  --hub-url "$HUB_URL" \\
  --hub-node-id "$NODE_ID" \\
  --hub-node-name "Docker $(hostname)" \\
  --hub-connect reverse \\
  --hub-register \\
  --hub-registration-token "$HUB_REGISTRATION_TOKEN"`;
}

export function activeSessionCount(nodes: HubNode[]): number {
  return nodes.reduce((total, node) => {
    if (!node.sessions) return total;
    const explicit = Number(node.sessions.active_count);
    if (Number.isFinite(explicit) && explicit > 0) return total + explicit;
    return total + (node.sessions.active?.length ?? 0);
  }, 0);
}
