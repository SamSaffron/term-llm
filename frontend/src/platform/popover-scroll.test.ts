import { afterEach, describe, expect, it, vi } from 'vitest';
import { containPopoverScroll } from './popover-scroll';

function touch(target: Element, type: string, ys: number[]) {
  const event = new Event(type, { bubbles: true, cancelable: true });
  Object.defineProperty(event, 'touches', { value: ys.map((clientY) => ({ clientY })) });
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
    clientHeight: { value: 200 },
  });
  const cleanup = containPopoverScroll(panel);
  return { parent, panel, scroller, item, footer, cleanup };
}

afterEach(() => document.body.replaceChildren());

describe('popover scroll containment', () => {
  it.each([true, false])('keeps native scrolling inside the picker (nested=%s)', (nested) => {
    const { parent, scroller, item, cleanup } = fixture(nested);
    const ancestor = vi.fn();
    parent.addEventListener('touchmove', ancestor);
    scroller.scrollTop = 100;
    touch(item, 'touchstart', [100]);
    expect(touch(item, 'touchmove', [80]).defaultPrevented).toBe(false);
    expect(touch(item, 'touchmove', [110]).defaultPrevented).toBe(false);
    expect(ancestor).not.toHaveBeenCalled();
    cleanup();
  });

  it.each([
    [0, 120, true],
    [0, 80, false],
    [400, 80, true],
    [400, 120, false],
    [-10, 120, true],
    [410, 80, true],
  ])('contains edge gestures at scrollTop=%s moving to %s', (top, y, prevented) => {
    const { scroller, item, cleanup } = fixture();
    scroller.scrollTop = top;
    touch(item, 'touchstart', [100]);
    expect(touch(item, 'touchmove', [y]).defaultPrevented).toBe(prevented);
    cleanup();
  });

  it('contains gestures over the footer and a filtered list that no longer overflows', () => {
    const { scroller, item, footer, cleanup } = fixture();
    touch(footer, 'touchstart', [100]);
    expect(touch(footer, 'touchmove', [80]).defaultPrevented).toBe(true);
    Object.defineProperty(scroller, 'scrollHeight', { value: 200 });
    touch(item, 'touchstart', [100]);
    expect(touch(item, 'touchmove', [120]).defaultPrevented).toBe(true);
    cleanup();
  });

  it('leaves pinch zoom alone and resets after cancellation', () => {
    const { item, cleanup } = fixture();
    touch(item, 'touchstart', [100]);
    expect(touch(item, 'touchmove', [80, 120]).defaultPrevented).toBe(false);
    touch(item, 'touchcancel', []);
    expect(touch(item, 'touchmove', [120]).defaultPrevented).toBe(false);
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
