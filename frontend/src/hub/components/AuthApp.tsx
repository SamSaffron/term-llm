import { useState } from 'preact/hooks';
import { hubPath, type HubConfig } from '../config';
import type { AuthStore } from '../stores/auth-store';

export function AuthApp({ config, store }: { config: HubConfig; store: AuthStore }) {
  const page = config.passkey;
  const [code, setCode] = useState('');
  const [displayName, setDisplayName] = useState(page?.defaultName ?? '');
  if (!page) throw new Error('Hub passkey configuration is missing.');
  const submit = (event: Event) => {
    event.preventDefault();
    const requested =
      new URLSearchParams(window.location.search).get('return') || hubPath(config.basePath, '/');
    void store.submit(page.mode, { code, displayName, returnPath: requested });
  };
  return (
    <div class="hub-auth">
      <main class="auth-card">
        <h1>{page.heading}</h1>
        <p>{page.description}</p>
        <form onSubmit={submit}>
          {page.needsCode && (
            <label>
              One-time setup code
              <input
                type="password"
                autoComplete="one-time-code"
                autoFocus
                value={code}
                onInput={(event) => setCode(event.currentTarget.value)}
              />
            </label>
          )}
          {page.needsName && (
            <label>
              Passkey name
              <input
                maxLength={80}
                required
                autoFocus={!page.needsCode}
                value={displayName}
                onInput={(event) => setDisplayName(event.currentTarget.value)}
              />
            </label>
          )}
          <button type="submit" disabled={store.busy.value}>
            {store.busy.value ? 'Waiting for your passkey…' : page.button}
          </button>
        </form>
        <div
          class={`status${store.error.value ? ' error' : ''}`}
          role={store.error.value ? 'alert' : 'status'}
          aria-live="polite"
        >
          {store.error.value}
        </div>
      </main>
    </div>
  );
}
