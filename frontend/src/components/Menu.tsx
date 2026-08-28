import { useEffect, useRef } from 'preact/hooks';

const menuItems = (root: HTMLElement): HTMLElement[] => [
  ...root.querySelectorAll<HTMLElement>('[role="menuitem"]:not([disabled])'),
];

export function useMenuKeyboard(
  open: boolean,
  onClose: () => void,
  triggerRef?: preact.RefObject<HTMLElement>,
) {
  const menu = useRef<HTMLDivElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  useEffect(() => {
    if (!open || !menu.current) return;
    const root = menu.current;
    const trigger = triggerRef?.current;
    const onKeyDown = (event: KeyboardEvent) => {
      const items = menuItems(root);
      if (!items.length) return;
      const current = items.indexOf(document.activeElement as HTMLElement);
      let next = -1;
      if (event.key === 'ArrowDown') next = current < 0 ? 0 : (current + 1) % items.length;
      else if (event.key === 'ArrowUp')
        next = current < 0 ? items.length - 1 : (current - 1 + items.length) % items.length;
      else if (event.key === 'Home') next = 0;
      else if (event.key === 'End') next = items.length - 1;
      else if (event.key === 'Escape') {
        event.preventDefault();
        onCloseRef.current();
        return;
      } else if (event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
        const query = event.key.toLocaleLowerCase();
        next = items.findIndex(
          (item, index) =>
            index > current && item.textContent?.trim().toLocaleLowerCase().startsWith(query),
        );
        if (next < 0)
          next = items.findIndex((item) =>
            item.textContent?.trim().toLocaleLowerCase().startsWith(query),
          );
      }
      if (next >= 0) {
        event.preventDefault();
        items[next].focus();
      }
    };
    root.addEventListener('keydown', onKeyDown);
    const outside = (event: PointerEvent) => {
      const target = event.target as Node | null;
      if (!root.contains(target) && !trigger?.contains(target)) onCloseRef.current();
    };
    document.addEventListener('pointerdown', outside);
    const focusOutside = (event: FocusEvent) => {
      const target = event.target as Node | null;
      if (!root.contains(target) && !trigger?.contains(target)) onCloseRef.current();
    };
    document.addEventListener('focusin', focusOutside);
    const items = menuItems(root);
    items.forEach((item, index) => (item.tabIndex = index === 0 ? 0 : -1));
    const frame = requestAnimationFrame(() => items[0]?.focus());
    return () => {
      cancelAnimationFrame(frame);
      root.removeEventListener('keydown', onKeyDown);
      document.removeEventListener('pointerdown', outside);
      document.removeEventListener('focusin', focusOutside);
      const active = document.activeElement;
      if (
        trigger?.isConnected &&
        (active === document.body ||
          active === trigger ||
          (active instanceof Node && root.contains(active)))
      )
        trigger.focus({ preventScroll: true });
    };
  }, [open, triggerRef]);
  return menu;
}

export function Menu({
  open,
  label,
  onClose,
  children,
  className = '',
  triggerRef,
  id,
}: {
  open: boolean;
  label: string;
  onClose: () => void;
  children: preact.ComponentChildren;
  className?: string;
  triggerRef?: preact.RefObject<HTMLElement>;
  id?: string;
}) {
  const ref = useMenuKeyboard(open, onClose, triggerRef);
  if (!open) return null;
  return (
    <div ref={ref} id={id} class={className} role="menu" aria-label={label}>
      {children}
    </div>
  );
}
