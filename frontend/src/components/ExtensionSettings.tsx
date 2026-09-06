import '../styles/features/extensions.css';
import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import {
  extensionRuntime,
  exitExtensionRecovery,
  recoveryURL,
  type ExtensionStatus,
} from '../stores/extension-runtime';
import { errorMessage } from '../domain/text';

export function ExtensionSettings() {
  const store = useStore();
  const [status, setStatus] = useState<ExtensionStatus | null>(null);
  const [enabled, setEnabled] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    void store.endpoints
      .extensionsStatus()
      .then((value) => {
        if (live) {
          setStatus(value);
          setEnabled(value.enabled);
        }
      })
      .catch((error) => {
        if (live) setError(errorMessage(error));
      });
    return () => {
      live = false;
    };
  }, [store]);
  const controlled = status?.disabled || status?.source === 'command-line';
  const safe = extensionRuntime.safeMode.value;
  const reload = async (save: boolean) => {
    if (!status) return;
    setBusy(true);
    setError('');
    try {
      const next = save
        ? await store.endpoints.extensionsSave(enabled, status.config_revision)
        : await store.endpoints.extensionsReload();
      setStatus(next);
      setEnabled(next.enabled);
      if (save && !safe) location.reload();
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy(false);
    }
  };
  const move = (id: string, offset: number) => {
    const next = [...enabled];
    const i = next.indexOf(id);
    const j = i + offset;
    if (i < 0 || j < 0 || j >= next.length) return;
    [next[i], next[j]] = [next[j], next[i]];
    setEnabled(next);
  };
  const startExtensionBuilder = async () => {
    store.modal.value = '';
    const existing = store.sessions
      .peek()
      .find((session) => session.agent === 'extension-builder' && !session.archived);
    if (existing) {
      await store.selectSession(existing);
      return;
    }
    store.newChat();
    store.setPreference('agent', 'extension-builder');
    store.composer.prompt.value =
      'Help me find a look that feels like mine. I’d like to personalize this interface.';
  };
  const ordered = status
    ? [
        ...enabled.map(
          (id) =>
            status.entries.find((e) => e.id === id) || {
              id,
              title: id,
              description: '',
              error: 'Not found in extension directory',
            },
        ),
        ...status.entries.filter((e) => !enabled.includes(e.id)),
      ]
    : [];
  return (
    <section class="extension-settings" aria-label="Extensions">
      {store.config.agentNames.includes('extension-builder') && (
        <button type="button" class="btn extension-builder" onClick={startExtensionBuilder}>
          ✦ Make UI Mine
        </button>
      )}
      {safe && (
        <div class="extension-recovery" role="status">
          <strong>Safe mode is on</strong>
          <button type="button" class="btn" onClick={exitExtensionRecovery}>
            Exit safe mode and reload
          </button>
        </div>
      )}
      {error && (
        <p class="extension-error" role="alert">
          {error}
        </p>
      )}
      {status && (
        <>
          {controlled && (
            <p class="extension-caption">
              {status.disabled ? 'Disabled by boot flag.' : 'Controlled by boot flags.'}
            </p>
          )}
          {!ordered.length && <p class="extension-empty">No extensions installed.</p>}
          <div class="extension-list">
            {ordered.map((entry) => (
              <div
                class={`extension-card ${enabled.includes(entry.id) ? 'is-enabled' : ''}`}
                key={entry.id}
              >
                <label>
                  <input
                    type="checkbox"
                    checked={enabled.includes(entry.id)}
                    disabled={controlled || busy}
                    onChange={(event) =>
                      setEnabled(
                        event.currentTarget.checked
                          ? [...enabled, entry.id]
                          : enabled.filter((id) => id !== entry.id),
                      )
                    }
                  />
                  <span>
                    <strong>{entry.title || entry.id}</strong>
                    {entry.description && <small>{entry.description}</small>}
                    {entry.error && <small class="extension-error">{entry.error}</small>}
                  </span>
                </label>
                {enabled.includes(entry.id) && (
                  <div class="extension-order">
                    <button
                      class="btn"
                      type="button"
                      aria-label={`Move ${entry.title} up`}
                      disabled={controlled || busy || enabled.indexOf(entry.id) === 0}
                      onClick={() => move(entry.id, -1)}
                    >
                      ↑
                    </button>
                    <button
                      class="btn"
                      type="button"
                      aria-label={`Move ${entry.title} down`}
                      disabled={
                        controlled || busy || enabled.indexOf(entry.id) === enabled.length - 1
                      }
                      onClick={() => move(entry.id, 1)}
                    >
                      ↓
                    </button>
                  </div>
                )}
              </div>
            ))}
          </div>
          <div class="extension-actions">
            {!safe &&
              !status.disabled &&
              status.generation !== extensionRuntime.generation.value && (
                <button type="button" class="btn" disabled={busy} onClick={() => location.reload()}>
                  Reload UI to apply changes
                </button>
              )}
            <button
              type="button"
              class="btn primary"
              disabled={busy || controlled}
              onClick={() => void reload(true)}
            >
              {busy ? 'Saving…' : safe ? 'Save configuration' : 'Save and reload UI'}
            </button>
            <button type="button" class="btn" disabled={busy} onClick={() => void reload(false)}>
              Rescan files
            </button>
            <button
              type="button"
              class="btn"
              disabled={busy || controlled || !enabled.length}
              onClick={() => setEnabled([])}
            >
              Disable all
            </button>
          </div>
          {extensionRuntime.errors.value.map((message) => (
            <p class="extension-error" key={message}>
              {message}
            </p>
          ))}
          <details class="extension-details">
            <summary>Directory & recovery</summary>
            <code>{status.directory}</code>
            <p>
              <a href={recoveryURL()} target="_blank" rel="noreferrer">
                Open safe mode
              </a>{' '}
              · Extensions run trusted code.
            </p>
          </details>
        </>
      )}
    </section>
  );
}
