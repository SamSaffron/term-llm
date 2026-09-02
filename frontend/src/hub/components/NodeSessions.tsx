import { relativeSessionTime, sessionMetaText } from '../domain/formatting';
import type { HubNode, HubNodeSession } from '../domain/types';

function SessionRows({ sessions }: { sessions: HubNodeSession[] }) {
  if (!sessions.length) return null;
  return (
    <div class="node-session-group">
      {sessions.map((session) => {
        const active = Boolean(session.active_run);
        const className = `node-session-row${session.interaction_required ? ' is-input-required' : active ? ' is-active' : ''}`;
        const content = (
          <>
            <span class="node-session-title">
              {session.short_title || session.long_title || session.id || 'Untitled session'}
              {session.pinned && (
                <span class="node-session-pin" title="Pinned" aria-label="Pinned">
                  <svg viewBox="0 0 16 16" aria-hidden="true">
                    <path
                      fill="currentColor"
                      d="m9.5 1.5 5 5L13 8l-.7-.2-2.7 2.7.2 2.3-1.1 1.1-3.1-3.1-3.4 3.4-.9-.9 3.4-3.4-3.1-3.1 1.1-1.1 2.3.2 2.7-2.7-.2-.7z"
                    />
                  </svg>
                </span>
              )}
            </span>
            <span class="node-session-meta">{sessionMetaText(session, { active })}</span>
          </>
        );
        return session.resume_path ? (
          <a
            key={session.id}
            class={className}
            href={session.resume_path}
            title={session.long_title || session.short_title || session.id || 'Session'}
          >
            {content}
          </a>
        ) : (
          <div key={session.id} class={className} aria-disabled="true">
            {content}
          </div>
        );
      })}
    </div>
  );
}

function displayedSessions(node: HubNode) {
  const sessions = node.sessions;
  if (!sessions) return [];

  const shown = new Set<string>();
  const result: HubNodeSession[] = [];
  for (const session of [...(sessions.recent ?? []), ...(sessions.active ?? [])]) {
    if (!session.id || shown.has(session.id)) continue;
    shown.add(session.id);
    result.push(session);
    if (result.length === 4) break;
  }
  return result;
}

export function NodeSessions({ node }: { node: HubNode }) {
  const sessions = node.sessions;
  if (!sessions) {
    return (
      <div class="node-sessions">
        <div class="node-session-empty">
          {node.status.reachable ? 'Sessions unavailable' : 'Sessions unavailable while offline'}
        </div>
      </div>
    );
  }
  const displayed = displayedSessions(node);
  const capability = String(sessions.attention_capability || '');
  const unseen = Number(sessions.unseen_count) || 0;
  const checked = Number(sessions.attention_last_success_at) || 0;
  const stale = !node.status.reachable || !checked || Date.now() - checked > 180_000;
  return (
    <div class="node-sessions">
      <SessionRows sessions={displayed} />
      {!displayed.length && <div class="node-session-empty">No sessions yet</div>}
      {capability === 'unavailable' && (
        <div class="node-attention-freshness">Terminal attention unavailable</div>
      )}
      {capability === 'lost' && (
        <div class="node-attention-freshness is-stale">
          Terminal attention capability lost · cached state retained
        </div>
      )}
      {capability !== 'lost' && capability !== 'unavailable' && unseen > 0 && stale && (
        <div class="node-attention-freshness is-stale">
          {unseen} ready to review · last checked{' '}
          {checked ? relativeSessionTime(checked) : 'not yet checked'}
        </div>
      )}
    </div>
  );
}
