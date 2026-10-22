import { useEffect, useRef } from 'preact/hooks';
import { useStore } from '../app/context';
import {
  clampSidebarWidth,
  DEFAULT_SIDEBAR_WIDTH,
  MAX_SIDEBAR_WIDTH,
  MIN_SIDEBAR_WIDTH,
} from '../domain/sidebar';

const KEY_STEP = 16;

/** The draggable right edge of the sessions sidebar, like the changes panel's left edge. */
export function SidebarResizeHandle() {
  const store = useStore();
  const resizeCleanup = useRef<(() => void) | null>(null);
  useEffect(() => () => resizeCleanup.current?.(), []);
  const width = store.sidebarWidth.value;

  const commit = (next: number) => {
    const clamped = clampSidebarWidth(next, innerWidth);
    store.sidebarWidth.value = clamped;
    store.storage.setItem(store.keys.sidebarWidth, String(clamped));
  };
  const reset = () => {
    store.sidebarWidth.value = DEFAULT_SIDEBAR_WIDTH;
    store.storage.removeItem(store.keys.sidebarWidth);
  };
  const startResize = (event: PointerEvent) => {
    if (event.button !== 0) return;
    event.preventDefault();
    const handle = event.currentTarget as HTMLElement;
    resizeCleanup.current?.();
    const shell = handle.closest<HTMLElement>('.app');
    const startX = event.clientX;
    const startWidth = width;
    let next = startWidth;
    const move = (pointer: PointerEvent) => {
      if (pointer.pointerId !== event.pointerId) return;
      next = clampSidebarWidth(startWidth + pointer.clientX - startX, innerWidth);
      // Presentation only while dragging: state and storage are written once, on release.
      shell?.style.setProperty('--sidebar-user-width', `${next}px`);
    };
    const cleanup = () => {
      removeEventListener('pointermove', move);
      removeEventListener('pointerup', finish);
      removeEventListener('pointercancel', finish);
      shell?.classList.remove('sidebar-resizing');
      shell?.style.setProperty('--sidebar-user-width', `${store.sidebarWidth.peek()}px`);
      if (handle.hasPointerCapture?.(event.pointerId))
        handle.releasePointerCapture?.(event.pointerId);
      resizeCleanup.current = null;
    };
    const finish = (pointer: PointerEvent) => {
      if (pointer.pointerId !== event.pointerId) return;
      if (pointer.type !== 'pointercancel') commit(next);
      cleanup();
    };
    resizeCleanup.current = cleanup;
    handle.setPointerCapture?.(event.pointerId);
    shell?.classList.add('sidebar-resizing');
    addEventListener('pointermove', move);
    addEventListener('pointerup', finish);
    addEventListener('pointercancel', finish);
  };
  const onKeyDown = (event: KeyboardEvent) => {
    const target =
      event.key === 'ArrowLeft'
        ? width - KEY_STEP
        : event.key === 'ArrowRight'
          ? width + KEY_STEP
          : event.key === 'Home'
            ? MIN_SIDEBAR_WIDTH
            : event.key === 'End'
              ? MAX_SIDEBAR_WIDTH
              : null;
    if (target === null) return;
    event.preventDefault();
    commit(target);
  };

  return (
    <div
      class="sidebar-resize-handle"
      role="separator"
      aria-label="Resize sidebar"
      aria-orientation="vertical"
      aria-valuemin={MIN_SIDEBAR_WIDTH}
      aria-valuemax={clampSidebarWidth(MAX_SIDEBAR_WIDTH, innerWidth)}
      aria-valuenow={clampSidebarWidth(width, innerWidth)}
      tabIndex={0}
      onPointerDown={startResize}
      onKeyDown={onKeyDown}
      onDblClick={reset}
    />
  );
}
