import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, expect, it, vi } from 'vitest';
import { readInjectedConfig } from './config';
import { AppStore } from '../stores/app-store';
import { App } from './App';

const pending = vi.hoisted(() => ({ reject: null as ((error: Error) => void) | null }));
vi.mock(
  '../components/ShellOverlay',
  () =>
    new Promise((_, reject) => {
      pending.reject = reject;
    }),
);

let store: AppStore;
afterEach(() => store?.dispose());

it('keeps shell loading/error frames and dismissal intact without loading a terminal', async () => {
  store = new AppStore(readInjectedConfig({ TERM_LLM_UI_PREFIX: '/ui' } as Window));
  store.bootstrap = vi.fn(async () => undefined);
  store.startupDone.value = true;
  store.shellStore.enabled.value = true;
  store.shellStore.visible.value = true;
  store.shellStore.layout.value = 'fullscreen';
  store.shellStore.back = vi.fn();
  const { container } = render(<App store={store} />);
  expect(await screen.findByText('Loading…')).toBeInTheDocument();
  expect(container.querySelector('.shell-overlay')).toHaveAttribute('aria-busy', 'true');
  fireEvent.click(screen.getByRole('button', { name: 'Back to chat' }));
  expect(store.shellStore.back).toHaveBeenCalledOnce();
  await act(async () => {
    store.shellStore.layout.value = 'right';
  });
  expect(screen.getByRole('button', { name: 'Hide terminal' })).toBeInTheDocument();
  await waitFor(() => expect(pending.reject).not.toBeNull());
  await act(async () => {
    pending.reject!(new Error('Terminal download failed'));
  });
  // Vitest wraps rejected module factories; assert the surfaced import error,
  // not the runner-specific wording of that wrapper.
  await waitFor(() =>
    expect(container.querySelector('.shell-error')).toHaveAttribute('role', 'alert'),
  );
  expect(container.querySelector('.shell-error')?.textContent).toBeTruthy();
  expect(container.querySelector('.shell-overlay')).not.toHaveAttribute('aria-busy');
  expect(container.querySelector('.shell-status-error')).toHaveTextContent('Could not load');
  expect(screen.queryByText('Loading…')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Hide terminal' }));
  expect(store.shellStore.back).toHaveBeenCalledTimes(2);
});
