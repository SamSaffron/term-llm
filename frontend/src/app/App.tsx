import { Component, type ComponentChildren, type ComponentType } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import { StoreContext } from './context';
import type { AppStore, Toast } from '../stores/app-store';
import { Sidebar } from '../components/Sidebar';
import { Header } from '../components/Header';
import { Transcript } from '../components/Transcript';
import { Composer } from '../components/Composer';
import { DiffSidebar, PlanSurface } from '../components/Panels';
import { Modals } from '../components/Modals';
import { Lightbox } from '../components/Lightbox';
import { Icon } from '../components/Icon';
import { installVisualViewportSizing } from '../platform/browser';

const toastIcon = (kind: Toast['kind']) =>
  kind === 'success' ? 'check' : kind === 'error' ? 'alert-circle' : 'info';

class ErrorBoundary extends Component<{ children: ComponentChildren }, { error: string }> {
  state = { error: '' };
  componentDidCatch(error: Error) {
    this.setState({ error: error.message || 'The chat UI encountered an error.' });
  }
  render(props: { children: ComponentChildren }, state: { error: string }) {
    return state.error ? (
      <div class="startup-splash" role="alert">
        <div class="startup-card">
          <div class="startup-title">The chat UI could not continue</div>
          <div class="startup-subtitle">{state.error}</div>
          <button class="btn primary" onClick={() => location.reload()}>
            Reload
          </button>
        </div>
      </div>
    ) : (
      props.children
    );
  }
}

function ShellOverlayLoader({ store }: { store: AppStore }) {
  const [Overlay, setOverlay] = useState<ComponentType<{ store: AppStore }> | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let current = true;
    void import('../components/ShellOverlay')
      .then((module) => {
        if (current) setOverlay(() => module.ShellOverlay);
      })
      .catch((reason: unknown) => {
        if (current)
          setError(reason instanceof Error ? reason.message : 'Could not load terminal.');
      });
    return () => {
      current = false;
    };
  }, []);
  if (error)
    return (
      <section class="shell-overlay" aria-label="Interactive shell">
        <header class="shell-overlay-header">
          <div class="shell-overlay-heading">
            <span class="shell-prompt-mark" aria-hidden="true">
              &gt;_
            </span>
            <div class="shell-title-block">
              <div class="shell-title-row">
                <h1>Shell</h1>
                <span class="shell-status shell-status-error">
                  <span class="shell-status-dot" aria-hidden="true" />
                  Could not load
                </span>
              </div>
            </div>
          </div>
          <div class="shell-overlay-actions">
            <button class="btn shell-back" type="button" onClick={() => store.shellStore.back()}>
              <Icon name="arrow-left" />
              <span>Back to chat</span>
            </button>
          </div>
        </header>
        <div class="shell-error" role="alert">
          {error}
        </div>
      </section>
    );
  return Overlay ? (
    <Overlay store={store} />
  ) : (
    <section class="shell-overlay" aria-label="Interactive shell" aria-busy="true">
      <header class="shell-overlay-header">
        <div class="shell-overlay-heading">
          <span class="shell-prompt-mark" aria-hidden="true">
            &gt;_
          </span>
          <div class="shell-title-block">
            <div class="shell-title-row">
              <h1>Shell</h1>
              <span class="shell-status">
                <span class="shell-status-dot" aria-hidden="true" />
                Loading…
              </span>
            </div>
          </div>
        </div>
        <div class="shell-overlay-actions">
          <button class="btn shell-back" type="button" onClick={() => store.shellStore.back()}>
            <Icon name="arrow-left" />
            <span>Back to chat</span>
          </button>
        </div>
      </header>
    </section>
  );
}

export function App({ store }: { store: AppStore }) {
  const session = store.activeSession.value;
  useEffect(() => {
    void store.bootstrap();
    const removeViewportSizing = installVisualViewportSizing();
    const shortcut = (event: KeyboardEvent) => {
      const mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform);
      const primary = mac ? event.metaKey && !event.ctrlKey : event.ctrlKey && !event.metaKey;
      if (
        (event.key === 'k' || event.key === 'K') &&
        primary &&
        !event.altKey &&
        !event.shiftKey &&
        store.widgets.peek().length
      ) {
        event.preventDefault();
        store.modal.value = store.modal.peek() === 'widgets' ? '' : 'widgets';
      }
    };
    addEventListener('keydown', shortcut);
    return () => {
      removeEventListener('keydown', shortcut);
      removeViewportSizing();
      store.dispose();
    };
  }, [store]);
  useEffect(() => {
    const brand = store.config.title.trim();
    const title = session?.title || '';
    document.title = brand ? (title ? `${title} · ${brand}` : brand) : title || 'Chat';
  }, [session?.title, store.config.title]);
  return (
    <ErrorBoundary>
      <StoreContext.Provider value={store}>
        <div class="toast-region" id="toastRegion" aria-label="Notifications" aria-live="polite">
          {store.toasts.value.map((toast) => (
            <div
              key={toast.id}
              class={`toast toast-${toast.kind} ${toast.leaving ? 'toast-leaving' : ''}`}
              role={toast.kind === 'error' ? 'alert' : 'status'}
            >
              <Icon class="toast-icon" name={toastIcon(toast.kind)} />
              <span class="toast-message">{toast.message}</span>
              <button
                class="toast-close close-button"
                type="button"
                aria-label="Dismiss notification"
                onClick={() => store.dismissToast(toast.id)}
              >
                <Icon name="close" />
              </button>
            </div>
          ))}
        </div>
        {!store.startupDone.value && (
          <div class="startup-splash" id="startupSplash" role="status" aria-live="polite">
            <div class="startup-card">
              <div class="startup-mark" aria-hidden="true">
                ⌘
              </div>
              <div class="startup-title">term-llm</div>
              <div class="startup-subtitle" id="startupStatus">
                {store.startup.value}
              </div>
              <div class="startup-spinner" aria-hidden="true" />
            </div>
          </div>
        )}
        <div
          class={`app ${store.sidebarCollapsed.value ? 'sidebar-collapsed' : ''} ${store.diff.value.open ? 'diff-open' : ''} ${store.diff.value.maximized ? 'diff-maximized' : ''} ${store.planVisible.value ? 'plan-open' : ''}`}
          id="appShell"
          style={{ '--diff-sidebar-user-width': `${store.diff.value.width}px` }}
          aria-hidden={!store.startupDone.value || store.shellStore.visible.value || undefined}
          inert={store.shellStore.visible.value || undefined}
        >
          <Sidebar />
          <main class="main" id="appMain">
            <Header />
            <Transcript />
            <Composer />
          </main>
          <DiffSidebar />
          <PlanSurface />
        </div>
        <Modals />
        <Lightbox />
        {store.shellStore.visible.value && <ShellOverlayLoader store={store} />}
      </StoreContext.Provider>
    </ErrorBoundary>
  );
}
