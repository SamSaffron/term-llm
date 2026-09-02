export interface ClipboardAdapter {
  writeText(value: string): Promise<void>;
}

export function browserClipboard(
  doc: Document = document,
  nav: Navigator = navigator,
): ClipboardAdapter {
  return {
    async writeText(value: string) {
      if (nav.clipboard?.writeText) {
        await nav.clipboard.writeText(value);
        return;
      }
      const textarea = doc.createElement('textarea');
      textarea.value = value;
      textarea.readOnly = true;
      textarea.style.position = 'fixed';
      textarea.style.left = '-9999px';
      doc.body.append(textarea);
      textarea.select();
      try {
        if (!doc.execCommand('copy')) throw new Error('clipboard copy unavailable');
      } finally {
        textarea.value = '';
        textarea.remove();
      }
    },
  };
}
