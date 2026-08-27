import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { deriveRunCenter } from '../domain/run-center';
import { AgentInbox } from './AgentInbox';
import { Overlay } from './Overlay';

const elapsed = (start: number | undefined, end: number | undefined, now: number): string => {
  if (!start) return '';
  const seconds = Math.max(0, Math.floor(((end || now) - start) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  return minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
};

export function RunCenter() {
  const store = useStore();
  const [now, setNow] = useState(Date.now());
  const items = deriveRunCenter(
    store.sessions.value,
    store.runs.value,
    store.interactions.value,
    store.childRuns.value,
    store.interjections.value,
  );
  useEffect(() => {
    if (!store.runCenterOpen.value) return;
    const tick = () => {
      if (document.visibilityState === 'visible') setNow(Date.now());
    };
    const timer = window.setInterval(tick, 1000);
    return () => clearInterval(timer);
  }, [store.runCenterOpen.value]);
  if (!store.runCenterOpen.value) return null;
  const groups = [
    ['Needs attention', items.filter((item) => item.attention)],
    [
      'Active',
      items.filter((item) => !item.attention && !['completed', 'failed'].includes(item.phase)),
    ],
    ['Completed', items.filter((item) => item.phase === 'completed')],
    ['Failed', items.filter((item) => item.phase === 'failed')],
  ] as const;
  return (
    <Overlay
      id="run-center"
      title="Run center"
      wide
      onClose={() => (store.runCenterOpen.value = false)}
    >
      <div class="run-center" aria-live="polite">
        {!items.length && <p>No current or recent runs.</p>}
        {groups.map(([label, group]) =>
          group.length ? (
            <section class="run-center-group" key={label} aria-labelledby={`run-group-${label}`}>
              <h3 id={`run-group-${label}`}>{label}</h3>
              <ul>
                {group.map((item) => (
                  <li key={item.id}>
                    <button
                      type="button"
                      onClick={() => {
                        store.runCenterOpen.value = false;
                        if (item.child) store.markChildRunRead(item.sessionId);
                        void store.resolveAndSelectSession(item.sessionId);
                      }}
                    >
                      <strong>{item.title}</strong>
                      <span>{item.phase}</span>
                      {item.summary && <span>{item.summary}</span>}
                      {item.queuedInterjections > 0 && (
                        <span>
                          {item.queuedInterjections} queued interjection
                          {item.queuedInterjections === 1 ? '' : 's'}
                        </span>
                      )}
                      {item.startedAt && (
                        <time>
                          {item.approximateTime ? 'about ' : ''}
                          {elapsed(item.startedAt, item.endedAt, now)}
                        </time>
                      )}
                    </button>
                  </li>
                ))}
              </ul>
            </section>
          ) : null,
        )}
        <AgentInbox />
      </div>
    </Overlay>
  );
}
