import { useStore } from '../app/context';
import { clampSidebarWidth, DEFAULT_SIDEBAR_WIDTH } from '../domain/sidebar';

import { PanelResizeHandle } from './PanelResizeHandle';

/** The draggable right edge of the sessions sidebar, like the changes panel's left edge. */
export function SidebarResizeHandle() {
  const store = useStore();
  const commit = (next: number) => {
    const clamped = clampSidebarWidth(next, innerWidth);
    store.sidebarWidth.value = clamped;
    store.storage.setItem(store.keys.sidebarWidth, String(clamped));
  };
  const reset = () => {
    store.sidebarWidth.value = DEFAULT_SIDEBAR_WIDTH;
    store.storage.removeItem(store.keys.sidebarWidth);
  };
  return (
    <PanelResizeHandle
      class="sidebar-resize-handle"
      label="Resize sidebar"
      edge="right"
      width={store.sidebarWidth.value}
      getWidth={() => store.sidebarWidth.peek()}
      clamp={clampSidebarWidth}
      cssProperty="--sidebar-user-width"
      resizingClass="sidebar-resizing"
      onCommit={commit}
      onReset={reset}
    />
  );
}
