export type ElapsedClockListener = () => void;

const listeners = new Set<ElapsedClockListener>();
let timer: ReturnType<typeof setTimeout> | undefined;
let listeningForVisibility = false;

function pageHidden(): boolean {
  return typeof document !== 'undefined' && document.hidden;
}

function schedule(): void {
  if (timer !== undefined || listeners.size === 0 || pageHidden()) return;
  timer = setTimeout(tick, 1_000);
}

function tick(): void {
  timer = undefined;
  for (const listener of [...listeners]) listener();
  schedule();
}

function visibilityChanged(): void {
  if (pageHidden()) {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
    return;
  }
  for (const listener of [...listeners]) listener();
  schedule();
}

function attachVisibilityListener(): void {
  if (listeningForVisibility || typeof document === 'undefined') return;
  document.addEventListener('visibilitychange', visibilityChanged);
  listeningForVisibility = true;
}

function detachVisibilityListener(): void {
  if (!listeningForVisibility || typeof document === 'undefined') return;
  document.removeEventListener('visibilitychange', visibilityChanged);
  listeningForVisibility = false;
}

/** Subscribe to the page-wide one-second clock used by imperative elapsed labels. */
export function subscribeElapsedClock(listener: ElapsedClockListener): () => void {
  listeners.add(listener);
  attachVisibilityListener();
  schedule();
  return () => {
    listeners.delete(listener);
    if (listeners.size > 0) return;
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
    detachVisibilityListener();
  };
}

/** Format a non-negative millisecond duration using the compact TUI convention. */
export function formatElapsedDuration(durationMs: number): string {
  const totalSeconds = Math.max(0, Math.floor((Number(durationMs) || 0) / 1_000));
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const totalMinutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (totalMinutes < 60) return seconds === 0 ? `${totalMinutes}m` : `${totalMinutes}m${seconds}s`;
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  return minutes === 0 ? `${hours}h` : `${hours}h${String(minutes).padStart(2, '0')}m`;
}
