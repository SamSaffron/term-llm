export async function copyText(
  text: string,
  doc: Document = document,
  nav: Navigator = navigator,
): Promise<void> {
  if (nav.clipboard?.writeText) return nav.clipboard.writeText(text);
  const textarea = doc.createElement('textarea');
  textarea.value = text;
  textarea.readOnly = true;
  textarea.setAttribute('aria-hidden', 'true');
  textarea.style.cssText = 'position:fixed;left:-9999px;top:-9999px;width:1px;height:1px;opacity:0';
  const previous = doc.activeElement as HTMLElement | null;
  doc.body.append(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange(0, textarea.value.length);
  try {
    if (!doc.execCommand('copy')) throw new Error('Clipboard unavailable');
  } finally {
    textarea.value = '';
    textarea.remove();
    previous?.focus({ preventScroll: true });
  }
}
