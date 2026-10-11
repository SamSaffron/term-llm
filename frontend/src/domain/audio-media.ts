import { escapeHTML } from './markdown-hooks';

/** Published assets must stay on the authenticated, content-addressed media route. */
export function trustedPublishedMediaURL(value: string, audioOnly = false): string {
  try {
    const url = new URL(value, location.href);
    if (
      !/^https?:$/.test(url.protocol) ||
      url.origin !== location.origin ||
      url.username ||
      url.password ||
      /[?#]/.test(value)
    )
      return '';
    const extensions = audioOnly
      ? 'mp3|wav|ogg|flac|m4a|weba'
      : 'png|jpe?g|gif|webp|bmp|mp4|webm|mp3|wav|ogg|flac|m4a|weba';
    return new RegExp(`/media/[a-f0-9]{32}\\.(?:${extensions})$`, 'i').test(url.pathname)
      ? value
      : '';
  } catch {
    return '';
  }
}

export interface AudioMediaDescriptor {
  url: string;
  label?: string;
  name?: string;
  caption?: string;
  downloadURL?: string;
}

export function audioMediaDescriptor(media: AudioMediaDescriptor) {
  // Neither a filesystem path nor control characters belong in a download name.
  const basename = (media.name || '').split(/[\\/]/).pop() || '';
  const name = [...basename]
    .filter((character) => character.charCodeAt(0) > 31 && character.charCodeAt(0) !== 127)
    .join('')
    .trim();
  const caption = media.caption?.trim() || '';
  const title = media.label?.trim() || caption || name || 'Audio recording';
  return {
    ...media,
    name,
    title,
    caption: caption === title ? '' : caption,
    downloadName: name || 'audio',
  };
}

/** Escaped inline-compatible markup; only validated resolver artifacts call this. */
export function audioMediaHTML(media: AudioMediaDescriptor): string {
  const card = audioMediaDescriptor(media);
  return `<span class="audio-media-card"><span class="audio-media-title">${escapeHTML(card.title)}</span>${card.name ? `<span class="audio-media-name">${escapeHTML(card.name)}</span>` : ''}${card.caption ? `<span class="audio-media-caption">${escapeHTML(card.caption)}</span>` : ''}<audio src="${escapeHTML(card.url)}" controls preload="metadata" aria-label="${escapeHTML(card.title)}" title="${escapeHTML(card.title)}"></audio><span class="audio-media-error" role="status" hidden>This recording could not be played. Download the audio to listen in another player.</span><a class="audio-media-download" href="${escapeHTML(card.downloadURL || card.url)}" download="${escapeHTML(card.downloadName)}">Download audio</a></span>`;
}

/** Move matching cards into a new render without resetting native playback/seek state. */
export function preserveAudioMediaCards(previous: HTMLElement, prepared: HTMLElement): void {
  const players = (root: HTMLElement) =>
    root.querySelectorAll<HTMLAudioElement>('.audio-media-card audio');
  const existing = new Map<string, HTMLAudioElement[]>();
  for (const player of players(previous)) {
    const matches = existing.get(player.src) || [];
    matches.push(player);
    existing.set(player.src, matches);
  }
  // Match source + occurrence, then check the label. Repeated references must not
  // all steal the first player's state, nor inherit state from a different label.
  for (const player of players(prepared)) {
    const match = existing.get(player.src)?.shift();
    if (!match || match.getAttribute('aria-label') !== player.getAttribute('aria-label')) continue;
    const card = match.closest('.audio-media-card');
    if (card) player.closest('.audio-media-card')?.replaceWith(card);
  }
}

export function showAudioMediaError(player: HTMLAudioElement): void {
  const status = player
    .closest('.audio-media-card')
    ?.querySelector<HTMLElement>('.audio-media-error');
  if (status) status.hidden = false;
}
