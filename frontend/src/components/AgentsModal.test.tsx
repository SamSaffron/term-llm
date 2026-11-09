import { fireEvent, render, screen, waitFor, within } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { AgentsModal } from './AgentsModal';

function renderModal(load: () => Promise<unknown>) {
  const store = new AppStore(testConfig);
  store.modal.value = 'agents';
  store.endpoints.agents = vi.fn(load) as typeof store.endpoints.agents;
  render(
    <StoreContext.Provider value={store}>
      <AgentsModal />
    </StoreContext.Provider>,
  );
  return store;
}

describe('AgentsModal', () => {
  it('lists agents grouped by source and filters by source and text', async () => {
    renderModal(async () => ({
      agents: [
        { name: 'reviewer', description: 'Reviews code', source: 'builtin' },
        { name: 'dev', description: 'Discourse developer', source: 'user', model: 'opus' },
      ],
    }));
    const user = await screen.findByRole('region', { name: 'User agents' });
    expect(within(user).getByText('dev')).toBeInTheDocument();
    expect(within(user).getByText('opus')).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Built-in agents' })).toHaveTextContent(
      'reviewerReviews code',
    );

    const builtin = screen.getByRole('button', { name: 'Built-in (1)' });
    fireEvent.click(builtin);
    expect(builtin).toHaveAttribute('aria-pressed', 'true');
    expect(screen.queryByRole('region', { name: 'User agents' })).not.toBeInTheDocument();
    expect(screen.getByText('reviewer')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'All (2)' }));
    fireEvent.input(screen.getByRole('searchbox', { name: 'Filter agents' }), {
      target: { value: 'nothing-matches' },
    });
    expect(screen.getByRole('status')).toHaveTextContent('No agents match this filter.');
  });

  it('reports an empty catalog', async () => {
    renderModal(async () => ({ agents: [] }));
    expect(await screen.findByText('No agents configured.')).toBeInTheDocument();
  });

  it('shows the load error', async () => {
    renderModal(async () => {
      throw new Error('boom');
    });
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent('Could not load agents: boom'),
    );
  });
});
