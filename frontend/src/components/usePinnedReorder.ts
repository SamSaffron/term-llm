import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';

/** Mouse travel before a press on a pinned row becomes a drag instead of a click. */
const DRAG_THRESHOLD = 5;
/**
 * How long a touch (or pen) must rest on a pinned row to lift it. Moving
 * sooner scrolls the sidebar or swipes the drawer, as it does on other rows.
 */
export const LONG_PRESS_MS = 300;
/** Drift a resting touch may make before it counts as a scroll or swipe. */
const LONG_PRESS_TOLERANCE = 8;
/** How long after a drag ends the click its release produces is swallowed. */
const RELEASE_CLICK_GRACE_MS = 400;
/** Distance from the scrollport edge where a drag scrolls the list. */
const AUTO_SCROLL_EDGE = 48;
const AUTO_SCROLL_MAX_STEP = 14;

/** Scroll speed for a pointer `depth` pixels into an auto-scroll edge. */
const edgeScrollStep = (depth: number): number =>
  Math.ceil((Math.min(depth, AUTO_SCROLL_EDGE) / AUTO_SCROLL_EDGE) * AUTO_SCROLL_MAX_STEP);

/** Moves the entry at `from` to `to`, shifting the entries between them. */
export function movedOrder(ids: readonly string[], from: number, to: number): string[] {
  const next = [...ids];
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved);
  return next;
}

/**
 * Swallows the click that releasing a dragged row produces, so a drag never
 * also opens the conversation. The next press or a short grace period disarms
 * it, so later clicks are unaffected.
 */
function swallowReleaseClick(): void {
  const disarm = () => {
    window.clearTimeout(timer);
    removeEventListener('click', swallow, true);
    removeEventListener('pointerdown', disarm, true);
  };
  const swallow = (event: MouseEvent) => {
    // Keyboard activation has detail=0 and must not be blocked by a previous
    // pointer drag. Only consume the synthetic pointer click from its release.
    if (event.detail === 0) return;
    event.preventDefault();
    event.stopPropagation();
    disarm();
  };
  addEventListener('click', swallow, true);
  addEventListener('pointerdown', disarm, true);
  const timer = window.setTimeout(disarm, RELEASE_CLICK_GRACE_MS);
}

interface DragGesture {
  /** Whether the pressed row is lifted and following the pointer. */
  readonly lifted: boolean;
  /** Abandons the gesture, restoring the list without reporting. */
  cancel: () => void;
}

/**
 * Pointer (mouse, touch, and pen) reordering for a list whose direct children
 * carry `data-pinned-id`. The whole row is the drag surface: a mouse press
 * lifts it after a few pixels of travel and a touch after a brief still press,
 * so clicks and taps still open the conversation and quick swipes still scroll
 * or close the drawer. The lifted row follows the pointer within the list's
 * bounds while the rows it passes slide to open its landing slot. Releasing
 * over a new slot reports the new order; Escape, pointer cancellation, or
 * releasing in place restores the list without reporting. A press that lifted
 * its row never also counts as a click.
 */
export function usePinnedReorder(
  list: preact.RefObject<HTMLElement>,
  onReorder: (orderedIds: string[], movedId: string, to: number) => void,
  orderKey: string,
) {
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const reorder = useRef(onReorder);
  reorder.current = onReorder;
  const active = useRef<DragGesture | null>(null);

  // A sidebar refresh can add/remove/reorder pins mid-gesture. Never save a
  // destination computed against an obsolete list of row IDs or rectangles.
  useLayoutEffect(() => {
    active.current?.cancel();
  }, [orderKey]);

  useEffect(() => {
    const root = list.current;
    if (!root) return;
    // A lifted row owns its touch: moves drag it rather than scroll the
    // sidebar, and holding it does not open the long-press menu. The listener
    // lives as long as the list because iOS only lets script cancel touch
    // scrolling when a non-passive listener already exists as the touch begins.
    const hold = (event: Event) => {
      if (active.current?.lifted && event.cancelable) event.preventDefault();
    };
    root.addEventListener('touchmove', hold, { passive: false });
    root.addEventListener('contextmenu', hold);
    return () => {
      root.removeEventListener('touchmove', hold);
      root.removeEventListener('contextmenu', hold);
      active.current?.cancel();
    };
  }, [list]);

  const press = useCallback(
    (event: PointerEvent, id: string) => {
      if (!event.isPrimary || (event.pointerType === 'mouse' && event.button !== 0)) return;
      const root = list.current;
      if (!root) return;
      const rows = [...root.children].filter(
        (row): row is HTMLElement => row instanceof HTMLElement && Boolean(row.dataset.pinnedId),
      );
      const from = rows.findIndex((row) => row.dataset.pinnedId === id);
      if (from < 0 || rows.length < 2) return;
      // The press is not prevented: until the row lifts it is an ordinary
      // click, tap, scroll, or swipe.
      active.current?.cancel();

      const row = rows[from];
      const ids = rows.map((entry) => entry.dataset.pinnedId || '');
      const scroller = root.closest<HTMLElement>('.sidebar-content');
      const pointerId = event.pointerId;
      // Touch and pen drags scroll, so they lift a row only after resting still.
      const longPress = event.pointerType !== 'mouse';
      const startX = event.clientX;
      const startY = event.clientY;
      let pointerY = startY;
      let lifted = false;
      // Whether the row ever lifted: such a press never also counts as a click.
      let held = false;
      let rects: DOMRect[] = [];
      let centers: number[] = [];
      let startScroll = 0;
      let to = from;
      let frame = 0;
      let timer = 0;

      const layout = () => {
        const travel = pointerY - startY + ((scroller?.scrollTop ?? 0) - startScroll);
        // The lifted row stays within the pinned list: it cannot cross into
        // unpinned conversations.
        const offset = Math.min(
          rects[rects.length - 1].bottom - rects[from].bottom,
          Math.max(rects[0].top - rects[from].top, travel),
        );
        const height = rects[from].height;
        // A row yields its slot once the lifted row's leading edge passes its
        // middle, which also works when rows differ in height.
        to = from;
        while (to > 0 && rects[from].top + offset < centers[to - 1]) to -= 1;
        while (to < rows.length - 1 && rects[from].bottom + offset > centers[to + 1]) to += 1;
        rows.forEach((entry, index) => {
          const shift =
            index === from
              ? offset
              : from < index && index <= to
                ? -height
                : to <= index && index < from
                  ? height
                  : 0;
          entry.style.transform = shift ? `translateY(${shift}px)` : '';
        });
      };
      const onScroll = () => {
        if (lifted) layout();
      };
      const lift = () => {
        window.clearTimeout(timer);
        rects = rows.map((entry) => entry.getBoundingClientRect());
        centers = rects.map((rect) => rect.top + rect.height / 2);
        startScroll = scroller?.scrollTop ?? 0;
        lifted = true;
        held = true;
        root.classList.add('is-reordering');
        setDraggingId(id);
        try {
          row.setPointerCapture?.(pointerId);
        } catch {
          // Capture is an optimization; window listeners still track the pointer.
        }
        // Capture may be lost while the button remains pressed. The window
        // pointerup/pointercancel listeners, not lostpointercapture, end a drag.
        scroller?.addEventListener('scroll', onScroll, { passive: true });
      };
      const autoScroll = () => {
        frame = 0;
        if (!lifted || !scroller) return;
        const bounds = scroller.getBoundingClientRect();
        const above = bounds.top + AUTO_SCROLL_EDGE - pointerY;
        const below = pointerY - (bounds.bottom - AUTO_SCROLL_EDGE);
        const step = above > 0 ? -edgeScrollStep(above) : below > 0 ? edgeScrollStep(below) : 0;
        if (!step) return;
        const before = scroller.scrollTop;
        scroller.scrollTop = before + step;
        if (scroller.scrollTop === before) return;
        layout();
        frame = requestAnimationFrame(autoScroll);
      };
      const move = (moveEvent: PointerEvent) => {
        if (moveEvent.pointerId !== pointerId) return;
        pointerY = moveEvent.clientY;
        if (!lifted) {
          // A drag cancelled with Escape waits for its release.
          if (held) return;
          if (longPress) {
            // Moving before the row lifts is a scroll or a swipe.
            const drift = Math.hypot(moveEvent.clientX - startX, pointerY - startY);
            if (drift > LONG_PRESS_TOLERANCE) end(false);
            return;
          }
          if (Math.abs(pointerY - startY) < DRAG_THRESHOLD) return;
          lift();
        }
        // Claim the move so enclosing gestures, such as swiping the mobile
        // drawer closed, leave this pointer alone.
        moveEvent.preventDefault();
        layout();
        if (!frame) frame = requestAnimationFrame(autoScroll);
      };
      const drop = (commit: boolean) => {
        lifted = false;
        root.classList.remove('is-reordering');
        if (frame) cancelAnimationFrame(frame);
        frame = 0;
        if (commit && to !== from) {
          // Land without animating from the drag offsets: the list re-renders in
          // its new order before the next paint.
          rows.forEach((entry) => {
            entry.style.transition = 'none';
            entry.style.removeProperty('transform');
          });
          root.getBoundingClientRect();
          requestAnimationFrame(() =>
            rows.forEach((entry) => entry.style.removeProperty('transition')),
          );
          reorder.current(movedOrder(ids, from, to), id, to);
        } else {
          rows.forEach((entry) => entry.style.removeProperty('transform'));
        }
        setDraggingId(null);
      };
      const end = (commit: boolean, swallowClick = held) => {
        if (lifted) drop(commit);
        window.clearTimeout(timer);
        removeEventListener('pointermove', move, true);
        removeEventListener('pointerup', release, true);
        removeEventListener('pointercancel', cancelPointer, true);
        removeEventListener('keydown', escape, true);
        scroller?.removeEventListener('scroll', onScroll);
        try {
          if (row.hasPointerCapture?.(pointerId)) row.releasePointerCapture(pointerId);
        } catch {
          // The browser may already have released capture on pointerup.
        }
        if (active.current === gesture) active.current = null;
        if (swallowClick) swallowReleaseClick();
      };
      const release = (upEvent: PointerEvent) => {
        if (upEvent.pointerId !== pointerId) return;
        if (!lifted && !longPress && !held && Math.abs(upEvent.clientY - startY) >= DRAG_THRESHOLD)
          lift();
        if (lifted && event.pointerType !== 'touch') {
          // Mouse and pen pointerup can be the first event at a fast drag's
          // final coordinates. Touch release is different: browsers can report the
          // capture origin on pointerup after touchEnd, so keep the last
          // pointermove destination that was actually previewed.
          pointerY = upEvent.clientY;
          layout();
        }
        end(true);
      };
      const cancelPointer = (cancelEvent: PointerEvent) => {
        if (cancelEvent.pointerId === pointerId) end(false);
      };
      const escape = (keyEvent: KeyboardEvent) => {
        if (keyEvent.key !== 'Escape' || !lifted) return;
        // Escape cancels the drag without also closing the mobile drawer.
        keyEvent.preventDefault();
        keyEvent.stopPropagation();
        drop(false);
      };
      const gesture: DragGesture = {
        get lifted() {
          return lifted;
        },
        // Cancelling is not a release, so no click follows to swallow.
        cancel: () => end(false, false),
      };
      active.current = gesture;
      addEventListener('pointermove', move, { capture: true, passive: false });
      addEventListener('pointerup', release, true);
      addEventListener('pointercancel', cancelPointer, true);
      addEventListener('keydown', escape, true);
      if (longPress) timer = window.setTimeout(lift, LONG_PRESS_MS);
    },
    [list],
  );

  return { draggingId, press };
}
