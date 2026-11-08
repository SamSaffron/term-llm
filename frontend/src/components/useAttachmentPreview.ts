import { useLayoutEffect, useState } from 'preact/hooks';

/** Keep the local original mounted until the durable image is decoded. */
export function useAttachmentPreview(file: Blob | undefined, source: string): string {
  const [original, setOriginal] = useState(file);
  const [url, setURL] = useState('');
  useLayoutEffect(() => {
    const preview = original ? URL.createObjectURL(original) : '';
    setURL(preview);
    return () => {
      if (preview) URL.revokeObjectURL(preview);
    };
  }, [original]);
  useLayoutEffect(() => {
    if (file) return setOriginal(file);
    if (!original || !source) return;
    let active = true;
    const image = new Image();
    image.src = source;
    void image.decode().then(
      () => {
        if (active) setOriginal(undefined);
      },
      () => {}, // A failed durable image must not discard a working preview.
    );
    return () => {
      active = false;
      image.src = '';
    };
  }, [file, original, source]);
  return url || source;
}
