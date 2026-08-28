import { act, render, screen } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { useMediaQuery } from './useMediaQuery';

describe('useMediaQuery', () => {
  it('reacts when a responsive breakpoint changes', () => {
    let matches = false;
    let listener: (() => void) | undefined;
    vi.mocked(window.matchMedia).mockReturnValue({
      get matches() {
        return matches;
      },
      addEventListener: vi.fn((_type, next: EventListenerOrEventListenerObject) => {
        listener = next as () => void;
      }),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList);

    function Fixture() {
      const mobile = useMediaQuery('(max-width: 767px)');
      return <div>{mobile ? 'mobile' : 'desktop'}</div>;
    }

    render(<Fixture />);
    expect(screen.getByText('desktop')).toBeInTheDocument();
    act(() => {
      matches = true;
      listener?.();
    });
    expect(screen.getByText('mobile')).toBeInTheDocument();
  });
});
