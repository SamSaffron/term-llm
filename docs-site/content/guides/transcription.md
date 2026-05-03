---
title: "Transcription"
weight: 8
description: "Transcribe audio files to text with OpenAI, Mistral Voxtral, Venice, ElevenLabs, a local Whisper server, or whisper.cpp CLI."
kicker: "Audio"
featured: true
next:
  label: Image generation
  url: /guides/image-generation/
---
## Basic usage

```bash
term-llm transcribe meeting.m4a
```

Supported input extensions include:

- `.ogg`
- `.mp3`
- `.wav`
- `.m4a`
- `.flac`
- `.mp4`
- `.webm`

## Useful flags

```bash
term-llm transcribe interview.mp3 --language en
term-llm transcribe note.m4a --provider openai
term-llm transcribe memo.wav --provider mistral
term-llm transcribe hello.mp3 --provider venice --model nvidia/parakeet-tdt-0.6b-v3
term-llm transcribe hello.mp3 --provider elevenlabs --model scribe_v2
term-llm transcribe call.ogg --provider whisper-cli --porcelain
```

Key options:

- `--language` for a language hint such as `en` or `ja`
- `--provider` to select the transcription backend
- `--model` to override the configured transcription model
- `--timestamps` to ask Venice for timestamp metadata before extracting the transcript text
- `--porcelain` to output only transcript text

## Provider options

term-llm supports several transcription backends:

- `openai`
- `mistral` (Voxtral)
- `venice`
- `elevenlabs`
- `local` for a local Whisper-compatible server
- `whisper-cli` for `whisper.cpp`

If you omit `--provider`, term-llm uses the configured transcription provider or falls back to OpenAI.

### Venice models

Venice uses `POST /api/v1/audio/transcriptions` and supports:

- `nvidia/parakeet-tdt-0.6b-v3`
- `openai/whisper-large-v3`
- `fal-ai/wizper`
- `elevenlabs/scribe-v2`
- `stt-xai-v1`

### ElevenLabs models

ElevenLabs uses `POST /v1/speech-to-text` and supports:

- `scribe_v2`
- `scribe_v1`

## Configuration

```yaml
transcription:
  provider: venice
  venice:
    api_key: ${VENICE_API_KEY}
    model: nvidia/parakeet-tdt-0.6b-v3
  elevenlabs:
    api_key: ${ELEVENLABS_API_KEY}
    model: scribe_v2
```

Credential fallback order:

- Venice: `transcription.venice.api_key`, `VENICE_API_KEY`, `audio.venice.api_key`, `image.venice.api_key`, or `providers.venice.api_key`
- ElevenLabs: `transcription.elevenlabs.api_key`, `ELEVENLABS_API_KEY`, `XI_API_KEY`, `audio.elevenlabs.api_key`, or `providers.elevenlabs.api_key`

## whisper.cpp CLI mode

For `--provider whisper-cli`, term-llm looks for a `whisper` binary in `PATH` and a model file via:

- `WHISPER_MODEL`
- `providers.local_whisper.model` in config
- common default model paths

Example:

```bash
export WHISPER_MODEL=~/.local/share/whisper/models/ggml-base.bin
term-llm transcribe note.m4a --provider whisper-cli
```

## When to use it

Use transcription when you want to:

- turn voice notes into text before summarizing or editing
- capture meeting audio for later analysis
- feed transcripts into `ask`, `edit`, or your own downstream tooling

## Related pages

- [Web UI and API](/guides/web-ui-and-api/)
- [Usage](/guides/usage/)
