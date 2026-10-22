import { signal } from '@preact/signals';
import { act, fireEvent, render, screen } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { PanelResizeHandle } from './PanelResizeHandle';

function setup(edge: 'left' | 'right' = 'right') {
  const width = signal(300);
  const commit = vi.fn((next: number) => (width.value = next));
  const reset = vi.fn();
  const view = render(
    <div class="app" style="--test-width: 300px">
      <PanelResizeHandle
        class="test-handle"
        label="Resize test panel"
        edge={edge}
        width={width.value}
        getWidth={() => width.peek()}
        clamp={(value, viewport) => Math.max(220, Math.min(560, viewport * 0.45, value))}
        cssProperty="--test-width"
        resizingClass="test-resizing"
        onCommit={commit}
        onReset={reset}
      />
    </div>,
  );
  const handle = screen.getByRole('separator');
  const shell = view.container.firstElementChild as HTMLElement;
  return { ...view, width, commit, reset, handle, shell };
}

describe('PanelResizeHandle', () => {
  it.each(['left', 'right'] as const)('previews then commits once on the %s edge', (edge) => {
    const { handle, shell, width, commit } = setup(edge);
    fireEvent.pointerDown(handle, { button: 0, clientX: 300, pointerId: 1 });
    fireEvent.pointerMove(window, { clientX: edge === 'right' ? 350 : 250, pointerId: 1 });
    expect(shell.style.getPropertyValue('--test-width')).toBe('350px');
    expect(width.value).toBe(300);
    expect(commit).not.toHaveBeenCalled();
    fireEvent.pointerUp(window, { pointerId: 1 });
    expect(commit).toHaveBeenCalledExactlyOnceWith(350);
    expect(shell.style.getPropertyValue('--test-width')).toBe('350px');
    expect(shell).not.toHaveClass('test-resizing');
  });

  it.each(['pointercancel', 'lostpointercapture'])(
    'restores the latest committed width on %s',
    (type) => {
      const { handle, shell, width, commit } = setup();
      fireEvent.pointerDown(handle, { clientX: 300, pointerId: 1 });
      fireEvent.pointerMove(window, { clientX: 350, pointerId: 1 });
      width.value = 320;
      const event = new PointerEvent(type, { pointerId: 1 });
      fireEvent(type === 'lostpointercapture' ? handle : window, event);
      expect(commit).not.toHaveBeenCalled();
      expect(shell.style.getPropertyValue('--test-width')).toBe('320px');
      expect(shell).not.toHaveClass('test-resizing');
      fireEvent.pointerUp(window, { pointerId: 1 });
      expect(commit).not.toHaveBeenCalled();
    },
  );

  it('ignores secondary buttons and unrelated pointers', () => {
    const { handle, shell, commit } = setup();
    fireEvent.pointerDown(handle, { button: 2, pointerId: 1 });
    expect(shell).not.toHaveClass('test-resizing');
    fireEvent.pointerDown(handle, { clientX: 300, pointerId: 1 });
    fireEvent.pointerMove(window, { clientX: 500, pointerId: 2 });
    fireEvent.pointerCancel(window, { pointerId: 2 });
    fireEvent.pointerUp(window, { pointerId: 2 });
    expect(shell.style.getPropertyValue('--test-width')).toBe('300px');
    expect(commit).not.toHaveBeenCalled();
    fireEvent.pointerUp(window, { pointerId: 1 });
    expect(commit).toHaveBeenCalledExactlyOnceWith(300);
  });

  it('keeps the active drag when another pointer presses the handle', () => {
    const { handle, shell, commit } = setup();
    fireEvent.pointerDown(handle, { clientX: 300, pointerId: 1 });
    fireEvent.pointerMove(window, { clientX: 350, pointerId: 1 });
    fireEvent.pointerDown(handle, { clientX: 400, pointerId: 2 });
    expect(shell.style.getPropertyValue('--test-width')).toBe('350px');
    fireEvent.pointerUp(window, { pointerId: 2 });
    expect(commit).not.toHaveBeenCalled();
    fireEvent.pointerMove(window, { clientX: 370, pointerId: 1 });
    fireEvent.pointerUp(window, { pointerId: 1 });
    expect(commit).toHaveBeenCalledExactlyOnceWith(370);
  });

  it('abandons a drag cleanly if pointer capture fails', () => {
    const { handle, shell, commit } = setup();
    Object.defineProperty(handle, 'setPointerCapture', {
      value: () => {
        throw new DOMException('Inactive pointer', 'NotFoundError');
      },
    });
    fireEvent.pointerDown(handle, { clientX: 300, pointerId: 1 });
    expect(shell).not.toHaveClass('test-resizing');
    expect(handle).not.toHaveAttribute('data-resizing');
    fireEvent.pointerMove(window, { clientX: 350, pointerId: 1 });
    fireEvent.pointerUp(window, { pointerId: 1 });
    expect(commit).not.toHaveBeenCalled();
    expect(shell.style.getPropertyValue('--test-width')).toBe('300px');
  });

  it('removes listeners and restores layout on unmount', () => {
    const { handle, shell, unmount, commit } = setup();
    fireEvent.pointerDown(handle, { clientX: 300, pointerId: 1 });
    fireEvent.pointerMove(window, { clientX: 350, pointerId: 1 });
    unmount();
    expect(shell.style.getPropertyValue('--test-width')).toBe('300px');
    fireEvent.pointerMove(window, { clientX: 400, pointerId: 1 });
    fireEvent.pointerUp(window, { pointerId: 1 });
    expect(commit).not.toHaveBeenCalled();
    expect(shell).not.toHaveClass('test-resizing');
  });

  it.each(['left', 'right'] as const)('supports keyboard and reset on the %s edge', (edge) => {
    const { handle, width, reset } = setup(edge);
    fireEvent.keyDown(handle, { key: 'ArrowLeft' });
    expect(width.value).toBe(edge === 'left' ? 316 : 284);
    fireEvent.keyDown(handle, { key: 'ArrowRight' });
    expect(width.value).toBe(300);
    fireEvent.keyDown(handle, { key: 'Home' });
    expect(width.value).toBe(220);
    fireEvent.keyDown(handle, { key: 'End' });
    expect(width.value).toBe(Math.min(560, innerWidth * 0.45));
    fireEvent.dblClick(handle);
    expect(reset).toHaveBeenCalledOnce();
    expect(handle).toHaveAttribute('aria-orientation', 'vertical');
    expect(handle).toHaveAttribute('aria-valuemin', '220');
  });

  it('updates accessible bounds on viewport resize without committing a preference', () => {
    const { handle, commit } = setup();
    vi.stubGlobal('innerWidth', 800);
    try {
      act(() => {
        fireEvent(window, new Event('resize'));
      });
      expect(handle).toHaveAttribute('aria-valuemax', '360');
      expect(commit).not.toHaveBeenCalled();
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
