import { afterEach, describe, expect, it, vi } from 'vitest';
import { observePopoverPosition, positionPopover } from './browser';

const originalVisualViewport = Object.getOwnPropertyDescriptor(window, 'visualViewport');

afterEach(() => {
  if (originalVisualViewport)
    Object.defineProperty(window, 'visualViewport', originalVisualViewport);
  else Reflect.deleteProperty(window, 'visualViewport');
});

describe('popover positioning', () => {
  it('anchors mobile popovers inside the current visual viewport and tracks its changes', () => {
    const listeners = new Map<string, EventListener>();
    const viewport = {
      width: 400,
      height: 500,
      offsetLeft: 0,
      offsetTop: 100,
      addEventListener: vi.fn((type: string, listener: EventListener) =>
        listeners.set(type, listener),
      ),
      removeEventListener: vi.fn((type: string) => listeners.delete(type)),
    } as unknown as VisualViewport;
    Object.defineProperty(window, 'visualViewport', { configurable: true, value: viewport });
    const trigger = document.createElement('button');
    const panel = document.createElement('dialog');
    panel.className = 'chip-popover';
    vi.spyOn(panel, 'getBoundingClientRect').mockReturnValue({
      width: 280,
      height: 200,
      top: 0,
      right: 280,
      bottom: 200,
      left: 0,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });
    document.body.append(trigger, panel);

    positionPopover(trigger, panel, true);
    expect(panel.style.top).toBe('392px');
    expect(panel.style.left).toBe('8px');
    expect(panel.style.right).toBe(`${window.innerWidth - 400 + 8}px`);

    const stop = observePopoverPosition(trigger, panel, true);
    Object.assign(viewport, { height: 300, offsetTop: 120 });
    listeners.get('resize')?.(new Event('resize'));
    expect(panel.style.top).toBe('212px');

    stop();
    expect(viewport.removeEventListener).toHaveBeenCalledWith('resize', expect.any(Function));
    expect(viewport.removeEventListener).toHaveBeenCalledWith('scroll', expect.any(Function));
  });
});
