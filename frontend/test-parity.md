# Web UI test and behavior parity

This checklist was created at the legacy baseline `d9887abd2fe5f4f710176469b336e3da04d3c489` before the cutover. The final merge-base is recorded in `payload-report.md`. The rewrite keeps the HTTP contracts and visual CSS while assigning every legacy production module and test group to an authored TypeScript/Preact owner.

## Production module disposition

| Legacy module(s) | Protected behavior | New owner and coverage |
|---|---|---|
| `app-core.js`, `toast.js` | injected config, scoped preferences, shell state, routing, connectivity, startup, clipboard, push, lightbox, toasts | `src/app/config.ts`, `src/stores/app-store.ts`, `src/platform/{storage,routing,browser}.ts`, Preact `App`/`Lightbox`; `config.test.ts`, component and Playwright shell tests |
| `app-network.js` | authenticated API policy, retry/recovery, UI-version guard, provider loading | `src/api/client.ts`, `src/api/endpoints.ts`; `client.test.ts`, Playwright startup/auth routing |
| `transcript-window.js`, `conversation.js` | conversion, transcript windowing, optimistic/durable handoff, stable session ownership | `src/domain/transcript.ts`, `AppStore`; `transcript.test.ts`, response and browser streaming tests |
| `active-response.js`, `app-stream.js`, `app-response-effects.js` | all `response.*` events, epochs/sequences, SSE reconnect/cancel/recovery and prompts/effects | `src/domain/response.ts`, `src/api/client.ts`, `AppStore`; `response.test.ts`, `client.test.ts`, Playwright send/stream |
| `markdown-setup.js`, `markdown-streaming.js`, `decoration.js`, `guardian-render.js` | marked configuration, DOMPurify policy, stable streaming boundary, strict math delimiters, lazy highlight/KaTeX, media and guardian rendering | `src/domain/markdown.ts`, Preact `Markdown`/`Transcript`/`Lightbox`; `rendering.test.ts`, component tests |
| `app-render.js`, `app-message-convert.js` | every transcript row, tools, attachments, compaction, model swap, skills, usage, copy | Preact `Transcript.tsx`, `Markdown.tsx`, `domain/transcript.ts`; transcript/component/Playwright tests |
| `app-plan.js` | plan state, progress and responsive panel | Preact `PlanSurface`; response reducer and component/browser coverage |
| `slash-commands.js`, `app-composer.js` | slash/mention completion, textarea keys, voice/transcription, send/interject controls | `domain/completions.ts`, Preact `Composer`, `platform/voice.ts`; rendering/component/browser tests |
| `app-attachments.js` | files/drop/media data URLs, dimensions and reserved image geometry | `AppStore.addAttachments`, Preact `Composer`/`Transcript`; transcript and component/browser coverage |
| `app-send.js`, `app-interject.js` | optimistic send, runtime payload, stable IDs, cancellation, compact, interrupt/interjection | `AppStore.send/cancel/interject`, endpoint module; response and Playwright send tests |
| `app-runtime.js` | provider/model/effort/reasoning/agent selection and per-session runtime updates | `AppStore` plus Preact `Header`/`Settings`; Playwright settings coverage |
| `app-modals.js`, `side-question.js` | ask-user, approvals, focus/dismiss, side-question history and continuation | Preact `Modals`/`SideQuestion`, response/store actions; reducer/component coverage |
| `app-skills.js` | skill discovery/invocation/run status | typed endpoint ownership and `skill-run` transcript rows; endpoint/server tests retained |
| `app-project-picker.js`, `app-sidebar.js` | projects/groups/search/filtering, widgets, Hub links, stable sidebar/session state | `AppStore` and Preact `Sidebar`; config/component/desktop/mobile Playwright tests |
| `app-sessions.js`, `intent-storage.js`, `app-session-events.js` | session loading/switching, URL history, cross-tab intent catch-up and lifecycle recovery | `AppStore`, `platform/storage.ts`, `platform/routing.ts`; storage/transcript/Playwright tests |
| `app-session-admin.js` | rename/AI title/pin/hide/unhide/delete semantics | `AppStore` and Preact session menus/rename modal; backend endpoint tests retained |
| `app-path-notes.js`, `app-branching.js`, `app-branch-commands.js` | branch tree, context choices and path-note contracts | `AppStore` branch actions and Preact branch modal; backend branch tests retained |
| `app-mcp.js`, `app-goals-location.js` | MCP toggles/status, persistent goals, pause/resume/clear and geolocation | `AppStore` and Preact MCP/goal/composer surfaces; endpoint tests retained |
| `app-diff-scopes.js`, `app-diff-context.js`, `app-diff-comments.js`, `app-diff-queue.js`, `app-diffs.js` | scopes, file/hunk rendering, inline comment context/queue/send, filtering/maximise | `AppStore` diff domain and Preact `DiffSidebar`; Playwright diff flow plus backend tests |
| `app-worktrees.js` | list/create/select project worktrees and runtime binding | `AppStore`, Preact worktree modal; backend worktree tests retained |
| `app-webrtc.js` | optional signaling/data-channel transport, HTTPS fallback, abort/cancel, watchdog and renegotiation | ESM `platform/webrtc.ts`, loaded only after deferred config parsing; existing frame protocol preserved, Go optional-asset tests and browser/WebRTC environment smoke |

All listed legacy files are deleted at cutover. There is no `legacy-bridge.ts`, global application namespace, authored application IIFE, or second DOM owner.

## Legacy test disposition

| Legacy test group | Disposition |
|---|---|
| `app_core_test.js`, `chip_picker_test.js`, `toast_test.js` | Ported to config/storage, Preact component, and browser settings/shell tests. Source line ratchets retired in favor of raw+gzip production bundle budgets in `embed_test.go`. |
| `app_network_test.js` | Ported to `api/client.test.ts`; source network policy replaced by `scripts/check_frontend_network_policy.sh`. |
| `conversation_test.js`, `transcript_window_test.js`, `app_sessions_test.js` | Pure conversion/window/handoff cases ported to `domain/transcript.test.ts`; lifecycle/session switching moved to Playwright. |
| `app_stream_test.js` | Event inventory, ordering, tool/guardian and terminal cases ported to `domain/response.test.ts`; SSE fragmentation/reconnect layer in `api/client.test.ts`; public send flow in Playwright. |
| `markdown_test.js`, `markdown_streaming_test.js`, `decoration_test.js` | Ported to `domain/rendering.test.ts` and Preact transcript tests. |
| `app_render_test.js`, `app_plan_test.js` | Ported to Preact component tests and response-domain tests. |
| `mention_completions_test.js`, `slash_commands_test.js` | Ported to `domain/rendering.test.ts` and Composer component tests. |
| `side_question_test.js` and `side_question_test.go` wrapper | Replaced by Preact side-question surface/store ownership; endpoint behavior remains covered by Go server tests. |
| `app_diff_comments_test.js`, `app_diff_queue_test.js`, `app_diffs_test.js` | Replaced by public Playwright diff/inline-comment flow and retained Go endpoint tests. |
| `app_sidebar_test.js`, `app_worktrees_test.js` | Replaced by Preact/sidebar browser flows and retained project/worktree Go endpoint tests. |
| `app_webrtc_test.js` | Transport implementation retained as an ESM platform bridge; Go optional chunk/MIME tests plus environment WebRTC smoke. |
| `sw_test.js` | Rewritten as Go `RenderServiceWorker` cache/version/optional-asset tests. |
| `internal/serveui/browser_lifecycle.spec.js` | Internal global-driven scenarios replaced by public-UI Playwright load/navigation/settings/send/stream/mobile/diff flows. The explicitly gated credential-free bridge is used only for otherwise inaccessible reducer inspection. |
| `internal/serveui/embed_test.go` legacy source-grep/line-count/asset tests | Rewritten for generated bundle policy, raw+gzip budgets, module ordering, embedded chunks and service-worker rendering. |
| `cmd/serve_test.go` asset cases and `serve_webrtc_test.go` script cases | Updated to deterministic `dist` assets, module MIME, SPA error behavior and bundled WebRTC chunk. |
| Hub passkey/recovery browser specs | Retained unchanged; they cover the separate Hub templates. |

## Review searches

Final verification rejects production `TermLLMApp`, stale legacy asset names, `legacy-bridge`, source maps, test output under `static`, and raw transport calls outside `frontend/src/api/**` plus `frontend/src/platform/webrtc.ts`. CSS selector IDs/classes intentionally remain where the preserved stylesheet requires them.
