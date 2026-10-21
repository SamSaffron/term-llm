import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { copyImage } from './clipboard';

class ImageClipboardItem {
  constructor(readonly data: Record<string, Promise<Blob>>) {}
}

describe('image clipboard', () => {
  let image: HTMLImageElement;
  let png: Blob;
  const writeText = vi.fn();
  const write = vi.fn(async (items: ImageClipboardItem[]) => {
    await items[0].data['image/png'];
  });
  const drawImage = vi.fn();

  beforeEach(() => {
    png = new Blob(['image bytes'], { type: 'image/png' });
    image = document.createElement('img');
    image.src = '/media/original.jpg';
    Object.defineProperties(image, {
      naturalWidth: { configurable: true, value: 2400 },
      naturalHeight: { configurable: true, value: 1600 },
      decode: { value: vi.fn(async () => undefined) },
    });
    image.style.width = '300px';
    image.style.transform = 'scale(4)';
    write.mockClear();
    writeText.mockClear();
    drawImage.mockReset();
    vi.stubGlobal('ClipboardItem', ImageClipboardItem);
    vi.stubGlobal('navigator', { clipboard: { write, writeText } });
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({
      drawImage,
    } as unknown as CanvasRenderingContext2D);
    vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation(function (
      this: HTMLCanvasElement,
      callback,
      type,
    ) {
      expect(this.width).toBe(2400);
      expect(this.height).toBe(1600);
      expect(type).toBe('image/png');
      callback(png);
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('writes only PNG image bytes at the original resolution, independent of zoom', async () => {
    await copyImage(image);
    expect(drawImage).toHaveBeenCalledWith(image, 0, 0);
    const item = write.mock.calls[0][0][0];
    expect(Object.keys(item.data)).toEqual(['image/png']);
    await expect(item.data['image/png']).resolves.toBe(png);
    expect(writeText).not.toHaveBeenCalled();
  });

  it('starts the clipboard write synchronously before decoding completes for Safari', async () => {
    let decoded!: () => void;
    vi.mocked(image.decode).mockReturnValue(new Promise<void>((resolve) => (decoded = resolve)));
    const copying = copyImage(image);
    expect(write).toHaveBeenCalledTimes(1);
    expect(drawImage).not.toHaveBeenCalled();
    expect(HTMLCanvasElement.prototype.toBlob).not.toHaveBeenCalled();
    decoded();
    await copying;
    expect(drawImage).toHaveBeenCalledOnce();
  });

  it('waits for encoding and the clipboard write before reporting success', async () => {
    let encoded!: BlobCallback;
    vi.mocked(HTMLCanvasElement.prototype.toBlob).mockImplementation((callback) => {
      encoded = callback;
    });
    const finished = vi.fn();
    const copying = copyImage(image).then(finished);
    await vi.waitFor(() => expect(encoded).toBeDefined());
    expect(finished).not.toHaveBeenCalled();
    encoded(png);
    await copying;
    expect(finished).toHaveBeenCalledOnce();
  });

  it.each(['decode', 'canvas', 'encode', 'cross-origin'])(
    'reports %s failures without writing text or a URL',
    async (failure) => {
      if (failure === 'decode')
        vi.mocked(image.decode).mockRejectedValue(new Error('Image decode failed'));
      if (failure === 'canvas')
        vi.mocked(HTMLCanvasElement.prototype.getContext).mockReturnValue(null);
      if (failure === 'encode')
        vi.mocked(HTMLCanvasElement.prototype.toBlob).mockImplementation((callback) =>
          callback(null),
        );
      if (failure === 'cross-origin')
        vi.mocked(HTMLCanvasElement.prototype.toBlob).mockImplementation(() => {
          throw new DOMException('Image is cross-origin', 'SecurityError');
        });
      await expect(copyImage(image)).rejects.toThrow();
      expect(writeText).not.toHaveBeenCalled();
    },
  );

  it('propagates clipboard permission rejection without a URL fallback', async () => {
    write.mockImplementationOnce(async (items) => {
      await items[0].data['image/png'];
      throw new Error('Clipboard denied');
    });
    await expect(copyImage(image)).rejects.toThrow('Clipboard denied');
    expect(writeText).not.toHaveBeenCalled();
  });

  it('handles encoding rejection even when permission is denied before consuming the image', async () => {
    vi.mocked(image.decode).mockRejectedValue(new Error('Image decode failed'));
    write.mockRejectedValueOnce(new Error('Clipboard denied'));
    await expect(copyImage(image)).rejects.toThrow('Clipboard denied');
    expect(writeText).not.toHaveBeenCalled();
  });

  it.each(['write', 'ClipboardItem'])(
    'reports unsupported image copying when %s is unavailable',
    async (missing) => {
      if (missing === 'write') vi.stubGlobal('navigator', { clipboard: { writeText } });
      else vi.stubGlobal('ClipboardItem', undefined);
      await expect(copyImage(image)).rejects.toThrow('Image copying is not supported');
      expect(writeText).not.toHaveBeenCalled();
      expect(image.decode).not.toHaveBeenCalled();
    },
  );

  it('rejects a missing image without attempting to write anything', async () => {
    await expect(copyImage(null)).rejects.toThrow('Image is unavailable');
    expect(write).not.toHaveBeenCalled();
    expect(writeText).not.toHaveBeenCalled();
  });
});
