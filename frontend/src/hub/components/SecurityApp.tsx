import { useEffect } from 'preact/hooks';
import { hubPath, type HubConfig } from '../config';
import type { HubStore } from '../stores/hub-store';
import { SecurityPanel } from './SecurityPanel';

/** Standalone access to the same security controls used by the Hub. */
export function SecurityApp({ config, store }: { config: HubConfig; store: HubStore }) {
  useEffect(() => {
    void store.openSecurity();
    return () => store.dispose();
  }, [store]);
  return (
    <div class="hub-auth">
      <main class="auth-card">
        <h1>Security</h1>
        <p>Manage your passkeys and browser sessions.</p>
        <button type="button" onClick={() => void store.openSecurity()}>
          Manage passkeys
        </button>
        <p>
          <a href={config.formAction || hubPath(config.basePath, '/')}>Back to chat</a>
        </p>
      </main>
      <SecurityPanel store={store} />
    </div>
  );
}
