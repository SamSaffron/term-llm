import { render } from 'preact';
import { HubClient } from '../api/hub-client';
import { AuthApp } from './components/AuthApp';
import { BearerLogin } from './components/BearerLogin';
import { HubApp } from './components/HubApp';
import { HubErrorBoundary } from './components/HubErrorBoundary';
import { readHubConfig } from './config';
import { browserClipboard } from './platform/clipboard';
import { browserPasskeyPlatform } from './platform/passkeys';
import { AuthStore } from './stores/auth-store';
import { HubStore } from './stores/hub-store';
import './styles/hub.css';

function bootstrap(): void {
  const root = document.getElementById('root');
  if (!root) throw new Error('Hub application mount is missing.');
  const config = readHubConfig(root);
  const client = new HubClient(config);
  let application;
  if (config.page === 'dashboard') {
    application = (
      <HubApp
        config={config}
        store={new HubStore(client, config.passkeyAuth ? browserPasskeyPlatform() : undefined)}
        clipboard={browserClipboard()}
      />
    );
  } else if (config.page === 'passkey-auth') {
    application = (
      <AuthApp config={config} store={new AuthStore(client, browserPasskeyPlatform())} />
    );
  } else {
    application = <BearerLogin config={config} />;
  }
  render(<HubErrorBoundary>{application}</HubErrorBoundary>, root);
}

queueMicrotask(() => {
  try {
    bootstrap();
  } catch (error) {
    const root = document.getElementById('root');
    if (root) {
      const detail = error instanceof Error ? error.message : 'The Hub UI could not start.';
      render(
        <main class="hub-fatal" role="alert">
          <h1>Hub could not start</h1>
          <p>{detail}</p>
          <button type="button" onClick={() => window.location.reload()}>
            Reload
          </button>
        </main>,
        root,
      );
    }
    console.error(error);
  }
});
