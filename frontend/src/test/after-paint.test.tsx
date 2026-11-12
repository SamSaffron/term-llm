import { render } from 'preact';
import { useEffect } from 'preact/hooks';
import { act } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';

it.each(['mount', 'unmount'] as const)(
  'does not leave an after-paint timer behind for a passive %s outside act',
  async (phase) => {
    vi.useFakeTimers();
    // Model a background tab where only Preact's fallback timer can fire.
    vi.spyOn(window, 'requestAnimationFrame').mockReturnValue(1);
    const container = document.createElement('div');
    const cleanup = vi.fn();
    function Probe() {
      useEffect(() => cleanup, []);
      return null;
    }
    try {
      if (phase === 'mount') {
        render(<Probe />, container);
      } else {
        await act(() => render(<Probe />, container));
        render(null, container);
      }
      // Match setup.ts: draining passive effects cannot cancel a browser
      // callback which Preact already scheduled before entering act().
      await act(() => render(null, container));
      if (phase === 'unmount') expect(cleanup).toHaveBeenCalledOnce();
      // Equivalent to jsdom globals disappearing before the fallback timer.
      vi.stubGlobal('cancelAnimationFrame', undefined);
      expect(() => vi.runOnlyPendingTimers()).not.toThrow();
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.unstubAllGlobals();
      vi.useRealTimers();
    }
  },
);
