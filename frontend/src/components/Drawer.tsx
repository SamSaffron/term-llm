import { useId, useLayoutEffect, useRef } from 'preact/hooks';
import { overlayManager } from '../platform/overlay-manager';

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
  const drawer = useRef<HTMLElement>(null);
  const token = useRef<symbol | null>(null);
  useLayoutEffect(() => {
    if (!open) return;
    token.current = overlayManager.acquire(undefined, drawer.current);
    const frame = requestAnimationFrame(() =>
      (
        drawer.current?.querySelector<HTMLElement>(
          'button:not([disabled]),[href],input:not([disabled])',
        ) || drawer.current
      )?.focus(),
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
      class="drawer-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
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
          if (event.key !== 'Tab') return;
          const items = [
            ...event.currentTarget.querySelectorAll<HTMLElement>(
              'button:not([disabled]),[href],input:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])',
            ),
          ];
          if (!items.length) return;
          const first = items[0];
          const last = items.at(-1)!;
          if (event.shiftKey && document.activeElement === first) {
            event.preventDefault();
            last.focus();
          } else if (!event.shiftKey && document.activeElement === last) {
            event.preventDefault();
            first.focus();
          }
        }}
      >
        {children}
      </aside>
    </div>
  );
}

export const Sheet = Drawer;
