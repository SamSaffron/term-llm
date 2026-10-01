import { afterEach, describe, expect, it, vi } from 'vitest';
import { containPopoverScroll } from './popover-scroll';

function touch(target: Element, type: string, ys: number[], timeStamp?: number) {
  const event = new Event(type, { bubbles: true, cancelable: true });
  Object.defineProperty(event, 'touches', { value: ys.map((clientY) => ({ clientY })) });
  if (timeStamp !== undefined) Object.defineProperty(event, 'timeStamp', { value: timeStamp });
  target.dispatchEvent(event);
  return event;
}

function fixture(nested = true) {
  const parent = document.createElement('section');
  const panel = document.createElement('dialog');
  const options = document.createElement('div');
  const item = document.createElement('button');
  const footer = document.createElement('button');
  parent.append(panel);
  panel.append(options, footer);
  options.append(item);
  document.body.append(parent);
  const scroller = nested ? options : panel;
  scroller.style.overflowY = 'auto';
  Object.defineProperties(scroller, {
    scrollHeight: { value: 600, configurable: true },
    clientHeight: { value: 200, configurable: true },
  });
  const cleanup = containPopoverScroll(panel);
  return { parent, panel, scroller, item, footer, cleanup };
}

afterEach(() => {
  vi.useRealTimers();
  document.body.replaceChildren();
});

describe('popover scroll containment', () => {
  it.each([true, false])(
    'scrolls the picker itself and contains the gesture (nested=%s)',
    (nested) => {
      const { parent, scroller, item, cleanup } = fixture(nested);
      const ancestor = vi.fn();
      parent.addEventListener('touchmove', ancestor);
      scroller.scrollTop = 100;
      touch(item, 'touchstart', [100]);
      expect(touch(item, 'touchmove', [80]).defaultPrevented).toBe(true);
      expect(scroller.scrollTop).toBe(120);
      expect(touch(item, 'touchmove', [110]).defaultPrevented).toBe(true);
      expect(scroller.scrollTop).toBe(90);
      expect(ancestor).not.toHaveBeenCalled();
      cleanup();
    },
  );

  it.each([
    [0, 120, 0],
    [0, 80, 20],
    [400, 80, 400],
    [400, 120, 380],
    [390, 80, 400],
  ])('clamps edge gestures at scrollTop=%s moving to %s', (top, y, expected) => {
    const { scroller, item, cleanup } = fixture();
    scroller.scrollTop = top;
    touch(item, 'touchstart', [100]);
    expect(touch(item, 'touchmove', [y]).defaultPrevented).toBe(true);
    expect(scroller.scrollTop).toBe(expected);
    cleanup();
  });

  it('contains gestures over the footer and a filtered list that no longer overflows', () => {
    const { scroller, item, footer, cleanup } = fixture();
    touch(footer, 'touchstart', [100]);
    expect(touch(footer, 'touchmove', [80]).defaultPrevented).toBe(true);
    expect(scroller.scrollTop).toBe(0);
    Object.defineProperty(scroller, 'scrollHeight', { value: 200, configurable: true });
    touch(item, 'touchstart', [100]);
    expect(touch(item, 'touchmove', [120]).defaultPrevented).toBe(true);
    expect(scroller.scrollTop).toBe(0);
    cleanup();
  });

  it('leaves pinch zoom alone and resets after cancellation', () => {
    const { scroller, item, cleanup } = fixture();
    touch(item, 'touchstart', [100]);
    expect(touch(item, 'touchmove', [80, 120]).defaultPrevented).toBe(false);
    touch(item, 'touchcancel', []);
    expect(touch(item, 'touchmove', [120]).defaultPrevented).toBe(false);
    expect(scroller.scrollTop).toBe(0);
    cleanup();
  });

  it('keeps gliding after a flick and stops on cleanup', () => {
    vi.useFakeTimers();
    const { scroller, item, cleanup } = fixture();
    scroller.scrollTop = 100;
    touch(item, 'touchstart', [200], 0);
    touch(item, 'touchmove', [180], 16);
    touch(item, 'touchmove', [160], 32);
    touch(item, 'touchend', [], 48);
    expect(scroller.scrollTop).toBe(140);
    vi.advanceTimersByTime(100);
    const glided = scroller.scrollTop;
    expect(glided).toBeGreaterThan(140);
    cleanup();
    vi.advanceTimersByTime(500);
    expect(scroller.scrollTop).toBe(glided);
  });

  it('stops a glide on tap without choosing the option under the finger', () => {
    vi.useFakeTimers();
    const { scroller, item, cleanup } = fixture();
    const choose = vi.fn();
    item.addEventListener('click', choose);
    scroller.scrollTop = 100;
    touch(item, 'touchstart', [200], 0);
    touch(item, 'touchmove', [180], 16);
    touch(item, 'touchmove', [160], 32);
    touch(item, 'touchend', [], 48);
    vi.advanceTimersByTime(32);
    const gliding = scroller.scrollTop;

    touch(item, 'touchstart', [160], 100);
    touch(item, 'touchend', [], 116);
    item.click();
    expect(choose).not.toHaveBeenCalled();
    vi.advanceTimersByTime(200);
    expect(scroller.scrollTop).toBe(gliding);
    cleanup();
  });

  it('swallows the tap that follows a drag but keeps plain taps working', () => {
    const { scroller, item, cleanup } = fixture();
    const choose = vi.fn();
    item.addEventListener('click', choose);

    // A slow drag scrolls without leaving momentum behind.
    touch(item, 'touchstart', [200], 0);
    touch(item, 'touchmove', [180], 1000);
    touch(item, 'touchend', [], 1016);
    expect(scroller.scrollTop).toBe(20);
    item.click();
    expect(choose).not.toHaveBeenCalled();

    touch(item, 'touchstart', [200], 2000);
    touch(item, 'touchmove', [197], 2016);
    touch(item, 'touchend', [], 2032);
    item.click();
    expect(choose).toHaveBeenCalledOnce();
    cleanup();
  });

  it('isolates gesture events from ancestors and removes all listeners on cleanup', () => {
    const { parent, item, cleanup } = fixture();
    const ancestor = vi.fn();
    const types = ['touchstart', 'touchmove', 'touchend', 'touchcancel', 'wheel'];
    for (const type of types) parent.addEventListener(type, ancestor);
    for (const type of types) touch(item, type, [100]);
    expect(ancestor).not.toHaveBeenCalled();
    cleanup();
    for (const type of types) touch(item, type, [100]);
    expect(ancestor).toHaveBeenCalledTimes(types.length);
  });
});
