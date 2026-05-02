---
title: "Audio generation"
weight: 5
description: "Generate speech audio with Venice AI and Gemini text-to-speech models."
kicker: "Media"
---

Generate speech audio using a configured text-to-speech provider.

```bash
term-llm audio "hello from term-llm"
```

By default, audio clips are:
- Saved to `~/Music/term-llm/` with timestamped filenames
- Generated with Venice `tts-kokoro`
- Rendered as MP3 using voice `af_sky`

You can also use Gemini TTS:

```bash
term-llm audio "Say cheerfully: hello from Gemini" --provider gemini --voice Kore
```

### Audio Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | `-p` | Audio provider override: `venice`, `gemini` |
| `--output` | `-o` | Custom output path, or `-` for stdout |
| `--model` | | TTS model override |
| `--voice` | | Model-specific voice, or a Venice cloned voice handle like `vv_<id>` |
| `--voice1` | | Gemini multi-speaker voice for `--speaker1` |
| `--voice2` | | Gemini multi-speaker voice for `--speaker2` |
| `--speaker1` | | Gemini multi-speaker label for the first speaker |
| `--speaker2` | | Gemini multi-speaker label for the second speaker |
| `--language` | | Optional Venice language hint (`English`, `en`, etc.; model-specific) |
| `--prompt` | | Style/emotion prompt for models that support it |
| `--format` | | Venice: `mp3`, `opus`, `aac`, `flac`, `wav`, `pcm`; Gemini: `wav`, `pcm` |
| `--speed` | | Venice speech speed, `0.25` to `4.0` |
| `--streaming` | | Ask Venice to stream sentence-by-sentence; term-llm still collects before saving |
| `--temperature` | | Sampling temperature for supported models, `0` to `2`; omitted by default |
| `--top-p` | | Nucleus sampling for supported models, `0` to `1`; omitted by default |
| `--json` | | Emit machine-readable JSON to stdout |
| `--debug` | `-d` | Show debug information |

`--model`, `--voice`, `--voice1`, `--voice2`, `--format`, and `--provider` include shell completion candidates.

### Examples

```bash
term-llm audio "hello from term-llm"
term-llm audio "quick smoke test" --output smoke.mp3
term-llm audio "faster please" --speed 1.25 --format wav
term-llm audio "sad robot noises" \
  --model tts-qwen3-0-6b \
  --voice Vivian \
  --prompt "Sad and slow."

term-llm audio "Say cheerfully: have a wonderful day" \
  --provider gemini \
  --model gemini-3.1-flash-tts-preview \
  --voice Kore \
  --format wav

term-llm audio "TTS the following conversation between Joe and Jane: Joe: Hi Jane. Jane: Hi Joe." \
  --provider gemini \
  --speaker1 Joe --voice1 Kore \
  --speaker2 Jane --voice2 Puck

echo "pipe me" | term-llm audio --voice af_bella -o - > out.mp3
```

### Venice TTS Models

term-llm includes the Venice text-to-speech model catalog:

| Model | Notes |
|-------|-------|
| `tts-kokoro` | Default, cheap general TTS |
| `tts-qwen3-0-6b` | Qwen 3 TTS, supports style prompt / sampling options |
| `tts-qwen3-1-7b` | Larger Qwen 3 TTS |
| `tts-xai-v1` | xAI TTS v1 |
| `tts-inworld-1-5-max` | Inworld TTS-1.5 Max |
| `tts-chatterbox-hd` | Chatterbox HD; supports cloned voices |
| `tts-orpheus` | Orpheus TTS |
| `tts-elevenlabs-turbo-v2-5` | ElevenLabs Turbo v2.5 |
| `tts-minimax-speech-02-hd` | MiniMax Speech-02 HD |
| `tts-gemini-3-1-flash` | Gemini 3.1 Flash TTS |

Voices are model-specific. If a model rejects a voice, Venice returns the API error directly.

### Gemini TTS Models

| Model | Single speaker | Multi-speaker |
|-------|----------------|---------------|
| `gemini-3.1-flash-tts-preview` | Yes | Yes |
| `gemini-2.5-flash-preview-tts` | Yes | Yes |
| `gemini-2.5-pro-preview-tts` | Yes | Yes |

Gemini TTS returns 24 kHz mono PCM. term-llm can save it directly as `pcm`, or wrap it as a `wav` file. Gemini TTS does not support streaming; language is auto-detected.

Gemini prebuilt voices:

`Zephyr`, `Puck`, `Charon`, `Kore`, `Fenrir`, `Leda`, `Orus`, `Aoede`, `Callirrhoe`, `Autonoe`, `Enceladus`, `Iapetus`, `Umbriel`, `Algieba`, `Despina`, `Erinome`, `Algenib`, `Rasalgethi`, `Laomedeia`, `Achernar`, `Alnilam`, `Schedar`, `Gacrux`, `Pulcherrima`, `Achird`, `Zubenelgenubi`, `Vindemiatrix`, `Sadachbia`, `Sadaltager`, `Sulafat`.

### JSON Output

`--json` prints a single structured object to stdout after saving the file.

```json
{
  "provider": "venice",
  "text": "hello from term-llm",
  "model": "tts-kokoro",
  "voice": "af_sky",
  "format": "mp3",
  "output": {
    "path": "/home/me/Music/term-llm/20260502-120000-hello_from_term-llm.mp3",
    "mime_type": "audio/mpeg",
    "bytes": 12345
  }
}
```

### Credentials and Config

`term-llm audio` reads Venice credentials from `VENICE_API_KEY`, `audio.venice.api_key`, or the existing `image.venice.api_key` fallback.

Gemini credentials are read from `GEMINI_API_KEY`, `audio.gemini.api_key`, `image.gemini.api_key`, or the configured `providers.gemini` API key.

```yaml
audio:
  provider: venice
  output_dir: ~/Music/term-llm
  venice:
    api_key: $VENICE_API_KEY
    model: tts-kokoro
    voice: af_sky
    format: mp3
  gemini:
    api_key: $GEMINI_API_KEY
    model: gemini-3.1-flash-tts-preview
    voice: Kore
    format: wav
```
