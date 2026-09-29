import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';

/** Mouse travel before a press on a reorderable row becomes a drag instead of a click. */
const DRAG_THRESHOLD = 5;
/**
 * How long a touch (or pen) must rest on a reorderable row to lift it. Moving
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

/** The move an Alt+Arrow key asks of a focused reorderable row, or 0 for any other key. */
export function reorderKeyOffset(event: KeyboardEvent): -1 | 0 | 1 {
  if (!event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return 0;
  return event.key === 'ArrowUp' ? -1 : event.key === 'ArrowDown' ? 1 : 0;
}

/** The reorderable rows of a list: its direct children carrying `data-reorder-id`. */
function reorderRows(list: HTMLElement): HTMLElement[] {
  return [...list.children].filter(
    (row): row is HTMLElement => row instanceof HTMLElement && Boolean(row.dataset.reorderId),
  );
}

/**
 * Swallows the click that releasing a dragged row produces, so a drag never
 * also opens the conversation or toggles the project. The next press or a
 * short grace period disarms it, so later clicks are unaffected.
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

/**
 * The landing slots of a lifted row, measured once as it lifts. Rows may
 * differ in height (a project group is as tall as its open conversation
 * list), so nothing assumes a fixed row height. `tops[i]` is where the lifted
 * row's top sits if it lands at position i: at row i's top above its origin,
 * or with its bottom at row i's bottom below it. The rows it passes slide by
 * its pitch (its height plus the spacing to its neighbour) to open that slot,
 * so the previewed gap is exactly where the re-rendered list puts the row.
 */
interface LandingSlots {
  tops: number[];
  /** How far rows below the lifted row rise when it moves past them. */
  rise: number;
  /** How far rows above the lifted row drop when it moves past them. */
  drop: number;
}

function landingSlots(rects: readonly DOMRect[], from: number): LandingSlots {
  const height = rects[from].height;
  return {
    tops: rects.map((rect, index) => (index <= from ? rect.top : rect.bottom - height)),
    rise: from < rects.length - 1 ? rects[from + 1].top - rects[from].top : height,
    drop: from > 0 ? rects[from].bottom - rects[from - 1].bottom : height,
  };
}

interface DragGesture {
  /** Whether the pressed row is lifted and following the pointer. */
  readonly lifted: boolean;
  /** Abandons the gesture, restoring the list without reporting. */
  cancel: () => void;
}

/**
 * Pointer (mouse, touch, and pen) reordering for a list whose direct children
 * carry `data-reorder-id`. The pressed control is the drag surface: a mouse
 * press lifts its row after a few pixels of travel and a touch after a brief
 * still press, so clicks and taps keep working and quick swipes still scroll
 * or close the drawer. The lifted row follows the pointer within the list's
 * bounds while the rows it passes slide to open the slot nearest to it, which
 * is where it lands. Releasing over a new slot reports the new order; Escape,
 * pointer cancellation, or releasing in place restores the list without
 * reporting. A press that lifted its row never also counts as a click.
 */
function useDragReorder(
  list: preact.RefObject<HTMLElement>,
  onReorder: (orderedIds: string[], movedId: string, to: number) => void,
  orderKey: string,
) {
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const reorder = useRef(onReorder);
  reorder.current = onReorder;
  const active = useRef<DragGesture | null>(null);

  // A sidebar refresh can add/remove/reorder rows mid-gesture. Never save a
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
      const rows = reorderRows(root);
      const from = rows.findIndex((row) => row.dataset.reorderId === id);
      if (from < 0 || rows.length < 2) return;
      // The press is not prevented: until the row lifts it is an ordinary
      // click, tap, scroll, or swipe.
      active.current?.cancel();

      const row = rows[from];
      const last = rows.length - 1;
      const ids = rows.map((entry) => entry.dataset.reorderId || '');
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
      let origin = 0;
      let slots: LandingSlots = { tops: [], rise: 0, drop: 0 };
      let startScroll = 0;
      let to = from;
      let frame = 0;
      let timer = 0;

      const layout = () => {
        const travel = pointerY - startY + ((scroller?.scrollTop ?? 0) - startScroll);
        // The lifted row stays within its list: it cannot cross into rows
        // outside it, such as unpinned conversations.
        const offset = Math.min(
          slots.tops[last] - origin,
          Math.max(slots.tops[0] - origin, travel),
        );
        // It lands in the slot nearest to where it is shown, so a drop never
        // jumps further than halfway to a neighbouring slot, whatever the
        // heights of the rows around it.
        const top = origin + offset;
        to = from;
        while (to > 0 && top < (slots.tops[to - 1] + slots.tops[to]) / 2) to -= 1;
        while (to < last && top > (slots.tops[to] + slots.tops[to + 1]) / 2) to += 1;
        rows.forEach((entry, index) => {
          const shift =
            index === from
              ? offset
              : from < index && index <= to
                ? -slots.rise
                : to <= index && index < from
                  ? slots.drop
                  : 0;
          entry.style.transform = shift ? `translateY(${shift}px)` : '';
        });
      };
      const onScroll = () => {
        if (lifted) layout();
      };
      const lift = () => {
        window.clearTimeout(timer);
        const rects = rows.map((entry) => entry.getBoundingClientRect());
        origin = rects[from].top;
        slots = landingSlots(rects, from);
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

/** Reordering controls for one row of a user-ordered list. */
export interface ReorderControls {
  /** Zero-based position within the list. */
  position: number;
  count: number;
  dragging: boolean;
  /** Tracks a press on the row's drag surface, which drags it once it moves (mouse) or rests (touch). */
  press: (event: PointerEvent) => void;
  /** Moves the row one place; `focus` names the control that keeps focus. */
  move: (offset: -1 | 1, focus: 'row' | 'menu') => void;
}

export interface ReorderableListOptions {
  /** Names a row in the announcement of its move. */
  label: (id: string) => string;
  /** Saves a new order of the list's IDs. */
  save: (orderedIds: string[]) => Promise<void>;
  /** Reports a save that failed; its announcement is withdrawn. */
  report: (error: unknown) => void;
  /** Selectors, within a row, of the controls that keep focus after a keyboard or menu move. */
  focusTargets: Record<'row' | 'menu', string>;
}

/** Focuses a row's control after a move re-renders the list. */
function focusRow(list: HTMLElement | null, id: string, selector: string): void {
  const row = list && reorderRows(list).find((entry) => entry.dataset.reorderId === id);
  const control = row?.querySelector<HTMLElement>(selector);
  if (control && document.activeElement !== control) control.focus();
}

/**
 * A user-ordered list, such as the pinned conversations or the projects, whose
 * rows are the direct children of `list` carrying `data-reorder-id`. Rows move
 * by dragging (see useDragReorder), Alt+Arrow keys, or menu items. Each move
 * is announced politely, keeps focus on the moved row's control, and is saved
 * through `options.save`.
 */
export function useReorderableList(ids: readonly string[], options: ReorderableListOptions) {
  const list = useRef<HTMLDivElement>(null);
  const [announcement, setAnnouncement] = useState('');
  const commit = (orderedIds: string[], id: string, to: number, focus?: 'row' | 'menu') => {
    setAnnouncement(`Moved ${options.label(id)} to position ${to + 1} of ${orderedIds.length}.`);
    void options.save(orderedIds).catch((error) => {
      setAnnouncement('');
      options.report(error);
    });
    if (focus) requestAnimationFrame(() => focusRow(list.current, id, options.focusTargets[focus]));
  };
  const { draggingId, press } = useDragReorder(list, commit, ids.join('\0'));
  const controls = (id: string, position: number): ReorderControls => ({
    position,
    count: ids.length,
    dragging: draggingId === id,
    press: (event) => press(event, id),
    move: (offset, focus) => {
      const to = position + offset;
      if (to >= 0 && to < ids.length) commit(movedOrder(ids, position, to), id, to, focus);
    },
  });
  return { list, reordering: draggingId !== null, announcement, controls };
}
