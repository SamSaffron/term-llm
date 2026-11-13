# Actual Safari regression smoke

This is a focused fixture, **not the full application**. It bundles the production
`DiffSidebar`/`DiffAction`, clipboard helpers, `Composer`, `ToggleSwitch`, and their
CSS. Only the store's provider/server boundaries are replaced. The fixture is not
included in the shipped chat or Hub entry points.

## Build and run

From the repository root with the locked frontend dependencies installed:

```sh
(cd frontend && npx vite build --config e2e/safari/vite.config.ts)
```

Transfer `tmp/safari-smoke/` and `scripts/safari_smoke.py` to the Mac if building
elsewhere. The guest needs only Python 3.9+ stdlib and the native Safari driver;
no Node, Selenium, Playwright, or running term-llm service is required there.

On a Mac already authorized for Safari Remote Automation, start a **task-owned**
driver in a separate terminal:

```sh
/usr/bin/safaridriver --port 18969
```

Then, from the directory containing the transferred artifacts:

```sh
python3 safari_smoke.py --fixture-dir ./safari-smoke \
  --driver-url http://127.0.0.1:18969 --verify-pasteboard
```

The runner starts a temporary loopback server on an available port, creates its
own Safari session, and closes both in `finally`. Stop the task-owned driver
when finished. It does not enable remote automation, change authorization or
input sources, install anything, or start permanent services. It deliberately
**overwrites the clipboard** with known fixture text. `--verify-pasteboard` uses
`pbpaste` to check exact bytes and requires the same desktop user/pasteboard as
Safari. Without it, success means the native clipboard write promise fulfilled,
not independently verified clipboard contents. Preserve the emitted JSON and
record the macOS/Safari versions. An existing compatible fixture server can be
selected with `--fixture-url` instead of `--fixture-dir`.

## Automated checks

- Trusted W3C element click on a direct text copy, as a positive control.
- Trusted click on the production unloaded-file copy action, with its diff
  response delayed **six seconds**, followed by the production copied feedback.
  Optional exact pasteboard checks verify both sentinel and complete patch bytes.
- Actual mouse pointer actions at all four production toggle wrapper corners,
  protecting the explicitly sized invisible checkbox hit target.
- Synthetic Safari-shaped `keydown` events (`isComposing: false`, `keyCode: 229`)
  against the production composer: draft retained, no send, completion highlight
  and menu unchanged for arrows/choose/dismiss keys. Real WebDriver Enter then
  sends/clears the draft or picks a completion normally.

The synthetic keyboard checks are **not a native OS IME reproduction**. Local
Chromium runs can check fixture wiring but do not prove Safari compatibility.
Playwright's Linux WebKit is also not shipped macOS Safari.

## Manual checks on an authorized test Mac

Do not change OS input-source or microphone settings without the test owner's
approval. Record any unavailable prerequisite as untested.

1. In the fixture and a full locally built app, use an already configured Japanese
   Hiragana (or another CJK) input source. Type a conversion and confirm it with
   Enter. Confirm the draft stays intact and is not sent. Repeat while `/` or `@`
   completions are visible; candidates must not be selected or navigated by the
   composition-confirming key. A subsequent non-composing Enter should work.
2. In the full app, copy an unloaded collapsed diff, an already loaded diff, and
   a path. Check pasted bytes, no duplicate fetch for loaded data, and copied
   feedback. Image/truncated/failed diffs must not overwrite existing clipboard
   content with an empty string, URL, error, or incomplete patch.
3. For a configured PCM voice backend, start from a cold/new chat and delayed
   startup; grant or deny microphone permission, cancel while permission/resume
   is pending, then retry. The mic must stop on cancel/failure. Confirm audio can
   start after capture unlocks it. Unit tests model parked/rejected early resumes
   and cancellation deterministically; they do **not** prove Safari hardware,
   autoplay, permission UI, or audio-route behavior.

Notification timing and overlay focus-return suspicions remain deferred; this
smoke does not cover those, iOS, Hub/passkeys, service workers, or full workflows.
