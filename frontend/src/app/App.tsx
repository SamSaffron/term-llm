import { Component, type ComponentChildren } from 'preact';
import { useEffect } from 'preact/hooks';
import { StoreContext } from './context';
import type { AppStore } from '../stores/app-store';
import { Sidebar } from '../components/Sidebar';
import { Header } from '../components/Header';
import { Transcript } from '../components/Transcript';
import { Composer } from '../components/Composer';
import { DiffSidebar, PlanSurface } from '../components/Panels';
import { Modals } from '../components/Modals';
import { Lightbox } from '../components/Lightbox';
import { RunCenter } from '../components/RunCenter';
import { installVisualViewportSizing, registerServiceWorker } from '../platform/browser';

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

export function App({ store }: { store: AppStore }) {
  const session = store.activeSession.value;
  useEffect(() => {
    void store.bootstrap();
    void registerServiceWorker(store.config);
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
              class={`toast toast-${toast.kind}`}
              role={toast.kind === 'error' ? 'alert' : 'status'}
            >
              {toast.message}
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
          aria-hidden={!store.startupDone.value || undefined}
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
        <RunCenter />
        <Lightbox />
      </StoreContext.Provider>
    </ErrorBoundary>
  );
}
