import { useEffect, useRef } from 'preact/hooks';

type SwipeAxis = 'x' | 'y';
type SwipeDirection = -1 | 1;

interface SwipeDismissOptions {
  enabled: boolean;
  axis: SwipeAxis;
  direction: SwipeDirection;
  property: `--${string}`;
  onDismiss: () => void;
  handleSelector?: string;
}

interface EdgeSwipeOpenOptions {
  enabled: boolean;
  edge?: 'left' | 'right';
  property: `--${string}`;
  onOpen: () => void;
}

export function useEdgeSwipeOpen(
  surface: preact.RefObject<HTMLElement>,
  { enabled, edge = 'left', property, onOpen }: EdgeSwipeOpenOptions,
): void {
  const openRef = useRef(onOpen);
  openRef.current = onOpen;
  useEffect(() => {
    const node = surface.current;
    if (!enabled || !node) return;
    let pointerID: number | null = null;
    let startX = 0;
    let startY = 0;
    let startedAt = 0;
    let distance = 0;
    let dragging = false;
    const clear = () => {
      pointerID = null;
      dragging = false;
      distance = 0;
      node.classList.remove('is-edge-swipe-opening');
      node.style.removeProperty(property);
    };
    const down = (event: PointerEvent) => {
      if (!event.isPrimary) {
        if (pointerID !== null) clear();
        return;
      }
      if (event.pointerType === 'mouse') return;
      const edgeDistance = edge === 'left' ? event.clientX : innerWidth - event.clientX;
      if (edgeDistance > 28) return;
      pointerID = event.pointerId;
      startX = event.clientX;
      startY = event.clientY;
      startedAt = performance.now();
    };
    const move = (event: PointerEvent) => {
      if (event.pointerId !== pointerID) return;
      const signed = (edge === 'left' ? 1 : -1) * (event.clientX - startX);
      const cross = Math.abs(event.clientY - startY);
      if (!dragging) {
        if (signed < 0 || cross > Math.max(START_DISTANCE, signed * 1.15)) {
          clear();
          return;
        }
        if (signed < START_DISTANCE) return;
        dragging = true;
        node.classList.add('is-edge-swipe-opening');
      }
      event.preventDefault();
      distance = Math.max(0, Math.min(node.getBoundingClientRect().width, signed));
      node.style.setProperty(property, `${(edge === 'left' ? 1 : -1) * distance}px`);
    };
    const finish = (event: PointerEvent) => {
      if (event.pointerId !== pointerID) return;
      const elapsed = Math.max(1, performance.now() - startedAt);
      const threshold = Math.min(96, Math.max(56, node.getBoundingClientRect().width * 0.25));
      const shouldOpen =
        dragging && (distance >= threshold || distance / elapsed >= VELOCITY_THRESHOLD);
      clear();
      if (shouldOpen) openRef.current();
    };
    addEventListener('pointerdown', down);
    addEventListener('pointermove', move, { passive: false });
    addEventListener('pointerup', finish);
    addEventListener('pointercancel', clear);
    return () => {
      removeEventListener('pointerdown', down);
      removeEventListener('pointermove', move);
      removeEventListener('pointerup', finish);
      removeEventListener('pointercancel', clear);
      clear();
    };
  }, [edge, enabled, property, surface]);
}

const START_DISTANCE = 10;
const VELOCITY_THRESHOLD = 0.55;

export function useSwipeDismiss(
  surface: preact.RefObject<HTMLElement>,
  { enabled, axis, direction, property, onDismiss, handleSelector }: SwipeDismissOptions,
): void {
  const dismissRef = useRef(onDismiss);
  dismissRef.current = onDismiss;

  useEffect(() => {
    const node = surface.current;
    if (!enabled || !node) return;

    let pointerID: number | null = null;
    let startPrimary = 0;
    let startCross = 0;
    let startedAt = 0;
    let distance = 0;
    let dragging = false;
    let rejected = false;

    const clear = () => {
      if (pointerID !== null && node.hasPointerCapture?.(pointerID))
        node.releasePointerCapture(pointerID);
      pointerID = null;
      dragging = false;
      rejected = false;
      distance = 0;
      node.classList.remove('is-swipe-dragging');
      node.style.removeProperty(property);
    };

    const begin = (event: PointerEvent) => {
      if (!event.isPrimary) {
        if (pointerID !== null) clear();
        return;
      }
      if (event.pointerType === 'mouse') return;
      const target = event.target instanceof Element ? event.target : null;
      if (handleSelector && !target?.closest(handleSelector)) return;
      pointerID = event.pointerId;
      startPrimary = axis === 'x' ? event.clientX : event.clientY;
      startCross = axis === 'x' ? event.clientY : event.clientX;
      startedAt = performance.now();
      distance = 0;
      dragging = false;
      rejected = false;
    };

    const move = (event: PointerEvent) => {
      if (pointerID !== event.pointerId || rejected) return;
      const primary = axis === 'x' ? event.clientX : event.clientY;
      const cross = axis === 'x' ? event.clientY : event.clientX;
      const signedDistance = direction * (primary - startPrimary);
      const crossDistance = Math.abs(cross - startCross);

      if (!dragging) {
        if (signedDistance < 0 || crossDistance > Math.max(START_DISTANCE, signedDistance * 1.15)) {
          rejected = true;
          return;
        }
        if (signedDistance < START_DISTANCE) return;
        dragging = true;
        node.classList.add('is-swipe-dragging');
        node.setPointerCapture?.(event.pointerId);
      }

      event.preventDefault();
      distance = Math.max(0, signedDistance);
      node.style.setProperty(property, `${direction * distance}px`);
    };

    const finish = (event: PointerEvent) => {
      if (pointerID !== event.pointerId) return;
      const elapsed = Math.max(1, performance.now() - startedAt);
      const extent =
        axis === 'x' ? node.getBoundingClientRect().width : node.getBoundingClientRect().height;
      const threshold = Math.min(96, Math.max(56, extent * 0.25));
      const shouldDismiss =
        dragging && (distance >= threshold || distance / elapsed >= VELOCITY_THRESHOLD);
      clear();
      if (shouldDismiss) dismissRef.current();
    };

    node.addEventListener('pointerdown', begin);
    node.addEventListener('pointermove', move, { passive: false });
    node.addEventListener('pointerup', finish);
    node.addEventListener('pointercancel', clear);
    return () => {
      node.removeEventListener('pointerdown', begin);
      node.removeEventListener('pointermove', move);
      node.removeEventListener('pointerup', finish);
      node.removeEventListener('pointercancel', clear);
      clear();
    };
  }, [axis, direction, enabled, handleSelector, property, surface]);
}
