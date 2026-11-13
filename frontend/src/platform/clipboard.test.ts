import { afterEach, describe, expect, it, vi } from 'vitest';
import { copyPendingText, copyText } from './clipboard';

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

describe('pending text clipboard', () => {
  afterEach(() => vi.unstubAllGlobals());

  function nativeClipboard() {
    let data!: Promise<Blob>;
    const item = vi.fn(function (this: object, values: Record<string, Promise<Blob>>) {
      data = values['text/plain'];
    });
    vi.stubGlobal('ClipboardItem', item);
    const write = vi.fn(() => data.then(() => undefined));
    const writeText = vi.fn();
    const nav = { clipboard: { write, writeText } } as unknown as Navigator;
    return { write, writeText, nav, item, blob: () => data };
  }

  it('admits write synchronously while text is still pending', async () => {
    const native = nativeClipboard();
    let resolve!: (text: string) => void;
    const copy = copyPendingText(
      new Promise((done) => {
        resolve = done;
      }),
      document,
      native.nav,
    );
    expect(native.write).toHaveBeenCalledOnce();
    expect(native.item).toHaveBeenCalledOnce();
    expect(native.writeText).not.toHaveBeenCalled();
    resolve('complete diff');
    await copy;
    expect((await native.blob()).type).toBe('text/plain');
    expect((await native.blob()).size).toBe('complete diff'.length);
  });

  it.each(['empty', 'reject'] as const)(
    'rejects %s data, never substituting clipboard content',
    async (kind) => {
      const native = nativeClipboard();
      const text =
        kind === 'empty' ? Promise.resolve('') : Promise.reject(new Error('fetch failed'));
      await expect(copyPendingText(text, document, native.nav)).rejects.toThrow(
        kind === 'empty' ? 'No text is available' : 'fetch failed',
      );
      expect(native.writeText).not.toHaveBeenCalled();
      await expect(native.blob()).rejects.toThrow();
    },
  );

  it.each(['empty', 'fetch'] as const)(
    'preserves %s failure when WebKit maps rejected item data to NotAllowedError',
    async (kind) => {
      const native = nativeClipboard();
      native.write.mockImplementation(() =>
        native.blob().then(
          () => undefined,
          () => {
            throw new DOMException('The request is not allowed', 'NotAllowedError');
          },
        ),
      );
      const failure = new Error('Diff request failed: 500');
      const text = kind === 'empty' ? Promise.resolve('') : Promise.reject(failure);
      const copy = copyPendingText(text, document, native.nav);
      if (kind === 'empty') await expect(copy).rejects.toThrow('No text is available');
      else await expect(copy).rejects.toBe(failure);
      expect(native.writeText).not.toHaveBeenCalled();
    },
  );

  it('returns immediate permission denial without waiting for pending data or its late rejection', async () => {
    const native = nativeClipboard();
    const denied = new DOMException('Permission denied', 'NotAllowedError');
    native.write.mockRejectedValue(denied);
    let reject!: (error: Error) => void;
    const text = new Promise<string>((_resolve, fail) => {
      reject = fail;
    });
    await expect(copyPendingText(text, document, native.nav)).rejects.toBe(denied);
    reject(new Error('Later data failure'));
    await Promise.resolve();
    await Promise.resolve();
    expect(native.writeText).not.toHaveBeenCalled();
  });

  it.each(['constructor', 'write'] as const)(
    'observes abandoned data on %s failure without falling back',
    async (kind) => {
      const native = nativeClipboard();
      if (kind === 'constructor')
        native.item.mockImplementation(function () {
          throw new Error('denied');
        });
      else native.write.mockRejectedValue(new Error('denied'));
      let reject!: (error: Error) => void;
      const text = new Promise<string>((_resolve, fail) => {
        reject = fail;
      });
      await expect(copyPendingText(text, document, native.nav)).rejects.toThrow('denied');
      reject(new Error('late fetch failure'));
      await Promise.resolve();
      await Promise.resolve();
      expect(native.writeText).not.toHaveBeenCalled();
    },
  );

  it('uses the legacy text fallback only after valid pending text arrives', async () => {
    const execCommand = vi.fn(() => {
      expect(document.querySelector('textarea')?.value).toBe('complete diff');
      return true;
    });
    const previous = Object.getOwnPropertyDescriptor(document, 'execCommand');
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });
    try {
      let resolve!: (text: string) => void;
      const copy = copyPendingText(
        new Promise((done) => {
          resolve = done;
        }),
        document,
        {} as Navigator,
      );
      expect(execCommand).not.toHaveBeenCalled();
      resolve('complete diff');
      await copy;
      expect(execCommand).toHaveBeenCalledWith('copy');
      expect(document.querySelector('textarea')).toBeNull();
      await expect(copyPendingText(Promise.resolve(''), document, {} as Navigator)).rejects.toThrow(
        'No text',
      );
      expect(execCommand).toHaveBeenCalledOnce();
    } finally {
      if (previous) Object.defineProperty(document, 'execCommand', previous);
      else Reflect.deleteProperty(document, 'execCommand');
    }
  });

  it.each(['no-item', 'no-write'] as const)(
    'uses existing native text semantics for %s',
    async (kind) => {
      const native = nativeClipboard();
      if (kind === 'no-item') vi.stubGlobal('ClipboardItem', undefined);
      else Reflect.deleteProperty(native.nav.clipboard, 'write');
      const error = new Error('permission denied');
      native.writeText.mockRejectedValue(error);
      await expect(copyPendingText(Promise.resolve('diff'), document, native.nav)).rejects.toBe(
        error,
      );
      expect(native.writeText).toHaveBeenCalledWith('diff');
      expect(document.querySelector('textarea')).toBeNull();
      native.writeText.mockClear();
      await expect(copyPendingText(Promise.resolve(''), document, native.nav)).rejects.toThrow(
        'No text',
      );
      expect(native.writeText).not.toHaveBeenCalled();
    },
  );
});
