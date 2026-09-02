import type { HubStore } from '../stores/hub-store';

export function SecurityPanel({ store }: { store: HubStore }) {
  if (!store.securityOpen.value) return null;
  const busy = Boolean(store.securityOperation.value);
  return (
    <section
      id="hub-security-panel"
      class="delegations-panel"
      aria-label="Security"
      aria-busy={store.securityLoading.value}
    >
      <div class="delegations-head">
        <div>
          <h2>Security</h2>
          <p>Passkeys and active browser sessions.</p>
        </div>
        <span>
          {store.activeSessions.value} active session{store.activeSessions.value === 1 ? '' : 's'}
        </span>
      </div>
      <div class="delegations-list">
        {store.credentials.value.map((credential) => (
          <div class="delegation-card" key={credential.record_id}>
            <strong>{credential.display_name}</strong>
            <div class="delegation-meta">
              Created {new Date(credential.created_at).toLocaleDateString()} · Last used{' '}
              {new Date(credential.last_used_at).toLocaleString()} ·{' '}
              {credential.transports?.join(', ') || 'transport not reported'}
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
        ))}
      </div>
      <div class="modal-actions">
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
    </section>
  );
}
