import { fireEvent, render, screen } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { Modals } from './Modals';

vi.mock('./ExtensionSettings', () => {
  throw new Error('Chunk unavailable');
});

it('preserves the extension settings loading and import-error messages', async () => {
  const store = new AppStore(testConfig);
  store.modal.value = 'settings';
  try {
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    fireEvent.click(screen.getByRole('tab', { name: 'Extensions' }));
    expect(screen.getByText('Loading extension settings…')).toBeInTheDocument();
    expect(
      await screen.findByText('Could not load extension settings. Reload the page to retry.'),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole('tab', { name: 'Model' }));
    fireEvent.click(screen.getByRole('tab', { name: 'Extensions' }));
    expect(
      screen.getByText('Could not load extension settings. Reload the page to retry.'),
    ).toBeVisible();
  } finally {
    store.dispose();
  }
});
