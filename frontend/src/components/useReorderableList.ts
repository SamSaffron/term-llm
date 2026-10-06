import type { RefObject } from 'preact';
import { useCallback, useLayoutEffect, useRef, useState } from 'preact/hooks';

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

interface Point {
  x: number;
  y: number;
}

/** A CSS transform moving an element by `offset`, or none. Vertical moves use translateY. */
const translate = ({ x, y }: Point): string =>
  x ? `translate(${x}px, ${y}px)` : y ? `translateY(${y}px)` : '';

/**
 * Where a lifted row can land, measured once as it lifts. Offsets are
 * relative to the lifted row's place when it lifted.
 */
interface LandingGeometry {
  /** Whether rows flow into a grid of cells rather than one column. */
  readonly grid: boolean;
  /** Where the lifted row is shown for pointer travel `travel`: within the list's bounds. */
  clamp: (travel: Point) => Point;
  /** The position the lifted row lands at when shown at `offset`: the nearest landing slot. */
  target: (offset: Point) => number;
  /** How far the row at `index` slides aside while the lifted row targets `to`. */
  shift: (index: number, to: number) => Point;
}

/**
 * A single column, such as a sidebar list. Rows may differ in height (a
 * project group is as tall as its open conversation list), so nothing assumes
 * a fixed row height. A landing slot is where the lifted row's top sits if it
 * lands at position i: at row i's top above its origin, or with its bottom at
 * row i's bottom below it. The rows it passes slide by its pitch (its height
 * plus the spacing to its neighbour) to open that slot, so the previewed gap
 * is exactly where the re-rendered list puts the row.
 */
function columnGeometry(rects: readonly DOMRect[], from: number): LandingGeometry {
  const last = rects.length - 1;
  const origin = rects[from].top;
  const height = rects[from].height;
  const tops = rects.map((rect, index) => (index <= from ? rect.top : rect.bottom - height));
  // How far rows below the lifted row rise, and rows above it drop, when it passes them.
  const rise = from < last ? rects[from + 1].top - rects[from].top : height;
  const drop = from > 0 ? rects[from].bottom - rects[from - 1].bottom : height;
  return {
    grid: false,
    // The lifted row stays within its list: it cannot cross into rows
    // outside it, such as unpinned conversations.
    clamp: (travel) => ({
      x: 0,
      y: Math.min(tops[last] - origin, Math.max(tops[0] - origin, travel.y)),
    }),
    // It lands in the slot nearest to where it is shown, so a drop never
    // jumps further than halfway to a neighbouring slot, whatever the
    // heights of the rows around it.
    target: (offset) => {
      const top = origin + offset.y;
      let to = from;
      while (to > 0 && top < (tops[to - 1] + tops[to]) / 2) to -= 1;
      while (to < last && top > (tops[to] + tops[to + 1]) / 2) to += 1;
      return to;
    },
    shift: (index, to) => ({
      x: 0,
      y: from < index && index <= to ? -rise : to <= index && index < from ? drop : 0,
    }),
  };
}

/**
 * A grid whose rows wrap into several columns, such as the Hub's agent cards.
 * Landing at position i puts the lifted card in cell i, where the card at i
 * starts; the cards it passes each slide one cell along the reading order,
 * across row ends included, to open it.
 */
function gridGeometry(rects: readonly DOMRect[], from: number): LandingGeometry {
  const cells = rects.map((rect) => ({
    x: rect.left - rects[from].left,
    y: rect.top - rects[from].top,
  }));
  const span = (axis: 'x' | 'y', value: number) =>
    Math.min(
      Math.max(...cells.map((cell) => cell[axis])),
      Math.max(Math.min(...cells.map((cell) => cell[axis])), value),
    );
  return {
    grid: true,
    clamp: (travel) => ({ x: span('x', travel.x), y: span('y', travel.y) }),
    target: (offset) => {
      let to = from;
      let nearest = Infinity;
      cells.forEach((cell, index) => {
        const distance = Math.hypot(cell.x - offset.x, cell.y - offset.y);
        if (distance < nearest) [to, nearest] = [index, distance];
      });
      return to;
    },
    shift: (index, to) => {
      const cell =
        from < index && index <= to ? index - 1 : to <= index && index < from ? index + 1 : index;
      return { x: cells[cell].x - cells[index].x, y: cells[cell].y - cells[index].y };
    },
  };
}

/** Whether rows share one column, measured from their layout boxes. */
const singleColumn = (rects: readonly DOMRect[]): boolean =>
  rects.every((rect) => Math.abs(rect.left - rects[0].left) < 1);

/** The scrolling that moves a list: an ancestor scroll container, or the page. */
interface Scroller {
  /** Scroll offset, which a drag adds to the pointer's travel. */
  top: () => number;
  /** Scrolls by `step` pixels and reports whether anything moved. */
  scrollBy: (step: number) => boolean;
  /** The visible band whose edges auto-scroll a drag. */
  edges: () => { top: number; bottom: number };
  /** Receives the scroll events. */
  events: EventTarget;
}

function scrollerFor(root: HTMLElement, selector: string | undefined): Scroller | null {
  const element = selector
    ? root.closest<HTMLElement>(selector)
    : (document.scrollingElement as HTMLElement | null);
  if (!element) return null;
  return {
    top: () => element.scrollTop,
    scrollBy: (step) => {
      const before = element.scrollTop;
      element.scrollTop = before + step;
      return element.scrollTop !== before;
    },
    edges: () => (selector ? element.getBoundingClientRect() : { top: 0, bottom: innerHeight }),
    // The page's scroll events fire on the window, not the scrolling element.
    events: selector ? element : window,
  };
}

function finishTransformTransitions(rows: readonly HTMLElement[]): void {
  if (typeof CSSTransition === 'undefined') return;
  // A second drag can begin while the previous cards are still settling.
  // Measure their final cells, not their transient animated positions.
  rows.forEach((row) =>
    row.getAnimations?.().forEach((animation) => {
      if (animation instanceof CSSTransition && animation.transitionProperty === 'transform')
        animation.finish();
    }),
  );
}

/**
 * Settles dropped grid cards into their new cells. The list has re-rendered
 * in its new order, so each card starts from where it was shown (`shown`) and
 * slides home, exactly like the preview but ending in the real layout.
 */
function settleCards(rows: readonly HTMLElement[], shown: readonly DOMRect[]): void {
  rows.forEach((row, index) => {
    const previous = shown[index];
    if (!previous) return; // The list may have changed before the animation frame.
    const now = row.getBoundingClientRect();
    row.style.transform = translate({
      x: previous.left - now.left,
      y: previous.top - now.top,
    });
  });
  rows[0]?.parentElement?.getBoundingClientRect();
  rows.forEach((row) => {
    row.style.removeProperty('transition');
    row.style.removeProperty('transform');
  });
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
 * is where it lands. Rows in one column move vertically; rows that wrap into a
 * grid move in both directions (see gridGeometry). Releasing over a new slot
 * reports the new order; Escape, pointer cancellation, or releasing in place
 * restores the list without reporting. A press that lifted its row never also
 * counts as a click.
 */
function useDragReorder(
  list: RefObject<HTMLElement | null>,
  onReorder: (orderedIds: string[], movedId: string, to: number) => void,
  orderKey: string,
  scrollContainer: string | undefined,
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

  useLayoutEffect(() => {
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
      finishTransformTransitions(rows);

      const row = rows[from];
      const ids = rows.map((entry) => entry.dataset.reorderId || '');
      const scroller = scrollerFor(root, scrollContainer);
      // A mouse lifts a row after vertical travel in a column, and after
      // travel in any direction in a grid, whose cards also move sideways.
      const grid = !singleColumn([rows[0], rows[1]].map((entry) => entry.getBoundingClientRect()));
      const pointerId = event.pointerId;
      // Touch and pen drags scroll, so they lift a row only after resting still.
      const longPress = event.pointerType !== 'mouse';
      const start: Point = { x: event.clientX, y: event.clientY };
      const pointer: Point = { ...start };
      const travelled = (at: Point) =>
        grid ? Math.hypot(at.x - start.x, at.y - start.y) : Math.abs(at.y - start.y);
      let lifted = false;
      // Whether the row ever lifted: such a press never also counts as a click.
      let held = false;
      let geometry: LandingGeometry | null = null;
      let startScroll = 0;
      let to = from;
      let frame = 0;
      let timer = 0;

      const layout = () => {
        if (!geometry) return;
        const offset = geometry.clamp({
          x: pointer.x - start.x,
          y: pointer.y - start.y + ((scroller?.top() ?? 0) - startScroll),
        });
        to = geometry.target(offset);
        rows.forEach((entry, index) => {
          entry.style.transform = translate(index === from ? offset : geometry!.shift(index, to));
        });
      };
      const onScroll = () => {
        if (lifted) layout();
      };
      const lift = () => {
        window.clearTimeout(timer);
        finishTransformTransitions(rows);
        const rects = rows.map((entry) => entry.getBoundingClientRect());
        geometry = singleColumn(rects) ? columnGeometry(rects, from) : gridGeometry(rects, from);
        startScroll = scroller?.top() ?? 0;
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
        scroller?.events.addEventListener('scroll', onScroll, { passive: true });
      };
      const autoScroll = () => {
        frame = 0;
        if (!lifted || !scroller) return;
        const edges = scroller.edges();
        const above = edges.top + AUTO_SCROLL_EDGE - pointer.y;
        const below = pointer.y - (edges.bottom - AUTO_SCROLL_EDGE);
        const step = above > 0 ? -edgeScrollStep(above) : below > 0 ? edgeScrollStep(below) : 0;
        if (!step || !scroller.scrollBy(step)) return;
        layout();
        frame = requestAnimationFrame(autoScroll);
      };
      const move = (moveEvent: PointerEvent) => {
        if (moveEvent.pointerId !== pointerId) return;
        pointer.x = moveEvent.clientX;
        pointer.y = moveEvent.clientY;
        if (!lifted) {
          // A drag cancelled with Escape waits for its release.
          if (held) return;
          if (longPress) {
            // Moving before the row lifts is a scroll or a swipe.
            const drift = Math.hypot(pointer.x - start.x, pointer.y - start.y);
            if (drift > LONG_PRESS_TOLERANCE) end(false);
            return;
          }
          if (travelled(pointer) < DRAG_THRESHOLD) return;
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
          // Grid cards change rows, so they settle from where they were shown
          // into the re-rendered layout. Rows in a column land exactly where
          // the preview showed them.
          const shown = geometry?.grid ? rows.map((entry) => entry.getBoundingClientRect()) : null;
          // Land without animating from the drag offsets: the list re-renders in
          // its new order before the next paint.
          rows.forEach((entry) => {
            entry.style.transition = 'none';
            entry.style.removeProperty('transform');
          });
          root.getBoundingClientRect();
          requestAnimationFrame(() =>
            shown
              ? settleCards(rows, shown)
              : rows.forEach((entry) => entry.style.removeProperty('transition')),
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
        scroller?.events.removeEventListener('scroll', onScroll);
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
        const at = { x: upEvent.clientX, y: upEvent.clientY };
        if (!lifted && !longPress && !held && travelled(at) >= DRAG_THRESHOLD) lift();
        if (lifted && event.pointerType !== 'touch') {
          // Mouse and pen pointerup can be the first event at a fast drag's
          // final coordinates. Touch release is different: browsers can report the
          // capture origin on pointerup after touchEnd, so keep the last
          // pointermove destination that was actually previewed.
          Object.assign(pointer, at);
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
    [list, scrollContainer],
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
  /**
   * Selectors, within a row, of the controls that keep focus after a keyboard
   * or menu move when the control that had focus is gone (a closed menu).
   */
  focusTargets: Record<'row' | 'menu', string>;
  /**
   * Selector of the closest ancestor that scrolls the list, such as the
   * sidebar; a drag near its edges scrolls it. Without one, the page scrolls.
   */
  scrollContainer?: string;
}

/**
 * Focuses a row's control after a move re-renders the list: the control that
 * had focus (`previous`) if it is still in the row, or else `selector`'s.
 */
function focusRow(
  list: HTMLElement | null,
  id: string,
  selector: string,
  previous: Element | null,
): void {
  const row = list && reorderRows(list).find((entry) => entry.dataset.reorderId === id);
  const control =
    previous instanceof HTMLElement && row?.contains(previous)
      ? previous
      : row?.querySelector<HTMLElement>(selector);
  if (control && document.activeElement !== control) control.focus();
}

/**
 * A user-ordered list, such as the pinned conversations, the projects, or the
 * Hub's agent cards, whose rows are the direct children of `list` carrying
 * `data-reorder-id`. Rows move by dragging (see useDragReorder), Alt+Arrow
 * keys, or menu items. Each move is announced politely, keeps focus on the
 * moved row's control, and is saved through `options.save`.
 *
 * `controls` builds one row's controls. Rows rendered through a memo boundary
 * can instead take the stable `press` and `move` callbacks with their own ID.
 */
export function useReorderableList<ListElement extends HTMLElement = HTMLDivElement>(
  ids: readonly string[],
  options: ReorderableListOptions,
) {
  const list = useRef<ListElement>(null);
  const [announcement, setAnnouncement] = useState('');
  const commit = (orderedIds: string[], id: string, to: number, focus?: 'row' | 'menu') => {
    const previous = document.activeElement;
    setAnnouncement(`Moved ${options.label(id)} to position ${to + 1} of ${orderedIds.length}.`);
    void options.save(orderedIds).catch((error) => {
      setAnnouncement('');
      options.report(error);
    });
    if (focus)
      requestAnimationFrame(() =>
        focusRow(list.current, id, options.focusTargets[focus], focus === 'menu' ? null : previous),
      );
  };
  const latest = useRef({ ids, commit });
  latest.current = { ids, commit };
  const { draggingId, press } = useDragReorder(
    list,
    commit,
    ids.join('\0'),
    options.scrollContainer,
  );
  /** Moves a row `offset` places, stopping at either end of the list. */
  const move = useCallback((id: string, offset: number, focus: 'row' | 'menu') => {
    const { ids: current, commit: save } = latest.current;
    const from = current.indexOf(id);
    const to = Math.max(0, Math.min(current.length - 1, from + offset));
    if (from >= 0 && to !== from) save(movedOrder(current, from, to), id, to, focus);
  }, []);
  const controls = (id: string, position: number): ReorderControls => ({
    position,
    count: ids.length,
    dragging: draggingId === id,
    press: (event) => press(event, id),
    move: (offset, focus) => move(id, offset, focus),
  });
  return { list, reordering: draggingId !== null, draggingId, announcement, controls, press, move };
}
