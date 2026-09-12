import type { ComponentType } from 'preact';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import type { LightboxProps } from './Lightbox';
import { useStore } from '../app/context';
import { Overlay } from './Overlay';

const loadLightbox = () => import('./Lightbox');

// Mounted only while a preview is requested; Vite loads its JS and CSS together.
export function LightboxLoader({ load = loadLightbox }: { load?: typeof loadLightbox } = {}) {
  const store = useStore();
  const media = store.lightbox.value;
  const origin = useRef(document.activeElement as HTMLElement | null);
  const [Preview, setPreview] = useState<ComponentType<LightboxProps> | null>(null);
  const [failed, setFailed] = useState(false);
  const dismissed = useRef(false);
  const ownership = useMemo(() => ({ media, mounted: false }), [media]);
  useLayoutEffect(
    () => () => {
      // The viewer takes ownership only once it actually mounts, not when import resolves.
      if (ownership.mounted || !media) return;
      const urls = new Set<string>();
      for (const item of media.items?.length ? media.items : [media]) {
        if (item.ownsObjectURL && item.src.startsWith('blob:')) urls.add(item.src);
      }
      for (const url of urls) URL.revokeObjectURL(url);
      if (dismissed.current)
        queueMicrotask(() => {
          const active = document.activeElement;
          const fallback = media.fallbackFocus?.();
          if (fallback?.isConnected && (!active || active === document.body || !active.isConnected))
            fallback.focus({ preventScroll: true });
        });
    },
    [media, ownership],
  );
  useEffect(() => {
    let live = true;
    void load().then(
      ({ Lightbox }) => {
        if (live) setPreview(() => Lightbox);
      },
      () => {
        if (live) setFailed(true);
      },
    );
    return () => {
      live = false;
    };
  }, [load]);
  if (Preview)
    return (
      <Preview
        returnFocus={origin.current}
        onMount={() => {
          ownership.mounted = true;
        }}
      />
    );
  const original = media?.items?.[media.index || 0]?.src || media?.src;
  return (
    <Overlay
      title="Media preview"
      onClose={() => {
        dismissed.current = true;
        store.lightbox.value = null;
      }}
    >
      {failed ? (
        <div role="alert">
          <p>Could not load the image viewer. Reload the page to try again.</p>
          <button class="btn" onClick={() => window.location.reload()}>
            Reload page
          </button>
          {original && (
            <a class="btn" href={original} target="_blank" rel="noreferrer">
              Open original
            </a>
          )}
        </div>
      ) : (
        <p role="status">Loading image viewer…</p>
      )}
    </Overlay>
  );
}
