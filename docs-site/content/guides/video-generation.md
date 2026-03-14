---
title: "Video generation"
weight: 4
description: "Generate videos with Venice AI using text-to-video or image-to-video models."
kicker: "Media"
---

Generate videos using Venice AI's native video API.

```bash
term-llm video "a corgi surfing at sunset"
```

By default, videos are:
- Saved to `~/Pictures/term-llm/` with timestamped filenames
- Quoted before queueing so you can see the estimated cost
- Polled until completion and written as `.mp4`

### Video Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--input` | `-i` | Input image for image-to-video |
| `--reference` | `-r` | Reference image(s) for models that support `reference_image_urls` (repeatable) |
| `--output` | `-o` | Custom output path |
| `--model` | | Venice video model override |
| `--duration` | | Video duration (`5s`, `10s`) |
| `--aspect-ratio` | | Aspect ratio, e.g. `16:9`, `9:16` |
| `--resolution` | | Output resolution (`480p`, `720p`, `1080p`) |
| `--negative-prompt` | | Negative prompt |
| `--audio` | | Request audio for models that support it |
| `--quote-only` | | Quote the job and exit |
| `--no-wait` | | Queue the job and exit without polling |
| `--json` | | Emit machine-readable JSON to stdout |
| `--poll-interval` | | Poll interval while waiting |
| `--timeout` | | Maximum wait time |
| `--debug` | `-d` | Show debug information |

### Video Examples

```bash
# Text-to-video
term-llm video "a neon train passing through Tokyo at night"
term-llm video "a corgi surfing at sunset" --model kling-v3-pro-text-to-video

# Image-to-video
term-llm video "make Romeo blink and wag his tail" -i romeo.png
term-llm video "cute dog, influencer reacts" -i romeo.png --aspect-ratio 9:16 --duration 10s

# Multi-reference image-to-video (model support varies)
term-llm video "keep Romeo's face consistent while a Japanese influencer reacts" \
  -i romeo.png \
  -r romeo-closeup.png \
  -r romeo-profile.jpg \
  --model kling-o3-pro-image-to-video

# Planning and batch workflows
term-llm video "astronaut on mars" --quote-only
term-llm video "astronaut on mars" --quote-only --json
term-llm video "cyberpunk city" --no-wait --json
```

### JSON Output

`--json` prints a single structured object to stdout, which is useful for scripts.
Human progress lines stay on stderr when JSON mode is off.

Example:

```json
{
  "provider": "venice",
  "prompt": "astronaut on mars",
  "model": "longcat-distilled-text-to-video",
  "duration": "5s",
  "resolution": "720p",
  "status": "queued",
  "quote": {
    "amount": 0.09
  },
  "job": {
    "queue_id": "123e4567-e89b-12d3-a456-426614174000"
  }
}
```

### Credentials

`term-llm video` currently uses Venice AI and reads credentials from `VENICE_API_KEY` or `image.venice.api_key` in your config.

### Defaults

If you do not specify a model, term-llm picks a cheap Venice default:
- `longcat-distilled-text-to-video` for text-to-video
- `longcat-distilled-image-to-video` for image-to-video

### Model-specific inputs

Venice exposes advanced fields like `reference_image_urls`, `scene_image_urls`, `end_image_url`, and element-based references, but support is model-dependent.
This command currently exposes the portable subset:
- primary `--input` image
- repeatable `--reference` images

If a chosen Venice model rejects reference images, the API will return that error directly instead of term-llm pretending otherwise.
