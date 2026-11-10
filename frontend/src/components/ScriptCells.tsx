import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { ScriptCellsResponse } from '../api/endpoints';
import { statePresentation } from '../domain/session-activity';

/** Session-scoped controls stay usable after the response ends or on reconnect. */
export function ScriptCells({ sessionId }: { sessionId: string }) {
  const store = useStore();
  const [snapshot, setSnapshot] = useState<ScriptCellsResponse | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  useEffect(() => {
    let current = true;
    let pending = false;
    setSnapshot(null);
    const refresh = async () => {
      if (pending || document.visibilityState === 'hidden') return;
      pending = true;
      try {
        const result = await store.endpoints.scriptCells(sessionId);
        if (current) {
          setSnapshot(result);
          setError('');
        }
      } catch {
        if (current) setError('Could not refresh scripts. Status may be out of date.');
      } finally {
        pending = false;
      }
    };
    void refresh();
    const timer = window.setInterval(() => void refresh(), 1500);
    return () => {
      current = false;
      window.clearInterval(timer);
    };
  }, [store, sessionId]);
  const act = async (key: string, action: () => Promise<unknown>) => {
    setBusy(key);
    try {
      await action();
      setSnapshot(await store.endpoints.scriptCells(sessionId));
      await store.childSessionStore.refreshActivity(true);
      setError('');
    } catch {
      setError('The request could not be confirmed. Refresh before retrying.');
    } finally {
      setBusy('');
    }
  };
  const cells = snapshot?.cells || [];
  const live = cells.filter((cell) => cell.live).length;
  if (!cells.length && !error) return null;
  return (
    <section class="script-cells" aria-label="Scripts">
      <header class="script-cells-heading">
        <h3>Scripts ({live}/8)</h3>
        {live > 0 && (
          <button
            type="button"
            class="btn"
            disabled={!!busy}
            onClick={() => void act('all', () => store.endpoints.cancelScripts(sessionId))}
          >
            Stop all scripts
          </button>
        )}
      </header>
      {error && <p role="alert">{error}</p>}
      {cells.map((cell) => (
        <div class="script-cell" key={cell.cell_id}>
          <div class="script-cell-heading">
            <code>{cell.cell_id}</code>
            {cell.live && (
              <button
                type="button"
                class="btn"
                disabled={!!busy}
                aria-label={`Cancel script ${cell.cell_id}`}
                onClick={() =>
                  void act(cell.cell_id, () =>
                    store.endpoints.cancelScripts(sessionId, cell.cell_id),
                  )
                }
              >
                Cancel
              </button>
            )}
          </div>
          <p role="status">
            {cell.status === 'quarantined'
              ? 'Stopped — external call still running (outcome unknown)'
              : statePresentation(cell.status, cell.live).label}
            {' · '}
            {Math.floor(cell.elapsed_ms / 1000)}s
            {cell.has_more_output ? ' · Output available to wait_script' : ''}
            {cell.unsettled_calls > 0 ? ` · ${cell.unsettled_calls} unsettled calls` : ''}
            {cell.error_code ? ` · ${cell.error_code}` : ''}
          </p>
          {(snapshot?.pending_approvals || [])
            .filter((prompt) => prompt.cell_id === cell.cell_id)
            .map((prompt) => (
              <div class="script-approval" key={prompt.approval_id}>
                <strong>{prompt.title}</strong>
                <pre>{prompt.path}</pre>
                {prompt.options.map((option) => (
                  <button
                    type="button"
                    class="btn"
                    disabled={!!busy}
                    key={option.index}
                    title={option.description}
                    onClick={() =>
                      void act(prompt.approval_id, () =>
                        store.endpoints.approval(
                          sessionId,
                          { approval_id: prompt.approval_id, choice: option.index },
                          `${prompt.approval_id}_${option.index}`,
                        ),
                      )
                    }
                  >
                    {option.label}
                  </button>
                ))}
              </div>
            ))}
        </div>
      ))}
    </section>
  );
}
