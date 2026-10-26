import { fireEvent, render, screen } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { Settings } from './SettingsModal';

vi.mock('./MobileSharingSettings', () => ({
  MobileSharingSettings: ({ active }: { active: boolean }) => (
    <p data-testid="mobile-sharing-loaded">{active ? 'mobile active' : 'mobile inactive'}</p>
  ),
}));

it('hides owner-only tabs and push controls for a scoped Mobile Sharing peer', () => {
  const store = new AppStore(testConfig);
  store.peerMode.value = true;
  try {
    render(
      <StoreContext.Provider value={store}>
        <Settings />
      </StoreContext.Provider>,
    );
    expect(screen.queryByRole('tab', { name: 'Extensions' })).toBeNull();
    expect(screen.queryByRole('tab', { name: 'Mobile Sharing' })).toBeNull();
    expect(screen.queryByRole('tab', { name: 'Connection' })).toBeNull();
    fireEvent.click(screen.getByRole('tab', { name: 'Interface' }));
    expect(
      screen.getByText('Push notifications are not available over Mobile Sharing.'),
    ).toBeVisible();
    expect(screen.queryByText('Show widgets in sidebar')).toBeNull();
  } finally {
    store.dispose();
  }
});

it('registers the lazy mobile-sharing settings tab with its stable slug', async () => {
  const store = new AppStore(testConfig);
  try {
    render(
      <StoreContext.Provider value={store}>
        <Settings />
      </StoreContext.Provider>,
    );
    const tab = screen.getByRole('tab', { name: 'Mobile Sharing' });
    expect(tab).toHaveAttribute('id', 'settings-mobile-sharing-tab');
    expect(tab).toHaveAttribute('aria-controls', 'settings-mobile-sharing-panel');
    expect(screen.queryByTestId('mobile-sharing-loaded')).toBeNull();

    fireEvent.click(tab);
    expect(await screen.findByText('mobile active')).toBeVisible();
    expect(document.getElementById('settings-mobile-sharing-panel')).not.toHaveAttribute('hidden');

    fireEvent.click(screen.getByRole('tab', { name: 'Model' }));
    expect(screen.queryByTestId('mobile-sharing-loaded')).toBeNull();
    expect(document.getElementById('settings-mobile-sharing-panel')).toHaveAttribute('hidden');
  } finally {
    store.dispose();
  }
});
