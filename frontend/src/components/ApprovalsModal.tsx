import { useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { Overlay } from './Overlay';
import type { ApprovalPolicyResponse } from '../api/endpoints';

function applyPolicy(
  store: ReturnType<typeof useStore>,
  sessionId: string,
  value: ApprovalPolicyResponse,
) {
  store.sessionStore.patch(sessionId, {
    approvalDefaultMode: value.default_mode,
    approvalRequestedMode: value.requested_mode,
    approvalEffectiveMode: value.effective_mode,
    guardianAvailable: value.guardian_available,
    guardianAutoSuspended: value.guardian_auto_suspended,
  });
}

export function ApprovalsModal() {
  const store = useStore();
  const session = store.activeSession.value;
  const sessionId = session?.id || '';
  const reportedMode =
    session?.approvalRequestedMode === 'auto' && session.guardianAvailable === false
      ? session.approvalEffectiveMode || store.config.approvalMode || 'prompt'
      : session?.approvalRequestedMode || store.config.approvalMode || 'prompt';
  const [mode, setMode] = useState(reportedMode);
  const [saving, setSaving] = useState(false);
  const [loading, setLoading] = useState(!session?.approvalRequestedMode);
  const [error, setError] = useState('');
  const dirty = useRef(false);
  const modeInputs = useRef<Record<string, HTMLInputElement | null>>({});
  useEffect(() => {
    if (!sessionId) return;
    dirty.current = false;
    setError('');
    setLoading(true);
    void store.endpoints
      .approvalPolicy(sessionId)
      .then((value) => applyPolicy(store, sessionId, value))
      .catch((value: unknown) => setError(errorMessage(value)))
      .finally(() => setLoading(false));
  }, [store, sessionId]);
  useEffect(() => {
    if (!dirty.current) setMode(reportedMode);
  }, [reportedMode]);
  useEffect(() => {
    const focused = document.activeElement;
    if (
      focused instanceof HTMLInputElement &&
      focused.name === 'approvalMode' &&
      focused.value !== mode
    )
      modeInputs.current[mode]?.focus({ preventScroll: true });
  }, [mode]);
  const save = async () => {
    setSaving(true);
    setError('');
    try {
      if (!sessionId) throw new Error('Start the conversation before changing approval mode.');
      const value = await store.endpoints.setApprovalMode(sessionId, mode);
      applyPolicy(store, sessionId, value);
      store.modal.value = '';
    } catch (value) {
      setError(errorMessage(value));
      setSaving(false);
    }
  };
  return (
    <Overlay title="Tool approvals" className="approvals-modal">
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (!saving && !loading) void save();
        }}
      >
        <p class="modal-subtitle">Choose how tool access is reviewed for this conversation.</p>
        <div class="approval-mode-options" role="radiogroup" aria-label="Approval mode">
          {(
            [
              ['prompt', 'Prompt', 'Ask you before unmatched tool access.'],
              ['auto', 'Auto', 'Let Guardian review unmatched tool access.'],
              ['yolo', 'Yolo', 'Approve all tool access without review.'],
            ] as const
          ).map(([value, label, description]) => (
            <label class={`approval-mode-option approval-mode-option-${value}`} key={value}>
              <input
                ref={(element) => {
                  modeInputs.current[value] = element;
                }}
                type="radio"
                name="approvalMode"
                value={value}
                checked={mode === value}
                autoFocus={
                  mode === value && !(value === 'auto' && session?.guardianAvailable === false)
                }
                aria-describedby={
                  value === 'auto' && session?.guardianAvailable === false
                    ? 'approvalGuardianUnavailable'
                    : undefined
                }
                disabled={
                  loading || saving || (value === 'auto' && session?.guardianAvailable === false)
                }
                onChange={() => {
                  dirty.current = true;
                  setMode(value);
                }}
              />
              <span>
                <strong>{label}</strong>
                <small>{description}</small>
              </span>
            </label>
          ))}
        </div>
        {session?.guardianAutoSuspended && (
          <div class="approval-mode-notice" role="status">
            Guardian auto-approval is paused. Select Auto and save to resume it.
          </div>
        )}
        {session?.guardianAvailable === false && (
          <div id="approvalGuardianUnavailable" class="approval-mode-notice" role="status">
            Guardian is unavailable for this runtime, so Auto cannot be selected.
          </div>
        )}
        {error && (
          <div class="modal-error" role="alert">
            {error}
          </div>
        )}
        <div class="modal-actions">
          <button
            class="btn"
            type="button"
            disabled={saving}
            onClick={() => (store.modal.value = '')}
          >
            Cancel
          </button>
          <button class="btn primary" type="submit" disabled={loading || saving}>
            {loading
              ? 'Loading…'
              : saving
                ? 'Saving…'
                : session?.guardianAutoSuspended && mode === 'auto'
                  ? 'Resume Auto'
                  : 'Save'}
          </button>
        </div>
      </form>
    </Overlay>
  );
}
