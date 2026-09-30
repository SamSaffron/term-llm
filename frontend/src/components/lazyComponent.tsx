import type { ComponentChildren, ComponentType } from 'preact';
import { useEffect, useState } from 'preact/hooks';

// Define at module scope: every mount shares the import and resolved component,
// while an unmounted instance cannot publish a late resolution into hook state.
// preload() starts the same shared import early, so a caller that awaits it
// (tests, or intent-based prefetching) gets a synchronous first render. Like a
// mount, it caches the outcome for the page's lifetime, so a loader should turn
// its own import failure into a fallback component rather than reject.
export function lazyComponent<Props extends object>(
  load: () => Promise<ComponentType<Props>>,
  fallback: ComponentChildren = null,
) {
  let loaded: ComponentType<Props> | null = null;
  let loading: Promise<ComponentType<Props>> | null = null;
  const preload = () =>
    (loading ||= load().then((component) => {
      loaded = component;
      return component;
    }));
  function LazyComponent(props: Props) {
    const [Component, setComponent] = useState(() => loaded);
    useEffect(() => {
      if (Component) return;
      let live = true;
      void preload().then((component) => {
        if (live) setComponent(() => component);
      });
      return () => {
        live = false;
      };
    }, [Component]);
    return Component ? <Component {...props} /> : <>{fallback}</>;
  }
  LazyComponent.preload = preload;
  return LazyComponent;
}
