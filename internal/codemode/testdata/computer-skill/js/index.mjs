/**
 * computer skill — ES module facade for isolated browser automation.
 *
 * Wraps the five browser tools exported by browser-backend.mjs
 * (MCP server name "computer") through the host's admitted tool dispatch.
 * The module holds no browser authority; all calls are subject to the cell's
 * capability set and normal approval / accounting policy.
 *
 * MCP server name: computer
 * Canonical tool identities (after namespacing):
 *   computer__click  computer__type  computer__observe
 *   computer__wait   computer__screenshot  computer__navigate
 *
 * Exports (named):
 *   click, type, observe, wait, screenshot, navigate
 * Bundled export (named "computer" object):
 *   computer — { click, type, observe, wait, screenshot, navigate }
 *
 * Usage:
 *   const { computer } = await import('skill:computer');
 *   await computer.navigate('data:text/html,<h1>Hello</h1>');
 *   const shot = await computer.screenshot();
 *   image(shot.image);  // shot.image has handle, not data
 *
 *   // or with named imports:
 *   const { click, screenshot } = await import('skill:computer');
 *
 * IMAGE HANDLE CONTRACT:
 *   screenshot() returns { image, dimensions, sessionId }.
 *   image is the first image content block from the MCP result:
 *     { type: 'image', mimeType: 'image/png', handle: 'cell-image:...' }
 *   Pass it directly to the cell's image() emitter:
 *     image(shot.image);
 *   Do NOT reconstruct from base64 data or modify the block.
 *
 * All exported functions are async. Errors propagate as rejected Promises.
 * Quarantined actions (after cancellation) resolve with { uncertain: true }.
 */

"use strict";

import { extractImageBlock, metadata } from "./util.mjs";

// ---------------------------------------------------------------------------
// click
// ---------------------------------------------------------------------------

/**
 * Click at viewport coordinates (x, y).
 *
 * Uses page.mouse.click() — operates at top-level viewport coordinates
 * regardless of which frame is focused. The frameId parameter is accepted
 * for API consistency but ignored.
 *
 * @param {number} x - Horizontal coordinate in CSS pixels.
 * @param {number} y - Vertical coordinate in CSS pixels.
 * @param {object} [opts]
 * @param {"left"|"right"|"middle"} [opts.button="left"]
 * @param {number} [opts.clickCount=1] - 1 (single), 2 (double), or 3 (triple).
 * @returns {Promise<{ok:boolean, action:string, coords:{x:number,y:number},
 *   button:string, clickCount:number, sessionId:string, isError?:boolean}>}
 */
export async function click(x, y, opts = {}) {
  if (typeof x !== "number" || typeof y !== "number") {
    throw new TypeError("computer.click: x and y must be numbers");
  }
  return metadata(await tools["computer__click"]({ x, y, ...opts }));
}

// ---------------------------------------------------------------------------
// type
// ---------------------------------------------------------------------------

/**
 * Type text via keyboard input at the current page focus.
 *
 * Uses page.keyboard.type() — operates at page level. Click an element
 * first to focus it.
 *
 * @param {string} text - Text to type (max 4096 characters).
 * @param {object} [opts]
 * @param {number} [opts.delay=0] - Delay between keystrokes in ms (0–500).
 * @returns {Promise<{ok:boolean, action:string, length:number, sessionId:string,
 *   isError?:boolean}>}
 */
export async function type(text, opts = {}) {
  if (typeof text !== "string") {
    throw new TypeError("computer.type: text must be a string");
  }
  return metadata(await tools["computer__type"]({ text, ...opts }));
}

// ---------------------------------------------------------------------------
// observe
// ---------------------------------------------------------------------------

/**
 * Observe visible interactive elements on the current page.
 *
 * Returns URL, title, sessionId, and an array of element descriptors
 * (role, name, tag, bounding box).
 *
 * @param {object} [opts]
 * @param {string} [opts.selector] - CSS selector filter (optional).
 * @param {number} [opts.maxElements=64] - Max elements to return (1–128).
 * @param {string} [opts.frameId] - Frame handle for sub-frame targeting.
 * @returns {Promise<{url:string, title:string, sessionId:string,
 *   elements:Array<{role:string, name:string, tag:string,
 *     bounds:{x:number,y:number,width:number,height:number}}>,
 *   isError?:boolean}>}
 */
export async function observe(opts = {}) {
  return metadata(await tools["computer__observe"](opts));
}

// ---------------------------------------------------------------------------
// wait
// ---------------------------------------------------------------------------

/**
 * Wait for a CSS selector to become visible, or for a fixed delay.
 *
 * @param {object} [opts]
 * @param {string} [opts.selector] - CSS selector to wait for.
 * @param {number} [opts.ms] - Milliseconds to wait (used when no selector).
 * @param {number} [opts.timeout=5000] - Maximum wait in ms (100–30000).
 * @param {string} [opts.frameId] - Frame handle for sub-frame selector.
 * @returns {Promise<{ok:boolean, waited:string, sessionId:string, isError?:boolean}>}
 */
export async function wait(opts = {}) {
  return metadata(await tools["computer__wait"](opts));
}

// ---------------------------------------------------------------------------
// screenshot
// ---------------------------------------------------------------------------

/**
 * Take a screenshot and return an image block ready for the cell's image()
 * emitter, plus viewport dimensions and session metadata.
 *
 * The .image field is the first image content block from the MCP result.
 * After engine projection it has `handle` (not `data`):
 *   { type: 'image', mimeType: 'image/png', handle: 'cell-image:...' }
 *
 * Example:
 *   const shot = await computer.screenshot();
 *   image(shot.image);  // emit to model and UI — handle preserved
 *   store('dims', shot.dimensions);
 *
 * Do NOT reconstruct image() calls from structuredContent.data:
 * it may be absent for large screenshots or stripped by the host.
 *
 * @param {object} [opts]
 * @param {boolean} [opts.fullPage=false] - Capture the full scrollable page.
 * @param {string} [opts.frameId] - Frame handle for sub-frame capture.
 * @returns {Promise<{
 *   image: {type:'image', mimeType:string, handle:string} | null,
 *   dimensions: {width:number, height:number} | undefined,
 *   sessionId: string | undefined,
 *   isError?: boolean
 * }>}
 */
export async function screenshot(opts = {}) {
  const result = await tools["computer__screenshot"](opts);
  // After engine projection:
  //   result.content[0] = { type:'image', mimeType:'image/png', handle:'cell-image:...' }
  //   result.content[1] = { type:'text', text:'{"dimensions":...,"sessionId":"..."}' }
  //   result.structuredContent = { mimeType, data, dimensions, sessionId }
  // Use extractImageBlock to get the handle-bearing block.
  const imageBlock = extractImageBlock(result);
  const meta = metadata(result);
  return {
    image: imageBlock, // pass to image() — do NOT access .data
    dimensions: meta.dimensions,
    sessionId: meta.sessionId,
    frameId: meta.frameId,
    coordinateSpace: meta.coordinateSpace,
    crop: meta.crop,
    scale: meta.scale,
  };
}

// ---------------------------------------------------------------------------
// navigate
// ---------------------------------------------------------------------------

/**
 * Navigate the browser to a URL and wait for domcontentloaded.
 *
 * The backend restricts navigation to: about:, data:,
 * http://localhost:, http://127.0.0.1:, https://localhost:,
 * https://127.0.0.1:. External URLs are rejected by the backend.
 *
 * @param {string} url - Target URL.
 * @param {object} [opts]
 * @param {number} [opts.timeout=10000] - Navigation timeout in ms (max 30000).
 * @returns {Promise<{ok:boolean, url:string, sessionId:string, isError?:boolean}>}
 */
export async function navigate(url, opts = {}) {
  if (typeof url !== "string") {
    throw new TypeError("computer.navigate: url must be a string");
  }
  return metadata(await tools["computer__navigate"]({ url, ...opts }));
}

// ---------------------------------------------------------------------------
// Bundled export
// ---------------------------------------------------------------------------

/**
 * computer — bundled object export for destructured imports.
 *
 * Both usage patterns work:
 *   const { computer } = await import('skill:computer');
 *   computer.click(100, 200);
 *
 *   const { click, screenshot } = await import('skill:computer');
 *   click(100, 200);
 */
export const computer = { click, type, observe, wait, screenshot, navigate };
