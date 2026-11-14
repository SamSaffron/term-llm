import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { activeExecutionCount, activityRows, activityUncertain } from '../domain/session-activity';
import { Icon } from './Icon';
import { Overlay } from './Overlay';
import { ActivityJobDetailView } from './ActivityJobDetailView';
import './activity.css';

function LifecycleIcon({ name }: { name: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.75"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {name === 'running' ? (
        <path d="M19.4 8a8 8 0 1 1-4-3.5" />
      ) : name === 'error' ? (
        <>
          <path d="m10.3 4.9-7 12a2 2 0 0 0 1.7 3h14a2 2 0 0 0 1.7-3l-7-12a2 2 0 0 0-3.4 0Z" />
          <path d="M12 9v4m0 3v.1" />
        </>
      ) : (
        <>
          <circle cx="12" cy="12" r="8" />
          {name === 'queued' ? (
            <path d="M12 7v5l3 2" />
          ) : name === 'waiting' ? (
            <path d="M9.5 9v6m5-6v6" />
          ) : name === 'done' ? (
            <path d="m8.5 12 2.5 2.5 4.5-5" />
          ) : name === 'stopped' ? (
            <rect x="9" y="9" width="6" height="6" rx=".8" />
          ) : (
            <path d="M10 9a2 2 0 1 1 3 1.7c-.8.4-1 1-1 1.8m0 3.5v.1" />
          )}
        </>
      )}
    </svg>
  );
}

/** The conversation whose activity is shown: its agent heads the subagent list. */
function ActivityRoot({ scope }: { scope: string }) {
  const store = useStore();
  const session = store.activeSession.value;
  if (!session || store.draftActive.value || session.id !== scope) return null;
  const model = [session.activeProvider, session.activeModel].filter(Boolean).join(':');
  return (
    <div class="activity-root">
      <span class="activity-root-icon" aria-hidden="true">
        <Icon name="bot" />
      </span>
      <span class="activity-copy">
        <span class="activity-label">
          <span class="activity-agent">{session.agent || 'chat'}</span>{' '}
          <span class="activity-root-here">this chat</span>
        </span>
        {model && <span class="activity-meta">{model}</span>}
      </span>
    </div>
  );
}

export function ActivitySheet({ onClose }: { onClose: () => void }) {
  const store = useStore();
  const childStore = store.childSessionStore;
  const snapshot = childStore.activity.value;
  const scope = childStore.activitySessionId.value;
  const [selectedRun, setSelectedRun] = useState<{ scope: string; runId: string } | null>(null);
  const runId = selectedRun?.scope === scope ? selectedRun.runId : '';
  const runButtons = useRef(new Map<string, HTMLButtonElement>());
  const restoreRun = useRef('');
  useLayoutEffect(() => {
    if (!runId && restoreRun.current) {
      runButtons.current.get(restoreRun.current)?.focus();
      restoreRun.current = '';
    }
  }, [runId]);
  const [now, setNow] = useState(Date.now);
  const rows = activityRows(snapshot, now);
  const activeCount = activeExecutionCount(snapshot);
  const uncertain = activityUncertain(snapshot, childStore.activityStale.value);
  useEffect(() => {
    if (!activeCount) return;
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') setNow(Date.now());
    }, 1000);
    return () => clearInterval(timer);
  }, [activeCount]);
  const openSession = (id: string) => {
    onClose();
    const session = store.sessions.peek().find((entry) => entry.id === id);
    if (session) void store.selectSession(session);
    else void store.resolveAndSelectSession(id, false, { newChatOnMiss: false });
  };
  return (
    <Overlay title="Activity" className="activity-panel" onClose={onClose} focusContent={false}>
      {runId ? (
        <ActivityJobDetailView
          key={`${scope}:${runId}`}
          scope={scope}
          runId={runId}
          onBack={() => {
            restoreRun.current = runId;
            setSelectedRun(null);
          }}
          openSession={openSession}
        />
      ) : (
        <>
          <ActivityRoot scope={scope} />
          <div class="activity-list">
            {(['agent', 'job'] as const).map((kind) => {
              const group = rows.filter((row) => row.kind === kind);
              if (!group.length) return null;
              const title = kind === 'agent' ? 'Subagents' : 'Jobs';
              const active = group.filter((row) => row.active).length;
              return (
                <section key={kind} aria-label={title}>
                  <h3 class="activity-section-title">
                    <span>{title}</span>
                    {!uncertain && (
                      <span class="activity-section-count">
                        <span class="visually-hidden">, </span>
                        {[
                          active && `${active} active`,
                          active < group.length && `${group.length - active} done`,
                        ]
                          .filter(Boolean)
                          .join(' · ')}
                      </span>
                    )}
                  </h3>
                  {group.map((row) => {
                    const content = (
                      <>
                        <span
                          class="activity-state"
                          data-tone={row.state.tone}
                          data-icon={row.state.icon}
                          role="img"
                          aria-label={row.state.label}
                        >
                          <LifecycleIcon name={row.state.icon} />
                        </span>
                        <span class="activity-copy">
                          <span class="activity-label">
                            {row.agent ? (
                              <>
                                <span class="activity-agent">{row.agent}</span>{' '}
                                <span class={`activity-task${row.title ? '' : ' empty'}`}>
                                  {row.title || 'no summary'}
                                </span>
                              </>
                            ) : (
                              row.title
                            )}
                          </span>
                          {row.subtitle && <span class="activity-meta">{row.subtitle}</span>}
                          <span class="visually-hidden">
                            {row.kind === 'job' ? 'Job run' : 'Agent'}
                          </span>
                        </span>
                        {(row.runId || row.sessionId) && (
                          <span class="activity-chevron">
                            <Icon name="chevron-right" />
                          </span>
                        )}
                      </>
                    );
                    return row.runId || row.sessionId ? (
                      <button
                        type="button"
                        class="activity-row"
                        key={row.key}
                        ref={(button) => {
                          if (row.runId) {
                            if (button) runButtons.current.set(row.runId, button);
                            else runButtons.current.delete(row.runId);
                          }
                        }}
                        onClick={() =>
                          row.runId
                            ? setSelectedRun({ scope, runId: row.runId })
                            : openSession(row.sessionId!)
                        }
                      >
                        {content}
                      </button>
                    ) : (
                      <div class="activity-row" key={row.key}>
                        {content}
                      </div>
                    );
                  })}
                </section>
              );
            })}
          </div>
          {!rows.length && (
            <p class="activity-notice">
              {uncertain
                ? 'Related activity could not be fully determined.'
                : 'No related activity in this snapshot.'}
            </p>
          )}
          {childStore.activityStale.value && (
            <p class="activity-notice" role="status">
              Activity could not be refreshed. Showing the last snapshot.
            </p>
          )}
          {snapshot?.agents_availability === 'unavailable' && (
            <p class="activity-notice">Agent activity is unavailable.</p>
          )}
          {snapshot?.jobs_availability === 'unavailable' &&
            Boolean(snapshot.jobs?.length || snapshot.jobs_partial) && (
              <p class="activity-notice">
                Job activity is unavailable. Agent activity is still shown.
              </p>
            )}
          {(snapshot?.agents_partial || snapshot?.jobs_partial) && (
            <p class="activity-notice">
              This is a bounded snapshot; some related activity may not be included.
            </p>
          )}
        </>
      )}
    </Overlay>
  );
}
