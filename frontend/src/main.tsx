import { effect } from '@preact/signals';
import { initializeExtensionRecovery } from './stores/extension-runtime';
import { loadExtensions, watchExtensionActivation } from './api/extensions';
import { render } from 'preact';
import { App } from './app/App';
import { readInjectedConfig } from './app/config';
import { AppStore } from './stores/app-store';
import { reduceResponse } from './domain/response';
import { convertServerMessages, windowTranscript } from './domain/transcript';
import './styles/app.css';

async function bootstrap(): Promise<void> {
  // Hub proxy context and <base> rewriting are injected before this deferred module executes.
  const config = readInjectedConfig();
  initializeExtensionRecovery(config);
  if (config.webRTC && config.signalingURL) {
    const { installWebRTC } = await import('./platform/webrtc');
    installWebRTC();
  }
  const store = new AppStore(config);
  const root = document.getElementById('root');
  if (!root) throw new Error('term-llm application mount is missing');
  render(<App store={store} />, root);
  const extensionMount = document.createElement('div');
  extensionMount.id = 'extension-mount';
  document.body.append(extensionMount);
  // Wait for authentication/startup, including a token entered on first launch.
  // Execute each extension once per document, never again on session changes.
  let extensionsStarted = false;
  effect(() => {
    if (store.startupDone.value && !store.authRequired.value && !extensionsStarted) {
      extensionsStarted = true;
      requestAnimationFrame(() => {
        void loadExtensions(store).then(() => watchExtensionActivation(store));
      });
    }
  });

  const explicitTestBridge = import.meta.env.DEV && window.__TERM_LLM_ENABLE_TEST_BRIDGE__ === true;
  if (explicitTestBridge) {
    // Deliberately excludes token, API headers and injected credentials.
    window.__TERM_LLM_TEST__ = Object.freeze({
      store: Object.freeze({
        sessions: store.sessions,
        activeSessionId: store.activeSessionId,
        runs: store.runs,
        selectSession: (id: string) => {
          const session = store.sessions.peek().find((entry) => entry.id === id);
          return session ? store.selectSession(session) : Promise.resolve();
        },
        applyResponseEvent: (
          sessionId: string,
          event: Parameters<typeof store.applyResponseEvent>[1],
        ) => store.applyResponseEvent(sessionId, event),
      }),
      domain: Object.freeze({ reduceResponse, convertServerMessages, windowTranscript }),
    });
  }
}

queueMicrotask(() => {
  void bootstrap().catch((error) => {
    const root = document.getElementById('root');
    if (root)
      root.textContent = error instanceof Error ? error.message : 'The chat UI could not start.';
    console.error(error);
  });
});
