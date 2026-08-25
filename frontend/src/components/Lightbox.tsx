import { useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { copyText } from '../platform/browser';

export function Lightbox() {
  const store = useStore(); const media = store.lightbox.value; const [maximized, setMaximized] = useState(false);
  if (!media) return null;
  return <div class={`lightbox active ${maximized ? 'lightbox-maximized' : ''}`} role="dialog" aria-modal="true" aria-label="Media preview">
    <button class="lightbox-backdrop" aria-label="Close media preview" onClick={() => { store.lightbox.value = null; setMaximized(false); }} />
    <div class="lightbox-content">{media.type === 'video' ? <video src={media.src} controls autoPlay playsInline /> : <img src={media.src} alt="Full size preview" />}</div>
    <div class="lightbox-toolbar"><button class="lightbox-btn" type="button" aria-label="Copy URL" onClick={() => void copyText(media.src)}>▣</button><button class="lightbox-btn" type="button" aria-label={maximized ? 'Collapse' : 'Expand'} onClick={() => setMaximized(!maximized)}>{maximized ? '↙' : '↗'}</button><button class="lightbox-btn" type="button" aria-label="Close" onClick={() => { store.lightbox.value = null; setMaximized(false); }}>✕</button></div>
  </div>;
}
