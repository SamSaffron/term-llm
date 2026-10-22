export const DEFAULT_SIDEBAR_WIDTH = 280;
export const MIN_SIDEBAR_WIDTH = 220;
export const MAX_SIDEBAR_WIDTH = 560;

/**
 * Keeps the left sidebar between its minimum and maximum, and capped at
 * 45% of the window unless the minimum is larger. A value that is not a
 * number falls back to the default.
 */
export function clampSidebarWidth(value: number, viewport: number): number {
  const max = Math.max(
    MIN_SIDEBAR_WIDTH,
    Math.min(MAX_SIDEBAR_WIDTH, Math.floor((Number(viewport) || 0) * 0.45)),
  );
  const width = Number.isFinite(Number(value)) ? Number(value) : DEFAULT_SIDEBAR_WIDTH;
  return Math.round(Math.min(max, Math.max(MIN_SIDEBAR_WIDTH, width)));
}
