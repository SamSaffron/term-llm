import { describe, expect, it, vi } from 'vitest';
import { copyText } from './clipboard';

describe('shared clipboard', () => {
  it('uses the native API and propagates rejection without a fallback', async () => {
    const error = new Error('Permission denied');
    const writeText = vi.fn().mockRejectedValue(error);
    await expect(
      copyText('secret', document, { clipboard: { writeText } } as unknown as Navigator),
    ).rejects.toBe(error);
    expect(writeText).toHaveBeenCalledWith('secret');
    expect(document.querySelector('textarea')).toBeNull();
  });

  it.each(['success', 'false', 'throw'] as const)(
    'clears temporary text and restores focus on fallback %s',
    async (outcome) => {
      const button = document.createElement('button');
      document.body.append(button);
      button.focus();
      let captured: HTMLTextAreaElement | undefined;
      const execCommand = vi.fn(() => {
        captured = document.querySelector('textarea')!;
        expect(captured.value).toBe('secret');
        expect(captured.selectionStart).toBe(0);
        expect(captured.selectionEnd).toBe(6);
        expect(document.activeElement).toBe(captured);
        if (outcome === 'throw') throw new Error('Copy failed');
        return outcome === 'success';
      });
      const previous = Object.getOwnPropertyDescriptor(document, 'execCommand');
      Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });
      try {
        const copy = copyText('secret', document, {} as Navigator);
        if (outcome === 'success') await copy;
        else
          await expect(copy).rejects.toThrow(
            outcome === 'throw' ? 'Copy failed' : 'Clipboard unavailable',
          );
        expect(execCommand).toHaveBeenCalledWith('copy');
        expect(captured?.value).toBe('');
        expect(document.querySelector('textarea')).toBeNull();
        expect(document.activeElement).toBe(button);
      } finally {
        button.remove();
        if (previous) Object.defineProperty(document, 'execCommand', previous);
        else Reflect.deleteProperty(document, 'execCommand');
      }
    },
  );
});
