import { Component, type ComponentChildren } from 'preact';

interface Props {
  children: ComponentChildren;
  /** Reconciled response identity: retry only when fresh data arrives. */
  resetKey: unknown;
  label: string;
}

/** Keep an individual node or response row from taking down the dashboard. */
export class HubRenderBoundary extends Component<Props, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  componentDidCatch(error: unknown) {
    console.error(`[hub] ${this.props.label} render failed`, error);
  }

  componentDidUpdate(previous: Props) {
    if (this.state.failed && previous.resetKey !== this.props.resetKey) {
      this.setState({ failed: false });
    }
  }

  render(props: Props, state: { failed: boolean }) {
    if (!state.failed) return props.children;
    return (
      <div class="hub-error" role="alert">
        {props.label} could not be displayed. Other Hub data is still available.
      </div>
    );
  }
}
