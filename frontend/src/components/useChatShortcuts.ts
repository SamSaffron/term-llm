import type { RefObject } from 'preact';
import { useLayoutEffect } from 'preact/hooks';
import { useStore } from '../app/context';
import { useMediaQuery } from './useMediaQuery';
import { overlayManager } from '../platform/overlay-manager';

// Read the rendered rows so pinning, search, pagination and locally folded
// groups all share the same order for hints and keyboard navigation. This hook
// owns only shortcut metadata; Preact owns the rows and their click handlers.
export function useChatShortcuts(sidebar: RefObject<HTMLElement>) {
  const store = useStore();
  const standalone = useMediaQuery('(display-mode: standalone)');
  useLayoutEffect(() => {
    const root = sidebar.current;
    if (!root || !standalone || !/Mac|iPhone|iPad|iPod/.test(navigator.platform)) return;
    const rows = () => [...root.querySelectorAll<HTMLButtonElement>('.session-btn')];
    const numberedRows = () =>
      rows()
        .filter((row) => row.hasAttribute('data-chat-shortcut-eligible'))
        .slice(0, 9);
    const update = () => {
      const eligible = numberedRows();
      for (const row of rows()) {
        const number = eligible.indexOf(row) + 1;
        if (number) {
          row.dataset.chatShortcutNumber = String(number);
          row.setAttribute('aria-keyshortcuts', `Meta+${number}`);
        } else {
          delete row.dataset.chatShortcutNumber;
          row.removeAttribute('aria-keyshortcuts');
        }
      }
    };
    const clear = () => delete root.dataset.showChatShortcuts;
    const unavailable = (event: KeyboardEvent) => {
      const target = event.composedPath()[0];
      return (
        store.authRequired.peek() ||
        overlayManager.size > 0 ||
        Boolean(root.closest('[inert], [hidden]')) ||
        (target instanceof Element && Boolean(target.closest('.shell-overlay')))
      );
    };
    const keydown = (event: KeyboardEvent) => {
      if (
        event.defaultPrevented ||
        event.isComposing ||
        event.ctrlKey ||
        event.altKey ||
        event.shiftKey ||
        unavailable(event)
      ) {
        clear();
        return;
      }
      if (!event.metaKey) {
        clear();
        return;
      }
      root.dataset.showChatShortcuts = '';
      if (!/^[1-9]$/.test(event.key)) return;
      // Resolve again here: a key can arrive before the mutation observer runs.
      const row = numberedRows()[Number(event.key) - 1];
      if (!row) return;
      event.preventDefault();
      if (!event.repeat) row.click();
    };
    const keyup = (event: KeyboardEvent) => {
      if (event.key === 'Meta' || !event.metaKey) clear();
    };
    update();
    const observer = new MutationObserver(update);
    observer.observe(root, {
      subtree: true,
      childList: true,
      attributes: true,
      attributeFilter: ['data-chat-shortcut-eligible'],
    });
    window.addEventListener('keydown', keydown);
    window.addEventListener('keyup', keyup);
    window.addEventListener('blur', clear);
    document.addEventListener('visibilitychange', clear);
    return () => {
      observer.disconnect();
      window.removeEventListener('keydown', keydown);
      window.removeEventListener('keyup', keyup);
      window.removeEventListener('blur', clear);
      document.removeEventListener('visibilitychange', clear);
      clear();
      for (const row of rows()) {
        delete row.dataset.chatShortcutNumber;
        row.removeAttribute('aria-keyshortcuts');
      }
    };
  }, [sidebar, standalone, store]);
}
