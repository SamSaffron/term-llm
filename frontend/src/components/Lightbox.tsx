import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { copyText } from '../platform/browser';
import { overlayManager } from '../platform/overlay-manager';
import { Icon } from './Icon';
import { trapOverlayFocus } from './Overlay';

export function Lightbox() {
  const store = useStore();
  const media = store.lightbox.value;
  const [maximized, setMaximized] = useState(false);
  const dialog = useRef<HTMLDivElement>(null);
  const token = useRef<symbol | null>(null);
  const video = useRef<HTMLVideoElement>(null);
  useLayoutEffect(() => {
    if (!media) return;
    token.current = overlayManager.acquire(undefined, dialog.current);
    const mediaElement = video.current;
    const frame = requestAnimationFrame(() => dialog.current?.focus());
    return () => {
      cancelAnimationFrame(frame);
      mediaElement?.pause();
      mediaElement?.removeAttribute('src');
      mediaElement?.load();
      if (media.ownsObjectURL && media.src.startsWith('blob:')) URL.revokeObjectURL(media.src);
      if (token.current) overlayManager.release(token.current);
      token.current = null;
    };
  }, [media]);
  if (!media) return null;
  const close = () => {
    video.current?.pause();
    store.lightbox.value = null;
    setMaximized(false);
  };
  return (
    <div
      ref={dialog}
      class={`lightbox active ${maximized ? 'lightbox-maximized' : ''}`}
      role="dialog"
      aria-modal="true"
      aria-label="Media preview"
      tabIndex={-1}
      onKeyDown={(event) => {
        if (event.key === 'Escape' && token.current && overlayManager.isTop(token.current)) {
          event.preventDefault();
          event.stopPropagation();
          close();
          return;
        }
        trapOverlayFocus(
          event,
          'button:not([disabled]),video[controls],[tabindex]:not([tabindex="-1"])',
        );
      }}
    >
      <button
        class="lightbox-backdrop"
        aria-label="Close media preview"
        onClick={() => {
          if (token.current && overlayManager.isTop(token.current)) close();
        }}
      />
      <div class="lightbox-content">
        {media.type === 'video' ? (
          <video
            ref={video}
            src={media.src}
            aria-label="Video preview"
            controls
            autoPlay
            playsInline
          />
        ) : (
          <img src={media.src} alt="Full size preview" />
        )}
      </div>
      <div class="lightbox-toolbar">
        <button
          class="lightbox-btn"
          type="button"
          aria-label="Copy URL"
          onClick={() => void copyText(media.src)}
        >
          <Icon name="copy" />
        </button>
        <button
          class="lightbox-btn"
          type="button"
          aria-label={maximized ? 'Collapse' : 'Expand'}
          onClick={() => setMaximized(!maximized)}
        >
          <Icon name={maximized ? 'restore' : 'expand'} />
        </button>
        <button class="lightbox-btn" type="button" aria-label="Close" onClick={close}>
          <Icon name="close" />
        </button>
      </div>
    </div>
  );
}
