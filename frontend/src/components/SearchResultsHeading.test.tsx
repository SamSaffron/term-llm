import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { SearchResultsHeading } from './SearchResultsHeading';

let store: AppStore;
beforeEach(() => {
  store = new AppStore(testConfig);
});
afterEach(() => store.dispose());
function show() {
  return render(
    <StoreContext.Provider value={store}>
      <SearchResultsHeading />
      <button type="button">Outside</button>
    </StoreContext.Provider>,
  );
}

describe('search result ordering menu', () => {
  it('announces selected ordering, navigates by keyboard, and restores focus', async () => {
    const setSort = vi
      .spyOn(store.sessionStore, 'setSearchSort')
      .mockImplementation(async (sort) => {
        store.sessionStore.searchSort.value = sort;
      });
    show();
    const trigger = screen.getByRole('button', { name: 'Sort search results: Relevance' });
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    trigger.focus();
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    const relevance = screen.getByRole('menuitemradio', { name: 'Relevance' });
    expect(relevance).toHaveAttribute('aria-checked', 'true');
    await waitFor(() => expect(relevance).toHaveFocus());
    fireEvent.keyDown(relevance, { key: 'ArrowDown' });
    const newest = screen.getByRole('menuitemradio', { name: 'Newest first' });
    expect(newest).toHaveFocus();
    fireEvent.click(newest);
    expect(setSort).toHaveBeenCalledWith('newest');
    await waitFor(() => expect(trigger).toHaveFocus());
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute('aria-label', 'Sort search results: Newest first');
    fireEvent.click(trigger);
    const chosen = screen.getByRole('menuitemradio', { name: 'Newest first' });
    await waitFor(() => expect(chosen).toHaveFocus());
    expect(chosen).toHaveAttribute('aria-checked', 'true');
    fireEvent.keyDown(chosen, { key: 'End' });
    const oldest = screen.getByRole('menuitemradio', { name: 'Oldest first' });
    expect(oldest).toHaveFocus();
    fireEvent.keyDown(oldest, { key: 'Home' });
    expect(screen.getByRole('menuitemradio', { name: 'Relevance' })).toHaveFocus();
    fireEvent.keyDown(screen.getByRole('menuitemradio', { name: 'Relevance' }), { key: 'Escape' });
    await waitFor(() => expect(trigger).toHaveFocus());
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
  });

  it('handles immediate Escape on the opener without dismissing the containing sidebar', () => {
    const parentKey = vi.fn();
    render(
      <StoreContext.Provider value={store}>
        <div onKeyDown={parentKey}>
          <SearchResultsHeading />
        </div>
      </StoreContext.Provider>,
    );
    const trigger = screen.getByRole('button', { name: /^Sort search results:/ });
    fireEvent.click(trigger);
    fireEvent.keyDown(trigger, { key: 'Escape' });
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(parentKey).not.toHaveBeenCalled();
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
  });

  it('closes on outside pointer or focus without stealing focus from another control', async () => {
    show();
    const trigger = screen.getByRole('button', { name: /^Sort search results:/ });
    fireEvent.click(trigger);
    await waitFor(() =>
      expect(screen.getByRole('menuitemradio', { name: 'Relevance' })).toHaveFocus(),
    );
    const outside = screen.getByRole('button', { name: 'Outside' });
    act(() => outside.focus());
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(outside).toHaveFocus();
    fireEvent.click(trigger);
    await waitFor(() =>
      expect(screen.getByRole('menuitemradio', { name: 'Relevance' })).toHaveFocus(),
    );
    fireEvent.pointerDown(outside);
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });
});
