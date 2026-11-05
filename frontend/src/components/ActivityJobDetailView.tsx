import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import type { ActivityJobDetail } from '../api/endpoints';
import { useStore } from '../app/context';
import { statePresentation } from '../domain/session-activity';
import { Icon } from './Icon';

/** A detail request exists only while this exact scope/run view is mounted. */
export function ActivityJobDetailView({
  scope,
  runId,
  onBack,
  openSession,
}: {
  scope: string;
  runId: string;
  onBack: () => void;
  openSession: (id: string) => void;
}) {
  const store = useStore();
  const [detail, setDetail] = useState<ActivityJobDetail | null>(null);
  const [failed, setFailed] = useState(false);
  const back = useRef<HTMLButtonElement>(null);
  useLayoutEffect(() => back.current?.focus(), []);
  useEffect(() => {
    const request = new AbortController();
    let current = true;
    setDetail(null);
    setFailed(false);
    void store.endpoints
      .sessionActivityRun(scope, runId, request.signal)
      .then((response) => {
        if (!current || request.signal.aborted) return;
        if (response.run_id !== runId) throw new Error('Activity run mismatch');
        setDetail(response);
      })
      .catch(() => {
        if (current && !request.signal.aborted) setFailed(true);
      });
    return () => {
      current = false;
      request.abort(new DOMException('Activity detail closed or replaced', 'AbortError'));
    };
  }, [store, scope, runId]);
  return (
    <div class="activity-detail">
      <button ref={back} type="button" class="activity-back" onClick={onBack}>
        <Icon name="chevron-left" />
        Back to activity
      </button>
      {failed ? (
        <p class="activity-notice" role="status">
          Job details are unavailable. The run may no longer belong to this conversation or this
          device may not have access.
        </p>
      ) : !detail ? (
        <p class="activity-notice" role="status">
          Loading job details…
        </p>
      ) : (
        <>
          <h3>{detail.title || 'Job run'}</h3>
          <p class="activity-detail-meta">
            {[detail.model, statePresentation(detail.status, true).label]
              .filter(Boolean)
              .join(' · ')}
          </p>
          {detail.session_id && (
            <button
              type="button"
              class="activity-back"
              onClick={() => openSession(detail.session_id!)}
            >
              Open conversation
              <Icon name="chevron-right" />
            </button>
          )}
          {(['response', 'stdout', 'stderr', 'error'] as const).map((field) =>
            detail[field] ? (
              <section key={field} aria-label={field === 'response' ? 'Response' : field}>
                <h4>{field === 'response' ? 'Response' : field}</h4>
                <pre class="activity-output">{detail[field]}</pre>
              </section>
            ) : null,
          )}
          {!detail.response && !detail.stdout && !detail.stderr && !detail.error && (
            <p class="activity-notice">No output recorded for this run yet.</p>
          )}
          {detail.truncated && (
            <p class="activity-notice">Output was truncated to a bounded preview.</p>
          )}
        </>
      )}
    </div>
  );
}
