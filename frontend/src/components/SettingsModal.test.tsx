import { fireEvent, render, screen } from '@testing-library/preact';
import { expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { Settings } from './SettingsModal';

vi.mock('./LinkedDevicesSettings', () => ({
  LinkedDevicesSettings: ({ active }: { active: boolean }) => (
    <p data-testid="linked-devices-loaded">
      {active ? 'linked devices active' : 'linked devices inactive'}
    </p>
  ),
}));

it('hides owner-only tabs and push controls for a linked-device client', () => {
  const store = new AppStore(testConfig);
  store.linkedDevicesMode.value = true;
  try {
    render(
      <StoreContext.Provider value={store}>
        <Settings />
      </StoreContext.Provider>,
    );
    expect(screen.queryByRole('tab', { name: 'Extensions' })).toBeNull();
    expect(screen.queryByRole('tab', { name: 'Linked devices' })).toBeNull();
    expect(screen.queryByRole('tab', { name: 'Connection' })).toBeNull();
    fireEvent.click(screen.getByRole('tab', { name: 'Interface' }));
    expect(
      screen.getByText('Push notifications are not available through Linked devices.'),
    ).toBeVisible();
    expect(screen.queryByText('Show widgets in sidebar')).toBeNull();
  } finally {
    store.dispose();
  }
});

it('registers the lazy linked-devices settings tab with its stable slug', async () => {
  const store = new AppStore(testConfig);
  try {
    render(
      <StoreContext.Provider value={store}>
        <Settings />
      </StoreContext.Provider>,
    );
    const tab = screen.getByRole('tab', { name: 'Linked devices' });
    expect(tab).toHaveAttribute('id', 'settings-linked-devices-tab');
    expect(tab).toHaveAttribute('aria-controls', 'settings-linked-devices-panel');
    expect(screen.queryByTestId('linked-devices-loaded')).toBeNull();

    fireEvent.click(tab);
    expect(await screen.findByText('linked devices active')).toBeVisible();
    expect(document.getElementById('settings-linked-devices-panel')).not.toHaveAttribute('hidden');

    fireEvent.click(screen.getByRole('tab', { name: 'Model' }));
    expect(screen.queryByTestId('linked-devices-loaded')).toBeNull();
    expect(document.getElementById('settings-linked-devices-panel')).toHaveAttribute('hidden');
  } finally {
    store.dispose();
  }
});
