import { Component, type ComponentChildren } from 'preact';

export class HubErrorBoundary extends Component<
  { children: ComponentChildren },
  { error: string }
> {
  state = { error: '' };

  static getDerivedStateFromError(error: unknown) {
    return { error: error instanceof Error ? error.message : 'The Hub UI could not continue.' };
  }

  render(props: { children: ComponentChildren }, state: { error: string }) {
    if (!state.error) return props.children;
    return (
      <main class="hub-fatal" role="alert">
        <h1>Hub could not start</h1>
        <p>{state.error}</p>
        <button type="button" onClick={() => window.location.reload()}>
          Reload
        </button>
      </main>
    );
  }
}
