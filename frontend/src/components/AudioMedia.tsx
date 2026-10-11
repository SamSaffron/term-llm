import { useLayoutEffect, useRef } from 'preact/hooks';
import {
  audioMediaDescriptor,
  showAudioMediaError,
  type AudioMediaDescriptor,
} from '../domain/audio-media';

/** Uploaded audio uses its own trusted attachment URL, not the published route. */
export function AudioMedia(props: AudioMediaDescriptor) {
  const card = audioMediaDescriptor(props);
  const player = useRef<HTMLAudioElement>(null);
  useLayoutEffect(() => {
    const element = player.current;
    return () => {
      if (element && !element.paused) element.pause();
    };
  }, [props.url]);
  return (
    <span class="audio-media-card">
      <span class="audio-media-title">{card.title}</span>
      {card.name && <span class="audio-media-name">{card.name}</span>}
      {card.caption && <span class="audio-media-caption">{card.caption}</span>}
      <audio
        ref={player}
        src={card.url}
        controls
        preload="metadata"
        aria-label={card.title}
        title={card.title}
        onError={(event) => showAudioMediaError(event.currentTarget)}
      />
      <span class="audio-media-error" role="status" hidden>
        This recording could not be played. Download the audio to listen in another player.
      </span>
      <a
        class="audio-media-download"
        href={card.downloadURL || card.url}
        download={card.downloadName}
      >
        Download audio
      </a>
    </span>
  );
}
