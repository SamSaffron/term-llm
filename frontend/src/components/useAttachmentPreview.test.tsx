import { act, render } from '@testing-library/preact';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { useAttachmentPreview } from './useAttachmentPreview';

function Preview({ file, source }: { file?: File; source: string }) {
  return <img src={useAttachmentPreview(file, source)} alt="Preview" />;
}

let pending: Array<{ image: HTMLImageElement; resolve: () => void; reject: () => void }>;
beforeEach(() => {
  pending = [];
  vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:local');
  vi.spyOn(URL, 'revokeObjectURL');
  vi.stubGlobal(
    'Image',
    class {
      src = '';
      decode() {
        return new Promise<void>((resolve, reject) => {
          pending.push({
            image: this as unknown as HTMLImageElement,
            resolve,
            reject: () => reject(new Error('failed')),
          });
        });
      }
    },
  );
});
afterEach(() => vi.unstubAllGlobals());

it('keeps the local preview if the durable image cannot decode', async () => {
  const file = new File(['image'], 'shot.png', { type: 'image/png' });
  const { container, rerender, unmount } = render(<Preview file={file} source="" />);
  rerender(<Preview source="/images/durable.png" />);
  expect(pending[0].image.src).toBe('/images/durable.png');
  await act(async () => {
    pending[0].reject();
  });
  expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:local');
  expect(URL.revokeObjectURL).not.toHaveBeenCalled();
  unmount();
  expect(URL.revokeObjectURL).toHaveBeenCalledExactlyOnceWith('blob:local');
});

it('ignores a superseded decode and releases the preview only for the current source', async () => {
  const file = new File(['image'], 'shot.png', { type: 'image/png' });
  const { container, rerender } = render(<Preview file={file} source="" />);
  rerender(<Preview source="/images/old.png" />);
  rerender(<Preview source="/images/new.png" />);
  expect(pending[0].image.src).toBe('');
  await act(async () => {
    pending[0].resolve();
  });
  expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:local');
  expect(URL.revokeObjectURL).not.toHaveBeenCalled();
  await act(async () => {
    pending[1].resolve();
  });
  expect(container.querySelector('img')?.getAttribute('src')).toBe('/images/new.png');
  expect(URL.revokeObjectURL).toHaveBeenCalledExactlyOnceWith('blob:local');
});

it('cancels the pending handoff and releases the local URL on unmount', async () => {
  const file = new File(['image'], 'shot.png', { type: 'image/png' });
  const { rerender, unmount } = render(<Preview file={file} source="" />);
  rerender(<Preview source="/images/durable.png" />);
  unmount();
  expect(pending[0].image.src).toBe('');
  await act(async () => {
    pending[0].resolve();
  });
  expect(URL.revokeObjectURL).toHaveBeenCalledExactlyOnceWith('blob:local');
});

it('does not preload or allocate a URL for an ordinary historical image', () => {
  const { container } = render(<Preview source="/images/history.png" />);
  expect(container.querySelector('img')?.getAttribute('src')).toBe('/images/history.png');
  expect(pending).toHaveLength(0);
  expect(URL.createObjectURL).not.toHaveBeenCalled();
});
