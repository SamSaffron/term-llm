import { describe, expect, it, vi } from 'vitest';
import { browserClipboard } from './clipboard';

describe('Hub clipboard platform', () => {
  it('uses the Clipboard API when available', async () => {
    const writeText = vi.fn(async () => undefined);
    await browserClipboard(document, {
      clipboard: { writeText },
    } as unknown as Navigator).writeText('secret');
    expect(writeText).toHaveBeenCalledWith('secret');
  });

  it('uses the reviewed execCommand fallback and clears its temporary textarea', async () => {
    const captured: { current: HTMLTextAreaElement | null } = { current: null };
    const execCommand = vi.fn(() => {
      captured.current = document.querySelector('textarea');
      expect(captured.current?.value).toBe('fallback value');
      return true;
    });
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });
    await browserClipboard(document, {} as Navigator).writeText('fallback value');
    expect(execCommand).toHaveBeenCalledWith('copy');
    expect(captured.current?.value).toBe('');
    expect(document.querySelector('textarea')).toBeNull();
  });
});
