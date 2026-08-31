import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { copyText } from '../platform/browser';
import { overlayManager } from '../platform/overlay-manager';
import { Icon } from './Icon';
import { trapOverlayFocus } from './Overlay';

const MIN_ZOOM = 1;
const MAX_ZOOM = 4;

export function Lightbox() {
  const store = useStore();
  const media = store.lightbox.value;
  const allItems = media?.items?.length
    ? media.items
    : media
      ? [
          {
            key: media.src,
            src: media.src,
            type: media.type,
            name: media.name,
            ownsObjectURL: media.ownsObjectURL,
          },
        ]
      : [];
  const [removed, setRemoved] = useState<{ media: typeof media; keys: Set<string> }>({
    media: null,
    keys: new Set(),
  });
  const items =
    removed.media === media ? allItems.filter((item) => !removed.keys.has(item.key)) : allItems;
  const [index, setIndex] = useState(media?.index || 0);
  const [maximized, setMaximized] = useState(false);
  const [zoom, setZoom] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const [error, setError] = useState('');
  const [retryKey, setRetryKey] = useState(0);
  const dialog = useRef<HTMLDivElement>(null);
  const token = useRef<symbol | null>(null);
  const fallbackOnRelease = useRef(false);
  const video = useRef<HTMLVideoElement>(null);
  const drag = useRef<{ id: number; x: number; y: number; panX: number; panY: number } | null>(
    null,
  );
  const revoked = useRef(new Set<string>());
  const current = items[Math.max(0, Math.min(items.length - 1, index))];

  const releaseVideo = () => {
    video.current?.pause();
    video.current?.removeAttribute('src');
    video.current?.load();
  };
  const releaseURLs = () => {
    for (const item of items) {
      if (!item.ownsObjectURL || !item.src.startsWith('blob:') || revoked.current.has(item.src))
        continue;
      revoked.current.add(item.src);
      URL.revokeObjectURL(item.src);
    }
  };
  useLayoutEffect(() => {
    if (!media) return;
    setIndex(Math.max(0, Math.min(items.length - 1, media.index || 0)));
    setZoom(1);
    setPan({ x: 0, y: 0 });
    setError('');
    fallbackOnRelease.current = false;
    const surface = dialog.current;
    token.current = overlayManager.acquire(undefined, surface);
    const frame = requestAnimationFrame(() => surface?.focus());
    return () => {
      cancelAnimationFrame(frame);
      releaseVideo();
      releaseURLs();
      if (token.current) overlayManager.release(token.current);
      token.current = null;
      if (fallbackOnRelease.current) {
        const fallback = media.fallbackFocus?.();
        const active = document.activeElement;
        if (
          fallback?.isConnected &&
          (!active ||
            active === document.body ||
            !active.isConnected ||
            Boolean(surface?.contains(active)))
        )
          fallback.focus({ preventScroll: true });
      }
      fallbackOnRelease.current = false;
    };
    // The gallery snapshot is stable for the lifetime of one open modal.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [media]);
  if (!media || !current) return null;

  const resetView = () => {
    setZoom(1);
    setPan({ x: 0, y: 0 });
    setError('');
  };
  const navigate = (next: number) => {
    if (next < 0 || next >= items.length || next === index) return;
    releaseVideo();
    setIndex(next);
    resetView();
    setRetryKey((value) => value + 1);
  };
  const close = () => {
    releaseVideo();
    fallbackOnRelease.current = true;
    store.lightbox.value = null;
    setRemoved({ media: null, keys: new Set() });
    setMaximized(false);
    resetView();
  };
  const removeCurrent = () => {
    if (!media.onRemove) return;
    const keys = new Set(removed.media === media ? removed.keys : []);
    keys.add(current.key);
    const remaining = allItems.length - keys.size;
    media.onRemove(current);
    if (remaining <= 0) {
      close();
      return;
    }
    setRemoved({ media, keys });
    if (index >= remaining) setIndex(remaining - 1);
    resetView();
    setRetryKey((value) => value + 1);
  };
  const changeZoom = (next: number) => {
    const clamped = Math.max(MIN_ZOOM, Math.min(MAX_ZOOM, next));
    setZoom(clamped);
    if (clamped === 1) setPan({ x: 0, y: 0 });
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
        if (event.key === 'ArrowLeft') {
          event.preventDefault();
          navigate(index - 1);
          return;
        }
        if (event.key === 'ArrowRight') {
          event.preventDefault();
          navigate(index + 1);
          return;
        }
        trapOverlayFocus(event);
      }}
    >
      <button
        class="lightbox-backdrop"
        aria-label="Close media preview"
        onClick={() => {
          if (token.current && overlayManager.isTop(token.current)) close();
        }}
      />
      <button
        class="lightbox-nav lightbox-prev"
        type="button"
        aria-label="Previous media"
        disabled={index <= 0}
        onClick={() => navigate(index - 1)}
      >
        ‹
      </button>
      <div
        class="lightbox-content"
        onDblClick={() => changeZoom(zoom > 1 ? 1 : 2)}
        onPointerDown={(event) => {
          if (zoom <= 1 || !event.isPrimary) return;
          drag.current = {
            id: event.pointerId,
            x: event.clientX,
            y: event.clientY,
            panX: pan.x,
            panY: pan.y,
          };
          event.currentTarget.setPointerCapture?.(event.pointerId);
        }}
        onPointerMove={(event) => {
          const active = drag.current;
          if (!active || active.id !== event.pointerId) return;
          const limit = 300 * (zoom - 1);
          setPan({
            x: Math.max(-limit, Math.min(limit, active.panX + event.clientX - active.x)),
            y: Math.max(-limit, Math.min(limit, active.panY + event.clientY - active.y)),
          });
        }}
        onPointerUp={() => {
          drag.current = null;
        }}
        onPointerCancel={() => {
          drag.current = null;
        }}
      >
        {error ? (
          <div class="lightbox-error" role="alert">
            <strong>Media could not be loaded.</strong>
            <button
              class="btn"
              type="button"
              onClick={() => {
                setError('');
                setRetryKey((value) => value + 1);
              }}
            >
              Retry
            </button>
            <a class="btn" href={current.src} target="_blank" rel="noreferrer">
              Open original
            </a>
          </div>
        ) : current.type === 'video' ? (
          <video
            key={`${current.key}-${retryKey}`}
            ref={video}
            src={current.src}
            aria-label={current.name || 'Video preview'}
            controls
            autoPlay
            playsInline
            onError={() => setError('video')}
          />
        ) : (
          <img
            key={`${current.key}-${retryKey}`}
            src={current.src}
            alt={current.name || 'Full size preview'}
            style={{ transform: `translate(${pan.x}px, ${pan.y}px) scale(${zoom})` }}
            onError={() => setError('image')}
          />
        )}
      </div>
      <button
        class="lightbox-nav lightbox-next"
        type="button"
        aria-label="Next media"
        disabled={index >= items.length - 1}
        onClick={() => navigate(index + 1)}
      >
        ›
      </button>
      <div class="lightbox-toolbar">
        <span class="lightbox-position" aria-live="polite">
          {index + 1} / {items.length}
        </span>
        <button
          class="lightbox-btn"
          type="button"
          aria-label="Zoom out"
          disabled={zoom <= 1}
          onClick={() => changeZoom(zoom - 0.5)}
        >
          −
        </button>
        <button
          class="lightbox-btn"
          type="button"
          aria-label="Reset zoom"
          disabled={zoom === 1 && !pan.x && !pan.y}
          onClick={resetView}
        >
          1:1
        </button>
        <button
          class="lightbox-btn"
          type="button"
          aria-label="Zoom in"
          disabled={zoom >= MAX_ZOOM}
          onClick={() => changeZoom(zoom + 0.5)}
        >
          +
        </button>
        {media.onRemove && (
          <button
            class="lightbox-btn lightbox-remove"
            type="button"
            aria-label={`Remove ${current.name || 'attachment'}`}
            onClick={removeCurrent}
          >
            <Icon name="trash" />
          </button>
        )}
        <a
          class="lightbox-btn"
          aria-label="Download"
          href={current.src}
          download={current.name || ''}
        >
          ↓
        </a>
        {!current.src.startsWith('blob:') && (
          <button
            class="lightbox-btn"
            type="button"
            aria-label="Copy URL"
            onClick={() => void copyText(current.src)}
          >
            <Icon name="copy" />
          </button>
        )}
        <button
          class="lightbox-btn"
          type="button"
          aria-label={maximized ? 'Collapse' : 'Expand'}
          onClick={() => setMaximized(!maximized)}
        >
          <Icon name={maximized ? 'restore' : 'expand'} />
        </button>
        <button class="lightbox-btn close-button" type="button" aria-label="Close" onClick={close}>
          <Icon name="close" />
        </button>
      </div>
    </div>
  );
}
