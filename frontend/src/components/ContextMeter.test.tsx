import { act, render, screen, within } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import type { ContextUsage, Session } from '../domain/types';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Header } from './Header';

let store: AppStore | undefined;
afterEach(() => {
  store?.dispose();
  store = undefined;
});

const reading = (patch: Partial<ContextUsage> = {}): ContextUsage => ({
  usedTokens: 35_200,
  inputLimit: 160_000,
  cachedInputTokens: 412_000,
  estimated: true,
  autoCompactAt: 144_000,
  ...patch,
});

function renderHeader(session: Partial<Session>) {
  store = new AppStore(testConfig);
  store.sessions.value = [testSession({ activeModel: 'opus', ...session })];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  render(
    <StoreContext.Provider value={store}>
      <Header />
    </StoreContext.Provider>,
  );
  return store;
}

const meter = () => screen.getByRole('button', { name: /^Context window:/ });

describe('header context meter', () => {
  it('shows the share of the window and the window size, and hides without a reading', () => {
    const app = renderHeader({ contextUsage: reading() });
    expect(meter()).toHaveAccessibleName(
      'Context window: 22% used, 35.2k of 160k tokens (estimated)',
    );
    expect(meter()).toHaveTextContent('22% · 160k');
    expect(meter()).toHaveAttribute('data-tone', 'calm');

    act(() => app.sessionStore.patch('s1', { contextUsage: reading({ usedTokens: 148_800 }) }));
    expect(meter()).toHaveTextContent('93% · 160k');
    expect(meter()).toHaveAttribute('data-tone', 'hot');

    act(() => app.sessionStore.patch('s1', { contextUsage: null }));
    expect(screen.queryByRole('button', { name: /^Context window:/ })).toBeNull();
  });

  it('opens a card with the figures, the auto-compaction point and actions', async () => {
    const app = renderHeader({ contextUsage: reading() });
    const compact = vi.spyOn(app.selectionStore, 'compactSession').mockResolvedValue();
    await userEvent.click(meter());
    const card = await screen.findByRole('dialog', { name: 'Context window' });
    expect(meter()).toHaveAttribute('aria-expanded', 'true');
    expect(card).toHaveTextContent('opus · estimated');
    expect(card).toHaveTextContent('In context35.2kFree124.8kWindow160k');
    expect(card).toHaveTextContent('auto-compact 144k');
    expect(card).toHaveTextContent('108.8k left before term-llm compacts automatically.');

    await userEvent.click(within(card).getByRole('button', { name: 'Compact now' }));
    expect(compact).toHaveBeenCalledOnce();
    expect(screen.queryByRole('dialog', { name: 'Context window' })).toBeNull();

    await userEvent.click(meter());
    await userEvent.click(await screen.findByRole('button', { name: 'Session stats' }));
    expect(app.modal.value).toBe('stats');
    expect(screen.queryByRole('dialog', { name: 'Context window' })).toBeNull();
  });

  it('marks no compaction point when term-llm does not compact this context', async () => {
    renderHeader({ contextUsage: reading({ autoCompactAt: undefined }) });
    await userEvent.click(meter());
    const card = await screen.findByRole('dialog', { name: 'Context window' });
    expect(card).not.toHaveTextContent('auto-compact');
    expect(card.querySelector('.context-popover-note')).toBeNull();
  });

  it('shows only the token count when the window size is unknown', async () => {
    renderHeader({ contextUsage: reading({ inputLimit: undefined }) });
    expect(meter()).toHaveTextContent('35.2k tokens');
    expect(meter().querySelector('svg')).toBeNull();
    await userEvent.click(meter());
    const card = await screen.findByRole('dialog', { name: 'Context window' });
    expect(card).toHaveTextContent('WindowUnknown');
    expect(card).toHaveTextContent('The window size for this model is not known.');
  });

  it('waits for the running reply before offering compaction', async () => {
    renderHeader({ contextUsage: reading(), activeRun: true });
    await userEvent.click(meter());
    expect(await screen.findByRole('button', { name: 'Compact now' })).toBeDisabled();
  });
});
