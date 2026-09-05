import { act, fireEvent, render, screen } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import type { Session } from '../domain/types';
import { CommitModal } from './CommitModal';

function fixture() {
  const store = new AppStore({
    prefix: '/ui',
    version: 'v1',
    sidebarCategories: ['all'],
    agentName: '',
    agentNames: [],
    title: '',
    locationSharing: false,
    worktrees: false,
    hub: null,
    vapidKey: '',
    webRTC: false,
    signalingURL: '',
  });
  store.sessions.value = [{ id: 's1', messages: [] } as unknown as Session];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.modal.value = 'commit';
  store.commitStore.state.value = {
    ...store.commitStore.state.peek(),
    sessionId: 's1',
    phase: 'success',
    result: {
      head_oid: 'committed',
      short_oid: 'abc123',
      subject: 'Change A',
      message: 'Change A\n\nDetails',
    },
    status: {
      branch: 'main',
      staged: [{ path: 'a.ts', kind: 'modified' }],
      unstaged: [],
      untracked: [],
      fingerprint: { checkout_id: 'checkout' },
    },
  };
  store.endpoints.commitPublishPlan = vi.fn(async () => ({
    checkout_id: 'checkout',
    head_oid: 'committed',
    branch: 'main',
    remote: 'origin',
    url: 'git@github.com:test/repo.git',
    target: 'main',
    repository: 'https://github.com/test/repo',
    default_branch: 'main',
  }));
  render(
    <StoreContext.Provider value={store}>
      <CommitModal />
    </StoreContext.Provider>,
  );
  return store;
}

describe('CommitModal publishing', () => {
  it('shows post-commit actions without stale staged counts and asks before pushing', async () => {
    const store = fixture();
    expect(screen.getByRole('button', { name: 'Push', exact: true })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Make PR' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Done' })).toBeEnabled();
    expect(screen.queryByText(/1 staged/)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Push', exact: true }));
    expect(await screen.findByRole('button', { name: 'Confirm push' })).toBeEnabled();
    expect(screen.getByText(/updates the remote branch directly/)).toBeInTheDocument();
    expect(store.commitStore.state.peek().publishForm?.kind).toBe('push');
    await userEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(screen.getByRole('button', { name: 'Done' })).toBeEnabled();
  });

  it('lets users review PR metadata and blocks head equal to base', async () => {
    const store = fixture();
    await userEvent.click(screen.getByRole('button', { name: 'Make PR' }));
    expect(await screen.findByLabelText('PR branch')).toHaveValue('pr/abc123');
    expect(screen.getByLabelText('Base branch')).toHaveValue('main');
    expect(screen.getByLabelText('PR title')).toHaveValue('Change A');
    expect(screen.getByLabelText('PR description')).toHaveValue('Details');
    await userEvent.click(screen.getByLabelText('Create as draft'));
    expect(store.commitStore.state.peek().publishForm?.draft).toBe(true);
    fireEvent.input(screen.getByLabelText('PR branch'), { target: { value: 'main' } });
    expect(screen.getByRole('button', { name: 'Push & make PR' })).toBeDisabled();
  });

  it('locks dismissal while publishing and renders safe PR links', async () => {
    const store = fixture();
    act(() => {
      store.commitStore.state.value = {
        ...store.commitStore.state.peek(),
        publishBusy: true,
        publishPending: true,
      };
    });
    expect(screen.getByRole('button', { name: 'Done' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Reconnect' })).toBeDisabled();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(store.modal.value).toBe('commit');
    act(() => {
      store.commitStore.state.value = {
        ...store.commitStore.state.peek(),
        publishBusy: false,
        publishPending: false,
        publishResult: { pushed: true, pr_url: 'https://github.com/test/repo/pull/1' },
      };
    });
    const link = screen.getByRole('link', { name: /Open pull request/ });
    expect(link).toHaveAttribute('href', 'https://github.com/test/repo/pull/1');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    expect(screen.getByRole('button', { name: 'Pushed' })).toBeDisabled();
    act(() => {
      store.commitStore.state.value = {
        ...store.commitStore.state.peek(),
        publishResult: { pr_url: 'javascript:alert(1)' },
      };
    });
    expect(screen.queryByRole('link', { name: /Open pull request/ })).not.toBeInTheDocument();
  });
});
