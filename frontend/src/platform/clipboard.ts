async function imagePNG(image: HTMLImageElement): Promise<Blob> {
  // Decode the original, not a screenshot of the zoomed/scaled lightbox.
  await image.decode();
  if (!image.naturalWidth || !image.naturalHeight) throw new Error('Image is unavailable');
  const canvas = document.createElement('canvas');
  canvas.width = image.naturalWidth;
  canvas.height = image.naturalHeight;
  try {
    const context = canvas.getContext('2d');
    if (!context) throw new Error('Image copying unavailable');
    context.drawImage(image, 0, 0);
    return await new Promise<Blob>((resolve, reject) => {
      canvas.toBlob((blob) => {
        if (blob) resolve(blob);
        else reject(new Error('Image encoding failed'));
      }, 'image/png');
    });
  } finally {
    canvas.width = 0;
    canvas.height = 0;
  }
}

export async function copyImage(image: HTMLImageElement | null): Promise<void> {
  if (!navigator.clipboard?.write || typeof ClipboardItem === 'undefined')
    throw new DOMException(
      'Image copying is not supported. Use Download instead.',
      'NotSupportedError',
    );
  if (!image) throw new Error('Image is unavailable');
  // Safari requires write() during the tap's user activation. Give ClipboardItem
  // the pending PNG promise rather than awaiting decoding/encoding before write().
  // Never substitute the URL or text if image copying fails.
  const png = imagePNG(image);
  // A denied write (or a throwing ClipboardItem constructor) may never consume
  // the image promise. Still observe encoding failures to avoid an unhandled rejection.
  void png.catch(() => {});
  return navigator.clipboard.write([new ClipboardItem({ 'image/png': png })]);
}

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
