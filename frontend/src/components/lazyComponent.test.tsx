import { act, render, screen, waitFor } from '@testing-library/preact';
import type { ComponentType } from 'preact';
import { describe, expect, it, vi } from 'vitest';
import { lazyComponent } from './lazyComponent';

describe('lazy component loading', () => {
  it('shares one import, ignores unmounted instances and renders cached mounts immediately', async () => {
    let resolve!: (component: ComponentType<{ label: string }>) => void;
    const load = vi.fn(
      () =>
        new Promise<ComponentType<{ label: string }>>((done) => {
          resolve = done;
        }),
    );
    const Lazy = lazyComponent(load, <span>Loading preview…</span>);
    const first = render(<Lazy label="first" />);
    await waitFor(() => expect(load).toHaveBeenCalledOnce());
    expect(screen.getByText('Loading preview…')).toBeInTheDocument();
    first.unmount();
    const second = render(<Lazy label="second" />);
    await act(async () => {
      resolve(({ label }) => <button>{label}</button>);
    });
    expect(await screen.findByRole('button', { name: 'second' })).toBeInTheDocument();
    expect(screen.queryByText('first')).toBeNull();
    expect(load).toHaveBeenCalledOnce();
    second.unmount();
    render(<Lazy label="cached" />);
    expect(screen.getByRole('button', { name: 'cached' })).toBeInTheDocument();
    expect(screen.queryByText('Loading preview…')).toBeNull();
    expect(load).toHaveBeenCalledOnce();
  });
});
