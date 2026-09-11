import { copyText } from '../../platform/clipboard';

export interface ClipboardAdapter {
  writeText(value: string): Promise<void>;
}

export function browserClipboard(
  doc: Document = document,
  nav: Navigator = navigator,
): ClipboardAdapter {
  return {
    writeText: (value) => copyText(value, doc, nav),
  };
}
