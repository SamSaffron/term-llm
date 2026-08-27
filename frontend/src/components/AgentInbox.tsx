import { useStore } from '../app/context';

const duration = (startedAt?: number, endedAt?: number, now = Date.now()): string => {
  if (!startedAt) return '';
  const seconds = Math.max(0, Math.floor(((endedAt || now) - startedAt) / 1000));
  return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m`;
};

/** Compact child-agent history used inside sidebar/run-center adopters. */
export function AgentInbox() {
  const store = useStore();
  return (
    <section class="agent-inbox" aria-labelledby="agent-inbox-title">
      <h3 id="agent-inbox-title">Agent inbox</h3>
      {!store.childRuns.value.length && <p>No child-agent work yet.</p>}
      <ul>
        {store.childRuns.value.map((child) => (
          <li key={child.sessionId} class={child.attention ? 'attention' : ''}>
            <div class="agent-inbox-item">
              <button
                type="button"
                onClick={() => {
                  store.markChildRunRead(child.sessionId);
                  store.runCenterOpen.value = false;
                  void store.resolveAndSelectSession(child.sessionId);
                }}
              >
                <strong>{child.title}</strong>
                <span>{child.state}</span>
                {child.taskSummary && <span>{child.taskSummary}</span>}
                {child.startedAt && (
                  <time>
                    {child.approximateTimes ? 'about ' : ''}
                    {duration(child.startedAt, child.endedAt)}
                  </time>
                )}
                {child.attention && <span class="sr-only">Needs attention</span>}
              </button>
              <button
                type="button"
                class="btn-link"
                aria-label={`Open parent session for ${child.title}`}
                onClick={() => {
                  store.runCenterOpen.value = false;
                  void store.resolveAndSelectSessionAtMessage(
                    child.parentSessionId,
                    child.parentSpawnItemId,
                  );
                }}
              >
                Parent
              </button>
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}
