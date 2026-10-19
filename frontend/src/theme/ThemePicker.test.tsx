import { fireEvent, render, screen } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { ExtensionSettings } from '../components/ExtensionSettings';
import type { AppStore } from '../stores/app-store';
import { ThemeStore } from './theme-store';

describe('built-in themes in Extensions', () => {
  it.each(['disabled', 'unavailable'])('works when server extensions are %s', async (state) => {
    const theme = new ThemeStore('/ui', localStorage);
    const status = vi.fn();
    if (state === 'disabled')
      status.mockResolvedValue({ disabled: true, enabled: [], entries: [], directory: '/tmp' });
    else status.mockRejectedValue(new Error('Extensions unavailable'));
    const store = {
      theme,
      config: { agentNames: [] },
      endpoints: { extensionsStatus: status },
    } as unknown as AppStore;
    render(
      <StoreContext.Provider value={store}>
        <ExtensionSettings />
      </StoreContext.Provider>,
    );
    const picker = screen.getByRole('combobox', { name: 'Theme', exact: true });
    expect(picker).toHaveValue('system');
    expect(picker).toHaveAccessibleDescription(/Applies immediately/);
    expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual([
      'Default (system light/dark)',
      'Sage Light',
      'Sage Dark',
      'Catppuccin Mocha',
      'Dracula',
      'Nord',
      'Tokyo Night',
    ]);
    await screen.findByText(
      state === 'disabled' ? 'Disabled by boot flag.' : 'Extensions unavailable',
    );
    expect(picker).toBeEnabled();
    fireEvent.change(picker, { target: { value: 'tokyo-night' } });
    expect(theme.selected.value).toBe('tokyo-night');
    expect(new ThemeStore('/ui', localStorage).selected.value).toBe('tokyo-night');
  });
});
