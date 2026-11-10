---
name: computer
description: >
  Isolated browser automation facade over the browser-backend.mjs MCP server.
  Provides real Playwright-backed click, type, observe, wait, screenshot, and
  navigate operations on a locally-owned browser session. Requires the
  "computer" MCP server to be connected before the cell runs (see
  testdata/browser-backend.mjs). All tool calls go through the host's admitted
  tool dispatch; this module holds no browser authority itself.
code-mode:
  module: js/index.mjs
  files:
    - js/index.mjs
    - js/util.mjs
  requires:
    tools:
      - computer__click
      - computer__type
      - computer__observe
      - computer__wait
      - computer__screenshot
      - computer__navigate
---

## Computer skill

This skill exposes a minimal ES module facade for real isolated browser
automation backed by the `browser-backend.mjs` MCP stdio server.

### Quick start

```javascript
// Two equivalent import patterns
const { computer } = await import('skill:computer');
// or: const { click, screenshot } = await import('skill:computer');

// Navigate to a local test page using a data: URL (no external requests)
await computer.navigate('data:text/html,<h1 id="h">Hello</h1><button id="b">Click me</button>');

// Wait for a local element to appear
await computer.wait({ selector: '#h', timeout: 5000 });

// Observe visible interactive elements
const page = await computer.observe({ selector: '#h, #b' });
text(`URL: ${page.url}\nTitle: ${page.title}\nElements: ${page.elements.length}`);

// Click the button (use observed bounds to get coordinates)
const btn = page.elements.find(e => e.tag === 'button');
if (btn) {
  await computer.click(btn.bounds.x + Math.floor(btn.bounds.width / 2),
                        btn.bounds.y + Math.floor(btn.bounds.height / 2));
}

// Take a real screenshot — shot.image has { type, mimeType, handle }
// Pass it directly to image(); do NOT use shot.data or reconstruct the block.
const shot = await computer.screenshot();
image(shot.image);

// Store session metadata for the next cell
store('session', { sessionId: shot.sessionId, dims: shot.dimensions });
```

### IMAGE HANDLE CONTRACT

`screenshot()` returns `{ image, dimensions, sessionId }`.

`image` is an MCP image content block with a **cell-scoped handle**, not raw
base64 data:

```javascript
// Correct — handle preserved by the host
const shot = await computer.screenshot();
image(shot.image);  // { type: 'image', mimeType: 'image/png', handle: 'cell-image:...' }

// WRONG — data is NOT available on the returned block
image({ type: 'image', mimeType: 'image/png', data: shot.data });  // shot.data is undefined
```

The `structuredContent` field returned by the raw tool call may contain the
original base64 `data`, but it is not surfaced by this facade and may be
absent for large screenshots. Use the `handle`-based block from `shot.image`.

Each screenshot has a NEW `frameId` identifying that capture, not a stable browser/frame session. Do not assert frameId equality across screenshots; assert only `sessionId` equality. Store frameId only as observation metadata.

### Session continuity

The browser session persists inside the `browser-backend.mjs` process for the
lifetime of that MCP connection. Pass `load('session')` between cells to carry
forward session metadata. The backend serializes all actions; there is no need
to coordinate concurrent callers inside a single cell.

### Error handling and cancellation

Failed actions return `{ isError: true }` or throw. When `uncertain` is set in
the `structuredContent` or `text` result, the action outcome is unknown (the
lane was quarantined after a cancellation). Do not automatically retry: call
`observe()` or `screenshot()` to inspect actual page state first (reconciliation
calls are allowed during quarantine).

### Module structure

```
js/index.mjs  — entry point; exports named functions and `computer` object
js/util.mjs   — shared content-extraction helpers (extractImageBlock, extractText, etc.)
```
