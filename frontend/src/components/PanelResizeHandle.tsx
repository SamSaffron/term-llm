import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useEventCallback } from './useEventCallback';

type PanelResizeHandleProps = {
  label: string;
  class: string;
  edge: 'left' | 'right';
  width: number;
  getWidth: () => number;
  clamp: (width: number, viewport: number) => number;
  cssProperty: `--${string}`;
  resizingClass: string;
  onCommit: (width: number) => void;
  onReset: () => void;
};

/** Shared shell-column splitter: preview in CSS, commit state only on release. */
export function PanelResizeHandle(props: PanelResizeHandleProps) {
  const cleanupRef = useRef<(() => void) | null>(null);
  const [viewport, setViewport] = useState(globalThis.innerWidth);
  const getWidth = useEventCallback(props.getWidth);
  const commit = useEventCallback(props.onCommit);
  useLayoutEffect(() => {
    const resize = () => setViewport(innerWidth);
    addEventListener('resize', resize);
    return () => {
      removeEventListener('resize', resize);
      cleanupRef.current?.();
    };
  }, []);
  const sign = props.edge === 'right' ? 1 : -1;
  const clamp = (value: number) => props.clamp(value, innerWidth);
  const startResize = (event: PointerEvent) => {
    if (event.button !== 0 || cleanupRef.current) return;
    event.preventDefault();
    const handle = event.currentTarget as HTMLElement;
    const shell = handle.closest<HTMLElement>('.app');
    const startWidth = clamp(getWidth());
    const startX = event.clientX;
    let next = startWidth;
    const move = (pointer: PointerEvent) => {
      if (pointer.pointerId !== event.pointerId) return;
      next = clamp(startWidth + sign * (pointer.clientX - startX));
      shell?.style.setProperty(props.cssProperty, `${next}px`);
    };
    const cleanup = () => {
      cleanupRef.current = null;
      removeEventListener('pointermove', move);
      removeEventListener('pointerup', finish);
      removeEventListener('pointercancel', cancel);
      handle.removeEventListener('lostpointercapture', cancel);
      handle.removeAttribute('data-resizing');
      shell?.classList.remove(props.resizingClass);
      shell?.style.setProperty(props.cssProperty, `${getWidth()}px`);
      if (handle.hasPointerCapture?.(event.pointerId))
        handle.releasePointerCapture?.(event.pointerId);
    };
    const finish = (pointer: PointerEvent) => {
      if (pointer.pointerId !== event.pointerId) return;
      try {
        commit(clamp(next));
      } finally {
        cleanup();
      }
    };
    const cancel = (pointer: PointerEvent) => {
      if (pointer.pointerId === event.pointerId) cleanup();
    };
    cleanupRef.current = cleanup;
    try {
      handle.setPointerCapture?.(event.pointerId);
    } catch {
      // An inactive pointer cannot be captured; do not leave a half-started drag.
      cleanup();
      return;
    }
    handle.setAttribute('data-resizing', '');
    shell?.classList.add(props.resizingClass);
    addEventListener('pointermove', move);
    addEventListener('pointerup', finish);
    addEventListener('pointercancel', cancel);
    handle.addEventListener('lostpointercapture', cancel);
  };
  const onKeyDown = (event: KeyboardEvent) => {
    const width = clamp(getWidth());
    const next =
      event.key === 'ArrowLeft'
        ? width - sign * 16
        : event.key === 'ArrowRight'
          ? width + sign * 16
          : event.key === 'Home'
            ? -Number.MAX_VALUE
            : event.key === 'End'
              ? Number.MAX_VALUE
              : null;
    if (next === null) return;
    event.preventDefault();
    commit(clamp(next));
  };
  return (
    <div
      class={`panel-resize-handle ${props.class}`}
      data-edge={props.edge}
      role="separator"
      aria-label={props.label}
      aria-orientation="vertical"
      aria-valuemin={props.clamp(-Number.MAX_VALUE, viewport)}
      aria-valuemax={props.clamp(Number.MAX_VALUE, viewport)}
      aria-valuenow={props.clamp(props.width, viewport)}
      tabIndex={0}
      onPointerDown={startResize}
      onKeyDown={onKeyDown}
      onDblClick={props.onReset}
    />
  );
}
