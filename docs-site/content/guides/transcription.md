---
title: "Transcription"
weight: 8
description: "Transcribe audio files to text with OpenAI, your ChatGPT login, Mistral Voxtral, Venice, ElevenLabs, or a local whisper.cpp HTTP server."
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
term-llm transcribe note.m4a --provider chatgpt
term-llm transcribe memo.wav --provider mistral
term-llm transcribe hello.mp3 --provider venice --model nvidia/parakeet-tdt-0.6b-v3
term-llm transcribe hello.mp3 --provider elevenlabs --model scribe_v2
term-llm transcribe call.ogg --provider local --porcelain
```

Key options:

- `--language` for a language hint such as `en` or `ja`
- `--provider` to select the transcription backend
- `--model` to override the configured transcription model
- `--timestamps` to ask supported providers for timestamp metadata. ElevenLabs Scribe emits the full JSON response, including `words` entries with `text`, `start`, `end`, `type`, and `logprob`.
- `--porcelain` to output only transcript text

## Provider options

term-llm supports several transcription backends:

- `openai`
- `chatgpt` to use your ChatGPT subscription login instead of an API key (experimental)
- `mistral` (Voxtral)
- `venice`
- `elevenlabs`
- `local` for a local Whisper-compatible server

If you omit `--provider`, term-llm uses the configured transcription provider or falls back to OpenAI.

### ChatGPT login (experimental)

The `chatgpt` provider uses the same ChatGPT OAuth session as the `chatgpt` chat provider, so no OpenAI API key is needed. Sign in once, then transcribe:

```bash
term-llm auth login chatgpt
term-llm transcribe note.m4a --provider chatgpt
```

To make it the default for `transcribe`, the web UI microphone, and Telegram voice notes:

```yaml
transcription:
  provider: chatgpt
```

term-llm uploads the audio to `https://chatgpt.com/backend-api/transcribe`, the endpoint Codex dictation uses. Keep in mind:

- The endpoint is undocumented and may change or disappear without notice.
- ChatGPT picks the model. `--model` and `transcription.model` are ignored.
- `--timestamps` is not supported. The response contains plain text only.
- Accepted extensions are `.flac`, `.m4a`, `.mp3`, `.mp4`, `.mpeg`, `.mpga`, `.oga`, `.ogg`, `.opus`, `.wav`, and `.webm`. Files over 25 MB are rejected before upload.
- Some networks get a Cloudflare challenge (HTTP 403) on this endpoint. term-llm reports this as an error and does not retry; if it happens, use another provider.
- Expired sessions are refreshed automatically. If ChatGPT rejects the session, term-llm refreshes it once and retries. If the refresh token is no longer valid, term-llm removes the stored login; run `term-llm auth login chatgpt` again.

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

## Local whisper.cpp server

Run a whisper.cpp HTTP server separately, then select `--provider local`. term-llm sends audio to `http://localhost:8080/inference` by default. To use another address, configure the server's base URL (without `/inference`):

```yaml
providers:
  local_whisper:
    base_url: http://localhost:8081
```

```bash
term-llm transcribe note.m4a --provider local
```

The `whisper-cli` option is still mentioned in CLI help, but the current transcription command does not dispatch to the local CLI helper. Use the HTTP server backend instead; setting `WHISPER_MODEL` does not enable CLI transcription.

## When to use it

Use transcription when you want to:

- turn voice notes into text before summarizing or editing
- capture meeting audio for later analysis
- feed transcripts into `ask`, `edit`, or your own downstream tooling

## Related pages

- [Web UI and API](/guides/web-ui-and-api/)
- [Usage](/guides/usage/)
