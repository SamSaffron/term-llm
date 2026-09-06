import type { ComponentChildren, ComponentType } from 'preact';
import { useEffect, useState } from 'preact/hooks';

// Define at module scope: every mount shares the import and resolved component,
// while an unmounted instance cannot publish a late resolution into hook state.
export function lazyComponent<Props extends object>(
  load: () => Promise<ComponentType<Props>>,
  fallback: ComponentChildren = null,
) {
  let loaded: ComponentType<Props> | null = null;
  let loading: Promise<ComponentType<Props>> | null = null;
  return function LazyComponent(props: Props) {
    const [Component, setComponent] = useState(() => loaded);
    useEffect(() => {
      if (Component) return;
      loading ||= load();
      let live = true;
      void loading.then((component) => {
        loaded = component;
        if (live) setComponent(() => component);
      });
      return () => {
        live = false;
      };
    }, [Component]);
    return Component ? <Component {...props} /> : <>{fallback}</>;
  };
}
