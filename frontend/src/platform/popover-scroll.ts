// Keep modal gestures out of ancestor swipe handlers and own the vertical
// scrolling inside the popover. iOS routes a single-finger drag over a scroller
// nested in a top-layer dialog to the page instead of the scroller, so the menu
// stayed put while the whole window panned. Driving scrollTop from the touch
// deltas and cancelling the gesture keeps the drag inside the popover on every
// platform; multi-touch stays native so pinch zoom still works.
const FRICTION = 0.94;
const MIN_VELOCITY = 0.04; // px/ms, roughly a pixel per frame
const MAX_FRAME = 32; // ms, so a stalled tab cannot fling the list
const DRAG_SLOP = 8; // px before a gesture counts as a scroll instead of a tap
const CLICK_GRACE = 500; // ms a browser may wait before the tap click arrives

function scrollerFor(target: EventTarget | null, panel: HTMLElement): HTMLElement | null {
  let element = target instanceof Element ? target : null;
  while (element && panel.contains(element)) {
    if (element instanceof HTMLElement) {
      const { overflowY } = getComputedStyle(element);
      if (
        (overflowY === 'auto' || overflowY === 'scroll') &&
        element.scrollHeight - element.clientHeight > 0
      )
        return element;
    }
    if (element === panel) break;
    element = element.parentElement;
  }
  return null;
}

function scrollBy(scroller: HTMLElement, delta: number): boolean {
  const limit = Math.max(0, scroller.scrollHeight - scroller.clientHeight);
  const current = Math.min(limit, Math.max(0, scroller.scrollTop));
  const next = Math.min(limit, Math.max(0, current + delta));
  scroller.scrollTop = next;
  return next !== current;
}

export function containPopoverScroll(panel: HTMLElement): () => void {
  let previousY: number | null = null;
  let previousAt = 0;
  let velocity = 0;
  let travelled = 0;
  let glide = 0;
  // Cancelling touchmove keeps the browser from replaying a tap after a drag,
  // so a scrolled list never picks the option that happened to be under the
  // finger when it lifted.
  let swallowClick = 0;
  const stopGlide = () => {
    if (glide) cancelAnimationFrame(glide);
    glide = 0;
  };
  const stopSwallow = () => {
    if (swallowClick) window.clearTimeout(swallowClick);
    swallowClick = 0;
  };
  const fling = (scroller: HTMLElement) => {
    let last = performance.now();
    const step = (now: number) => {
      const frame = Math.min(Math.max(now - last, 1), MAX_FRAME);
      last = now;
      velocity *= FRICTION ** (frame / 16);
      if (Math.abs(velocity) < MIN_VELOCITY || !scrollBy(scroller, -velocity * frame)) {
        glide = 0;
        return;
      }
      glide = requestAnimationFrame(step);
    };
    glide = requestAnimationFrame(step);
  };

  const start = (event: TouchEvent) => {
    event.stopPropagation();
    stopSwallow();
    velocity = 0;
    // A tap that catches a gliding list only stops it, the way native momentum
    // scrolling behaves, so count it as a drag and swallow its click.
    travelled = glide ? DRAG_SLOP + 1 : 0;
    stopGlide();
    previousAt = event.timeStamp;
    previousY = event.touches.length === 1 ? event.touches[0].clientY : null;
  };
  const move = (event: TouchEvent) => {
    event.stopPropagation();
    if (event.touches.length !== 1 || previousY === null) {
      previousY = null;
      return;
    }
    const y = event.touches[0].clientY;
    const delta = y - previousY;
    const elapsed = event.timeStamp - previousAt;
    previousY = y;
    previousAt = event.timeStamp;
    if (!delta) return;
    travelled += Math.abs(delta);
    velocity = elapsed > 0 ? delta / elapsed : 0;
    const scroller = scrollerFor(event.target, panel);
    if (scroller) scrollBy(scroller, -delta);
    else velocity = 0;
    if (event.cancelable) event.preventDefault();
  };
  const end = (event: TouchEvent) => {
    event.stopPropagation();
    const scroller = previousY === null ? null : scrollerFor(event.target, panel);
    previousY = null;
    if (travelled > DRAG_SLOP) swallowClick = window.setTimeout(stopSwallow, CLICK_GRACE);
    travelled = 0;
    if (scroller && Math.abs(velocity) >= MIN_VELOCITY) fling(scroller);
    else velocity = 0;
  };
  const cancel = (event: TouchEvent) => {
    event.stopPropagation();
    previousY = null;
    travelled = 0;
    velocity = 0;
  };
  const click = (event: MouseEvent) => {
    if (!swallowClick) return;
    stopSwallow();
    event.preventDefault();
    event.stopImmediatePropagation();
  };
  const wheel = (event: WheelEvent) => event.stopPropagation();

  panel.addEventListener('touchstart', start, { passive: true });
  panel.addEventListener('touchmove', move, { passive: false });
  panel.addEventListener('touchend', end, { passive: true });
  panel.addEventListener('touchcancel', cancel, { passive: true });
  panel.addEventListener('click', click, { capture: true });
  panel.addEventListener('wheel', wheel, { passive: true });
  return () => {
    stopGlide();
    stopSwallow();
    panel.removeEventListener('touchstart', start);
    panel.removeEventListener('touchmove', move);
    panel.removeEventListener('touchend', end);
    panel.removeEventListener('touchcancel', cancel);
    panel.removeEventListener('click', click, { capture: true });
    panel.removeEventListener('wheel', wheel);
  };
}
