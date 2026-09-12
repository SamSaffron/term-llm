import '../styles/features/lightbox.css';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { copyText } from '../platform/clipboard';
import { overlayManager } from '../platform/overlay-manager';
import { Icon } from './Icon';
import { trapOverlayFocus } from './Overlay';

const MIN_ZOOM = 1;
const MAX_ZOOM = 32;
const MIN_PINCH_SPAN = 16;
const INITIAL_VIEW = { zoom: 1, pan: { x: 0, y: 0 } };
const clamp = (value: number, min: number, max: number) => Math.max(min, Math.min(max, value));

function CopyMediaURL({ src, onError }: { src: string; onError: (error: unknown) => void }) {
  const [copied, setCopied] = useState(false);
  const active = useRef(true);
  const timer = useRef<number | undefined>(undefined);
  useEffect(
    () => () => {
      active.current = false;
      window.clearTimeout(timer.current);
    },
    [],
  );
  const copy = async () => {
    try {
      await copyText(src);
      if (!active.current) return;
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 2000);
    } catch (error) {
      if (active.current) onError(error);
    }
  };
  return (
    <button
      class={`lightbox-btn lightbox-copy ${copied ? 'copied' : ''}`}
      type="button"
      aria-label={copied ? 'Copied' : 'Copy URL'}
      title={copied ? 'Copied' : 'Copy URL'}
      onClick={() => void copy()}
    >
      <Icon name={copied ? 'check' : 'copy'} />
      {copied && (
        <span class="lightbox-copy-feedback" role="status">
          Copied
        </span>
      )}
    </button>
  );
}

export interface LightboxProps {
  returnFocus?: HTMLElement | null;
  onMount?: () => void;
}

export function Lightbox({ returnFocus, onMount }: LightboxProps = {}) {
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
  const [view, setView] = useState(INITIAL_VIEW);
  const { zoom, pan } = view;
  const zoomLabel = `${Math.round(zoom * 100)}%`;
  // Pointer events may arrive before Preact commits a render (especially on finger lift).
  const liveView = useRef(INITIAL_VIEW);
  const viewFrame = useRef<number | null>(null);
  const image = useRef<HTMLImageElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const [error, setError] = useState('');
  const [retryKey, setRetryKey] = useState(0);
  const dialog = useRef<HTMLDivElement>(null);
  const token = useRef<symbol | null>(null);
  const fallbackOnRelease = useRef(false);
  const video = useRef<HTMLVideoElement>(null);
  const pointers = useRef(new Map<number, { x: number; y: number }>());
  const gesture = useRef<{
    x: number;
    y: number;
    distance: number;
    centerX: number;
    centerY: number;
    width: number;
    height: number;
    zoom: number;
    pan: { x: number; y: number };
  } | null>(null);
  const revoked = useRef(new Set<string>());
  const current = items[Math.max(0, Math.min(items.length - 1, index))];

  const updateView = (next: typeof INITIAL_VIEW, deferred = false) => {
    liveView.current = next;
    if (deferred) {
      // Coalesce both fingers' updates into a single render per animation frame.
      if (viewFrame.current === null) {
        viewFrame.current = requestAnimationFrame(() => {
          viewFrame.current = null;
          setView(liveView.current);
        });
      }
    } else {
      if (viewFrame.current !== null) cancelAnimationFrame(viewFrame.current);
      viewFrame.current = null;
      setView(next);
    }
  };
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
    const activePointers = pointers.current;
    activePointers.clear();
    gesture.current = null;
    setIndex(Math.max(0, Math.min(items.length - 1, media.index || 0)));
    updateView(INITIAL_VIEW);
    setError('');
    fallbackOnRelease.current = false;
    const surface = dialog.current;
    onMount?.();
    token.current = overlayManager.acquire(returnFocus, surface);
    surface?.focus({ preventScroll: true });
    const resize = () => {
      const { zoom, pan } = liveView.current;
      updateView(
        boundedView(zoom, pan, image.current?.offsetWidth || 0, image.current?.offsetHeight || 0),
      );
      rebaseGesture();
    };
    window.addEventListener('resize', resize);
    return () => {
      window.removeEventListener('resize', resize);
      if (viewFrame.current !== null) cancelAnimationFrame(viewFrame.current);
      viewFrame.current = null;
      activePointers.clear();
      gesture.current = null;
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
    pointers.current.clear();
    gesture.current = null;
    updateView(INITIAL_VIEW);
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
  const boundedView = (nextZoom: number, nextPan: typeof pan, width: number, height: number) => {
    const limitX = (width * (nextZoom - 1)) / 2;
    const limitY = (height * (nextZoom - 1)) / 2;
    return {
      zoom: nextZoom,
      pan: {
        x: clamp(nextPan.x, -limitX, limitX),
        y: clamp(nextPan.y, -limitY, limitY),
      },
    };
  };
  const changeZoom = (next: number, focus = { x: 0, y: 0 }, deferred = false) => {
    const clamped = clamp(next, MIN_ZOOM, MAX_ZOOM);
    const ratio = clamped / liveView.current.zoom;
    updateView(
      boundedView(
        clamped,
        {
          x: liveView.current.pan.x * ratio + focus.x * (1 - ratio),
          y: liveView.current.pan.y * ratio + focus.y * (1 - ratio),
        },
        image.current?.offsetWidth || 0,
        image.current?.offsetHeight || 0,
      ),
      deferred,
    );
    rebaseGesture();
  };
  const pointerGeometry = () => {
    const [first, second = first] = [...pointers.current.values()];
    if (!first) return null;
    return {
      x: (first.x + second.x) / 2,
      y: (first.y + second.y) / 2,
      distance: Math.hypot(second.x - first.x, second.y - first.y),
    };
  };
  const rebaseGesture = () => {
    const geometry = pointerGeometry();
    const rect = content.current?.getBoundingClientRect();
    gesture.current =
      geometry && rect
        ? {
            ...geometry,
            ...liveView.current,
            // The wrapper is not transformed, so this origin is stable even before a render.
            centerX: rect.left + rect.width / 2,
            centerY: rect.top + rect.height / 2,
            width: image.current?.offsetWidth || 0,
            height: image.current?.offsetHeight || 0,
          }
        : null;
  };
  const endPointer = (id: number) => {
    // pointerup is followed by lostpointercapture; only rebase once.
    if (pointers.current.delete(id)) {
      updateView(liveView.current);
      rebaseGesture();
    }
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
      <div
        ref={content}
        class={`lightbox-content ${current.type === 'image' && !error ? 'lightbox-image-content' : ''}`}
        onDblClick={() => changeZoom(liveView.current.zoom > 1 ? 1 : 2)}
        onWheel={(event) => {
          if (current.type !== 'image' || error || !event.deltaY) return;
          event.preventDefault();
          event.stopPropagation();
          const rect = event.currentTarget.getBoundingClientRect();
          // Wheel deltas may be pixels, lines, or pages depending on the device/browser.
          const unit = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? rect.height : 1;
          changeZoom(
            liveView.current.zoom * 2 ** ((-event.deltaY * unit) / 300),
            {
              x: event.clientX - rect.left - rect.width / 2,
              y: event.clientY - rect.top - rect.height / 2,
            },
            true,
          );
        }}
        onPointerDown={(event) => {
          if (current.type !== 'image' || error) return;
          if (!image.current?.offsetWidth || !image.current.offsetHeight) return;
          if (event.pointerType !== 'touch' && event.button !== 0) return;
          if (pointers.current.size >= 2) return;
          pointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
          rebaseGesture();
          event.currentTarget.setPointerCapture?.(event.pointerId);
        }}
        onPointerMove={(event) => {
          if (!pointers.current.has(event.pointerId)) return;
          pointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
          const active = gesture.current;
          const geometry = pointerGeometry();
          if (!active || !geometry) return;
          const nextZoom = clamp(
            pointers.current.size === 2
              ? active.zoom *
                  (Math.max(MIN_PINCH_SPAN, geometry.distance) /
                    Math.max(MIN_PINCH_SPAN, active.distance))
              : active.zoom,
            MIN_ZOOM,
            MAX_ZOOM,
          );
          const ratio = nextZoom / active.zoom;
          // Keep the image point under the pinch midpoint anchored while scaling.
          updateView(
            boundedView(
              nextZoom,
              {
                x: geometry.x - active.centerX - (active.x - active.centerX - active.pan.x) * ratio,
                y: geometry.y - active.centerY - (active.y - active.centerY - active.pan.y) * ratio,
              },
              active.width,
              active.height,
            ),
            true,
          );
        }}
        onPointerUp={(event) => endPointer(event.pointerId)}
        onPointerCancel={(event) => endPointer(event.pointerId)}
        onLostPointerCapture={(event) => endPointer(event.pointerId)}
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
            ref={image}
            key={`${current.key}-${retryKey}`}
            src={current.src}
            draggable={false}
            alt={current.name || 'Full size preview'}
            style={{ transform: `translate(${pan.x}px, ${pan.y}px) scale(${zoom})` }}
            onError={() => setError('image')}
          />
        )}
      </div>
      <div class="lightbox-toolbar lightbox-view-controls">
        {items.length > 1 && (
          <div class="lightbox-gallery-controls">
            <button
              class="lightbox-btn"
              type="button"
              aria-label="Previous media"
              disabled={index <= 0}
              onClick={() => navigate(index - 1)}
            >
              ‹
            </button>
            <span class="lightbox-position" aria-live="polite">
              {index + 1} / {items.length}
            </span>
            <button
              class="lightbox-btn"
              type="button"
              aria-label="Next media"
              disabled={index >= items.length - 1}
              onClick={() => navigate(index + 1)}
            >
              ›
            </button>
          </div>
        )}
        <div class="lightbox-zoom-controls">
          <button
            class="lightbox-btn"
            type="button"
            aria-label="Zoom out"
            disabled={zoom <= 1}
            onClick={() => changeZoom(liveView.current.zoom / 2)}
          >
            −
          </button>
          <button
            class="lightbox-btn lightbox-zoom"
            type="button"
            aria-label="Reset zoom"
            title={`Current zoom: ${zoomLabel}. Reset to 100%`}
            disabled={zoom === 1 && !pan.x && !pan.y}
            onClick={resetView}
          >
            {zoomLabel}
          </button>
          <button
            class="lightbox-btn"
            type="button"
            aria-label="Zoom in"
            disabled={zoom >= MAX_ZOOM}
            onClick={() => changeZoom(liveView.current.zoom * 2)}
          >
            +
          </button>
        </div>
      </div>
      <div class="lightbox-toolbar lightbox-actions">
        <span class="lightbox-name" title={current.name}>
          {current.name || 'Media preview'}
        </span>
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
          <CopyMediaURL
            key={current.src}
            src={current.src}
            onError={(error) => store.toast(error, 'error')}
          />
        )}
        <button
          class="lightbox-btn"
          type="button"
          aria-label={maximized ? 'Collapse' : 'Expand'}
          onClick={() => {
            resetView();
            setMaximized(!maximized);
          }}
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
