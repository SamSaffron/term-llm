import { useLayoutEffect, useRef, useState } from 'preact/hooks';

/** Keep a dropdown within its nearest scrollport and flip it when needed. */
export function useMenuFlip(open: boolean) {
  const menu = useRef<HTMLDivElement>(null);
  const [up, setUp] = useState(false);
  useLayoutEffect(() => {
    if (!open) {
      setUp(false);
      return;
    }
    const panel = menu.current;
    if (!panel) return;
    const scrollport = panel.closest<HTMLElement>('.sidebar-content');
    const update = () => {
      const rect = panel.getBoundingClientRect();
      const menuHeight = Math.max(rect.height, panel.scrollHeight);
      const triggerRect = panel.parentElement?.getBoundingClientRect() || rect;
      const bounds = scrollport?.getBoundingClientRect() || {
        top: 8,
        bottom: window.innerHeight - 8,
      };
      const below = Math.max(0, bounds.bottom - triggerRect.bottom - 8);
      const above = Math.max(0, triggerRect.top - bounds.top - 8);
      const nextUp = menuHeight > below && above > below;
      setUp(nextUp);
      panel.style.maxHeight = `${Math.max(72, nextUp ? above : below)}px`;
    };
    update();
    scrollport?.addEventListener('scroll', update, { passive: true });
    window.addEventListener('resize', update);
    const observer = typeof ResizeObserver === 'function' ? new ResizeObserver(update) : null;
    observer?.observe(panel);
    return () => {
      scrollport?.removeEventListener('scroll', update);
      window.removeEventListener('resize', update);
      observer?.disconnect();
    };
  }, [open]);
  return { menu, up };
}
