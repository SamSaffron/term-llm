import { useEffect, useRef } from 'preact/hooks';
import type { HubStore } from '../stores/hub-store';

const focusable =
  'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';

export function SecurityPanel({ store }: { store: HubStore }) {
  const dialog = useRef<HTMLDivElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!store.securityOpen.value) return;
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    closeButton.current?.focus();
    const keydown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        store.closeSecurity();
        return;
      }
      if (event.key !== 'Tab' || !dialog.current) return;
      const items = [...dialog.current.querySelectorAll<HTMLElement>(focusable)];
      if (!items.length) return;
      const first = items[0];
      const last = items.at(-1)!;
      if (!dialog.current.contains(document.activeElement)) {
        event.preventDefault();
        (event.shiftKey ? last : first).focus();
      } else if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', keydown);
    return () => {
      document.removeEventListener('keydown', keydown);
      document.body.style.overflow = previousOverflow;
      if (previous?.isConnected) previous.focus({ preventScroll: true });
    };
  }, [store, store.securityOpen.value]);

  if (!store.securityOpen.value) return null;
  const busy = Boolean(store.securityOperation.value);
  return (
    <div
      class="modal-overlay"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) store.closeSecurity();
      }}
    >
      <div
        ref={dialog}
        id="hub-security-dialog"
        class="modal security-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="hub-security-title"
        aria-busy={store.securityLoading.value}
      >
        <div class="modal-header">
          <div>
            <h2 id="hub-security-title">Security</h2>
            <p>Passkeys and active browser sessions.</p>
          </div>
          <button
            ref={closeButton}
            class="hub-btn ghost"
            type="button"
            aria-label="Close security"
            onClick={() => store.closeSecurity()}
          >
            ✕
          </button>
        </div>
        <div class="security-summary">
          {store.activeSessions.value} active session{store.activeSessions.value === 1 ? '' : 's'}
        </div>
        <div class="security-list">
          {store.credentials.value.map((credential) => (
            <div class="security-credential" key={credential.record_id}>
              <div class="security-credential-head">
                <div>
                  <strong>{credential.display_name}</strong>
                  <div class="delegation-meta">
                    Created {new Date(credential.created_at).toLocaleDateString()} · Last used{' '}
                    {new Date(credential.last_used_at).toLocaleString()} ·{' '}
                    {credential.transports?.join(', ') || 'transport not reported'}
                  </div>
                </div>
                <div class="modal-actions">
                  <button
                    class="hub-btn ghost small"
                    type="button"
                    disabled={busy}
                    onClick={() => {
                      const name = window.prompt('Passkey name', credential.display_name);
                      if (name) void store.renameCredential(credential.record_id, name);
                    }}
                  >
                    Rename
                  </button>
                  <button
                    class="hub-btn ghost small"
                    type="button"
                    disabled={busy || store.credentials.value.length === 1}
                    title={
                      store.credentials.value.length === 1
                        ? 'At least one passkey is required'
                        : undefined
                    }
                    onClick={() => void store.removeCredential(credential.record_id)}
                  >
                    Remove
                  </button>
                </div>
              </div>
            </div>
          ))}
        </div>
        <div class="modal-actions security-actions">
          <button
            class="hub-btn ghost"
            type="button"
            disabled={busy}
            onClick={() => {
              const name = window.prompt('Name this passkey', 'Additional passkey');
              if (name) void store.addPasskey(name);
            }}
          >
            Add passkey
          </button>
          <button
            class="hub-btn ghost"
            type="button"
            disabled={busy}
            onClick={() => void store.revokeOtherSessions()}
          >
            Revoke other sessions
          </button>
          <button
            class="hub-btn ghost"
            type="button"
            disabled={busy}
            onClick={() =>
              void store.signOut().then((redirect) => {
                if (redirect) window.location.assign(redirect);
              })
            }
          >
            Sign out
          </button>
        </div>
        <div class="modal-status" role="status" aria-live="polite">
          {store.securityStatus.value}
        </div>
      </div>
    </div>
  );
}
