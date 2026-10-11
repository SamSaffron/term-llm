import { showAudioMediaError } from '../domain/audio-media';

/** Native play/error do not bubble. Keep playback scoped to one transcript. */
export function bindTranscriptMedia(root: HTMLElement): () => void {
  const players = () => [...root.querySelectorAll<HTMLMediaElement>('audio,video')];
  let active: HTMLMediaElement | null = null;
  const pause = (player: HTMLMediaElement) => {
    if (!player.paused) player.pause();
  };
  const onPlay = (event: Event) => {
    const player = event.target;
    if (!(player instanceof HTMLMediaElement)) return;
    if (active && active !== player) pause(active);
    active = player;
    for (const other of players()) if (other !== player) pause(other);
  };
  const onError = (event: Event) => {
    if (event.target instanceof HTMLAudioElement) showAudioMediaError(event.target);
  };
  const observer = new MutationObserver(() => {
    if (active && !root.contains(active)) {
      pause(active);
      active = null;
    }
  });
  root.addEventListener('play', onPlay, true);
  root.addEventListener('error', onError, true);
  observer.observe(root, { childList: true, subtree: true });
  return () => {
    root.removeEventListener('play', onPlay, true);
    root.removeEventListener('error', onError, true);
    observer.disconnect();
    if (active) pause(active);
    for (const player of players()) if (player !== active) pause(player);
    active = null;
  };
}
