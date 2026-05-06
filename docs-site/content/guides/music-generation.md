---
title: "Music generation"
weight: 6
description: "Generate music and sound effects with Venice AI and ElevenLabs."
kicker: "Media"
---

Generate music, songs, or sound effects from a prompt.

```bash
term-llm music "single bright xylophone ping" --duration 1
```

By default, music clips are:

- Saved to `~/Music/term-llm/` with timestamped filenames
- Generated with Venice `elevenlabs-sound-effects-v2`
- Returned as MP3

The command also reads the prompt from stdin when no positional prompt is supplied:

```bash
echo "cinematic whoosh" | term-llm music -o - > whoosh.mp3
```

### Providers

`term-llm music` supports:

- `venice`: Venice async audio queue for music, songs, sound effects, and Venice-hosted TTS audio models
- `elevenlabs`: ElevenLabs `/v1/music`, `/v1/music/stream`, and `/v1/music/detailed`

### Music flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | `-p` | Music provider override: `venice`, `elevenlabs` |
| `--output` | `-o` | Custom output path, or `-` for stdout |
| `--model` | | Music model override |
| `--format` | | Output format. Venice: model-default `mp3`, `wav`, or `flac`; ElevenLabs: `mp3_44100_128`, `pcm_24000`, `wav_44100`, etc. |
| `--duration` | | Requested duration in seconds. Venice is model-specific; ElevenLabs prompt mode supports 3 to 600 seconds |
| `--lyrics` / `--lyrics-file` | | Venice lyrics prompt/text for lyric-capable models |
| `--lyrics-optimizer` | | Venice: auto-generate lyrics from the prompt where supported |
| `--voice` | | Venice voice for voice-enabled models |
| `--language` | | Venice language code for models that support `language_code` |
| `--speed` | | Venice speed multiplier for models that support speed |
| `--streaming` | | ElevenLabs: use the streaming music endpoint |
| `--detailed` | | ElevenLabs: use the detailed endpoint and keep returned metadata when available |
| `--composition-plan-file` | | ElevenLabs: JSON composition plan file |
| `--seed` | | ElevenLabs deterministic seed |
| `--force-instrumental` | | Force instrumental generation where supported |
| `--respect-sections-durations` | | ElevenLabs composition-plan mode: strictly respect section durations |
| `--store-for-inpainting` | | ElevenLabs enterprise option to store generated song for inpainting |
| `--sign-with-c2pa` | | ElevenLabs: sign generated MP3 with C2PA |
| `--with-timestamps` | | ElevenLabs detailed endpoint: include word timestamps |
| `--delete-media-on-completion` | | Venice: delete queued provider-side media after retrieval; enabled by default |
| `--quote` | | Venice: return price quote instead of queueing generation |
| `--poll-interval` / `--poll-timeout` | | Venice async queue polling controls |
| `--json` | | Emit machine-readable JSON to stdout |
| `--debug` | `-d` | Show debug information |

`--provider`, `--model`, `--format`, and `--voice` include shell completion candidates.

### Examples

```bash
term-llm music "single bright xylophone ping" \
  --provider venice \
  --model elevenlabs-sound-effects-v2 \
  --duration 1

term-llm music "upbeat chiptune victory sting" \
  --provider venice \
  --model mmaudio-v2-text-to-audio \
  --duration 1 \
  --format wav

term-llm music "polished instrumental funk loop" \
  --provider elevenlabs \
  --duration 3 \
  --force-instrumental \
  --format mp3_44100_128

term-llm music "80s synth pop song" \
  --provider venice \
  --model minimax-music-v25 \
  --lyrics "Verse 1: Neon lights over the avenue" \
  --duration 60

term-llm music "quote a one second sound effect" \
  --provider venice \
  --model elevenlabs-sound-effects-v2 \
  --duration 1 \
  --quote
```

### Venice music models

term-llm includes the Venice music/audio model catalog:

| Model | Default format | Duration support | Notes |
|-------|----------------|------------------|-------|
| `ace-step-15` | `flac` | 60–210 seconds | Song generation with optional lyrics |
| `elevenlabs-music` | `mp3` | 3–600 seconds | High-quality instrumental music; supports `--force-instrumental` |
| `minimax-music-v2` | `mp3` | Provider default | Requires lyrics |
| `minimax-music-v25` | `mp3` | Provider default | Lyrics optional; supports lyric optimizer and instrumental mode |
| `minimax-music-v26` | `mp3` | Provider default | Lyrics optional; supports instrumental mode |
| `stable-audio-25` | `wav` | 5–190 seconds | Sound effects, ambient textures, short clips |
| `elevenlabs-sound-effects-v2` | `mp3` | 1–22 seconds | Default; good for one-second smoke clips |
| `mmaudio-v2-text-to-audio` | `wav` | 1–30 seconds | Text-to-audio / sound effects |
| `elevenlabs-tts-v3` | `mp3` | Character-priced | Venice-hosted ElevenLabs TTS v3 with voices |
| `elevenlabs-tts-multilingual-v2` | `mp3` | Character-priced | Venice-hosted ElevenLabs multilingual TTS |

Venice options are model-specific. If a model does not support a supplied field, Venice returns the API error directly.

### ElevenLabs music model

ElevenLabs currently documents one music model for the compose endpoints:

| Model | Notes |
|-------|-------|
| `music_v1` | Prompt or composition-plan driven music generation |

ElevenLabs prompt-mode duration is `3` to `600` seconds. So a literal one-second test is not accepted by the direct ElevenLabs music API; use `--duration 3` there. Venice has one-second-capable sound-effect models.

ElevenLabs output formats:

`alaw_8000`, `mp3_22050_32`, `mp3_24000_48`, `mp3_44100_32`, `mp3_44100_64`, `mp3_44100_96`, `mp3_44100_128`, `mp3_44100_192`, `opus_48000_32`, `opus_48000_64`, `opus_48000_96`, `opus_48000_128`, `opus_48000_192`, `pcm_8000`, `pcm_16000`, `pcm_22050`, `pcm_24000`, `pcm_32000`, `pcm_44100`, `pcm_48000`, `ulaw_8000`, `wav_8000`, `wav_16000`, `wav_22050`, `wav_24000`, `wav_32000`, `wav_44100`, `wav_48000`.

### JSON output

`--json` prints a single structured object to stdout after saving the file.

```json
{
  "provider": "venice",
  "prompt": "single bright xylophone ping",
  "model": "elevenlabs-sound-effects-v2",
  "format": "mp3",
  "output": {
    "path": "/home/me/Music/term-llm/20260503-120000-single_bright_xylophone_ping.mp3",
    "mime_type": "audio/mpeg",
    "bytes": 17180
  }
}
```

### Credentials and config

`term-llm music` reads Venice credentials from `VENICE_API_KEY`, `music.venice.api_key`, `audio.venice.api_key`, or the existing `image.venice.api_key` fallback.

ElevenLabs credentials are read from `ELEVENLABS_API_KEY`, `XI_API_KEY`, `music.elevenlabs.api_key`, `audio.elevenlabs.api_key`, or the configured `providers.elevenlabs` API key.

```yaml
music:
  provider: venice
  output_dir: ~/Music/term-llm
  venice:
    api_key: $VENICE_API_KEY
    model: elevenlabs-sound-effects-v2
    format: mp3
  elevenlabs:
    api_key: $ELEVENLABS_API_KEY
    model: music_v1
    format: mp3_44100_128
```
