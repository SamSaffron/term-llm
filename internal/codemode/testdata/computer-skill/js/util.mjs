/**
 * util.mjs — Shared content-extraction helpers for code-mode cells.
 *
 * These utilities normalise the two shapes a tool call can return:
 *
 * MCP tool (via programmatic envelope):
 *   { content: [{type,…}], structuredContent?, isError? }
 *   Image blocks in content arrive with `handle` (not `data`) after engine
 *   projection. Pass them directly to image() to emit.
 *
 * Native host tool (non-MCP):
 *   { text, isError, timedOut, content? }
 *   The `content` array may include image blocks with `handle` as above.
 *
 * IMPORTANT: image blocks returned by tool calls always have `handle`, not
 * raw base64 `data`. Do NOT reconstruct image()/content blocks from
 * structuredContent.data — the structuredContent field is for structured
 * metadata only and may be absent or oversized.
 */

"use strict";

/**
 * Return the first image content block from a tool call result.
 *
 * After engine projection, image blocks have:
 *   { type: 'image', mimeType: 'image/png', handle: 'cell-image:...' }
 * Pass the returned block directly to the cell's image() emitter.
 *
 * @param {object} result - Raw tool call result.
 * @returns {{ type: 'image', mimeType: string, handle: string } | null}
 */
export function extractImageBlock(result) {
  const blocks = result?.content;
  if (!Array.isArray(blocks)) return null;
  for (const b of blocks) {
    if (b?.type === "image") return b;
  }
  return null;
}

/**
 * Return the text string from the first text content block of a tool result.
 *
 * Returns the raw string — does NOT JSON.parse it. Many tools serialize their
 * result JSON as the text value; callers must parse explicitly if needed.
 *
 * Falls back to result.text for native tool results.
 *
 * @param {object} result - Raw tool call result.
 * @returns {string | null}
 */
export function extractText(result) {
  const blocks = result?.content;
  if (Array.isArray(blocks)) {
    for (const b of blocks) {
      if (b?.type === "text" && typeof b.text === "string") return b.text;
    }
  }
  // Native tool result fallback
  if (typeof result?.text === "string") return result.text;
  return null;
}

/**
 * Return all text content blocks joined by newline.
 *
 * @param {object} result - Raw tool call result.
 * @returns {string}
 */
export function extractAllText(result) {
  const blocks = result?.content;
  if (!Array.isArray(blocks)) return result?.text ?? "";
  return blocks
    .filter((b) => b?.type === "text" && typeof b.text === "string")
    .map((b) => b.text)
    .join("\n");
}

/**
 * Safely parse the structured content from a tool result without throwing.
 *
 * Prefers result.structuredContent when present. Falls back to JSON.parse
 * of the first text block ONLY when structuredContent is absent AND the
 * caller passes { parseFallback: true }.
 *
 * @param {object} result - Raw tool call result.
 * @param {{ parseFallback?: boolean }} [opts]
 * @returns {object | null}
 */
export function extractStructured(result, { parseFallback = false } = {}) {
  if (result?.structuredContent != null) return result.structuredContent;
  if (!parseFallback) return null;
  const t = extractText(result);
  if (!t) return null;
  try {
    return JSON.parse(t);
  } catch {
    return null;
  }
}

// This backend promises structured metadata; absence is an error, not guessed JSON.
export function metadata(result) {
  if (result?.isError) {
    const error = Error(
      result.structuredContent?.error || "computer action failed",
    );
    error.outcome_unknown = !!result.structuredContent?.outcome_unknown;
    throw error;
  }
  if (
    !result?.structuredContent ||
    typeof result.structuredContent !== "object"
  )
    throw Error("computer metadata missing");
  return result.structuredContent;
}
