---
title: "Audio generation"
weight: 5
description: "Generate speech audio with Venice AI, Gemini, and ElevenLabs text-to-speech models."
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

You can also use Gemini or ElevenLabs TTS:

```bash
term-llm audio "Say cheerfully: hello from Gemini" --provider gemini --voice Kore
term-llm audio "Hello from ElevenLabs" --provider elevenlabs --voice Rachel --model eleven_flash_v2_5
```

### Audio Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | `-p` | Audio provider override: `venice`, `gemini`, `elevenlabs` |
| `--output` | `-o` | Custom output path, or `-` for stdout |
| `--model` | | TTS model override |
| `--voice` | | Model-specific voice; ElevenLabs accepts a voice ID or account voice name; Venice accepts cloned voice handles like `vv_<id>` |
| `--voice1` | | Gemini multi-speaker voice for `--speaker1` |
| `--voice2` | | Gemini multi-speaker voice for `--speaker2` |
| `--speaker1` | | Gemini multi-speaker label for the first speaker |
| `--speaker2` | | Gemini multi-speaker label for the second speaker |
| `--language` | | Optional provider language hint (`English`, `en`, etc.; provider/model-specific) |
| `--prompt` | | Style/emotion prompt for models that support it |
| `--format` | | Venice: `mp3`, `opus`, `aac`, `flac`, `wav`, `pcm`; Gemini: `wav`, `pcm`; ElevenLabs: `mp3_44100_128`, `pcm_24000`, `wav_44100`, etc. |
| `--speed` | | Venice speech speed `0.25` to `4.0`; ElevenLabs voice speed `0.7` to `1.2` |
| `--streaming` | | Ask supported providers to stream; term-llm still collects before saving |
| `--temperature` | | Sampling temperature for supported models, `0` to `2`; omitted by default |
| `--top-p` | | Nucleus sampling for supported models, `0` to `1`; omitted by default |
| `--stability` | | ElevenLabs voice stability, `0` to `1`; omitted by default |
| `--similarity-boost` | | ElevenLabs similarity boost, `0` to `1`; omitted by default |
| `--style` | | ElevenLabs style exaggeration, `0` to `1`; omitted by default |
| `--speaker-boost` | | ElevenLabs speaker boost voice setting |
| `--seed` | | ElevenLabs deterministic seed |
| `--previous-text` / `--next-text` | | ElevenLabs continuity context |
| `--previous-request-ids` / `--next-request-ids` | | ElevenLabs comma-separated request IDs for continuity |
| `--pronunciation-dictionaries` | | ElevenLabs comma-separated pronunciation dictionary IDs, optionally `id:version` |
| `--use-pvc-as-ivc` | | ElevenLabs lower-latency PVC workaround |
| `--apply-text-normalization` | | ElevenLabs text normalization: `auto`, `on`, `off` |
| `--apply-language-text-normalization` | | ElevenLabs language text normalization |
| `--optimize-streaming-latency` | | ElevenLabs latency optimization level, `0` to `4` |
| `--enable-logging` | | ElevenLabs request logging/history; set false for zero-retention-capable accounts |
| `--json` | | Emit machine-readable JSON to stdout |
| `--debug` | `-d` | Show debug information |

`--model`, `--voice`, `--voice1`, `--voice2`, `--format`, `--provider`, and `--apply-text-normalization` include shell completion candidates.

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

term-llm audio "A one second ElevenLabs smoke test." \
  --provider elevenlabs \
  --model eleven_flash_v2_5 \
  --voice Rachel \
  --format mp3_44100_128 \
  --stability 0.5 \
  --similarity-boost 0.75

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

### ElevenLabs TTS Models

term-llm includes the documented ElevenLabs text-to-speech models:

| Model | Notes |
|-------|-------|
| `eleven_v3` | Latest expressive speech model |
| `eleven_multilingual_v2` | Default high-quality multilingual speech |
| `eleven_flash_v2_5` | Low-latency multilingual speech |
| `eleven_flash_v2` | Low-latency English speech |
| `eleven_turbo_v2_5` | Deprecated predecessor of Flash v2.5 |
| `eleven_turbo_v2` | Deprecated predecessor of Flash v2 |
| `eleven_monolingual_v1` | Deprecated English-only model |
| `eleven_multilingual_v1` | Deprecated multilingual model |

ElevenLabs voices are account-specific. `--voice` accepts either a raw `voice_id` or an exact voice name from the account; name lookup uses the ElevenLabs voices API before generating speech.

ElevenLabs output formats:

`alaw_8000`, `mp3_22050_32`, `mp3_24000_48`, `mp3_44100_32`, `mp3_44100_64`, `mp3_44100_96`, `mp3_44100_128`, `mp3_44100_192`, `opus_48000_32`, `opus_48000_64`, `opus_48000_96`, `opus_48000_128`, `opus_48000_192`, `pcm_8000`, `pcm_16000`, `pcm_22050`, `pcm_24000`, `pcm_32000`, `pcm_44100`, `pcm_48000`, `ulaw_8000`, `wav_8000`, `wav_16000`, `wav_22050`, `wav_24000`, `wav_32000`, `wav_44100`, `wav_48000`.

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

ElevenLabs credentials are read from `ELEVENLABS_API_KEY`, `XI_API_KEY`, or `audio.elevenlabs.api_key`.

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
  elevenlabs:
    api_key: $ELEVENLABS_API_KEY
    model: eleven_multilingual_v2
    voice: JBFqnCBsd6RMkjVDRZzb
    format: mp3_44100_128
```
