---
title: "Image generation"
weight: 3
description: "Generate and edit images with Gemini, OpenAI, ChatGPT, xAI, Venice, Flux, or OpenRouter."
kicker: "Media"
source_readme_heading: "Image Generation"
next:
  label: Text embeddings
  url: /guides/text-embeddings/
---
Generate and edit images using AI models from Gemini, OpenAI, ChatGPT, xAI, Venice, Flux (Black Forest Labs), and OpenRouter.

```bash
term-llm image "a robot cat on a rainbow"
```

By default, images are:
- Saved to `~/Pictures/term-llm/` with timestamped filenames
- Displayed in terminal via `icat` (if available)
- Copied to clipboard (actual image data, pasteable in apps)

### Image Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--input` | `-i` | Input image to edit; repeat for providers that support multiple inputs. |
| `--provider` | `-p` | Override a built-in image provider or configured OpenAI-compatible provider name |
| `--output` | `-o` | Custom output path; `-` writes image bytes to stdout for pipelines. |
| `--size` | `-s` | Requested resolution: `1K`, `2K`, or `4K`; provider/model support varies. |
| `--no-spinner` | | Disable the interactive progress display. |
| `--no-display` | | Skip terminal display |
| `--no-clipboard` | | Skip clipboard copy |
| `--no-save` | | Don't save to default location |
| `--debug` | `-d` | Show debug information |

### Image Examples

```bash
# Generate
term-llm image "cyberpunk cityscape at night"
term-llm image "minimalist logo" --provider flux
term-llm image "futuristic city" --provider xai              # uses Grok image model
term-llm image "storybook fox in the snow" --provider chatgpt:gpt-5.4
term-llm image "watercolor painting" -o ./art.png

# Edit existing image (not supported by xAI)
term-llm image "add a hat" -i photo.png
term-llm image "make it look vintage" -i input.png --provider gemini
term-llm image "add sparkles" -i clipboard       # edit from clipboard
term-llm image "combine these references" -p venice -i a.png -i b.png
term-llm image "robot cat" -o - | term-llm video "animate it" -i -

# Options
term-llm image "portrait" --no-clipboard        # don't copy to clipboard
term-llm image "landscape" --no-display         # don't show in terminal
```

### Image Providers

| Provider | Models | Environment Variable | Config Key |
|----------|--------|---------------------|------------|
| Gemini (default) | gemini-2.5-flash-image | `GEMINI_API_KEY` | `image.gemini.api_key` |
| OpenAI | gpt-image-2, gpt-image-1.5, gpt-image-1-mini | `OPENAI_API_KEY` | `image.openai.api_key` |
| ChatGPT | gpt-5.4-mini, gpt-5.4 | — (uses ChatGPT OAuth) | `image.chatgpt.model` |
| xAI | grok-2-image-1212 | `XAI_API_KEY` | `image.xai.api_key` |
| Venice | nano-banana-pro | `VENICE_API_KEY` | `image.venice.api_key` |
| Flux | flux-2-pro, flux-2-max, flux-kontext-pro | `BFL_API_KEY` | `image.flux.api_key` |
| OpenRouter | various | `OPENROUTER_API_KEY` | `image.openrouter.api_key` |

Image generation has its own provider selection and config. Where supported, image-specific keys can differ from text-provider keys; normal environment/config fallback still applies. ChatGPT reuses the same OAuth login as text. The listed model names are shipped defaults or examples, not a live availability or pricing guarantee.

**ChatGPT image provider:** log in with `term-llm auth login chatgpt`, then you can use `term-llm image --provider chatgpt:gpt-5.4 "..."` for subscription-backed image generation without an API key.

**Editing limits:** xAI does not support editing. ChatGPT accepts one input image. Venice supports one-image editing and multi-image editing with up to three inputs, subject to the selected model. Venice uses `image.venice.edit_model` when set; otherwise it derives the edit model by appending `-edit` to the generation model (unless already present). Gemini and OpenRouter also expose multi-image editing; upstream model limits still apply.

### Local and OpenAI-compatible image providers

An existing named `providers` entry with `type: openai_compatible` can also
serve images. Set its `base_url` to the API root (including `/v1` when required)
and select it through `image.provider` or `--provider`:

```yaml
providers:
  janus:
    type: openai_compatible
    base_url: http://127.0.0.1:8080/janus/v1
    model: Janus-Pro-1B
  demo:
    type: openai_compatible
    base_url: http://127.0.0.1:8080/demo/v1
    model: demo

image:
  provider: janus
```

```sh
term-llm image "A pelican riding a bike." -o pelican.png
term-llm image "A cat" --provider demo -o demo.png
term-llm config set image.provider demo
```

These are example endpoints: you must run a server implementing the Images API.
term-llm does not bundle Janus weights or a demo server. This is normal provider
configuration, not a different image command or an automatic fallback.

The client posts to `<base_url>/images/generations` with `model`, `prompt`,
`n: 1`, and `response_format: b64_json`. The server returns an OpenAI-format
`data` array containing `b64_json` or an image `url`. No size is sent unless
requested; `--size`/aspect-ratio requests are converted to pixel dimensions.
OpenAI-specific quality and output-format options are not imposed. Editing is
not supported by this generic adapter; built-in providers retain their existing
editing capabilities.

`api_key` is optional for local servers. When provided it uses normal provider
credential resolution (including environment variables and deferred secrets).
Unrelated `OPENAI_API_KEY` credentials are not forwarded. The full chat-only
`url` field is not accepted: use `base_url`. Built-in provider names keep their
existing image configuration, so choose a distinct custom name.
