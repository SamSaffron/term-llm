import { useEffect } from 'preact/hooks';
import { overlayManager } from '../platform/overlay-manager';
import type { ThinkingStore } from '../stores/thinking-store';

export function handleThinkingShortcut(event: KeyboardEvent, thinking: ThinkingStore): boolean {
  if (
    event.defaultPrevented ||
    event.isComposing ||
    !event.ctrlKey ||
    event.altKey ||
    event.shiftKey ||
    event.metaKey ||
    event.key.toLowerCase() !== 'e' ||
    overlayManager.size
  )
    return false;
  const target = event.target instanceof Element ? event.target : document.activeElement;
  // The shell overlay owns its keys, as for the sidebar chat shortcuts.
  if (target?.closest('.shell-overlay')) return false;
  const editable = target?.closest(
    'input, textarea, select, [contenteditable]:not([contenteditable="false"])',
  );
  if (
    editable &&
    !(
      editable instanceof HTMLTextAreaElement &&
      editable.id === 'promptInput' &&
      editable.value === ''
    )
  )
    return false;
  event.preventDefault();
  thinking.toggleDetails();
  return true;
}
export function useThinkingShortcut(thinking: ThinkingStore): void {
  useEffect(() => {
    const listener = (event: KeyboardEvent) => {
      handleThinkingShortcut(event, thinking);
    };
    document.addEventListener('keydown', listener);
    return () => document.removeEventListener('keydown', listener);
  }, [thinking]);
}
