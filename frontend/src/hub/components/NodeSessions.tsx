import { relativeSessionTime, sessionMetaText } from '../domain/formatting';
import type { HubNode, HubNodeSession } from '../domain/types';

function SessionGroup({
  label,
  sessions,
  active = false,
}: {
  label: string;
  sessions: HubNodeSession[];
  active?: boolean;
}) {
  if (!sessions.length) return null;
  return (
    <div class="node-session-group">
      <div class="node-session-label">{label}</div>
      {sessions.map((session) => {
        const className = `node-session-row${session.interaction_required ? ' is-input-required' : active ? ' is-active' : ''}`;
        const content = (
          <>
            <span class="node-session-title">
              {session.short_title || session.long_title || session.id || 'Untitled session'}
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
  const active = sessions.active ?? [];
  const recent = sessions.recent ?? [];
  const capability = String(sessions.attention_capability || '');
  const unseen = Number(sessions.unseen_count) || 0;
  const checked = Number(sessions.attention_last_success_at) || 0;
  const stale = !node.status.reachable || !checked || Date.now() - checked > 180_000;
  return (
    <div class="node-sessions">
      <SessionGroup label="Active" sessions={active} active />
      {active.length > 0 ? (
        <SessionGroup label="Recent" sessions={recent} />
      ) : (
        <>
          <SessionGroup label="Last session" sessions={recent.slice(0, 1)} />
          <SessionGroup label="Recent" sessions={recent.slice(1)} />
        </>
      )}
      {!active.length && !recent.length && <div class="node-session-empty">No sessions yet</div>}
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
