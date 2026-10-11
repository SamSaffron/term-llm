# End-to-end audio presentation with `show_media`

## Status, scope, and acceptance criteria

This is an implementation plan, not an implementation. Repository inspection started at the repository root (`pwd` was run). The planning task changes only this file. All proposed new symbols/files below are explicitly described as new; other names refer to existing code inspected in this checkout.

An agent must be able to register an approved local TTS recording and place it at the intended position in its answer using:

```markdown
![Listen to the narrated summary](term-llm-media://<32-hex-reference>)
```

The same artifact must survive live streaming, durable transcript reload, subagent completion, JSON emission, and session persistence. Web users get an accessible native player and download link; terminal users get an honest, attractive file link; Telegram users get an appropriate native upload or document; HTML exports get bounded, safe audio embedding. Existing images, videos, and explicit `show_image` configurations remain compatible.

Key decisions:

- Keep the existing Markdown **image syntax for every media kind**. Do not invent an audio URL scheme, Markdown extension, or new tool.
- Support MP3, WAV, Ogg Opus/Vorbis, FLAC, and audio MP4/M4A. Identify audio-only WebM when bounded track inspection succeeds. No extension-only MIME detection or transcoding.
- Audio limit: **100 MiB**; retain images at 25 MiB and videos at 256 MiB. Container classification limitations are explicit below.
- Native `<audio controls preload="metadata">`; never autoplay. Show descriptive label, filename, optional caption, native duration/seek controls, and a persistent download affordance. One player at a time within the chat transcript.
- No Go duration parser and no terminal playback subprocess/keybinding. Browser metadata supplies duration; terminal output does not invent it. Keep the persisted `MediaArtifact` shape unchanged.
- HTML audio is bounded inline data, not a new binary sharing protocol. Public/remote sharing of audio is **explicit opt-in**, since recordings can contain private speech and embedded metadata. Existing video exports remain placeholders.

## 1. Findings from the actual code

### Tool, storage, and transport

`internal/tools/show_media.go` currently reads only 512 bytes in `ShowMediaTool.Execute`, calls `detectShowMediaType`, and chooses image/video size limits with a prefix branch. `Spec`, schema path wording, unsupported-format errors, and comments only describe images/videos. `DetectShowMediaType` is an exported byte-slice wrapper. `isMP4Brand` permits video/general ISO brands but not `M4A ` or `M4B `; `hasWebMDocType` identifies WebM, not its tracks.

`cmd/serve_media.go` duplicates format limits and extensions in `serveMediaExtension` and `serveMediaTypeForName`. Both `toolMediaEntries` and `mediaEntriesFromValue` explicitly filter out audio. `serveMediaPublisher.PublishMedia` copies bounded bytes to private content-addressed storage, re-sniffs the copy using the same 512-byte detector, and checks MIME equality. `mediaURL` projects only owned, top-level stored paths. Do not weaken any of these properties.

**Range support already exists.** `handleMedia` calls `cmd/serve_handlers.go:serveResolvedFile`, which opens the file and calls `http.ServeContent`. `cmd/serve_media_test.go:TestServeMediaPublisherAndHandler` already checks a video range. Preserve this path rather than adding manual Range parsing or reading entire audio files into memory. `handleMedia` currently relies on publication-time sniffing and extension mapping; it does not independently sniff on every GET. The route in `cmd/serve.go` is wrapped with `s.auth(s.cors(s.handleMedia))`.

### Current presentation behavior

- `internal/ui/media.go:RenderMediaMarkdown` renders only images with terminal graphics; all other kinds fall back to `RenderMediaLink`. That link currently labels any non-video as “Image”. Videos are **file links**, not terminal players.
- `internal/render/chat/message_block.go:renderToolMedia` assumes every non-video is an image and must be fixed. `collectReferencedMedia`, `SetPersistedSubagents`, and `renderMarkdown` already resolve artifacts generically.
- TUI live media handling moved from the original commit's monolithic `chat.go` into `internal/tui/chat/update_stream_events.go:handleStreamMedia` and `update_dispatch.go`'s `SubagentProgressMsg` branch. `chat.go` owns the map; `render.go:renderMarkdown` delegates to `ui.RenderMediaMarkdown`.
- Frontend Markdown resolution is in `frontend/src/domain/markdown.ts:mediaRenderer`; `MarkdownMediaTarget.type` is only `image | video`. `sanitizeMarkdownHTML` uses DOMPurify's HTML profile, which is **not a provenance check** for audio sources.
- Additional current frontend owners are `domain/transcript-indexes.ts:createTranscriptIndexes` and `domain/media-resolvers.ts:createMessageMediaResolvers`. Both compare target URL/type to avoid rerendering unrelated history and destroying media playback. Audio metadata additions must update those comparisons.
- `components/Transcript.tsx:publishedMediaURL` permits only same-origin, no-query/no-fragment media URLs with a 32-hex basename and a small extension allowlist. User-uploaded audio already renders as a bare `<audio src={src} controls />` in the attachment projection; align that with the new card rather than adding a second visual language.
- `domain/markdown.ts:rebaseRenderedAssetURLs` currently rebases images and links, not audio/video sources. `VIDEO_LINK_PATTERN` includes `.ogg`; an audio download link must not open the video lightbox.
- Telegram sending moved into `internal/serve/telegram_presentation.go:telegramPresentation.deliverMedia`. It sends images as photos and **everything else as video**, reopening the file for a document fallback. `telegram_completion.go` selects referenced artifacts using `referencedTelegramMedia` before delivery.

### Export and sharing are not already video-capable

`internal/session/export_html.go:replaceHTMLMediaReferences` resolves image references through `exportImages`, but videos become text placeholders. `renderSafeMarkdown` uses Goldmark's safe defaults, so inserting raw `<audio>` into Markdown will not work and enabling unsafe HTML globally would be wrong.

`export_html.tmpl` has CSP `default-src 'none'` with `img-src data:` (and conditional `'self'`); it currently has **no `media-src`**. `export_images.go` normalizes images, strips metadata, and has a 2 MiB inline budget, 8 MiB per-asset limit and 24 MiB total asset budget. These image transforms must never receive audio.

`export_gist.go:ShareBundle` emits HTML, Markdown, and image assets; `ShareFiles` forces assets back to inline for text-map destinations. `internal/share/share.go` defines image-only `AssetImageMediaTypes`, a 16 MiB file limit, 32 MiB total request limit and 32-file limit. `share_export.go:SelectShare` and `StripToolActivity` already preserve referenced `Media` separately, including response-only shares. Do not confuse preserving references with embedding their bytes.

## 2. Shared format inspection and limits

### Organization and API

Introduce a dependency-light **new `internal/mediatype/` package** (name is proposed) for show-media allowlisting, canonical extensions, limits, and bounded byte inspection. This avoids importing `internal/tools` from `internal/session` when export needs to re-sniff audio, and prevents tool/publisher rules drifting. It must not depend on `llm`, tools, sessions, external players, or codec libraries.

Proposed API/data:

```go
type Kind string // image, video, audio

type Format struct {
    MIME      string
    Extension string
    Kind      Kind
    MaxBytes  int64
}

// Proposed helpers, not existing symbols:
func Lookup(mime string) (Format, bool)
func ForExtension(ext string) (Format, bool)
func Detect(data []byte) string
func Inspect(r io.ReaderAt, size int64) (Format, error)
```

Use a small format table for MIME/extension/limit mappings and a list of focused recognizers. Keep complicated MPEG, ISO-BMFF and EBML parsing in separate helpers/files, not inside `Execute` or one giant detector switch. Preserve the existing exported `tools.DetectShowMediaType([]byte)` wrapper and unexported detector tests by delegating to the shared byte API. `Detect` operates only on supplied bytes; `Inspect` may seek within a file. The latter is authoritative for tool registration and publication and must use identical rules on both paths.

`Inspect` distinguishes unsupported/truncated input from genuine I/O errors. Bound every offset, addition, length, recursion level, and iteration count against file size; never allocate based directly on a container's claimed length. Read a modest initial buffer (e.g. 64 KiB), then use bounded `ReadAt` for headers/metadata, skipping payload instead of reading it. Check context cancellation around additional work at the caller, or accept context in the final API if needed. Do not read 100 MiB to determine the MIME type.

### Detection policy

| Format | MIME / stored extension | Required recognition and limits |
|---|---|---|
| MP3 | `audio/mpeg` / `.mp3` | ID3v2.2/2.3/2.4 with valid version, flags and synchsafe size; skip the declared tag (and v2.4 footer where present) within file bounds, then validate MPEG audio frames. Bare MP3 must begin with a valid MPEG **Layer III** frame, not just `0xff`. Reject reserved MPEG version, non-Layer-III/reserved layer, free/reserved bitrate indexes, reserved sample-rate index and reserved emphasis. Calculate version-specific frame length, require a complete frame and a second valid compatible header at its boundary. Bitrate may vary. Do not search arbitrary text for frame sync. Tiny/truncated or single-frame samples may be rejected conservatively. |
| WAV | `audio/wav` / `.wav` | RIFF at offset 0 and WAVE at offset 8, bounded RIFF chunks, a plausible `fmt ` chunk (channels/rate/block alignment) and `data` chunk. Support common PCM/IEEE-float and valid extensible PCM/float headers; reject arbitrary RIFF/WebP/AVI and unsupported compressed WAV encodings rather than promising playback. Account for odd-byte chunk padding. RF64/RIFX are deferred. |
| Ogg | `audio/ogg` / `.ogg` | Parse version-0 `OggS` page header and segment table, reconstruct bounded identification packet(s), require Opus `OpusHead` or Vorbis `\x01vorbis` with plausible fields. Never classify `OggS` alone as audio; reject Theora/unknown/mixed identification streams for the audio recognizer. Bound initial-page inspection (64 KiB, capped page/stream count); no checksum/whole-stream decoder is required. |
| FLAC | `audio/flac` / `.flac` | `fLaC` plus the mandatory first STREAMINFO metadata block, correct 34-byte payload length, plausible stream fields. No ID3-prefixed FLAC fallback in v1. |
| M4A / audio MP4 | `audio/mp4` / `.m4a` | Parse the `ftyp` box structurally, including compatible brands. Recognize `M4A ` and `M4B `; for generic `isom`/`mp41`/`mp42` etc., inspect complete track handlers under `moov/trak/mdia/hdlr`. At least one `soun` and **no `vide`** in a completely inspected track list means audio; any video track means video. Skip large `mdat` boxes by offset so a trailing `moov` works. No raw byte search for “soun”. Audio brands may be accepted as audio if track metadata is unavailable within budget; malformed boxes or contradictory video tracks must not be ignored. Codec availability is browser-dependent (AAC is the recommended interoperable encoding). |
| Audio-only WebM | `audio/webm` / `.weba` | Keep EBML DocType `webm` requirement. Parse Segment/Tracks/TrackEntry safely; a complete Tracks list with audio TrackType 2, supported `A_OPUS`/`A_VORBIS`, and no video TrackType 1 means audio. Any video track remains `video/webm`. `.weba` is the canonical stored extension so extension-to-MIME mapping is unambiguous; source filenames may still be `.webm`. |

For ISO-BMFF and WebM, cap inspected metadata bytes (4 MiB total), entries (e.g. 4096), and nesting (e.g. 16). EBML VINTs, unknown-sized segments, 64-bit MP4 box sizes and zero-to-EOF boxes need explicit handling. Do not turn these into full demuxers. On valid existing general/video-branded MP4 or WebM with no complete track metadata within budget, **retain legacy `video/mp4` / `video/webm` classification**, not an audio guess. This means unusual audio-only files can still receive a video player and the video cap; document that limitation and recommend ordinary M4A/MP3/WAV for generated TTS. Invalid/overflowing structures are errors, not fallback success. Old signature-only fixtures may need valid synthesized container structure where validation becomes stricter; real existing image/video formats must remain supported.

Raw AAC ADTS is **out of scope**: common browser behavior is less consistent, frame-sync overlaps invite false positives, and AAC in M4A is the better output target. Reject ADTS rather than calling it MP3. No AIFF, MIDI, WMA, arbitrary Matroska, Ogg video, encrypted media, or extension-based fallback. A conservative MIME allowlist is not a guarantee that every browser decodes every codec; every player must retain a download fallback.

### Tool integration

In `ShowMediaTool.Execute`, replace fixed-header reading and image/video branching with shared inspection and format lookup. Apply the selected size limit before publication; publisher still independently enforces it while copying. Keep read approval, canonical path resolution, regular-file checks, random reference generation, caption sanitization, no artifact on failure, and `Images == nil` behavior.

Update `Spec` description, `path` description, comments, unsupported-format list, and kind-specific limit errors. Say “see or listen to” rather than just “see”. Suggested result text remains backward-compatible:

> Media ready (audio/mpeg). Embed the exact URL below on its own line using Markdown image syntax, even for audio. Use concise descriptive text such as “Listen to the narrated summary”. Do not substitute a local path or write an HTML audio tag.

Keep the existing `![descriptive alt text](...)` example and suggested caption. Do not claim the recording played successfully when only registration succeeded.

## 3. Artifact contract, propagation, and persistence

Keep this existing Go shape and JSON names unchanged:

```go
llm.MediaArtifact{
    Reference, SourcePath, StoredPath, MediaType, Name, Caption string,
}
```

`Path`/`PublishedPath`, `NormalizeMedia`, and the copy paths are already kind-agnostic. Do not create `ToolAudio`, base64 LLM content, a new session part, or a database migration. Audio is presentation data, not automatic audio input to the model. Update image/video-only comments in `internal/llm/types.go`, `internal/ui/stream_event.go`, and `internal/tools/spawn_agent.go`.

Exercise, rather than mechanically edit, these generic owners:

- `internal/llm/types.go:ToolResultMessageFromOutput`; `engine.go`'s `ToolMedia` event copy; `tool_history_sanitize.go`'s cloned `Media` slice.
- `internal/ui/stream_adapter.go` media emission and `stream_event.go:MediaEvent`.
- `cmd/ask.go:streamPlainText`, `askStreamModel` media map and `renderMd`/`renderMdWithWidth`; `cmd/ask_progressive.go` bridge; `cmd/loop.go`'s `MediaMarkdownStream` buffering and final flush.
- `cmd/ask_json.go:emitStreamEvent`: continue emitting event `media` with `{reference,path,media_type,name,caption}`. Audio is not a new event type. Preserve `path` semantics and omission of empty-path events.
- `cmd/spawn_runner.go:SetMediaPublisher`, `currentMediaPublisher`, `setupAgentTools`, `subagentEventFromLLM`; `cmd/session.go:WireSpawnAgentRunnerWithStoreAndDepth`; `internal/tools/registry.go:SetMediaPublisher` including parent registries without their own `show_media`.
- `internal/tools/spawn_agent.go` plus `internal/tools/agent_registry.go:agentManager.output`/`deliver` and `internal/tools/agent_control.go:agentControlTool.wait`: retain nested `Media` in returned results, persisted child runs, detached collection and continuation; extend `TestSpawnAgentToolRetainsNestedMediaOnResult` and `agent_media_continuation_test.go` for audio. The manager copies `e.media` into `session.AgentRun.Media`; collection must not lose or redeliver it.
- `internal/subagentview/completed.go:Build`, `dedupeMedia`, `fingerprint`; `internal/render/chat/renderer.go:messagePartsSignature` already includes all current artifact fields.
- Persisted transcript visibility now lives in `internal/session/sqlite_messages.go:transcriptRowHasDisplayBody`, not the original `sqlite.go` location. Its `len(Media)>0` path is generic; test SQLite round-trip and reload.
- `internal/sidequestion/sidequestion.go:deepCopyMessages` preserves media; `truncateConversationPayloads` intentionally removes it. Keep that policy; do not accidentally feed recordings into sidequestions.

Change `internal/llm/compaction.go:compactToolResultSummary` to count audio separately, avoiding the present “non-video means image” assumption. Test artifact retention through the actual retained-tail/sanitize paths as well as summary text; do not promise that compaction materializes old files. Missing local-only source files must still produce graceful presentation failure after restart.

Web envelope stays `{reference,url,type,name,caption}` (`webMediaEntry` / frontend `MediaArtifact`). No source/stored filesystem paths reach browser JSON. Audio must pass both live and replay paths:

- `cmd/serve_media.go:toolMediaEntries`, `mediaEntriesFromValue`.
- `cmd/serve_response_runs.go:responseRunRecoveryTool.Media`; current split implementations in `serve_response_run_event_projection.go`, `serve_response_run_stream.go:appendResponseRunEvent`, `serve_response_run_recovery.go:applyRecoveryEventLocked`/`recoveryPayloadLocked`, `serve_response_run_recovery_events.go`, and `serve_response_run_manager.go:appendUniqueWebMedia`.
- `cmd/serve_handlers.go:sessionMessageEntries`, `appendSessionMessageToolResultPart`, and `appendSessionMessageToolResult` project persisted tool media via `toolMediaEntries`. Both recovered and fresh entries must use the same kind policy.

## 4. Authenticated HTTP audio serving

Use the shared registry for `serveMediaExtension` and `serveMediaTypeForName`, or remove redundant switches in favor of thin delegating wrappers. Canonical new extensions are `.mp3`, `.wav`, `.ogg`, `.flac`, `.m4a`, `.weba`. Do not remap existing `.mp4` or `.webm` stored objects to audio based only on their suffix.

`PublishMedia` must inspect the **completed temporary copy** with the new reader-based inspector, compare normalized MIME, and preserve bounded copy, context cancellation, same-size check, private modes, hashing, deduplication, sync and cleanup. This is especially important for tags or container metadata beyond the first 512 bytes. An audio file over 100 MiB must not enter the durable store under an audio MIME.

`handleMedia` should continue serving only owned allowlisted files through `serveResolvedFile`/`http.ServeContent`. Keep:

- exact canonical `Content-Type`, `Content-Disposition: inline`, `nosniff`;
- `Cache-Control: private, max-age=86400` and `Vary: Authorization, Cookie`;
- GET/HEAD only, authentication on the mounted route, no path traversal or symlink exposure;
- automatic `Accept-Ranges: bytes`, `206`, `Content-Range`, suffix/open-ended ranges and `416` from Go's server implementation.

Do not add a public audio route, token-in-query URLs, or cache audio in the shell service worker. Same-origin authenticated requests must work under custom base paths and Hub node rebasing. Download uses the same authenticated URL and a safe client `download` filename; no arbitrary `download=path` server parameter. Publication-time re-sniff is the trust boundary; do not add expensive request-time demuxing. If request-time inspection is added for another reason, inspect and serve the same open file rather than creating a new check/open race.

The response-local CSP set by `serveResolvedFile` protects arbitrary-file navigation; do not relax the shared arbitrary HTML/SVG sandbox to make audio work. Verify actual Safari playback and seeking with the existing response headers before considering a narrowly media-only header adjustment.

## 5. Web UI: native playback with a consistent audio card

### Projection and resolver changes

1. Add audio acceptance to `domain/transcript.ts:mediaArtifacts` and `domain/response.ts:responseMedia`; preserve caption/name/reference and URL rebasing. Reject unrelated MIME families.
2. Extend `MarkdownMediaTarget` to `type: 'image' | 'video' | 'audio'`, plus optional `name` and `caption` (and optionally MIME if needed for fallback copy; not required for native `src`). These fields come only from registered artifacts.
3. In `createTranscriptIndexes`, classify all three kinds explicitly, never default unknown media to image. Pass name/caption through. Update target-map equality and `createMessageMediaResolvers`' used-reference comparisons for displayed metadata so corrected captions update only affected rows. Keep stable identities when values did not change.
4. Extend `publishedMediaURL`'s extension allowlist, preserving same-origin/hash/query restrictions. Test `/ui`, a non-default base path, and Hub-node proxy prefixes.

### Rendering and interaction

In `domain/markdown.ts:mediaRenderer`, resolve an audio token to a shared audio-card structure with:

- descriptive Markdown label as accessible player name/title; fallback caption, filename, then “Audio recording” when needed;
- original filename on a secondary line; optional caption without duplicating the title;
- `<audio controls preload="metadata" aria-label="…">`, no `autoplay`, no forced looping;
- an explicit “Download audio” link using the registered URL and sanitized basename, available even when decoding fails;
- reserved modest player height to reduce transcript layout jumps. Native controls provide duration/current time, seeking and keyboard operation; no waveform library or custom range widget.

Use safe inline-compatible markup (e.g. a block-styled `span` card) if the Marked image renderer runs inside a paragraph. Do not emit invalid nested block/paragraph HTML and rely on browser repair. Escape text/attributes with `markdown-hooks.ts:escapeHTML`.

Add a small proposed `components/AudioMedia.tsx` for uploaded-attachment rendering and a shared domain card descriptor/class contract for the string-rendered Markdown branch. The attachment branch in `Transcript.tsx` should gain the same caption/name/download/metadata behavior. Do not force user-uploaded attachment URLs through `publishedMediaURL`: uploads have their own existing trusted URL path.

Add shared transcript-root media event handling (proposed small helper/hook) for **capture-phase `play`**, since native play does not bubble. Pause other audio players, preferably also currently playing transcript videos, only within this transcript; never auto-resume them. Native controls keep focus and keyboard behavior. Clean listeners up on unmount/session change, and pause detached players. Avoid global window handlers, unrelated page media, and lightbox interference.

`Markdown.tsx` must not intercept audio controls or audio download clicks for `onMedia`/video lightbox. Keep lightbox `onMedia` typed image/video; it does not need an audio mode. Explicitly skip audio-card links before `VIDEO_LINK_PATTERN` handling (notably `.ogg`). Rebase trusted audio/video `src` as well as image/link URLs in `rebaseRenderedAssetURLs`, without double-rebasing Hub URLs.

Playback state matters: preserve the existing per-message resolver/index caches. Test that unrelated incoming deltas do not replace a completed historical audio element. On intentional source replacement, session switch, or virtualized row removal, stop playback; do not promise background playback across unmounted transcript rows. Avoid adding duration state that rerenders the full message. If an error message is added on `error`, use an accessible status and keep download visible; do not announce every playback-time tick.

### Sanitizer policy

Do **not** merely add `audio`/`src` to a global allowlist. Raw model HTML such as `<audio src="https://tracker.example/x">`, `<source>`, data/blob audio, autoplay, and event handlers must not acquire permission through this feature.

Recommended concrete construction: during one `renderMarkdown` invocation, `mediaRenderer` records trusted resolved audio cards under collision-resistant per-render placeholders. Sanitize ordinary Markdown HTML with raw `audio`/`source` forbidden for chat; then replace only exact generated placeholders with separately escaped, allowlisted card markup built from validated resolver targets. The placeholder namespace must be per-render and unpredictable, not an attribute the model can forge. Never replace inside code/pre nodes. Alternatively use a scoped DOM pass with equivalent provenance guarantees; tests must prove the same property. Keep document rendering policy separate and avoid global DOMPurify hooks leaking between renders.

Final audio markup permits only the needed `src`, `controls`, `preload`, accessible labels, class and safe download-link attributes. Resolver URL validation must occur before construction, not only at the component caller. Reject `javascript:`, cross-origin, credential-bearing, query/hash-bearing and non-media routes. Existing ordinary image/video rendering is not permission to introduce arbitrary remote audio.

### Styling

Add `.audio-media-card` and related rules under `frontend/src/styles/integration/preact-overrides.css` (or a nearby existing chat-owned media stylesheet after following imports). Use existing color/border/text variables, restrained spacing, visible focus indicators, dark/light native `color-scheme`, `min-width: 0`, wrapping filename/caption and `audio { width: 100%; max-width: 100%; }`. Check 320px/430px mobile layouts and coarse pointer targets. No animation required. Do not add audio to image zoom buttons or lightbox galleries. Hub remains separate; no Hub assets in the chat service worker and no generated `internal/serveui/static/dist` edits.

## 6. Terminal/TUI/plain output

Update `internal/ui/media.go:RenderMediaLink` to choose Audio explicitly and render a compact two-line result, for example:

```text
♪ Audio: Listen to the summary — narration.mp3
Open file:///…/narration.mp3
```

The label/open target should use the existing OSC 8 `terminalFileHyperlink` and escaped `LocalFileURI`; retain the visible URI for non-OSC-8 terminals and piped output. Use `terminaltext.SanitizeSingleLine`/`Sanitize` for names, labels and captions. Avoid duplicate caption lines. Keep existing image/video output stable where practical.

`RenderMediaMarkdown` currently substitutes the Markdown alt into `item.Name`; preserve the real filename for audio by passing a distinct display label to a small helper or by using a local display structure, not by changing stored artifacts. No stat/read/codec parse on every render. Omit size and duration in v1 because `MediaArtifact` does not contain them; native web playback still supplies duration. A later optional metadata extension can add size/duration with explicit cache-signature and propagation work.

Change `MessageBlockRenderer.renderToolMedia` to invoke image rendering **only for `image/`** (including legacy `image/*`); all other supported kinds use links. Audio must never reach terminal graphics decoders. Exercise both active TUI and persisted history, completed subagent views, plain `ask`, progressive output, and `loop`. Keep `MediaMarkdownStream` split-token buffering and unavailable-reference behavior.

## 7. HTML export and sharing

### Bounded HTML audio embedding

Add proposed `internal/session/export_audio.go` with an export-scoped resolver/cache parallel to, not inside, the image normalizer. Re-sniff using the shared package, require an approved artifact's existing regular local/durable file, limit reads to a **2 MiB aggregate raw-audio budget**, and deduplicate repeated references to the same content/path within an export. Base64 adds approximately one third; account for actual final HTML size when constructing share requests. Never read files above the export cap merely to decide to omit them. Audio is not resized, transcoded or stripped of embedded metadata.

Introduce `ExportOptions.IncludeAudio bool`, default false. `ExportToHTML` callers that explicitly choose audio can produce `<audio controls preload="metadata">` using generated `data:audio/<allowlisted-type>;base64,…` URLs. Add explicit `--include-audio` wiring to the existing session export/share CLI owners (`cmd/sessions.go`, `cmd/sessions_share.go`), and explain the default. The zero-value options remain privacy-preserving and backward-compatible.

Extend `htmlExportBlock` with a typed audio block (proposed `htmlExportAudio`), render it through `export_html.tmpl`, and add `media-src data:` to the export CSP only where required. Use `html/template` escaping; permit `template.URL` only for URLs the trusted exporter generated from validated bytes. Keep Goldmark safe mode. Use a Goldmark image-node transform/renderer for registered references (preferred), or a code-fence-aware block splitter; do not enable unsafe HTML or regex-inject arbitrary HTML into Markdown. Preserve placement between surrounding prose and keep literal references inside code unchanged.

Keep `replaceHTMLMediaReferences` for Markdown export/image resolution or split its responsibilities deliberately: audio must become an Audio placeholder in Markdown and a typed player only in HTML. Video remains a Video placeholder; do not opportunistically implement video export. Missing, invalid, excluded and over-budget audio get a human-readable reason without `SourcePath`, `StoredPath`, private server URL or unresolved `term-llm-media://` leakage. Do not embed local `file://` or authenticated `/media/` URLs in shared exports.

### Explicit sharing consent; no protocol expansion

`ShareBundle`/`ShareFiles` can carry the bounded inline player in `index.html` only when `IncludeAudio` is true. `session.md` gets a descriptive placeholder, not giant base64 content. Leave `AssetImageMediaTypes`, image normalization, provider capability validation, and binary asset policies unchanged. Audio sidecars and large recording shares are non-goals for this release; users can download the original through the authenticated UI.

Wire opt-in consistently:

- `cmd/serve_share.go` request: additive optional `include_audio` (default false), forwarded to `ExportOptions.IncludeAudio`.
- `frontend/src/components/ShareModal.tsx`: separate **Include audio** checkbox, default off; warn that recordings and embedded metadata are copied, without claiming image-style metadata removal. Keep existing Include images semantics unchanged.
- `cmd/sessions_share.go`: explicit `--include-audio` and correct option forwarding for initial create/update flows. Existing `--no-images` continues to affect images only; audio still needs its own consent.
- Existing TUI/share shortcuts that do not expose this new choice remain audio-excluding; their output should clearly state that audio is omitted and direct users to the explicit CLI/Web share option. Do not silently widen a previously configured share on update.
- `SelectShare`/`StripToolActivity` already retain generic `selection.Media`; test response-only, conversation-prefix, and no-tool shares with audio-producing tool rows removed.

Inspect provider final-size validation before transmission; stay below the existing 16 MiB/file and 32 MiB/request limits. The 2 MiB audio budget plus the existing 2 MiB inline image budget normally leaves headroom, but text can still exceed limits and must use existing errors rather than an unbounded fallback.

## 8. Telegram delivery

Refactor `telegramPresentation.deliverMedia` into small upload-selection/open/send/fallback helpers to avoid worsening its complexity. Keep existing referenced-order/dedup behavior and the **50 MiB** `telegramMaxMediaUploadBytes` cap, which is intentionally lower than the tool's audio cap.

Dispatch policy:

- `audio/mpeg` and `audio/mp4`: `tgbotapi.NewAudio` / Telegram `sendAudio`; caption from the artifact and title from a safe name/caption fallback.
- `audio/ogg`: inspect the bounded identification packet again; **Opus only** uses `tgbotapi.NewVoice` / `sendVoice`. Vorbis uses `sendDocument`. Do not infer Opus from `.ogg` or MIME alone.
- WAV, FLAC and audio WebM: `sendDocument` directly, with correct extension/name. No transcoding or claim of native Telegram music playback.
- If native audio/voice delivery fails, reopen the original file and attempt document fallback exactly as the video path does. Ensure handles close on success, failure and fallback. Do not reuse a consumed reader.

Extend `telegramMediaFilename` with all canonical audio extensions; prefer a sanitized basename where safe so users receive a meaningful download name. Fix oversize fallback label (currently “video”) and `extractToolResultTextWithPlaceholders` to say audio. Keep captions as plain text unless explicitly escaped for a configured parse mode; respect Telegram caption limits. Errors should be actionable without posting local paths or private serve URLs into chat. Use existing bot sender fakes, not a real bot or network.

## 9. Prefix-switch audit: change versus intentionally retain

The repository-wide Go/TS/TSX search for `image/`, `video/`, `HasPrefix`, and `startsWith` found these presentation decisions:

| Current owner | Planned disposition |
|---|---|
| `tools/show_media.go:Execute` | Replace image/video size branch with shared format kind/limit. |
| `cmd/serve_media.go:toolMediaEntries`, `mediaEntriesFromValue` | Accept allowlisted audio along with existing kinds. |
| `ui/media.go:RenderMediaMarkdown` | Image-only graphics guard is correct; preserve it. |
| `ui/media.go:RenderMediaLink` | Add explicit audio kind/label. |
| `render/chat/message_block.go:renderToolMedia` | Stop treating non-video as image. |
| `llm/compaction.go:compactToolResultSummary` | Separate audio count. |
| `serve/telegram_presentation.go:deliverMedia` | Separate audio/voice/document dispatch; image guard remains image-only. |
| `serve/telegram.go:extractToolResultTextWithPlaceholders` | Separate audio placeholder. |
| `session/export_html.go:replaceHTMLMediaReferences` | Audio label plus separate typed HTML audio path. |
| `session/export_images.go:isExportImageMediaType` | Remain image-only; use new audio resolver. |
| `frontend/domain/transcript.ts:mediaArtifacts`, `response.ts:responseMedia` | Accept audio projection. |
| `frontend/domain/transcript-indexes.ts:createTranscriptIndexes` | Explicit three-kind classification and metadata-aware equality. |
| `frontend/components/Transcript.tsx` attachment projection | Keep image/video branches and upgrade existing audio branch. |
| `frontend/components/Composer.tsx`, `internal/tui/chat/image_attachments.go` | Image preview/attachment-specific behavior, not show_media; leave unchanged. |
| `internal/clipboard/clipboard.go`, `internal/image/venice.go`, Telegram photo download | Image-only ingress/provider checks, not media presentation; leave unchanged. |
| `internal/llm/file_mime.go` | Already recognizes `audio/` as a family for file input; do not conflate file-input MIME detection with this conservative publication allowlist. |

Also audit non-prefix switches: `MarkdownMediaTarget`, URL extension regex, `VIDEO_LINK_PATTERN`, MIME-to-extension mappings, CSP, template block kinds and media labels. Re-run this search during implementation to catch drift and record newly discovered owners rather than blanket-replacing image checks.

## 10. Tests and fixtures

### Go detection/tool tests

Extend `internal/tools/show_media_test.go` and add table-driven tests next to the proposed shared inspector. Use byte builders with `encoding/binary`, not committed binary samples:

- Valid existing image/video types, all accepted audio families, misleading filename extensions, M4A/M4B and compatible brands.
- Bare MPEG Layer III versions/bitrates and VBR consecutive frames; ID3 sizes/flags/versions, footer, tag crossing the initial probe, truncated/oversized tag, and offset bounds.
- Negative `0xff` followed by text, all-`0xff`, reserved version/layer/sample-rate/bitrate/emphasis, ADTS, incomplete second frame, random “ID3” text. No accidental `audio/mpeg` fallback from `http.DetectContentType`.
- RIFF/WAVE versus WEBP/AVI, padded chunks, bad lengths/fmt, supported PCM/float; Ogg Opus/Vorbis versus Theora/unknown/truncated lacing; FLAC STREAMINFO versus marker-only input.
- MP4 generic brand plus audio-only tracks, mixed audio/video, misleading embedded `soun` bytes, trailing `moov` after a large skipped `mdat`, extended/zero box lengths, overflow and scan cap. WebM audio-only/mixed/video, invalid VINT/DocType, unknown-size segment and bounded fallback behavior.
- Byte `Detect` versus reader `Inspect` semantics: if metadata is unavailable to a byte slice, document/test conservative fallback; tool and publisher must agree on the same complete file.
- Audio just below/at/above 100 MiB with sparse temporary files or bounded fake readers where possible, preserving image/video boundaries. Include publication-side independent rejection.
- Read permission denial, missing/nonregular source, unsupported input, publisher failure/empty path, clean caption, no legacy Images, correct model embedding instructions.
- Fuzz the shared audio/container inspectors for no panic, no unbounded allocation/work and no out-of-bounds read. Synthetic valid headers are identification fixtures, not claims of decoder-valid playable media.

### HTTP and pipeline tests

Extend `cmd/serve_media_test.go`'s three existing tests rather than replacing their protections. For every audio MIME verify deduped storage, extension/type round-trip, copied-file re-sniff mismatch, local source deletion after publication, HEAD and GET, ranges (first bytes, middle, suffix, open-ended, unsatisfiable), Content-Length/Content-Range/Accept-Ranges, private caching and `nosniff`.

Add mounted-router auth tests (direct `handleMedia` calls do not exercise auth), invalid methods, traversal, symlinks, unknown extensions, external paths and custom base paths. Test frontend/recovery envelopes do not expose local paths.

Add audio variants to JSON event, stream adapter/progressive/plain streaming, nested spawn, subagent view, SQLite round-trip, compaction and renderer tests. Use `internal/llm/mock_provider.go` and existing harnesses, never a live model. Assert order and reference preservation, not just `len(Media)>0`. Retain legacy image tests.

### Terminal tests

`internal/ui/media_test.go`: audio link label, actual filename plus contextual alt, caption sanitation, URI escaping/Windows paths, ANSI-stripped fallback, unknown reference, fenced examples and split-token streaming. Supply an image-renderer spy that fails if audio reaches it. `internal/render/chat/renderer_test.go` plus nearby TUI tests: loaded history and completed subagent audio render as links, including text mode; video/image behavior remains unchanged.

### HTML/share tests

Add `export_audio_test.go` and extend `export_html_test.go`, `export_images_test.go`, `share_export_test.go` and CLI/server share tests:

- Explicit opt-in produces typed audio controls, safe data MIME, CSP permission, label/filename/download; zero-value options and unselected share checkbox embed no audio bytes.
- Total budget/dedup, missing file, wrong magic, over-limit/changed file, escaped malicious caption/name, literal references in code, no private URL/path leakage.
- Image and video behavior unchanged; no audio passed through image decoding/normalization. Markdown contains a clear audio note, not base64/HTML.
- Response-only and stripped-tool shares resolve audio from `ExportOptions.Media`; opted-out audio never reappears via this side channel.
- `ShareFiles` and `ShareBundle` keep protocol limits and no audio sidecar assets; test consent forwarding, defaults, updates and image/audio choices independently.

### Telegram tests

Extend `telegram_markdown_test.go` reference-order test with audio; add focused `telegram_presentation_test.go` cases using `fakeBotSender` or a narrow recording fake. Assert `AudioConfig` for MP3/M4A, `VoiceConfig` only for confirmed Opus, `DocumentConfig` for Vorbis/WAV/FLAC/WebM, correct captions/names, native-send failure reopening a nonempty document stream, oversize message, missing file and close/failure paths. No real API calls or sleeps.

### Frontend tests and browser checks

- `frontend/src/domain/rendering.test.ts`: resolved audio card, escaped captions, no autoplay, controls/preload/label/download, unknown references, fenced/inline code, dangerous URLs, forged placeholders, raw audio/source/event attributes, protocol and cross-origin rejection.
- `transcript.test.ts` and `response.test.ts`: live, recovery and durable audio projection/rebasing, metadata retention, rejecting unrelated types.
- `transcript-indexes.test.ts` and `media-resolvers.test.ts`: audio type, stable identity under unrelated updates, caption/name invalidation, duplicate references and removal.
- `components/components.test.tsx`: uploaded and generated cards, download not intercepted by video lightbox (especially Ogg), play event pauses another player, cleanup/session switch, stream updates preserve historical audio DOM, error fallback, and separate share consent.
- DOM tests cannot prove codec playback. Use a tiny **synthesized valid PCM WAV** for actual Chromium/WebKit browser smoke: play/pause, finite metadata duration, seeking with Range, download, two-player exclusion, keyboard focus, narrow viewport and dark mode. Optional manually generated real MP3/M4A/Ogg/FLAC/WebM samples belong under ignored `tmp/`, not binary test fixtures in source. Confirm Safari/iOS native control behavior and codec fallback manually if CI lacks the needed codec; report that limitation.

## 11. Ordered implementation phases

1. **Shared recognition and contract.** Add bounded inspectors/format registry and tests; delegate existing tool wrappers, preserve image/video behavior, update `Spec` and limits. Deliverable: approved audio produces a typed artifact with unchanged Markdown contract.
2. **Durable transport and generic pipeline.** Replace duplicate server tables, inspect copied files, open projection filters; test auth/Range and JSON/recovery/subagent/persistence. Fix compaction labels. Deliverable: artifacts survive live/reloaded sessions without path leaks.
3. **Web audio card.** Extend resolver metadata and caches, enforce trusted-source sanitizer policy, add native controls/download, align upload rendering, scoped single-player policy and responsive styling. Deliverable: delightful player under streaming/reload/Hub routes with security tests.
4. **Terminal and Telegram.** Add honest audio links and guard image decoders; split Telegram upload dispatch with codec-aware voice handling and document fallback. Deliverable: no audio mislabelled as image/video on non-web surfaces.
5. **Export and explicit sharing.** Add bounded typed HTML audio blocks/CSP and opt-in CLI/Web/API consent; preserve image assets and video placeholders. Deliverable: safe portable short recordings without silently publishing private audio.
6. **Docs and integration verification.** Update descriptions/examples and limits, run complete checks and browser smoke, inspect final diff, confirm no generated assets or unrelated changes are staged. Re-run the prefix/enum/extension audit.

Each phase should land tests with its code. Do not “finish” by accepting detection-only support while audio is still dropped by projection or rendered as an image.

## 12. Documentation and original-feature touchpoint checklist

Update `docs-site/content/reference/built-in-tools.md` (contract/formats/limits/audio example), `guides/web-ui-and-api.md` (authenticated player, Range, upload/download, container/codec caveats and JSON shape), and `reference/sharing.md` (opt-in audio, 2 MiB inline budget, unstripped metadata, exclusions and Markdown fallback). Include Telegram's lower 50 MiB cap and terminal file-link-only behavior. Avoid “all audio plays everywhere” claims.

Inspect existing tool-enabled YAML and prose before editing: `internal/agents/builtin/{artist,codebase,developer,file-organizer,widget-builder,extension-builder}/agent.yaml` and `internal/contain/images/agent/bootstrap/bootstrap.yaml`. These currently list `show_media`; listing the tool does not require YAML churn. Update only image/video-only presentation descriptions/prompts that actually need broadening. Do not repurpose the visual artist agent into a TTS generator or change deny-list compatibility in `internal/agents/agent.go:GetEnabledTools`. Keep `internal/agents/agent_test.go` and `internal/contain/images_test.go` coverage. `cmd/flags.go` already lists show_media and needs no rename.

Original commit inspected: `a6fd6857` (“feat(media): add inline image and video display”), 63 touched files. Use this complete surface inventory as a regression checklist, not a mandate to modify all files:

- CLI/server: `cmd/{ask.go,ask_json.go,ask_progressive.go,flags.go,loop.go,serve.go,serve_handlers.go,serve_media.go,serve_media_test.go,serve_response_runs.go,session.go,spawn_runner.go}`. Follow current split files noted above.
- Frontend: `components/{Markdown,Transcript}.tsx`; `domain/{markdown,rendering.test,response,response.test,transcript,transcript.test,types}.ts`; `styles/integration/preact-overrides.css`. Current additional index/cache/share-modal owners are described above.
- Agent/container: `internal/agents/{agent.go,agent_test.go}`, the original five builtin YAML files (artist, codebase, developer, file-organizer, widget-builder), `internal/contain/images/agent/bootstrap/bootstrap.yaml`, `internal/contain/images_test.go`.
- Engine: `internal/llm/{compaction.go,engine.go,tool_history_sanitize.go,types.go,types_test.go}`.
- Terminal rendering: `internal/render/chat/{message_block.go,renderer.go,renderer_test.go}`, `internal/tui/chat/{chat.go,render.go}`, `internal/ui/{media.go,media_test.go,stream_adapter.go,stream_event.go}`.
- Telegram/UI embedding: `internal/serve/{telegram.go,telegram_markdown_test.go}`, `internal/serveui/embed_test.go`. Check bundle-size tests after building; do not automatically raise budgets.
- Session/subagents: `internal/session/{export_html.go,export_html_test.go,sqlite.go}`, `internal/sidequestion/sidequestion.go`, `internal/subagentview/completed.go`.
- Tools: `internal/tools/{registry.go,show_media.go,show_media_test.go,spawn_agent.go,spawn_agent_test.go,types.go}`. Approval policy is already read-path capable; audio must not widen it.
- Original documentation: built-in-tools and web-ui-and-api above; current sharing documentation is additionally in scope.
- Original `tmp/circle-5s.mp4` and `tmp/circle.png` are scratch/example assets, not a model for new committed audio fixtures.

## 13. Risks and explicit non-goals

- Parsing untrusted containers is the largest correctness/security risk. Keep bounded arithmetic and helpers, fuzz them, and distinguish incomplete metadata from malformed files. Classification is not complete codec validation.
- MP3 ID3 sizes and MPEG/ADTS sync collisions require structural validation. Do not “fix” rejected bytes by trusting filenames or a broad HTTP MIME fallback.
- Browser codec support varies, especially Safari/Ogg/WebM/FLAC. Prefer MP3, PCM WAV or AAC/M4A in examples and retain downloads/errors for other formats.
- Audio can contain private speech and ID3/other metadata. Sharing must be opt-in and must not claim to strip metadata. Ordinary show_media approval remains the prerequisite for local read/publication.
- DOM replacement during streaming can reset playback. Preserve target identity and test existing caches rather than adding global rerenders or timers.
- Single-file export grows by base64 expansion; budgets intentionally exclude large recordings. No public source-path or authenticated-URL fallback.
- Existing local-only sessions do not make source files durable; a moved/deleted local file may cease to play. Serve-owned storage is the durable path.
- No TTS generation, transcription, model audio input, microphone recording, waveform, equalizer, playlist/background playback, autoplay, terminal playback subprocess, ffmpeg dependency, duration estimation, raw ADTS, RF64/RIFX, arbitrary Ogg/Matroska, new binary audio share protocol, or video export expansion.

## 14. Verification commands and completion gate

For implementation, use the Go version in `go.mod` (currently `go 1.26.9`). Run narrow tests after each phase, then:

```sh
gofmt -w <changed-go-files>
npm --prefix frontend run format
npm --prefix frontend run format:check
npm --prefix frontend run lint
npm --prefix frontend run typecheck
npm --prefix frontend test
make frontend
make build
go test ./...
go vet ./...
make complexity
git diff --check
git diff --stat
git diff
```

Use an isolated temporary HOME/XDG directory for Go tests if needed while preserving `GOMODCACHE`; never mutate user configuration to pass tests. Node must be on PATH for Go-driven JS tests. Follow `.github/workflows/ci.yml` for relevant lifecycle coverage, including `scripts/check_frontend_network_policy.sh`, `scripts/browser_lifecycle_smoke.sh` when event/DOM lifecycle changes warrant it, and targeted race tests for changed shared state. No nested terminal/reflow module change is proposed, so nested-module checks are not required unless implementation actually touches them.

`make complexity` runs `go run ./cmd/complexity --check plans/go-complexity-baseline.json --threshold 30`. Prefer table-driven MIME metadata and small inspector/dispatch functions; do not raise the baseline to conceal new complexity. Never hand-edit generated `internal/serveui/static/dist`; build it with `make frontend`, and review only authored source changes. Report every command run, skipped browser/codec check and any remaining format limitation.

For this planning-only task, do not run builds/formatters that rewrite other repository files. Validate this plan's diff and run the read-only complexity check; implementation checks above are instructions for the implementer, not claims of completed feature verification.
