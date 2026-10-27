// What a stylesheet can't do for the page's look: measure the chat's scrollbar, and
// paint the browser's own bar in the page colour.

/** How wide a box's scrollbar is, from its outer and inner widths (0 for none, or for numbers that make no sense). */
export function scrollbarWidth(offsetWidth: number, clientWidth: number): number {
  const width = Math.round(offsetWidth - clientWidth);
  return width > 0 && width <= 40 ? width : 0;
}

/**
 * Keeps --chat-gutter at the chat scroller's scrollbar width. The messages are centred
 * left of that scrollbar and the message box in the whole width, so the box gives the
 * width up on its right to sit exactly under them. Returns a function that stops.
 */
export function followChatGutter(chat: HTMLElement | null): () => void {
  if (!chat) return () => undefined;
  const root = document.documentElement;
  const measure = () =>
    root.style.setProperty(
      '--chat-gutter',
      `${scrollbarWidth(chat.offsetWidth, chat.clientWidth)}px`,
    );
  measure();
  if (typeof ResizeObserver !== 'function') return () => undefined;
  // A scrollbar that appears or goes shrinks or grows the scroller's content box.
  const observer = new ResizeObserver(measure);
  observer.observe(chat);
  return () => observer.disconnect();
}

/** Paints the browser's own bar (a phone's status bar) in the page colour, so a theme's --bg reaches it too. */
export function syncThemeColor(): void {
  const page = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim();
  if (!page) return;
  const scheme = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  for (const tag of document.querySelectorAll('meta[name="theme-color"]')) {
    const media = tag.getAttribute('media') ?? '';
    if (!media.includes('prefers-color-scheme') || media.includes(scheme))
      tag.setAttribute('content', page);
  }
}

/** Keeps the browser's bar in the page colour now and when the device switches light and dark. */
export function followThemeColor(): void {
  syncThemeColor();
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', syncThemeColor);
}
