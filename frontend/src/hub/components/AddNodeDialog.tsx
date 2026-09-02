import { useEffect, useRef, useState } from 'preact/hooks';
import type { HubConfig } from '../config';
import type { ClipboardAdapter } from '../platform/clipboard';
import type { HubStore } from '../stores/hub-store';
import { RegistrationHelp } from './RegistrationHelp';

const focusable =
  'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';

export function AddNodeDialog({
  config,
  store,
  clipboard,
}: {
  config: HubConfig;
  store: HubStore;
  clipboard: ClipboardAdapter;
}) {
  const dialog = useRef<HTMLDivElement>(null);
  const nameInput = useRef<HTMLInputElement>(null);
  const [name, setName] = useState('');
  const [url, setURL] = useState('');
  const [token, setToken] = useState('');

  useEffect(() => {
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    nameInput.current?.focus();
    const keydown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        store.closeAddDialog();
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
  }, [store]);

  const payload = () => ({ name: name.trim(), url: url.trim(), token: token.trim() });
  const submit = async (event: Event) => {
    event.preventDefault();
    await store.addNode(payload());
  };
  const busy = store.nodeOperation.value !== 'idle';

  return (
    <div
      class="modal-overlay"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) store.closeAddDialog();
      }}
    >
      <div
        ref={dialog}
        class="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="add-node-title"
      >
        <div class="modal-header">
          <div class="modal-title">
            <h2 id="add-node-title">Add node</h2>
            <button
              class="hub-btn ghost help-toggle"
              type="button"
              aria-expanded={store.registrationOpen.value}
              aria-controls="registration-help"
              onClick={() =>
                store.registrationOpen.value
                  ? store.closeRegistrationHelp()
                  : void store.openRegistrationHelp()
              }
            >
              Private node
            </button>
          </div>
          <button
            class="hub-btn ghost"
            type="button"
            aria-label="Close"
            onClick={() => store.closeAddDialog()}
          >
            ✕
          </button>
        </div>
        {store.registrationOpen.value && (
          <div id="registration-help">
            <RegistrationHelp config={config} store={store} clipboard={clipboard} />
          </div>
        )}
        <form onSubmit={submit}>
          <label>
            Name
            <input
              ref={nameInput}
              type="text"
              placeholder="jarvis"
              autoComplete="off"
              required
              value={name}
              onInput={(event) => setName(event.currentTarget.value)}
            />
          </label>
          <label>
            URL
            <input
              type="url"
              placeholder="http://127.0.0.1:8081/chat"
              autoComplete="off"
              required
              value={url}
              onInput={(event) => setURL(event.currentTarget.value)}
            />
          </label>
          <label>
            Bearer token <span class="optional">(optional, kept server-side)</span>
            <input
              type="password"
              autoComplete="off"
              value={token}
              onInput={(event) => setToken(event.currentTarget.value)}
            />
          </label>
          <div class="modal-status" role="status" aria-live="polite">
            {store.nodeOperationResult.value}
          </div>
          <div class="modal-actions">
            <button
              class="hub-btn ghost"
              type="button"
              disabled={busy || !url.trim()}
              onClick={() => void store.testNode(payload())}
            >
              {store.nodeOperation.value === 'testing' ? 'Testing…' : 'Test connection'}
            </button>
            <button class="hub-btn primary" type="submit" disabled={busy}>
              {store.nodeOperation.value === 'adding' ? 'Adding…' : 'Add node'}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
