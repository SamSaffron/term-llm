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
| `--output` | `-o` | Custom output path |
| `--model` | | Venice video model override |
| `--duration` | | Video duration (`5s`, `10s`) |
| `--aspect-ratio` | | Aspect ratio, e.g. `16:9`, `9:16` |
| `--resolution` | | Output resolution (`480p`, `720p`, `1080p`) |
| `--negative-prompt` | | Negative prompt |
| `--audio` | | Request audio for models that support it |
| `--quote-only` | | Quote the job and exit |
| `--no-wait` | | Queue the job and exit without polling |
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

# Planning and batch workflows
term-llm video "astronaut on mars" --quote-only
term-llm video "cyberpunk city" --no-wait
```

### Credentials

`term-llm video` currently uses Venice AI and reads credentials from `VENICE_API_KEY` or `image.venice.api_key` in your config.

### Defaults

If you do not specify a model, term-llm picks a cheap Venice default:
- `longcat-distilled-text-to-video` for text-to-video
- `longcat-distilled-image-to-video` for image-to-video
