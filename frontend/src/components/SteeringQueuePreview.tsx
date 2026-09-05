import { useStore } from '../app/context';
import { rushActive } from '../domain/steering';

export function SteeringQueuePreview({ selectedSteering }: { selectedSteering: string | null }) {
  const store = useStore();
  const pending = store.steering.value.filter(
    (entry) => entry.sessionId === store.activeSession.value?.id,
  );
  if (!pending.some((entry) => entry.id === selectedSteering)) selectedSteering = null;
  const activeRush = store.activeRush.value;
  const transitioning =
    rushActive(activeRush) && activeRush?.session_id === store.activeSession.value?.id;
  const accepted = pending.filter((entry) => entry.state === 'pending');
  const canRush = Boolean(
    store.steeringCapabilities.value[store.activeSession.value?.id || '']?.can_rush &&
    accepted.length &&
    !transitioning,
  );
  const recovery =
    activeRush &&
    activeRush.session_id === store.activeSession.value?.id &&
    ['blocked', 'failed', 'cancelled'].includes(activeRush.status)
      ? activeRush
      : null;
  return (
    <>
      {recovery && (
        <div role="status" class="steering-queue-footer">
          {recovery.reason || 'Rush stopped'}. Pending guidance is kept below; remove it explicitly
          if no longer needed.
        </div>
      )}
      {pending.length > 0 && (
        <div
          class="pending-steering pending-steering-banner"
          role="list"
          aria-label="Pending messages"
        >
          {pending.map((entry) => (
            <div
              class={`pending-steering-row ${entry.state} ${selectedSteering === entry.id ? 'selected' : ''}`}
              role="listitem"
              key={entry.id}
            >
              <span class="pending-steering-icon" aria-hidden="true">
                …
              </span>
              <span class="pending-steering-text">{entry.content}</span>
              <span class="pending-steering-label">
                ({entry.state === 'sending' ? 'sending…' : entry.state})
              </span>
              {entry.state !== 'committed' && (
                <div class="steering-actions">
                  {canRush && entry.id === accepted.at(-1)?.id && (
                    <button
                      class="pending-steering-action"
                      type="button"
                      onClick={() => void store.rush()}
                    >
                      {accepted.length > 1 ? 'Steer all now' : 'Steer now'}
                    </button>
                  )}
                  <button
                    class="pending-steering-action"
                    type="button"
                    disabled={transitioning}
                    aria-label={`Remove pending steering: ${entry.content}`}
                    onClick={() => void store.cancelSteering(entry.id)}
                  >
                    Remove
                  </button>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
      {transitioning && (
        <div class="steering-queue-footer" role="status" aria-live="polite">
          {activeRush?.status === 'starting' ? 'Starting steered run…' : 'Interrupting…'}
        </div>
      )}
    </>
  );
}
