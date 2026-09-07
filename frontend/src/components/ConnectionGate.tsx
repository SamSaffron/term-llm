import '../styles/features/connection.css';
import { useEffect, useRef, useState } from 'preact/hooks';
import type { AppStore } from '../stores/app-store';
import { APIError } from '../api/client';
import { Icon } from './Icon';

/** Authentication is a prerequisite, not a settings panel over an unusable chat. */
export function ConnectionGate({ store }: { store: AppStore }) {
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const input = useRef<HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  // Announce the screen change without opening a mobile keyboard.
  useEffect(() => heading.current?.focus(), []);
  const connect = async (event: SubmitEvent) => {
    event.preventDefault();
    if (busy || !token.trim()) return;
    setBusy(true);
    setError('');
    try {
      await store.connect(token.trim());
    } catch (reason) {
      setError(
        reason instanceof APIError && [401, 403].includes(reason.status)
          ? token.trim().startsWith('sk-')
            ? 'This looks like an API key. Use the server token instead — copy the value after token: in your term-llm terminal.'
            : 'That token didn’t match. Copy the value after token: in your term-llm terminal. Restarted the server? Use its new token.'
          : 'Could not connect to the server. Check that term-llm is still running, then try again.',
      );
      input.current?.focus();
      input.current?.select();
    } finally {
      setBusy(false);
    }
  };
  return (
    <main class="connection-gate">
      <div class="connection-card">
        <div class="connection-brand">
          <span aria-hidden="true">&gt;_</span> term-llm
        </div>
        <h1 ref={heading} tabIndex={-1}>
          Connect to your server
        </h1>
        {store.startupDone.value && (
          <p role="status">Connection expired. Use the server’s current token.</p>
        )}
        <div class="connection-instructions">
          <p id="connection-help">
            Copy the token from the terminal running <code>term-llm serve web</code>.
          </p>
          <div class="connection-terminal" aria-label="Example server output">
            <div class="connection-terminal-title">
              <span aria-hidden="true">● ● ●</span> SERVER OUTPUT · EXAMPLE
            </div>
            <pre>
              <span>auth: bearer required</span>
              {'\n'}token: <mark>your-server-token</mark>
              {'\n'}
              <span> (auto-generated; …)</span>
            </pre>
          </div>
        </div>
        <form onSubmit={connect} aria-busy={busy}>
          <label class="connection-label" for="connection-token">
            Server token
          </label>
          <input
            ref={input}
            id="connection-token"
            type="password"
            autoComplete="off"
            autoCorrect="off"
            spellcheck={false}
            autoCapitalize="none"
            placeholder="Paste the value after token:"
            value={token}
            aria-describedby={`connection-help${error ? ' connection-error' : ''}`}
            aria-invalid={error ? true : undefined}
            onInput={(event) => {
              setToken(event.currentTarget.value);
              setError('');
            }}
            readOnly={busy}
          />
          {error && (
            <div class="connection-error" id="connection-error" role="alert">
              <Icon name="alert-circle" />
              <p>{error}</p>
            </div>
          )}
          <button
            class="btn primary connection-submit"
            type="submit"
            disabled={busy || !token.trim()}
          >
            {busy ? 'Connecting…' : error ? 'Try again' : 'Connect to server'}
            {!busy && <Icon name="chevron-right" />}
          </button>
        </form>
        <p class="connection-footer">Saved in this browser.</p>
      </div>
    </main>
  );
}
