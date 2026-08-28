import { useId, useLayoutEffect, useRef } from 'preact/hooks';
import { overlayManager } from '../platform/overlay-manager';
import { OVERLAY_FOCUSABLE, trapOverlayFocus } from './Overlay';
import { useSwipeDismiss } from './useSwipeDismiss';

export function Drawer({
  open,
  title,
  onClose,
  children,
  side = 'right',
  id: providedId,
  className = '',
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  children: preact.ComponentChildren;
  side?: 'left' | 'right' | 'bottom';
  id?: string;
  className?: string;
}) {
  const generated = useId();
  const id = providedId || generated;
  const backdrop = useRef<HTMLDivElement>(null);
  const drawer = useRef<HTMLElement>(null);
  const token = useRef<symbol | null>(null);
  useSwipeDismiss(drawer, {
    enabled: open,
    axis: side === 'bottom' ? 'y' : 'x',
    direction: side === 'left' ? -1 : 1,
    property: side === 'bottom' ? '--drawer-swipe-offset-y' : '--drawer-swipe-offset-x',
    handleSelector: side === 'bottom' ? '[data-drawer-handle]' : undefined,
    onDismiss: onClose,
  });
  useLayoutEffect(() => {
    if (!open) return;
    token.current = overlayManager.acquire(undefined, backdrop.current);
    const frame = requestAnimationFrame(() =>
      (drawer.current?.querySelector<HTMLElement>(OVERLAY_FOCUSABLE) || drawer.current)?.focus(),
    );
    return () => {
      cancelAnimationFrame(frame);
      if (token.current) overlayManager.release(token.current);
      token.current = null;
    };
  }, [open]);
  if (!open) return null;
  return (
    <div
      ref={backdrop}
      class={`drawer-backdrop drawer-backdrop-${side}`}
      onPointerDown={(event) => {
        if (event.target === event.currentTarget)
          event.currentTarget.dataset.dismissPointer = String(event.pointerId);
      }}
      onPointerUp={(event) => {
        const startedOutside =
          event.currentTarget.dataset.dismissPointer === String(event.pointerId);
        delete event.currentTarget.dataset.dismissPointer;
        if (
          startedOutside &&
          event.target === event.currentTarget &&
          token.current &&
          overlayManager.isTop(token.current)
        )
          onClose();
      }}
      onPointerCancel={(event) => {
        delete event.currentTarget.dataset.dismissPointer;
      }}
    >
      <aside
        ref={drawer}
        id={id}
        class={`drawer drawer-${side} ${className}`.trim()}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        onKeyDown={(event) => {
          if (event.key === 'Escape' && token.current && overlayManager.isTop(token.current)) {
            event.preventDefault();
            event.stopPropagation();
            onClose();
          }
          trapOverlayFocus(event);
        }}
      >
        {children}
      </aside>
    </div>
  );
}

export const Sheet = Drawer;
