// Keep modal gestures out of ancestor swipe handlers and prevent scroll chaining
// on iOS, where CSS overscroll containment alone is not always sufficient.
export function containPopoverScroll(panel: HTMLElement): () => void {
  let previousY: number | null = null;
  const start = (event: TouchEvent) => {
    event.stopPropagation();
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
    previousY = y;
    if (!delta) return;

    // Allow native scrolling (including momentum) in any scrollable descendant.
    // Only cancel when no container inside the popover can consume the gesture.
    let element = event.target instanceof Element ? event.target : null;
    while (element && panel.contains(element)) {
      const { overflowY } = getComputedStyle(element);
      if (overflowY === 'auto' || overflowY === 'scroll') {
        const maxScroll = element.scrollHeight - element.clientHeight;
        if (
          maxScroll > 0 &&
          ((delta > 0 && element.scrollTop > 0) || (delta < 0 && element.scrollTop < maxScroll))
        ) {
          return;
        }
      }
      if (element === panel) break;
      element = element.parentElement;
    }
    if (event.cancelable) event.preventDefault();
  };
  const end = (event: TouchEvent) => {
    event.stopPropagation();
    previousY = null;
  };
  const wheel = (event: WheelEvent) => event.stopPropagation();

  panel.addEventListener('touchstart', start, { passive: true });
  panel.addEventListener('touchmove', move, { passive: false });
  panel.addEventListener('touchend', end, { passive: true });
  panel.addEventListener('touchcancel', end, { passive: true });
  panel.addEventListener('wheel', wheel, { passive: true });
  return () => {
    panel.removeEventListener('touchstart', start);
    panel.removeEventListener('touchmove', move);
    panel.removeEventListener('touchend', end);
    panel.removeEventListener('touchcancel', end);
    panel.removeEventListener('wheel', wheel);
  };
}
