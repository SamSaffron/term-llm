import { useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { Overlay } from './Overlay';

export function GoalModal() {
  const store = useStore();
  const current = store.goal.value;
  const [objective, setObjective] = useState(current?.objective || '');
  const [budget, setBudget] = useState(current?.token_budget ? String(current.token_budget) : '');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');
  const savingRef = useRef(false);
  const save = async (goal: Parameters<typeof store.saveGoal>[0]) => {
    if (savingRef.current) return;
    savingRef.current = true;
    setSaving(true);
    setSaveError('');
    try {
      await store.saveGoal(goal);
    } catch (error) {
      setSaveError(errorMessage(error));
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
  };
  return (
    <Overlay title="Session goal" dismissDisabled={saving}>
      <p>Set a persistent objective the agent can keep pursuing across automatic continuations.</p>
      <label class="settings-label">Objective</label>
      <textarea
        class="goal-objective-input"
        rows={5}
        value={objective}
        onInput={(event) => setObjective(event.currentTarget.value)}
      />
      <label class="settings-label">Token budget (optional)</label>
      <input
        type="number"
        min={1}
        value={budget}
        onInput={(event) => setBudget(event.currentTarget.value)}
      />
      {saveError && <p role="alert">{saveError}</p>}
      <div class="modal-actions goal-actions">
        {current && (
          <button class="btn" disabled={saving} onClick={() => void save({ action: 'clear' })}>
            Clear
          </button>
        )}
        {current?.status === 'paused' ? (
          <button class="btn" disabled={saving} onClick={() => void save({ action: 'resume' })}>
            Resume
          </button>
        ) : (
          current && (
            <button class="btn" disabled={saving} onClick={() => void save({ action: 'pause' })}>
              Pause
            </button>
          )
        )}
        <button
          class="btn primary"
          disabled={saving || !objective.trim()}
          onClick={() =>
            void save({
              objective: objective.trim(),
              token_budget: Number(budget) || undefined,
              status: 'active',
            })
          }
        >
          {saving ? 'Saving…' : 'Set goal'}
        </button>
      </div>
    </Overlay>
  );
}
