import { render, screen, waitFor, within } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { NewChatControls, Transcript } from './Transcript';
import { Composer } from './Composer';
import { ApprovalsModal } from './ApprovalsModal';
import type { AppConfig } from '../app/config';

const config: AppConfig = {
  ...testConfig,
  agentNames: [
    'widget-builder',
    'reviewer',
    'developer',
    'artist',
    'sysadmin',
    'extension-builder',
  ],
  projectDefaultAgent: 'developer',
  agentPickerCommon: ['developer', 'sysadmin', 'extension-builder', 'widget-builder'],
  agentApprovalActions: {
    '': false,
    developer: true,
    sysadmin: true,
    'extension-builder': true,
    'widget-builder': true,
    reviewer: true,
    artist: true,
  },
};

beforeAll(async () => {
  await NewChatControls.preload();
});

const stores: AppStore[] = [];

function createStore(overrides: Partial<AppConfig> = {}): AppStore {
  const store = new AppStore({ ...config, ...overrides });
  stores.push(store);
  store.projects.value = [
    { id: 'p1', name: 'Alpha', archived: false, available: true, sessions: [] },
  ];
  store.projectsEnabled.value = true;
  return store;
}

function draftStore(overrides: Partial<AppConfig> = {}): AppStore {
  const store = createStore(overrides);
  store.sessions.value = [];
  store.activeSessionId.value = '';
  store.draftActive.value = true;
  return store;
}

afterEach(() => {
  for (const store of stores.splice(0)) store.dispose();
  localStorage.clear();
});

describe('new-chat agent picker', () => {
  it('renders Chat, the common block, one separator, then the alphabetical remainder', async () => {
    const store = draftStore();
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    const agent = screen.getByRole('button', { name: 'Choose agent for new chat' });
    expect(agent).toHaveTextContent('AgentChat');
    await userEvent.click(agent);
    const listbox = screen.getByRole('listbox', { name: 'Choose agent for new chat' });
    expect(
      within(listbox)
        .getAllByRole('option')
        .map((option) => option.textContent),
    ).toEqual([
      'Chat',
      'developer',
      'sysadmin',
      'extension-builder',
      'widget-builder',
      'artist',
      'reviewer',
    ]);
    expect(container.querySelectorAll('.chip-popover-separator')).toHaveLength(1);
    expect(container.querySelector('.chip-popover-separator')?.nextElementSibling).toHaveAttribute(
      'data-value',
      'agent:artist',
    );
  });

  it('filters both blocks, keeps their order, and drops the separator when a block empties', async () => {
    const store = draftStore();
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    await userEvent.click(screen.getByRole('button', { name: 'Choose agent for new chat' }));
    const filter = screen.getByRole('searchbox', { name: 'Filter options' });
    await userEvent.type(filter, 'review');
    expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual(['reviewer']);
    expect(container.querySelector('.chip-popover-separator')).toBeNull();

    await userEvent.clear(filter);
    await userEvent.type(filter, 'dev');
    expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual([
      'developer',
    ]);
    await userEvent.clear(filter);
    await userEvent.type(filter, 'e');
    expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual([
      'developer',
      'extension-builder',
      'widget-builder',
      'reviewer',
    ]);
    // `artist` (the first alphabetical remainder) is filtered out, so the
    // divider must still separate the surviving remainder from the common block.
    expect(container.querySelectorAll('.chip-popover-separator')).toHaveLength(1);
  });

  it('switches to the project default when a project is selected and back to Chat', async () => {
    const store = draftStore();
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    await userEvent.click(screen.getByRole('button', { name: 'Choose chat or project' }));
    await userEvent.click(screen.getByRole('option', { name: 'Alpha' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Choose agent for new chat' })).toHaveTextContent(
        'Agentdeveloper',
      ),
    );
    expect(store.selectedAgent.value).toBe('');
    expect(store.effectiveAgent.value).toBe('developer');

    await userEvent.click(screen.getByRole('button', { name: 'Choose agent for new chat' }));
    expect(screen.queryByRole('option', { name: 'Chat' })).toBeNull();
    await userEvent.keyboard('{Escape}');

    await userEvent.click(screen.getByRole('button', { name: 'Choose chat or project' }));
    await userEvent.click(screen.getByRole('option', { name: 'No project' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Choose agent for new chat' })).toHaveTextContent(
        'AgentChat',
      ),
    );
    expect(store.selectedAgent.value).toBe('');
  });

  it('marks an auto-resolved agent explicit when it is chosen from the picker', async () => {
    const store = draftStore();
    store.activeProjectId.value = 'p1';
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    const agent = await screen.findByRole('button', { name: 'Choose agent for new chat' });
    expect(agent).toHaveTextContent('Agentdeveloper');
    await userEvent.click(agent);
    await userEvent.click(screen.getByRole('option', { name: 'developer' }));
    expect(store.selectedAgent.value).toBe('developer');
    expect(localStorage.getItem(store.keys.selectedAgent)).toBe('developer');
    expect(store.agentAutomatic.value).toBe(false);
  });

  it('shows a pinned server agent without offering a picker', () => {
    const store = draftStore({
      agentName: 'pinned-agent',
      agentNames: [],
      projectDefaultAgent: '',
    });
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(screen.queryByRole('button', { name: 'Choose agent for new chat' })).toBeNull();
    expect(container.querySelector('.new-chat-agent-pinned')).toHaveTextContent('pinned-agent');
  });
});

describe('approval capability', () => {
  function sessionStore(actions: boolean | undefined): AppStore {
    const store = createStore();
    store.draftActive.value = false;
    store.sessions.value = [
      testSession({
        approvalDefaultMode: 'auto',
        approvalRequestedMode: 'auto',
        approvalEffectiveMode: 'prompt',
        guardianAvailable: false,
        guardianAutoSuspended: false,
        approvalActions: actions,
      }),
    ];
    store.activeSessionId.value = 's1';
    return store;
  }

  it.each([
    [false, false],
    [true, true],
    [undefined, true],
  ])('renders the Guardian chip for approval_actions=%s', (actions, visible) => {
    const store = sessionStore(actions);
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    expect(Boolean(container.querySelector('.approval-mode-chip'))).toBe(visible);
  });

  it('shows the no-tools notice for a tool-less Chat draft without Guardian warnings', () => {
    const store = draftStore();
    render(
      <StoreContext.Provider value={store}>
        <ApprovalsModal />
      </StoreContext.Provider>,
    );
    expect(screen.getByText(/no tools that require approval/i)).toBeInTheDocument();
    expect(screen.queryByText(/Guardian is unavailable/i)).toBeNull();
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  it('keeps approval controls available for a tool-enabled project draft', () => {
    const store = draftStore();
    store.activeProjectId.value = 'p1';
    render(
      <StoreContext.Provider value={store}>
        <ApprovalsModal />
      </StoreContext.Provider>,
    );
    expect(screen.queryByText(/no tools that require approval/i)).toBeNull();
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
    expect(screen.getByRole('radio', { name: /Prompt/ })).toBeEnabled();
  });
});
