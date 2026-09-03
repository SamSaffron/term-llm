import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { App } from '../app/App';
import { AppStore } from '../stores/app-store';
import { APIError } from '../api/client';
import type { SessionShareResponse } from '../api/endpoints';
import { Transcript } from './Transcript';
import { Composer } from './Composer';
import { Markdown } from './Markdown';
import { Modals } from './Modals';
import { Sidebar } from './Sidebar';
import { Header } from './Header';
import { DiffSidebar, PlanSurface } from './Panels';
import { ChipPicker } from './ChipPicker';
import { Lightbox } from './Lightbox';
import type { AppConfig } from '../app/config';
import { initialProjection } from '../domain/response';
import { convertServerMessages } from '../domain/transcript';
import { markdownDocumentBlocks } from '../domain/markdown-document';
import { readJSON } from '../platform/storage';

const config: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: ['jarvis'],
  title: '',
  locationSharing: true,
  worktrees: true,
  hub: null,
  vapidKey: '',
  webRTC: false,
  signalingURL: '',
};
const createStore = () => {
  const store = new AppStore(config);
  store.sessions.value = [
    {
      id: 's1',
      title: 'Test',
      name: '',
      mode: 'chat',
      origin: 'web',
      archived: false,
      pinned: false,
      created: 1,
      lastMessageAt: 1,
      messages: [
        { id: 'u1', role: 'user', content: 'Question', created: 1 },
        { id: 'a1', role: 'assistant', content: '**Answer**', created: 2 },
        {
          id: 't1',
          role: 'tool-group',
          content: '',
          created: 3,
          tools: [
            {
              id: 'call',
              name: 'read_file',
              arguments: '{"path":"x"}',
              status: 'done',
              result: 'ok',
            },
          ],
        },
      ],
    },
  ];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.endpoints.diffComments = vi.fn(async () => ({ comments: [], transcript_rev: 0 }));
  return store;
};

const expectPasswordManagersIgnored = (element: HTMLElement) => {
  expect(element).toHaveAttribute('autocomplete', 'off');
  expect(element).toHaveAttribute('data-1p-ignore', 'true');
  expect(element).toHaveAttribute('data-bwignore', 'true');
  expect(element).toHaveAttribute('data-form-type', 'other');
  expect(element).toHaveAttribute('data-lpignore', 'true');
  expect(element).toHaveAttribute('data-protonpass-ignore', 'true');
};

describe('Preact-owned chat surfaces', () => {
  it('shows the MCP count for a new chat before a session exists', () => {
    const store = createStore();
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.mcp.value = {
      ownerId: store.composer.runtimeDraftId(),
      servers: [],
      enabled: ['github'],
      loading: false,
      pending: '',
      error: '',
    };

    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('button', { name: 'Manage MCP servers' })).toHaveTextContent('MCP 1');
  });

  it('shows a persistent Shell return control for the active conversation', async () => {
    const store = createStore();
    store.shellStore.enabled.value = true;
    store.shellStore.sessionId.value = 's1';
    store.shellStore.shellId.value = 'sh_one';
    store.shellStore.status.value = 'running';
    store.prompt.value = 'unfinished message';
    const show = vi.spyOn(store.shellStore, 'show');

    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    const shell = screen.getByRole('button', { name: 'Return to shell' });
    expect(shell).toHaveTextContent('Shell');
    await userEvent.click(shell);
    expect(show).toHaveBeenCalledWith('s1');
    expect(store.shellStore.visible.value).toBe(true);
    expect(screen.queryByRole('button', { name: 'Return to shell' })).not.toBeInTheDocument();
    expect(store.prompt.value).toBe('unfinished message');
  });

  it('restores the compact runtime chip and unified runtime popover', async () => {
    const store = createStore();
    store.providers.value = [
      {
        id: 'openai',
        name: 'openai',
        is_default: true,
        default_model: 'openai/gpt-5',
        models: ['openai/gpt-5'],
      },
    ];
    store.models.value = [
      {
        id: 'openai/gpt-5',
        name: 'openai/gpt-5',
        provider: 'openai',
        reasoning_efforts: ['low', 'medium', 'high'],
      },
    ];
    store.selectedEffort.value = 'medium';
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );
    const trigger = screen.getByRole('button', { name: /^Runtime settings:/ });
    expect(trigger).toHaveAccessibleName('Runtime settings: gpt-5, medium effort');
    expect(trigger).toHaveTextContent('gpt-5');
    expect(trigger.querySelectorAll('.effort-meter-bar')).toHaveLength(4);
    expect(trigger).toHaveAttribute('data-effort-level', 'medium');
    await userEvent.click(trigger);
    const dialog = screen.getByRole('dialog', { name: 'Runtime settings' });
    expect(dialog.tagName).toBe('DIALOG');
    expect(dialog).toHaveAttribute('open');
    expect(dialog).toHaveTextContent('Provider, model, effort, and speed for the next reply');
    const closeRuntime = screen.getByRole('button', { name: 'Close runtime settings' });
    expect(closeRuntime).toHaveFocus();
    expect(screen.getByRole('combobox', { name: 'Provider' })).not.toHaveFocus();
    expect(screen.getByRole('combobox', { name: 'Provider' })).toHaveValue('');
    expect(screen.getByRole('combobox', { name: 'Runtime model' })).toHaveValue('');
    expect(screen.getByRole('combobox', { name: 'Reasoning effort' })).toHaveTextContent('high');
    expect(screen.queryByRole('button', { name: /paths/ })).not.toBeInTheDocument();
    act(() => {
      store.branchPathCount.value = 2;
    });
    expect(screen.getByRole('button', { name: '2 paths' })).toBeInTheDocument();
    fireEvent(dialog, new Event('cancel', { cancelable: true }));
    expect(dialog).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('offers fast mode only for supported models and adds it to the runtime label', async () => {
    const store = createStore();
    store.selectedProvider.value = 'chatgpt';
    store.selectedModel.value = 'gpt-5.6-sol';
    store.models.value = [
      {
        id: 'gpt-5.6-sol',
        name: 'gpt-5.6-sol',
        provider: 'chatgpt',
        service_tiers: [{ id: 'priority', name: 'fast' }],
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    const trigger = screen.getByRole('button', { name: /^Runtime settings:/ });
    expect(trigger).toHaveTextContent('gpt-5.6-sol');
    await userEvent.click(trigger);
    const fast = screen.getByRole('switch', { name: 'Fast mode' });
    expect(screen.getByText('Fast')).toHaveClass('runtime-popover-label');
    expect(fast).toHaveAttribute('aria-checked', 'false');

    await userEvent.click(fast);

    expect(store.selectedFast.value).toBe(true);
    expect(fast).toHaveAttribute('aria-checked', 'true');
    expect(trigger).toHaveTextContent('gpt-5.6-sol-fast');
    expect(trigger).toHaveAccessibleName('Runtime settings: gpt-5.6-sol-fast');
  });

  it('inherits provider-enforced fast mode without showing a toggle', async () => {
    const store = createStore();
    store.selectedProvider.value = 'chatgpt';
    store.selectedModel.value = 'gpt-5.6-sol';
    store.providers.value = [
      {
        id: 'chatgpt',
        name: 'chatgpt',
        service_tier: 'priority',
      },
    ];
    store.models.value = [
      {
        id: 'gpt-5.6-sol',
        name: 'gpt-5.6-sol',
        provider: 'chatgpt',
        service_tiers: [{ id: 'priority', name: 'fast' }],
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    const trigger = screen.getByRole('button', {
      name: 'Runtime settings: gpt-5.6-sol-fast',
    });
    expect(trigger).toHaveTextContent('gpt-5.6-sol-fast');
    await userEvent.click(trigger);

    expect(screen.queryByRole('switch', { name: 'Fast mode' })).not.toBeInTheDocument();
  });

  it('hides fast mode for a model without fast-tier metadata', async () => {
    const store = createStore();
    store.selectedModel.value = 'plain-model';
    store.models.value = [{ id: 'plain-model', name: 'plain-model' }];
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: /^Runtime settings:/ }));

    expect(screen.queryByRole('switch', { name: 'Fast mode' })).not.toBeInTheDocument();
  });

  it('shows the worktree browser for every chat attached to an available Git project', async () => {
    const store = createStore();
    store.sessions.value = [
      { ...store.sessions.value[0], projectId: 'project-1', projectName: 'Project' },
    ];
    store.projectsEnabled.value = true;
    store.worktreesEnabled.value = false;
    store.projects.value = [
      {
        id: 'project-1',
        name: 'Project',
        archived: false,
        available: true,
        git: true,
        sessions: [],
      },
    ];
    store.endpoints.projectWorktrees = vi.fn(async () => ({ worktrees: [] }));

    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: 'Worktree' }));
    expect(store.modal.value).toBe('worktrees');
    expect(store.endpoints.projectWorktrees).toHaveBeenCalledWith('project-1');
  });

  it('shows root after merging the active worktree, ignoring stale draft state', async () => {
    const store = createStore();
    store.worktreesEnabled.value = true;
    store.selectedDraftWorktree.value = '/worktrees/stale-draft';
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        projectId: 'project-1',
        workingDir: '/worktrees/feature',
        worktreeDir: '/worktrees/feature',
      },
    ];
    store.endpoints.mergeWorktree = vi.fn(async () => ({
      result: { root_dir: '/repo' },
      cleanup: { removed: true },
      session: { id: 's1', cwd: '/repo', worktree_dir: '' },
    }));
    store.endpoints.legacyWorktrees = vi.fn(async () => ({ worktrees: [] }));

    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    const trigger = screen.getByRole('button', { name: 'Worktree' });
    expect(trigger).toHaveTextContent('feature');

    await act(async () => {
      await store.mergeWorktree('/worktrees/feature');
    });

    expect(store.endpoints.mergeWorktree).toHaveBeenCalledWith(
      'project-1',
      '/worktrees/feature',
      's1',
      false,
    );
    expect(trigger).toHaveTextContent('root');
    expect(trigger).not.toHaveTextContent('stale-draft');
  });

  it('shows the active plan position and semantic checklist', async () => {
    const store = createStore();
    store.currentPlan.value = {
      explanation: 'Make the plan easy to follow.',
      plan: [
        { step: 'Inspect the old flow', status: 'completed' },
        { step: 'Polish the responsive surface', status: 'in_progress' },
        { step: 'Verify the result', status: 'pending' },
      ],
    };
    render(
      <StoreContext.Provider value={store}>
        <Header />
        <PlanSurface />
      </StoreContext.Provider>,
    );

    const toggle = screen.getByRole('button', { name: /Open current plan/ });
    expect(toggle).toHaveTextContent('Plan2/3');
    expect(toggle).toHaveAccessibleName(/Step 2 of 3, 1 of 3 complete/);

    await userEvent.click(toggle);
    expect(store.planVisible.value).toBe(true);
    expect(toggle).toHaveAttribute('aria-controls', 'planSurface');
    expect(screen.getByRole('complementary', { name: 'Current plan' })).toBeInTheDocument();
    expect(screen.getByText('Step 2 of 3')).toBeInTheDocument();
    expect(screen.getByText('Polish the responsive surface').closest('li')).toHaveAttribute(
      'aria-current',
      'step',
    );
  });

  it('collapses a completed plan to a tick-only header action', () => {
    const store = createStore();
    store.currentPlan.value = {
      plan: [
        { step: 'Build the plan', status: 'completed' },
        { step: 'Verify the plan', status: 'completed' },
      ],
    };

    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    const toggle = screen.getByRole('button', { name: /Open current plan/ });
    expect(toggle).toHaveClass('complete');
    expect(toggle).toHaveAccessibleName('Open current plan. All 2 steps complete');
    expect(toggle).toHaveAttribute('title', 'All 2 steps complete');
    expect(toggle).toHaveTextContent('');
    expect(toggle.querySelector('.plan-toggle-check')).not.toBeNull();
    expect(toggle.querySelector('.plan-toggle-word')).toBeNull();
    expect(toggle.querySelector('.plan-toggle-progress')).toBeNull();
  });

  it('presents every plan step in a dismissible modal sheet on mobile', async () => {
    vi.mocked(window.matchMedia).mockReturnValue({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList);
    const store = createStore();
    store.currentPlan.value = {
      plan: [
        { step: 'Inspect the existing sheet', status: 'completed' },
        { step: 'Build the sheet', status: 'completed' },
        { step: 'Test dismissal', status: 'in_progress' },
        { step: 'Verify the result', status: 'pending' },
      ],
    };
    const { container } = render(
      <StoreContext.Provider value={store}>
        <div id="appShell">
          <aside id="sidebar" />
          <main id="appMain">
            <Header />
          </main>
          <PlanSurface />
        </div>
      </StoreContext.Provider>,
    );

    const toggle = screen.getByRole('button', { name: /Open current plan/ });
    toggle.focus();
    await userEvent.click(toggle);
    const sheet = screen.getByRole('dialog', { name: 'Current plan' });
    const planSurface = container.querySelector('#planSurface');
    expect(sheet).toHaveClass('plan-sheet', 'open');
    expect(planSurface).toHaveClass('plan-surface', 'plan-sheet-content', 'open');
    expect(planSurface).not.toHaveClass('plan-sheet');
    expect(screen.getByText('Inspect the existing sheet')).toBeVisible();
    expect(screen.getByText('Build the sheet')).toBeVisible();
    expect(planSurface?.querySelectorAll('.current-plan-step-completed')).toHaveLength(2);
    await waitFor(() => {
      expect(container.querySelector('#appMain')).toHaveProperty('inert', true);
      expect(container.querySelector('#sidebar')).toHaveProperty('inert', true);
    });

    fireEvent.keyDown(sheet, { key: 'Escape' });
    await waitFor(() => {
      expect(store.planOpen.value).toBe(false);
      expect(toggle).toHaveFocus();
      expect(container.querySelector('#appMain')).not.toHaveProperty('inert', true);
    });

    await userEvent.click(toggle);
    await screen.findByRole('dialog', { name: 'Current plan' });
    const backdrop = container.querySelector('.drawer-backdrop')!;
    fireEvent.pointerDown(backdrop, { pointerId: 1 });
    fireEvent.pointerUp(backdrop, { pointerId: 1 });
    expect(store.planOpen.value).toBe(false);
  });

  it('dismisses a tablet plan panel on an outside tap without modalizing chat', () => {
    vi.mocked(window.matchMedia).mockImplementation(
      (query) =>
        ({
          matches: query === '(max-width: 1099px)',
          addEventListener: vi.fn(),
          removeEventListener: vi.fn(),
        }) as unknown as MediaQueryList,
    );
    const store = createStore();
    store.currentPlan.value = {
      plan: [{ step: 'Check outside dismissal', status: 'in_progress' }],
    };
    store.planOpen.value = true;
    const { container } = render(
      <StoreContext.Provider value={store}>
        <div id="appShell">
          <main id="appMain" />
          <PlanSurface />
        </div>
      </StoreContext.Provider>,
    );

    expect(screen.queryByRole('dialog', { name: 'Current plan' })).toBeNull();
    fireEvent.pointerDown(container.querySelector('#appMain')!);

    expect(store.planOpen.value).toBe(false);
    expect(container.querySelector('#appMain')).not.toHaveProperty('inert', true);
    vi.mocked(window.matchMedia).mockReturnValue({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList);
  });

  it('keeps the mobile sidebar backdrop interactive and dismisses immediately', async () => {
    vi.mocked(window.matchMedia).mockReturnValue({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList);
    const store = createStore();
    store.sidebarOpen.value = true;
    const { container } = render(
      <StoreContext.Provider value={store}>
        <div id="appShell">
          <Sidebar />
          <main id="appMain" />
        </div>
      </StoreContext.Provider>,
    );
    const backdrop = container.querySelector<HTMLElement>('#sidebarBackdrop')!;
    const dialog = screen.getByRole('dialog', { name: 'Sessions' });

    await waitFor(() => expect(dialog).toHaveFocus());
    expect(screen.getByRole('button', { name: 'Close sidebar' })).not.toHaveFocus();
    await waitFor(() => expect(container.querySelector('#appMain')).toHaveProperty('inert', true));
    expect(backdrop.inert).not.toBe(true);
    await userEvent.click(backdrop);

    expect(store.sidebarOpen.value).toBe(false);
    await waitFor(() => expect(container.querySelector('#appMain')).toHaveProperty('inert', false));
    vi.mocked(window.matchMedia).mockReturnValue({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList);
  });

  it('only animates sidebar groups after a user expands them', async () => {
    const store = createStore();
    store.projectsEnabled.value = true;
    store.setSidebarView('projects');
    const projectSession = {
      ...store.sessions.value[0],
      projectId: 'p1',
      projectName: 'Alpha',
    };
    store.sessions.value = [projectSession];
    store.projects.value = [
      {
        id: 'p1',
        name: 'Alpha',
        path: '/tmp/alpha',
        available: true,
        sessions: [projectSession],
        sessionCount: 1,
      },
    ];
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    expect(container.querySelectorAll('.is-opening')).toHaveLength(0);

    // A catalog subscriber can still receive fresh identities from another
    // update path. That rerender must not look like a user-triggered expansion.
    act(() => {
      store.sessions.value = [{ ...projectSession }];
      store.projects.value = store.projects.value.map((project) => ({
        ...project,
        sessions: project.sessions?.map((session) => ({ ...session })),
      }));
    });
    expect(container.querySelectorAll('.is-opening')).toHaveLength(0);

    const projectsToggle = screen.getByRole('button', { name: 'Projects', exact: true });
    await userEvent.click(projectsToggle);
    await userEvent.click(projectsToggle);
    const list = container.querySelector('.sidebar-project-groups > .session-group-list')!;
    expect(list).toHaveClass('is-opening');

    fireEvent.animationEnd(list, { animationName: 'project-list-reveal' });
    expect(list).not.toHaveClass('is-opening');
  });

  it('renders /side as one private Markdown conversation with an accessible composer', async () => {
    const store = createStore();
    store.modal.value = 'side';
    store.sideQuestion.value = {
      sessionId: 's1',
      loading: false,
      running: false,
      draft: '',
      question: '',
      response: '',
      error: 'A useful error',
      history: [
        { question: 'What changed?', response: '**A polished side flow.**' },
        { question: 'Is it private?', response: 'Yes — it stays out of the transcript.' },
      ],
    };
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('dialog', { name: 'Side question' })).toBeInTheDocument();
    expect(container.querySelectorAll('.side-question-transcript')).toHaveLength(1);
    expect(container.querySelectorAll('.side-question-exchange')).toHaveLength(2);
    expect(container.querySelector('.message.assistant .markdown-body strong')).toHaveTextContent(
      'A polished side flow.',
    );
    expect(screen.getByLabelText('Ask a side question')).toHaveAttribute(
      'placeholder',
      'Ask about this conversation…',
    );
    expect(screen.getByRole('button', { name: 'Send side question' })).toBeDisabled();
    expect(screen.getByRole('alert')).toHaveTextContent('A useful error');
    expect(container.querySelector('.side-question-transcript')).not.toHaveAttribute('aria-live');
  });

  it('uses Escape to stop, clear the draft, and then close /side', () => {
    const store = createStore();
    store.modal.value = 'side';
    store.endpoints.cancelSideQuestion = vi.fn(async () => new Response());
    store.sideQuestion.value = {
      sessionId: 's1',
      loading: false,
      running: true,
      draft: '',
      question: 'Explain this',
      response: 'Working',
      error: '',
      history: [],
    };
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Side question' }), { key: 'Escape' });
    expect(store.modal.value).toBe('side');
    expect(store.sideQuestion.value.running).toBe(false);
    expect(screen.getByLabelText('Ask a side question')).toBeInTheDocument();

    act(() => store.setSideQuestionDraft('unfinished follow-up'));
    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Side question' }), { key: 'Escape' });
    expect(store.modal.value).toBe('side');
    expect(store.sideQuestion.value.draft).toBe('');

    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Side question' }), { key: 'Escape' });
    expect(store.modal.value).toBe('');
    expect(screen.queryByRole('dialog', { name: 'Side question' })).not.toBeInTheDocument();
  });

  it('renders legacy additions and deletions instead of one combined diff count', () => {
    const store = createStore();
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        fileChangeSummary: { fileCount: 2, additions: 2, deletions: 2, git: true },
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );
    const toggle = screen.getByRole('button', {
      name: 'Toggle file changes: 2 changed files (+2 −2)',
    });
    expect(toggle.querySelector('.diff-toggle-stat-add')).toHaveTextContent('+2');
    expect(toggle.querySelector('.diff-toggle-stat-del')).toHaveTextContent('−2');
    expect(toggle.querySelector('.diff-toggle-badge')).not.toHaveTextContent('4');
    expect(toggle.closest('.header-controls-row')).not.toBeNull();
  });

  it('replaces header diff totals with the file icon while the changes panel is open', () => {
    const store = createStore();
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        fileChangeSummary: { fileCount: 2, additions: 2, deletions: 2, git: true },
      },
    ];
    store.diff.value = { ...store.diff.value, open: true };
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    const toggle = screen.getByRole('button', {
      name: 'Toggle file changes: 2 changed files (+2 −2)',
    });
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(toggle.querySelector('.diff-toggle-file-icon')).toBeInTheDocument();
    expect(toggle.querySelector('.diff-toggle-stat-add')).not.toBeInTheDocument();
    expect(toggle.querySelector('.diff-toggle-stat-del')).not.toBeInTheDocument();
  });

  it('resizes the grid column with the changes panel handle', () => {
    const store = createStore();
    store.startupDone.value = true;
    store.bootstrap = vi.fn(async () => undefined);
    store.diff.value = { ...store.diff.value, open: true, width: 555 };
    const { container } = render(<App store={store} />);
    const shell = container.querySelector<HTMLElement>('#appShell')!;
    const sidebar = container.querySelector<HTMLElement>('#diffSidebar')!;
    const handle = screen.getByRole('separator', { name: 'Resize changes panel' });

    expect(shell.style.getPropertyValue('--diff-sidebar-user-width')).toBe('555px');
    expect(sidebar.style.width).toBe('');

    fireEvent.pointerDown(handle, { clientX: 600, pointerId: 1 });
    expect(shell).toHaveClass('diff-resizing');
    fireEvent.pointerMove(window, { clientX: 500, pointerId: 1 });
    expect(store.diff.value.width).toBe(655);
    expect(shell.style.getPropertyValue('--diff-sidebar-user-width')).toBe('655px');
    expect(sidebar.style.width).toBe('');
    fireEvent.pointerUp(window, { clientX: 500, pointerId: 1 });
    expect(shell).not.toHaveClass('diff-resizing');
  });

  it('removes low-value changed-file navigation controls', () => {
    const store = createStore();
    store.diff.value = { ...store.diff.value, open: true };
    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    const actions = [...container.querySelectorAll('.diff-sidebar-header > .icon-btn')].map(
      (button) => button.getAttribute('aria-label'),
    );
    expect(actions).toEqual(['Expand all files', 'Maximize changes', 'Hide changes']);
    expect(screen.queryByRole('button', { name: 'Previous changed file' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Next changed file' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Follow current file' })).toBeNull();
  });

  it('keeps only fullscreen and close icon actions in the mobile changes header', () => {
    vi.mocked(window.matchMedia).mockImplementation(
      (query) =>
        ({
          matches: query === '(max-width: 767px)',
          addEventListener: vi.fn(),
          removeEventListener: vi.fn(),
        }) as unknown as MediaQueryList,
    );
    const store = createStore();
    store.diff.value = { ...store.diff.value, open: true };
    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    const actions = [...container.querySelectorAll('.diff-sidebar-header > .icon-btn')].map(
      (button) => button.getAttribute('aria-label'),
    );
    expect(actions).toEqual(['Maximize changes', 'Hide changes']);
    expect(screen.getByRole('button', { name: 'Change scope' })).toBeInTheDocument();
  });

  it('keeps internal file-tracking diagnostics out of the changes panel', () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      unavailableLineCountFiles: 2,
      files: [
        {
          path: 'missing.ts',
          status: 'modify',
          expanded: true,
          truncated: true,
          contentStatus: 'retention_unavailable',
          lines: [],
        },
      ],
      materializations: [
        {
          id: 2,
          classification: 'materialized',
          root: '/tmp/output',
          createdCount: 14,
          modifiedCount: 3,
          deletedCount: 0,
          sampledPaths: ['/tmp/output/result.bin'],
          samplesTruncated: false,
          coverageStatus: 'complete',
          eventSeq: 2,
        },
      ],
      observations: [
        {
          id: 1,
          classification: 'observed',
          root: '/tmp/worktree',
          createdCount: 0,
          modifiedCount: 0,
          deletedCount: 0,
          sampledPaths: ['/tmp/worktree/app.ts'],
          samplesTruncated: false,
          coverageStatus: 'complete',
          eventSeq: 1,
        },
      ],
      claimDiagnostics: [
        {
          normalizedPattern: '/tmp/worktree/app.ts',
          claimKind: 'transform',
          reason: 'claim_noop',
          coverageStatus: 'complete',
          matchingPathCount: 0,
        },
      ],
    };

    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    expect(screen.getByText('Diff unavailable.')).toBeInTheDocument();
    expect(screen.queryByText(/retention/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/line counts may be partial/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/partial \(2\)/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/Materialized outputs/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/no authored line totals/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/Observed side effects/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/Output claim diagnostics/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/claim_noop/i)).not.toBeInTheDocument();
  });

  it('offers directional context controls above and below a partial diff', async () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      files: [
        {
          path: 'main.go',
          status: 'modify',
          expanded: true,
          context: 3,
          oldLineCount: 80,
          newLineCount: 82,
          lines: [
            { kind: 'hunk', content: '@@ -20 +20 @@' },
            { kind: 'context', content: 'func main() {', oldLine: 20, newLine: 20 },
            { kind: 'add', content: '  run()', newLine: 21 },
          ],
        },
      ],
    };
    store.expandDiff = vi.fn(async () => undefined);
    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    const above = screen.getByRole('button', { name: 'Show more context above' });
    const below = screen.getByRole('button', { name: 'Show more context below' });
    const rows = container.querySelector('.diff-rows')!;
    expect(above.nextElementSibling).toBe(rows);
    expect(rows.nextElementSibling).toBe(below);
    expect(screen.queryByRole('button', { name: /Show full file/i })).not.toBeInTheDocument();

    await userEvent.click(above);
    await userEvent.click(below);
    expect(store.expandDiff).toHaveBeenNthCalledWith(1, store.diff.value.files[0], 12);
    expect(store.expandDiff).toHaveBeenNthCalledWith(2, store.diff.value.files[0], 12);
  });

  it('matches the legacy diff scope popover and syntax-highlights code rows', async () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      git: true,
      files: [
        {
          path: 'job.rb',
          status: 'modify',
          additions: 1,
          deletions: 0,
          expanded: true,
          lang: 'rb',
          lines: [
            { kind: 'context', content: 'def perform', oldLine: 1, newLine: 1 },
            { kind: 'add', content: '  puts "done"', newLine: 2 },
          ],
        },
      ],
    };
    store.endpoints.fileChanges = vi.fn(async () => ({
      git: true,
      scope: 'uncommitted',
      file_changes: [],
    }));
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    const scope = screen.getByRole('button', { name: 'Change scope' });
    expect(scope).toHaveTextContent('Last turn');
    await userEvent.click(scope);
    const picker = screen.getByRole('dialog', { name: 'Change scope' });
    expect(picker.querySelector('[aria-selected="true"]')).toHaveTextContent('Last turn');
    await vi.waitFor(() =>
      expect(document.querySelector('.diff-code .hljs-keyword')).toHaveTextContent('def'),
    );
    await userEvent.click(screen.getByRole('option', { name: 'Uncommitted' }));
    expect(store.diff.value.scope).toBe('uncommitted');
    expect(store.endpoints.fileChanges).toHaveBeenCalledWith('s1', 'uncommitted');
  });

  it('renders managed worktree patches in the highlighted Changes panel', async () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      git: true,
      worktreeDir: '/worktrees/feature',
      worktreeTitle: 'feature',
      readOnly: true,
      files: [
        {
          path: 'app.ts',
          status: 'modify',
          additions: 1,
          deletions: 1,
          expanded: true,
          lang: 'ts',
          lines: [
            { kind: 'context', content: 'function feature() {', oldLine: 1, newLine: 1 },
            { kind: 'delete', content: '  const oldValue = 1;', oldLine: 2 },
            { kind: 'add', content: '  const newValue = 2;', newLine: 2 },
          ],
        },
      ],
    };

    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    expect(screen.getByText('feature changes')).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Change scope' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Comment on line 1' })).not.toBeInTheDocument();
    await vi.waitFor(() =>
      expect(document.querySelector('.diff-code .hljs-keyword')).toHaveTextContent('function'),
    );
  });

  it('keeps the Markdown view toggle visible when source availability refreshes', async () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      scope: 'uncommitted',
      git: true,
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          contentAvailable: true,
          expanded: false,
          lines: [{ kind: 'add', content: '# Plan', newLine: 1 }],
        },
      ],
    };
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('button', { name: 'Show rendered Markdown' })).toBeEnabled();
    act(() => {
      store.diff.value = {
        ...store.diff.value,
        files: store.diff.value.files.map((file) => ({ ...file, contentAvailable: false })),
      };
    });
    expect(screen.getByRole('button', { name: 'Show rendered Markdown' })).toBeDisabled();
    act(() => {
      store.diff.value = {
        ...store.diff.value,
        files: store.diff.value.files.map((file) => ({ ...file, contentAvailable: true })),
      };
    });
    expect(screen.getByRole('button', { name: 'Show rendered Markdown' })).toBeEnabled();
  });

  it('switches a Markdown file to a source-mapped rendered review without losing drafts', async () => {
    const store = createStore();
    const source = '# Title\n\nFirst line\nsecond line\n';
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      scope: 'last_turn',
      historyComments: [
        {
          id: 'sent-rendered',
          path: '/work/plan.md',
          side: 'new',
          line: 4,
          body: 'Existing line comment',
          sessionId: 's1',
          scope: 'last_turn',
        },
        {
          id: 'source-only',
          path: '/work/plan.md',
          side: 'new',
          line: 2,
          body: 'Comment on source-only line',
          sessionId: 's1',
          scope: 'last_turn',
        },
      ],
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          sequence: 8,
          snapshotSeq: 8,
          contentAvailable: true,
          expanded: true,
          lines: [{ kind: 'context', content: 'second line', oldLine: 4, newLine: 4 }],
          markdownPreview: {
            view: 'diff',
            side: 'after',
            source,
            blocks: markdownDocumentBlocks(source),
            sequence: 8,
            snapshotSeq: 8,
            scope: 'last_turn',
          },
        },
      ],
    };
    store.queueDiffComment = vi.fn();
    store.revealDiffLine = vi.fn(async () => undefined);
    store.expandDiff = vi.fn(async () => undefined);
    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    const viewToggle = await screen.findByRole('button', { name: 'Show rendered Markdown' });
    expect(viewToggle.closest('.diff-file-row')).not.toBeNull();
    expect(viewToggle.closest('.diff-file-actions')).not.toBeNull();
    expect(viewToggle.closest('.diff-file-body')).toBeNull();
    expect(container.querySelectorAll('.diff-file-actions .diff-action-btn svg')).toHaveLength(4);
    expect(viewToggle).toHaveAttribute('aria-pressed', 'false');
    expect(container.querySelector('[data-diff-anchor="new:4"]')).toHaveAttribute('tabindex', '-1');
    expect(screen.queryByText('Title', { selector: 'h1' })).not.toBeInTheDocument();
    await userEvent.click(viewToggle);
    expect(store.expandDiff).not.toHaveBeenCalled();
    expect(await screen.findByRole('button', { name: 'Show Markdown diff' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(await screen.findByText('Title', { selector: 'h1' })).toBeVisible();
    expect(screen.getByRole('region', { name: 'Source-only comments' })).toHaveTextContent(
      'Comment on source-only line',
    );
    await userEvent.click(screen.getByRole('button', { name: 'Reveal in Diff' }));
    expect(store.revealDiffLine).toHaveBeenCalledWith(
      expect.objectContaining({ path: '/work/plan.md' }),
      'new',
      2,
    );

    await userEvent.click(
      screen.getByRole('button', { name: 'Show 1 comment for Paragraph, lines 3–4' }),
    );
    expect(screen.getByText('Existing line comment')).toBeVisible();
    const revealActions = screen.getAllByRole('button', { name: 'Reveal in Diff' });
    expect(revealActions).toHaveLength(2);
    await userEvent.click(revealActions[0]);
    expect(store.revealDiffLine).toHaveBeenLastCalledWith(
      expect.objectContaining({ path: '/work/plan.md' }),
      'new',
      4,
    );
    const editor = screen.getByRole('textbox', { name: 'Inline comment' });
    await userEvent.type(editor, 'Rendered draft');
    await userEvent.click(screen.getByRole('button', { name: 'Show Markdown diff' }));
    await userEvent.click(screen.getByRole('button', { name: 'Show rendered Markdown' }));
    expect(screen.getByRole('textbox', { name: 'Inline comment' })).toHaveValue('Rendered draft');

    await userEvent.click(screen.getByRole('button', { name: 'More send options' }));
    await userEvent.click(screen.getByRole('menuitem', { name: /Queue comment/ }));
    expect(store.queueDiffComment).toHaveBeenCalledWith(
      expect.objectContaining({
        path: '/work/plan.md',
        side: 'new',
        line: 3,
        context: 'First line',
        contextBefore: ['# Title', ''],
        contextAfter: ['second line', ''],
        body: 'Rendered draft',
        fileChangeSeq: 8,
      }),
    );
  });

  it('renders Markdown while the Uncommitted file diff is still in flight', async () => {
    const store = createStore();
    const source = '# Immediate preview\n';
    let resolveDiff!: (value: Record<string, unknown>) => void;
    const pendingDiff = new Promise<Record<string, unknown>>((resolve) => {
      resolveDiff = resolve;
    });
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      scope: 'uncommitted',
      files: [
        {
          path: '/work/immediate.md',
          status: 'modify',
          sequence: 2,
          snapshotSeq: 2,
          contentAvailable: true,
          expanded: false,
        },
      ],
    };
    store.endpoints.fileDiff = vi.fn(() => pendingDiff);
    store.endpoints.fileText = vi.fn(async () => source);
    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    await userEvent.click(container.querySelector<HTMLElement>('.diff-file-row')!);
    expect(store.diff.value.files[0]).toMatchObject({ expanded: true, loading: true });
    await userEvent.click(screen.getByRole('button', { name: 'Show rendered Markdown' }));

    expect(store.endpoints.fileDiff).toHaveBeenCalledTimes(1);
    await vi.waitFor(() =>
      expect(store.endpoints.fileText).toHaveBeenCalledWith(
        's1',
        '/work/immediate.md',
        'uncommitted',
        'after',
        2,
        expect.any(AbortSignal),
      ),
    );
    expect(await screen.findByText('Immediate preview', { selector: 'h1' })).toBeVisible();
    expect(store.diff.value.files[0]).toMatchObject({
      expanded: true,
      loading: true,
      markdownPreview: { view: 'rendered' },
    });

    resolveDiff({
      kind: 'modify',
      content_available: true,
      old_line_count: 0,
      new_line_count: 1,
      hunks: [],
    });
    await vi.waitFor(() => expect(store.diff.value.files[0].loading).toBe(false));
    expect(screen.getByText('Immediate preview', { selector: 'h1' })).toBeVisible();
    expect(store.diff.value.files[0].markdownPreview?.view).toBe('rendered');
  });

  it('fails closed when a rendered draft block changes and allows explicit re-anchoring', async () => {
    const store = createStore();
    const source = '# Plan\n\nOriginal paragraph.\n';
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      scope: 'last_turn',
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          sequence: 3,
          snapshotSeq: 3,
          contentAvailable: true,
          expanded: true,
          lines: [{ kind: 'add', content: 'Original paragraph.', newLine: 3 }],
          markdownPreview: {
            view: 'rendered',
            side: 'after',
            source,
            blocks: markdownDocumentBlocks(source),
            sequence: 3,
            snapshotSeq: 3,
            scope: 'last_turn',
          },
        },
      ],
    };
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    await userEvent.click(
      await screen.findByRole('button', { name: 'Comment on Paragraph, line 3' }),
    );
    fireEvent.input(screen.getByRole('textbox', { name: 'Inline comment' }), {
      target: { value: 'Preserved draft' },
    });
    expect(screen.getByRole('button', { name: 'Send now' })).toBeEnabled();

    const changed = '# Plan\n\nChanged paragraph.\n';
    act(() => {
      store.diff.value = {
        ...store.diff.value,
        files: store.diff.value.files.map((file) => ({
          ...file,
          markdownPreview: file.markdownPreview
            ? {
                ...file.markdownPreview,
                source: changed,
                blocks: markdownDocumentBlocks(changed),
              }
            : undefined,
        })),
      };
    });

    expect(screen.getByRole('textbox', { name: 'Inline comment' })).toHaveValue('Preserved draft');
    expect(screen.getByRole('button', { name: 'Send now' })).toBeDisabled();
    await userEvent.click(screen.getByRole('button', { name: 'Re-anchor draft here' }));
    expect(screen.getByRole('button', { name: 'Send now' })).toBeEnabled();
  });

  it('keeps rendered history inspectable without offering new comments in read-only mode', async () => {
    const store = createStore();
    const source = '# Plan\n';
    store.diff.value = {
      ...store.diff.value,
      open: true,
      readOnly: true,
      sessionId: 's1',
      scope: 'last_turn',
      historyComments: [
        {
          id: 'history',
          path: '/work/plan.md',
          side: 'new',
          line: 1,
          body: 'Existing history',
          sessionId: 's1',
          scope: 'last_turn',
        },
      ],
      files: [
        {
          path: '/work/plan.md',
          status: 'modify',
          contentAvailable: true,
          expanded: true,
          markdownPreview: {
            view: 'rendered',
            side: 'after',
            source,
            blocks: markdownDocumentBlocks(source),
            sequence: 1,
            snapshotSeq: 1,
            scope: 'last_turn',
          },
        },
      ],
    };
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    expect(
      await screen.findByRole('button', { name: 'Show 1 comment for Heading, line 1' }),
    ).toBeVisible();
    expect(
      screen.queryByRole('button', { name: 'Comment on Heading, line 1' }),
    ).not.toBeInTheDocument();
    await userEvent.click(
      screen.getByRole('button', { name: 'Show 1 comment for Heading, line 1' }),
    );
    expect(screen.getByText('Existing history')).toBeVisible();
    expect(screen.queryByRole('textbox', { name: 'Inline comment' })).not.toBeInTheDocument();
  });

  it('offers immediate or queued delivery for inline review comments', async () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      scope: 'uncommitted',
      files: [
        {
          path: 'main.go',
          status: 'modify',
          additions: 2,
          deletions: 0,
          expanded: true,
          lines: [
            { kind: 'add', content: 'first change', newLine: 1 },
            { kind: 'add', content: 'second change', newLine: 2 },
          ],
        },
      ],
    };
    store.queueDiffComment = vi.fn();
    store.sendDiffComment = vi.fn(async () => undefined);
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: 'Comment on line 1' }));
    let firstEditor = screen.getByRole('textbox', { name: 'Inline comment' });
    expect(screen.getByRole('button', { name: 'Send now' })).toBeDisabled();
    await userEvent.type(firstEditor, 'Temporary draft');

    await userEvent.click(screen.getByRole('button', { name: 'Comment on line 1' }));
    expect(
      screen.queryByRole('region', { name: 'Inline comments for line 1' }),
    ).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Comment on line 1' }));
    firstEditor = screen.getByRole('textbox', { name: 'Inline comment' });
    expect(firstEditor).toHaveValue('Temporary draft');

    await userEvent.click(screen.getByRole('button', { name: 'Comment on line 2' }));
    expect(screen.getByRole('textbox', { name: 'Inline comment' })).toHaveValue('');
    await userEvent.click(screen.getByRole('button', { name: 'Comment on line 1' }));
    firstEditor = screen.getByRole('textbox', { name: 'Inline comment' });
    expect(firstEditor).toHaveValue('Temporary draft');

    await userEvent.click(screen.getByRole('button', { name: 'More send options' }));
    await userEvent.clear(firstEditor);
    await waitFor(() =>
      expect(screen.queryByText('Deliver later as one batch')).not.toBeInTheDocument(),
    );
    expect(screen.getByRole('button', { name: 'More send options' })).toBeDisabled();
    await userEvent.type(firstEditor, 'Queue this');
    expect(screen.getByRole('button', { name: 'Send now' })).toBeEnabled();
    await userEvent.click(screen.getByRole('button', { name: 'More send options' }));
    await userEvent.click(screen.getByRole('menuitem', { name: /Queue comment/ }));
    expect(store.queueDiffComment).toHaveBeenCalledWith(
      expect.objectContaining({ path: 'main.go', line: 1, body: 'Queue this' }),
    );
    expect(store.sendDiffComment).not.toHaveBeenCalled();
    expect(screen.getByRole('region', { name: 'Inline comments for line 1' })).toBeInTheDocument();
    firstEditor.focus();
    await userEvent.keyboard('{Escape}');
    expect(
      screen.queryByRole('region', { name: 'Inline comments for line 1' }),
    ).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Comment on line 1' })).toHaveFocus(),
    );
    expect(store.diff.value.open).toBe(true);

    await userEvent.click(screen.getByRole('button', { name: 'Comment on line 2' }));
    const secondEditor = screen.getByRole('textbox', { name: 'Inline comment' });
    await userEvent.type(secondEditor, 'Send this');
    await userEvent.keyboard('{Escape}');
    expect(secondEditor).toHaveValue('Send this');
    expect(store.diff.value.open).toBe(true);
    await userEvent.keyboard('{Control>}{Enter}{/Control}');
    expect(store.sendDiffComment).toHaveBeenCalledWith(
      expect.objectContaining({ path: 'main.go', line: 2, body: 'Send this' }),
    );
    expect(screen.getByRole('region', { name: 'Inline comments for line 2' })).toBeInTheDocument();
    expect(secondEditor).toHaveValue('');
    expect(screen.getByRole('button', { name: 'Send now' })).toBeDisabled();
  });

  it('leaves a per-line trail for sent and queued inline comments', async () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      sessionId: 's1',
      scope: 'last_turn',
      historyComments: [
        {
          id: 'sent',
          path: 'main.go',
          side: 'new',
          line: 1,
          body: 'Sent instruction',
          createdAt: Math.floor((Date.now() - 120_000) / 1000),
          sessionId: 's1',
          scope: 'last_turn',
        },
      ],
      comments: [
        {
          id: 'queued',
          path: 'main.go',
          side: 'new',
          line: 1,
          body: 'Queued instruction',
          sessionId: 's1',
          scope: 'last_turn',
        },
      ],
      files: [
        {
          path: 'main.go',
          status: 'modify',
          additions: 1,
          deletions: 0,
          expanded: true,
          lines: [{ kind: 'add', content: 'changed', newLine: 1 }],
        },
      ],
    };
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    const marker = screen.getByRole('button', { name: 'Show 2 inline comments for line 1' });
    expect(marker).toHaveClass('has-comments', 'queued');
    await userEvent.click(marker);
    expect(screen.getByText('Sent instruction')).toBeInTheDocument();
    expect(screen.getByText('Queued instruction')).toBeInTheDocument();
    expect(screen.getByText('Queued')).toBeInTheDocument();
    const timestamp = screen.getByText(/m ago/).closest('time');
    expect(timestamp).toHaveAttribute('datetime');
    expect(timestamp).toHaveAttribute('title');
  });

  it('uses aligned SVG diff actions and transient copied state', async () => {
    vi.useFakeTimers();
    const descriptor = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    try {
      const store = createStore();
      store.sessions.value = [
        { ...store.sessions.value[0], workingDir: '/home/sam/Source/term-llm' },
      ];
      store.diff.value = {
        ...store.diff.value,
        open: true,
        sessionId: 's1',
        files: [
          {
            path: '/home/sam/Source/term-llm/frontend/src/stores/app-store.ts',
            status: 'create',
            additions: 1,
            deletions: 0,
            provenance: 'direct',
            expanded: true,
            lines: [
              { kind: 'hunk', content: '@@ -1 +1 @@' },
              { kind: 'add', content: 'package main', newLine: 1 },
            ],
          },
        ],
      };
      const { container } = render(
        <StoreContext.Provider value={store}>
          <DiffSidebar />
        </StoreContext.Provider>,
      );
      const row = container.querySelector('.diff-file-row')!;
      expect(row.querySelector('.diff-kind-badge')).toHaveTextContent('A');
      expect(row.querySelector('.diff-file-base')).toHaveTextContent('app-store.ts');
      expect(row.querySelector('.diff-file-dir')).toHaveTextContent('term-llm/frontend/src/stores');
      expect(row).not.toHaveTextContent('direct tool');
      expect(row).not.toHaveTextContent('/home/sam/Source/term-llm');
      const fullscreen = screen.getByRole('button', {
        name: 'View fullscreen /home/sam/Source/term-llm/frontend/src/stores/app-store.ts',
      });
      const path = screen.getByRole('button', {
        name: 'Copy path /home/sam/Source/term-llm/frontend/src/stores/app-store.ts',
      });
      const patch = screen.getByRole('button', {
        name: 'Copy diff for /home/sam/Source/term-llm/frontend/src/stores/app-store.ts',
      });
      expect(fullscreen.querySelector('svg')).toHaveAttribute('viewBox', '0 0 24 24');
      expect(path.querySelector('svg')).toHaveAttribute('viewBox', '0 0 24 24');
      expect(patch.querySelector('svg')).toHaveAttribute('viewBox', '0 0 24 24');
      expect(screen.queryByText('Patch')).not.toBeInTheDocument();

      fireEvent.click(fullscreen);
      const exitFullscreen = screen.getByRole('button', {
        name: 'Exit fullscreen for /home/sam/Source/term-llm/frontend/src/stores/app-store.ts',
      });
      expect(container.querySelector('.diff-sidebar')).toHaveClass('file-fullscreen');
      expect(container.querySelector('.diff-file')).toHaveClass('diff-file-fullscreen');
      expect(exitFullscreen).toHaveAttribute('aria-pressed', 'true');
      expect(store.diff.value.followCurrentFile).toBe(false);
      fireEvent.keyDown(exitFullscreen, { key: 'Escape' });
      expect(container.querySelector('.diff-sidebar')).not.toHaveClass('file-fullscreen');
      expect(store.diff.value.followCurrentFile).toBe(true);
      expect(screen.getByRole('button', { name: /^View fullscreen/ })).toHaveAttribute(
        'aria-pressed',
        'false',
      );

      await act(async () => {
        fireEvent.click(path);
        await Promise.resolve();
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(writeText).toHaveBeenCalledWith(
        '/home/sam/Source/term-llm/frontend/src/stores/app-store.ts',
      );
      expect(path).toHaveClass('copied');
      await act(async () => {
        fireEvent.click(patch);
        await Promise.resolve();
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(writeText).toHaveBeenLastCalledWith(
        '--- a//home/sam/Source/term-llm/frontend/src/stores/app-store.ts\n+++ b//home/sam/Source/term-llm/frontend/src/stores/app-store.ts\n@@ -1 +1 @@\n+package main\n',
      );
      expect(patch).toHaveClass('copied');
      await act(async () => {
        await vi.advanceTimersByTimeAsync(700);
      });
      expect(path).not.toHaveClass('copied');
      expect(patch).not.toHaveClass('copied');
    } finally {
      if (descriptor) Object.defineProperty(navigator, 'clipboard', descriptor);
      else Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
      vi.useRealTimers();
    }
  });

  it('shows active work in the maximized changes header without inserting refresh rows', () => {
    const store = createStore();
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.diff.value = {
      ...store.diff.value,
      open: true,
      maximized: true,
      loading: true,
      sessionId: 's1',
      files: [
        {
          path: 'live.ts',
          status: 'modify',
          additions: 1,
          deletions: 0,
          expanded: true,
          loading: true,
          lines: [{ kind: 'add', content: 'still visible', newLine: 1 }],
        },
      ],
    };

    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    expect(
      container.querySelector('.diff-sidebar-header .diff-sidebar-activity'),
    ).toHaveAccessibleName('Assistant is responding: Working');
    expect(screen.queryByText('Loading changes…')).not.toBeInTheDocument();
    expect(screen.queryByText('Loading…')).not.toBeInTheDocument();
    expect(container.querySelector('.diff-row')).toHaveTextContent('still visible');

    fireEvent.click(screen.getByRole('button', { name: 'View fullscreen live.ts' }));
    expect(container.querySelector('.diff-file-row .diff-sidebar-activity')).toHaveAccessibleName(
      'Assistant is responding: Working',
    );
    fireEvent.click(screen.getByRole('button', { name: 'Exit fullscreen for live.ts' }));

    act(() => {
      store.diff.value = { ...store.diff.value, maximized: false };
    });
    expect(container.querySelector('.diff-sidebar-activity')).toBeNull();
  });

  it('preserves the visible diff anchor when live rows move', async () => {
    const store = createStore();
    const lines = [{ kind: 'add' as const, content: 'anchor', newLine: 1 }];
    store.diff.value = {
      ...store.diff.value,
      open: true,
      maximized: true,
      sessionId: 's1',
      files: [
        {
          path: 'live.ts',
          status: 'modify',
          additions: 1,
          deletions: 0,
          expanded: true,
          lines,
        },
      ],
    };
    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    const scroller = container.querySelector<HTMLElement>('.diff-file-list')!;
    const row = container.querySelector<HTMLElement>('[data-diff-anchor="new:1"]')!;
    const file = container.querySelector<HTMLElement>('[data-diff-file-path="live.ts"]')!;
    let scrollTop = 200;
    let rowTop = 100;
    Object.defineProperties(scroller, {
      scrollTop: {
        configurable: true,
        get: () => scrollTop,
        set: (value: number) => {
          scrollTop = value;
        },
      },
      getBoundingClientRect: {
        configurable: true,
        value: () => ({ top: 0, bottom: 500, left: 0, right: 500, width: 500, height: 500 }),
      },
    });
    Object.defineProperty(row, 'getBoundingClientRect', {
      configurable: true,
      value: () => ({
        top: rowTop,
        bottom: rowTop + 20,
        left: 0,
        right: 500,
        width: 500,
        height: 20,
      }),
    });
    Object.defineProperty(file, 'getBoundingClientRect', {
      configurable: true,
      value: () => ({ top: -50, bottom: 400, left: 0, right: 500, width: 500, height: 450 }),
    });
    fireEvent.scroll(scroller);
    await act(
      () =>
        new Promise<void>((resolve) => {
          requestAnimationFrame(() => resolve());
        }),
    );

    rowTop = 250;
    act(() => {
      store.diff.value = {
        ...store.diff.value,
        files: store.diff.value.files.map((entry) => ({ ...entry, additions: 2 })),
      };
    });

    expect(scrollTop).toBe(350);

    fireEvent.click(screen.getByRole('button', { name: 'View fullscreen live.ts' }));
    await act(async () => Promise.resolve());
    const fullscreenScroller = container.querySelector<HTMLElement>('.diff-file-body')!;
    const fullscreenRow = container.querySelector<HTMLElement>('[data-diff-anchor="new:1"]')!;
    const originalRect = HTMLElement.prototype.getBoundingClientRect;
    const rowRect = vi
      .spyOn(HTMLElement.prototype, 'getBoundingClientRect')
      .mockImplementation(function (this: HTMLElement) {
        if (this.matches('[data-diff-anchor="new:1"]'))
          return {
            top: rowTop,
            bottom: rowTop + 20,
            left: 0,
            right: 500,
            width: 500,
            height: 20,
          } as DOMRect;
        return originalRect.call(this);
      });
    let fullscreenScrollTop = 120;
    Object.defineProperty(fullscreenRow, 'getBoundingClientRect', {
      configurable: true,
      value: () => ({
        top: rowTop,
        bottom: rowTop + 20,
        left: 0,
        right: 500,
        width: 500,
        height: 20,
      }),
    });
    Object.defineProperties(fullscreenScroller, {
      scrollTop: {
        configurable: true,
        get: () => fullscreenScrollTop,
        set: (value: number) => {
          fullscreenScrollTop = value;
        },
      },
      getBoundingClientRect: {
        configurable: true,
        value: () => ({ top: 0, bottom: 500, left: 0, right: 500, width: 500, height: 500 }),
      },
    });
    rowTop = 80;
    fireEvent.scroll(fullscreenScroller);
    await act(
      () =>
        new Promise<void>((resolve) => {
          requestAnimationFrame(() => resolve());
        }),
    );

    rowTop = 180;
    act(() => {
      store.diff.value = {
        ...store.diff.value,
        files: [...store.diff.value.files],
      };
    });
    rowRect.mockRestore();

    expect(container.querySelector('.diff-file-body')).toBe(fullscreenScroller);
    expect(fullscreenScrollTop).toBe(220);
  });

  it('keeps fullscreen file navigation synchronized with the selected file', () => {
    const store = createStore();
    store.diff.value = {
      ...store.diff.value,
      open: true,
      files: [
        {
          path: 'first.ts',
          status: 'modify',
          additions: 1,
          deletions: 0,
          expanded: true,
          lines: [{ kind: 'add', content: 'first', newLine: 1 }],
        },
        {
          path: 'second.ts',
          status: 'modify',
          additions: 1,
          deletions: 0,
          expanded: true,
          lines: [{ kind: 'add', content: 'second', newLine: 1 }],
        },
      ],
      selectedPath: 'first.ts',
    };
    const { container } = render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'View fullscreen first.ts' }));
    fireEvent.keyDown(window, { key: ']' });

    expect(store.diff.value.selectedPath).toBe('second.ts');
    expect(
      screen.getByRole('button', { name: 'Exit fullscreen for second.ts' }),
    ).toBeInTheDocument();
    expect(container.querySelector('[data-diff-file-path="first.ts"]')).toBeNull();
  });

  it('exits file fullscreen before closing the mobile changes drawer', () => {
    vi.mocked(window.matchMedia).mockImplementation(
      (query) =>
        ({
          matches: query === '(max-width: 767px)',
          addEventListener: vi.fn(),
          removeEventListener: vi.fn(),
        }) as unknown as MediaQueryList,
    );
    try {
      const store = createStore();
      store.expandDiff = vi.fn(async () => undefined);
      store.diff.value = {
        ...store.diff.value,
        open: true,
        files: [
          {
            path: 'mobile.ts',
            status: 'modify',
            additions: 1,
            deletions: 0,
            expanded: false,
            lines: [{ kind: 'add', content: 'mobile', newLine: 1 }],
          },
        ],
      };
      const { container } = render(
        <StoreContext.Provider value={store}>
          <DiffSidebar />
        </StoreContext.Provider>,
      );

      fireEvent.click(screen.getByRole('button', { name: 'View fullscreen mobile.ts' }));
      expect(store.expandDiff).toHaveBeenCalledWith(store.diff.value.files[0]);
      fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });

      expect(store.diff.value.open).toBe(true);
      expect(store.diff.value.followCurrentFile).toBe(true);
      expect(container.querySelector('.diff-sidebar')).not.toHaveClass('file-fullscreen');
    } finally {
      vi.mocked(window.matchMedia).mockReturnValue({
        matches: false,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      } as unknown as MediaQueryList);
    }
  });

  it('renders keyed messages, sanitized markdown and expandable tool details', async () => {
    const store = createStore();
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(screen.getByText('Question')).toBeInTheDocument();
    expect(screen.getByText('Answer').tagName).toBe('STRONG');
    await userEvent.click(screen.getByRole('button', { name: /read_file/ }));
    const argument = document.querySelector('.tool-argument');
    expect(argument?.querySelector('dt')).toHaveTextContent('path');
    expect(argument?.querySelector('dd')).toHaveTextContent('x');
    expect(screen.queryByText(/"path": "x"/)).not.toBeInTheDocument();
    expect(screen.getByText('ok')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Copy details' })).not.toBeInTheDocument();
  });

  it('renders a model switch as a quiet structured boundary', () => {
    const store = createStore();
    store.sessions.value[0].messages.push({
      id: 'swap-1',
      role: 'model-swap',
      content: '',
      created: 4,
      fromProvider: 'chatgpt',
      fromModel: 'gpt-5.6-luna-high',
      fromEffort: 'high',
      toProvider: 'chatgpt',
      toModel: 'gpt-5.6-sol-high',
      toEffort: 'medium',
    });

    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const boundary = container.querySelector('[data-message-id="swap-1"]');
    expect(boundary?.classList.contains('model-swap-divider')).toBe(true);
    expect(boundary?.querySelector('.model-swap-from')?.textContent).toBe(
      'chatgpt:gpt-5.6-luna-high · high',
    );
    expect(boundary?.querySelector('.model-swap-to')?.textContent).toBe(
      'chatgpt:gpt-5.6-sol-high · medium',
    );
    expect(boundary?.textContent).not.toContain('Model switch');
    expect(boundary?.textContent).not.toContain('↔');
  });

  it('shows the active plan step as the live response activity', () => {
    const store = createStore();
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'response-1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 0,
          startedRev: 0,
          reconnects: 0,
        }),
        messages: [
          {
            id: 'running-tools',
            role: 'tool-group',
            content: '',
            created: 4,
            tools: [{ id: 'shell', name: 'shell', status: 'running' }],
          },
        ],
      },
    };
    store.runEngine.markResponseTransportActive('s1', 'response-1');
    store.currentPlan.value = {
      plan: [{ step: 'Implement the semantic activity label', status: 'in_progress' }],
    };
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const initial = screen.getByRole('status', {
      name: 'Assistant is responding: Implement the semantic activity label',
    });
    const initialText = initial.querySelector('.streaming-indicator-text');
    expect(initial).toHaveTextContent('Implement the semantic activity label');
    expect(initial.querySelectorAll('span')).toHaveLength(1);

    act(() => {
      store.currentPlan.value = {
        plan: [
          { step: 'Implement the semantic activity label', status: 'completed' },
          { step: 'Verify the result', status: 'in_progress' },
        ],
      };
    });

    const updated = screen.getByRole('status', {
      name: 'Assistant is responding: Verify the result',
    });
    expect(updated).toHaveTextContent('Verify the result');
    expect(updated.querySelector('.streaming-indicator-text')).not.toBe(initialText);

    act(() => {
      const current = store.runs.value.s1;
      store.runs.value = {
        ...store.runs.value,
        s1: { ...current, run: { ...current.run, status: 'cancelling' } },
      };
    });
    expect(screen.getByRole('status', { name: 'Stopping response' })).toHaveTextContent('Stopping');
  });

  it('hides the generic working activity once assistant text starts streaming', () => {
    const store = createStore();
    store.runs.value = {
      s1: initialProjection({
        responseId: 'response-1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.runEngine.markResponseTransportActive('s1', 'response-1');
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    expect(
      screen.getByRole('status', { name: 'Assistant is responding: Working' }),
    ).toBeInTheDocument();

    act(() => {
      const current = store.runs.value.s1;
      store.runs.value = {
        ...store.runs.value,
        s1: {
          ...current,
          messages: [
            {
              id: 'response-1:assistant:0',
              role: 'assistant',
              content: 'The answer is arriving',
              created: Date.now(),
              responseId: 'response-1',
              assistantSegmentOrdinal: 0,
            },
          ],
        },
      };
    });

    expect(screen.getByText('The answer is arriving')).toBeInTheDocument();
    expect(
      screen.queryByRole('status', { name: 'Assistant is responding: Working' }),
    ).not.toBeInTheDocument();
  });

  it('keeps a projected response running while its transport reconnects', () => {
    const store = createStore();
    try {
      store.runs.value = {
        s1: initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 1,
          startedRev: 0,
          reconnects: 0,
        }),
      };
      store.runEngine.markResponseTransportActive('s1', 'r1');
      render(
        <StoreContext.Provider value={store}>
          <Transcript />
          <Composer />
        </StoreContext.Provider>,
      );

      expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
      expect(
        screen.getByRole('status', { name: 'Assistant is responding: Working' }),
      ).toBeInTheDocument();

      act(() => store.runEngine.clearResponseTransport('s1', 'r1'));

      expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
      expect(
        screen.getByRole('status', { name: 'Assistant is responding: Working' }),
      ).toBeInTheDocument();
      expect(
        screen.queryByRole('status', { name: 'Response status is unknown' }),
      ).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Response is running' })).toBeEnabled();
    } finally {
      store.dispose();
    }
  });

  it('keeps server-confirmed runs visually active while their transport reattaches', async () => {
    const store = createStore();
    try {
      store.sessions.value = [
        { ...store.sessions.value[0], activeRun: true, activeResponseId: 'r1' },
      ];
      store.runs.value = {
        s1: initialProjection({
          responseId: 'r1',
          sessionId: 's1',
          epoch: 1,
          status: 'connecting',
          lastSequence: 1,
          startedRev: 0,
          reconnects: 1,
        }),
      };
      render(
        <StoreContext.Provider value={store}>
          <Transcript />
          <Composer />
        </StoreContext.Provider>,
      );

      expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Response is running' })).toBeEnabled();
      expect(screen.getByPlaceholderText('Type to interject…')).toBeInTheDocument();
      expect(
        screen.getByRole('status', { name: 'Assistant is responding: Working' }),
      ).toBeInTheDocument();
      expect(
        screen.queryByRole('status', { name: 'Response status is unknown' }),
      ).not.toBeInTheDocument();

      store.interject = vi.fn(async () => undefined);
      await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'steer this run');
      const interject = screen.getByRole('button', { name: 'Interject' });
      expect(interject).toBeEnabled();
      await userEvent.click(interject);
      expect(store.interject).toHaveBeenCalledWith('steer this run');
      act(() => {
        store.prompt.value = '';
      });

      act(() => {
        store.sessions.value = [
          { ...store.sessions.value[0], activeRun: false, activeResponseId: null },
        ];
      });

      // An idle status sample starts snapshot reconciliation; it does not own
      // the projected response's terminal transition.
      expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
      expect(
        screen.queryByRole('status', { name: 'Response status is unknown' }),
      ).not.toBeInTheDocument();

      act(() => {
        store.runs.value = {
          s1: {
            ...store.runs.value.s1,
            run: { ...store.runs.value.s1.run, status: 'completed' },
          },
        };
      });
      expect(screen.queryByRole('button', { name: 'Stop' })).not.toBeInTheDocument();
    } finally {
      store.dispose();
    }
  });

  it('renders ask_user questions and keeps the answer inside its tool card', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'ask-tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'ask-1',
            name: 'ask_user',
            arguments: JSON.stringify({
              questions: [
                {
                  header: 'Frontend',
                  question: 'Which frontend approach should I use?',
                  multi_select: false,
                  options: [
                    {
                      label: 'Vendor xterm.js',
                      description: 'Download and serve the full terminal renderer.',
                    },
                    {
                      label: 'Basic renderer',
                      description: 'Use a smaller renderer with limited terminal support.',
                    },
                  ],
                },
                {
                  header: 'Features',
                  question: 'Which extras should be enabled?',
                  multi_select: true,
                  options: [{ label: 'Search', description: 'Enable transcript search.' }],
                },
              ],
            }),
            argumentsFinalized: true,
            status: 'done',
            askUserAnswer: 'Frontend: Vendor xterm.js | Features: Search',
          },
        ],
      },
    ];

    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const toggle = screen.getByRole('button', { name: /Which frontend approach should I use/ });
    expect(toggle).toHaveTextContent('2 questions');
    await userEvent.click(toggle);

    expect(screen.getByText('Frontend')).toBeInTheDocument();
    expect(screen.getByText('Which frontend approach should I use?')).toBeInTheDocument();
    expect(screen.getByText('Download and serve the full terminal renderer.')).toBeInTheDocument();
    expect(screen.getByText('Choose multiple')).toBeInTheDocument();
    const answer = screen.getByText('Frontend: Vendor xterm.js | Features: Search');
    const card = container.querySelector('[data-tool-id="ask-1"] .tool-card');
    expect(answer.closest('.tool-card')).toBe(card);
    expect(card?.querySelector('.ask-user-result')).toHaveTextContent('Answer');
    expect(card?.querySelector('.tool-argument')).toBeNull();
    expect(container.querySelector('.message.user')).toBeNull();
  });

  it('formats tool parameters as readable typed rows and preserves partial argument fallbacks', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'shell',
            name: 'shell',
            arguments: JSON.stringify({
              command: 'printf "hello\\nworld"',
              description: 'Print two lines',
              timeout_seconds: 15,
              approved: true,
              modes: ['safe', 'fast'],
            }),
            status: 'error',
            result: 'cancelled',
          },
          {
            id: 'partial',
            name: 'write_file',
            arguments: '{"path":"notes.txt","content":',
            status: 'error',
          },
        ],
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: /2 tool calls/ }));
    for (const toggle of document.querySelectorAll<HTMLButtonElement>('.tool-toggle'))
      await userEvent.click(toggle);
    const rows = Array.from(document.querySelectorAll('.tool-argument'));
    expect(rows.map((row) => row.querySelector('dt')?.textContent)).toEqual([
      'command',
      'description',
      'timeout_seconds',
      'approved',
      'modes',
    ]);
    expect(screen.getByText('printf "hello\\nworld"')).toHaveClass('tool-argument-text');
    expect(screen.getByText('15')).toHaveClass('tool-argument-literal');
    expect(screen.getByText('true')).toHaveClass('tool-argument-literal');
    expect(screen.getByText('["safe","fast"]')).toHaveClass('tool-argument-structured');
    expect(document.querySelector('.tool-arguments-fallback')).toHaveTextContent(
      '{"path":"notes.txt","content":',
    );
  });

  it('keeps a collapsed tool group closed when another running call arrives', () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'read',
            name: 'read_file',
            arguments: '{"path":"main.go"}',
            status: 'done',
            result: 'ok',
          },
          {
            id: 'grep',
            name: 'grep',
            arguments: '{"pattern":"TODO"}',
            status: 'running',
          },
        ],
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const group = screen.getByRole('button', { name: /2 tool calls/ });
    expect(group).toHaveAttribute('aria-expanded', 'false');

    act(() => {
      const session = store.sessions.value[0];
      const toolGroup = session.messages[0];
      store.sessions.value = [
        {
          ...session,
          messages: [
            {
              ...toolGroup,
              tools: [
                ...(toolGroup.tools || []),
                {
                  id: 'shell',
                  name: 'shell',
                  arguments: '{"command":"npm test"}',
                  status: 'running',
                },
              ],
            },
          ],
        },
      ];
    });

    expect(screen.getByRole('button', { name: /3 tool calls/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
  });

  it('keeps expansion local to the block the user opened while tool calls stream in', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'tools-a',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'read',
            name: 'read_file',
            arguments: '{"path":"main.go"}',
            status: 'running',
          },
        ],
      },
      {
        id: 'tools-b',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'search',
            name: 'grep',
            arguments: '{"pattern":"TODO"}',
            status: 'running',
          },
        ],
      },
    ];
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const first = container.querySelector<HTMLButtonElement>(
      '[data-message-id="tools-a"] .tool-toggle',
    )!;
    const second = container.querySelector<HTMLButtonElement>(
      '[data-message-id="tools-b"] .tool-toggle',
    )!;
    expect(first).toHaveAttribute('aria-expanded', 'false');
    expect(second).toHaveAttribute('aria-expanded', 'false');

    await userEvent.click(first);
    expect(first).toHaveAttribute('aria-expanded', 'true');
    expect(second).toHaveAttribute('aria-expanded', 'false');

    act(() => {
      const session = store.sessions.value[0];
      const [firstGroup, secondGroup] = session.messages;
      store.sessions.value = [
        {
          ...session,
          messages: [
            {
              ...firstGroup,
              tools: [
                ...(firstGroup.tools || []),
                {
                  id: 'shell',
                  name: 'shell',
                  arguments: '{"command":"npm test"}',
                  status: 'running',
                },
              ],
            },
            secondGroup,
          ],
        },
      ];
    });

    expect(
      container.querySelector('[data-message-id="tools-a"] .tool-group-toggle'),
    ).toHaveAttribute('aria-expanded', 'true');
    expect(container.querySelector('[data-message-id="tools-b"] .tool-toggle')).toHaveAttribute(
      'aria-expanded',
      'false',
    );
  });

  it('stays pinned as content grows until the user scrolls up', () => {
    const store = createStore();
    let resize: ResizeObserverCallback | undefined;
    class TestResizeObserver {
      constructor(callback: ResizeObserverCallback) {
        resize = callback;
      }
      observe() {}
      unobserve() {}
      disconnect() {}
    }
    vi.stubGlobal('ResizeObserver', TestResizeObserver);
    try {
      const { container } = render(
        <StoreContext.Provider value={store}>
          <Transcript />
        </StoreContext.Provider>,
      );
      const viewport = container.querySelector<HTMLElement>('#chatScroll')!;
      let scrollHeight = 1_000;
      let scrollTop = 0;
      Object.defineProperties(viewport, {
        clientHeight: { configurable: true, value: 300 },
        scrollHeight: { configurable: true, get: () => scrollHeight },
        scrollTop: {
          configurable: true,
          get: () => scrollTop,
          set: (value: number) => {
            scrollTop = Math.min(value, scrollHeight - 300);
          },
        },
      });
      viewport.scrollTop = 700;

      scrollHeight = 1_400;
      act(() => resize?.([], {} as ResizeObserver));
      expect(viewport.scrollTop).toBe(1_100);

      // Canonical tail reparsing can briefly shrink the document. Chromium clamps
      // scrollTop, then may restore an older programmatic position after it grows.
      scrollHeight = 1_390;
      viewport.scrollTop = 1_090;
      fireEvent.scroll(viewport);
      act(() => resize?.([], {} as ResizeObserver));
      expect(viewport.scrollTop).toBe(1_090);

      scrollHeight = 1_500;
      viewport.scrollTop = 1_100;
      fireEvent.scroll(viewport);
      act(() => resize?.([], {} as ResizeObserver));
      expect(viewport.scrollTop).toBe(1_200);

      viewport.scrollTop = 1_195;
      fireEvent.scroll(viewport);
      scrollHeight = 1_600;
      act(() => resize?.([], {} as ResizeObserver));
      expect(viewport.scrollTop).toBe(1_195);

      // Returning to the old programmatic position via scrollbar or keyboard
      // must not revive a stale marker and re-pin the reader.
      viewport.scrollTop = 1_200;
      fireEvent.scroll(viewport);
      scrollHeight = 1_700;
      act(() => resize?.([], {} as ResizeObserver));
      expect(viewport.scrollTop).toBe(1_200);

      viewport.scrollTop = 1_400;
      fireEvent.scroll(viewport);
      fireEvent.wheel(viewport, { deltaY: -1 });
      scrollHeight = 1_800;
      act(() => resize?.([], {} as ResizeObserver));
      expect(viewport.scrollTop).toBe(1_400);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('re-pins the transcript to the bottom when a message is submitted', async () => {
    const store = createStore();
    store.send = vi.fn(async () => undefined);
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
        <Composer />
      </StoreContext.Provider>,
    );
    const viewport = container.querySelector<HTMLElement>('#chatScroll')!;
    let scrollTop = 100;
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, value: 300 },
      scrollHeight: { configurable: true, value: 1_000 },
      scrollTop: {
        configurable: true,
        get: () => scrollTop,
        set: (value: number) => {
          scrollTop = Math.min(value, 700);
        },
      },
    });
    fireEvent.scroll(viewport);

    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'hello');
    await userEvent.keyboard('{Enter}');

    expect(store.send).toHaveBeenCalledOnce();
    expect(viewport.scrollTop).toBe(700);
  });

  it('shows persisted spawn_agent calls as running until agents finish', () => {
    const store = createStore();
    store.sessions.value[0].messages = convertServerMessages([
      {
        id: 1,
        sequence: 0,
        role: 'assistant',
        parts: [
          {
            type: 'tool_call',
            tool_call_id: 'spawn-grok',
            tool_name: 'spawn_agent',
            tool_arguments: '{"agent_name":"reviewer"}',
          },
          {
            type: 'tool_call',
            tool_call_id: 'spawn-gemini',
            tool_name: 'spawn_agent',
            tool_arguments: '{"agent_name":"reviewer"}',
          },
        ],
      },
    ]);

    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const group = screen.getByRole('button', { name: /2 tool calls · spawn_agent/ });
    const status = group.querySelector('.tool-status');
    expect(status).toHaveTextContent(/^\s*0s\s*$/);
    expect(status).not.toHaveTextContent('running…');
    expect(status).not.toHaveClass('done');
  });

  it('ticks spawn-agent duration without replacing unrelated transcript DOM', async () => {
    vi.useFakeTimers();
    try {
      const now = new Date('2026-09-02T12:00:00Z').getTime();
      vi.setSystemTime(now);
      const store = createStore();
      store.sessions.value[0].messages = [
        { id: 'answer', role: 'assistant', content: 'Still here', created: now - 10_000 },
        {
          id: 'spawn-tools',
          role: 'tool-group',
          content: '',
          created: now - 5_000,
          tools: [
            {
              id: 'spawn-1',
              name: 'spawn_agent',
              status: 'running',
              startedAt: now - 5_000,
            },
          ],
        },
      ];
      const { container } = render(
        <StoreContext.Provider value={store}>
          <Transcript />
        </StoreContext.Provider>,
      );
      const unrelated = container.querySelector('[data-message-id="answer"]');
      const status = container.querySelector('[data-tool-id="spawn-1"] .tool-status');
      expect(status).toHaveTextContent('5s');

      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_000);
      });
      expect(status).toHaveTextContent('6s');
      expect(container.querySelector('[data-message-id="answer"]')).toBe(unrelated);
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows a frozen duration beside a completed spawn-agent status', () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'spawn-tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'spawn-1',
            name: 'spawn_agent',
            status: 'done',
            resultStatus: 'success',
            durationMs: 90_000,
          },
        ],
      },
    ];
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(container.querySelector('[data-tool-id="spawn-1"] .tool-status')).toHaveTextContent(
      '1m30s ✓',
    );
  });

  it('groups completed tool calls compactly and omits redundant role labels', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      { id: 'a1', role: 'assistant', content: 'Done', created: Date.now() },
      {
        id: 'tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'read',
            name: 'read_file',
            arguments: '{"path":"main.go"}',
            status: 'done',
            result: 'ok',
          },
          {
            id: 'shell',
            name: 'shell',
            arguments: '{"description":"Run tests","command":"go test ./..."}',
            status: 'done',
            result: 'ok',
          },
        ],
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    const group = screen.getByRole('button', { name: /2 tool calls · read_file, shell/ });
    expect(group).toHaveAttribute('aria-expanded', 'false');
    expect(group.querySelector('.tool-status')).toHaveTextContent('✓');
    expect(group.querySelector('.tool-status')).not.toHaveTextContent('done');
    expect(screen.queryByText('Assistant')).not.toBeInTheDocument();
    expect(screen.getByText('now')).toBeInTheDocument();
    await userEvent.click(group);
    expect(group).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText('path: main.go')).toBeInTheDocument();
  });

  it('uses a search icon for grep and omits redundant per-tool chevrons', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'grep',
            name: 'grep',
            arguments: '{"pattern":"needle","path":"frontend"}',
            status: 'done',
            result: 'match',
          },
          {
            id: 'read',
            name: 'read_file',
            arguments: '{"path":"frontend/index.ts"}',
            status: 'done',
            result: 'content',
          },
        ],
      },
    ];
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: /2 tool calls · grep, read_file/ }));
    expect(container.querySelector('.tool-entry-icon')).toHaveTextContent('🔍');
    expect(container.querySelector('.tool-toggle .tool-arrow')).toBeNull();
    expect(container.querySelector('.tool-group-toggle .tool-arrow')).not.toBeNull();
  });

  it('renders concise Guardian outcomes and places failure reasons last', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          {
            id: 'approved-shell',
            name: 'shell',
            arguments: '{"description":"Safe command"}',
            status: 'done',
            result: 'ok',
            guardianReviews: [
              {
                outcome: 'approved',
                message: 'guardian: approved (low risk; clearly user-authorized)',
                command: 'do-not-repeat-this-command',
              },
            ],
          },
          {
            id: 'denied-edit',
            name: 'edit_file',
            arguments: '{"path":"restricted.txt"}',
            status: 'error',
            result: 'write access denied for restricted.txt',
            guardianReviews: [
              {
                outcome: 'denied',
                message: 'guardian: denied: outside approved workspace',
                command: 'do-not-repeat-denied-command',
              },
            ],
          },
        ],
      },
    ];
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: /2 tool calls/ }));
    const toolToggles = [...container.querySelectorAll<HTMLButtonElement>('.tool-toggle')];
    const shellToggle = toolToggles.find(
      (button) => button.querySelector('.tool-name')?.textContent === 'shell',
    )!;
    const deniedToggle = toolToggles.find(
      (button) => button.querySelector('.tool-name')?.textContent === 'edit_file',
    )!;
    await userEvent.click(shellToggle);
    await userEvent.click(deniedToggle);

    const approved = container.querySelector('.guardian-approved')!;
    const denied = container.querySelector('.guardian-denied')!;
    expect(approved).toHaveTextContent(/^approved$/);
    expect(denied.querySelector('strong')).toHaveTextContent('denied');
    expect(denied.querySelector('span')).toHaveTextContent('outside approved workspace');
    expect(container).not.toHaveTextContent('do-not-repeat-this-command');
    expect(container).not.toHaveTextContent('do-not-repeat-denied-command');

    const failure = container.querySelector('.tool-failure-reason')!;
    expect(failure).toHaveTextContent('Failure');
    expect(failure).toHaveTextContent('write access denied for restricted.txt');
    expect(failure.parentElement?.lastElementChild).toBe(failure);
  });

  it('collapses reloaded tool groups with failures without labeling the group as an error', () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      {
        id: 'tools',
        role: 'tool-group',
        content: '',
        created: Date.now(),
        tools: [
          { id: 'read', name: 'read_file', status: 'done' },
          { id: 'shell', name: 'shell', status: 'error', result: 'exit status 1' },
        ],
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    const group = screen.getByRole('button', { name: /2 tool calls · read_file, shell/ });
    expect(group).toHaveAttribute('aria-expanded', 'false');
    expect(group.querySelector('.tool-status')).toHaveTextContent('✓');
    expect(group.querySelector('.tool-status')).not.toHaveTextContent('error');
    const toolToggles = [...document.querySelectorAll('.tool-toggle')];
    expect(toolToggles).toHaveLength(2);
    toolToggles.forEach((toggle) => expect(toggle).toHaveAttribute('aria-expanded', 'false'));
    expect(document.querySelector('.tool-failure-reason')).not.toBeInTheDocument();
    const failed = document.querySelector('.tool-status.error');
    expect(failed).toHaveTextContent('✕');
    expect(failed).not.toHaveTextContent('error');
  });

  it('ports the compact turn-copy control and transient copied feedback', async () => {
    vi.useFakeTimers();
    const clipboardDescriptor = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    try {
      const store = createStore();
      store.sessions.value[0].messages = [
        { id: 'u1', role: 'user', content: 'Question', created: 1 },
        {
          id: 'a1',
          role: 'assistant',
          content: 'First segment',
          created: 2,
          responseId: 'response-1',
        },
        {
          id: 't1',
          role: 'tool-group',
          content: '',
          created: 3,
          responseId: 'response-1',
          tools: [
            {
              id: 'shell-1',
              name: 'shell',
              status: 'done',
              arguments: '{"command":"noisy command"}',
              result: 'noisy tool output',
            },
          ],
        },
        {
          id: 'a2',
          role: 'assistant',
          content: 'Final segment',
          created: 4,
          responseId: 'response-1',
        },
      ];
      const { container } = render(
        <StoreContext.Provider value={store}>
          <Transcript />
        </StoreContext.Provider>,
      );
      expect(screen.getAllByRole('button', { name: 'Copy response' })).toHaveLength(1);
      const assistant = container.querySelectorAll('.message.assistant')[1];
      expect(assistant.querySelector('.turn-action-panel + .message-meta')).not.toBeNull();
      const button = screen.getByRole('button', { name: 'Copy response' });
      await act(async () => {
        fireEvent.click(button);
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(writeText).toHaveBeenCalledWith('First segment\n\nFinal segment');
      expect(button).toHaveClass('copied');
      expect(button).toHaveAttribute('aria-label', 'Copied');
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_500);
      });
      expect(button).not.toHaveClass('copied');
      expect(button).toHaveAttribute('aria-label', 'Copy response');
    } finally {
      if (clipboardDescriptor) Object.defineProperty(navigator, 'clipboard', clipboardDescriptor);
      else Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
      vi.useRealTimers();
    }
  });

  it('renders branching as a compact icon action', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      { id: 'u1', role: 'user', content: 'Question', created: 1 },
      {
        id: 'a1',
        role: 'assistant',
        content: 'Answer',
        created: 2,
        durableRowId: 42,
      },
    ];
    store.openBranchContext = vi.fn();

    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const button = screen.getByRole('button', { name: 'Branch from here' });
    expect(button).toHaveClass('turn-action-btn', 'turn-branch-btn');
    expect(button).toHaveAttribute('title', 'Branch from here');
    expect(button).toHaveTextContent('');
    expect(button.querySelector('svg')).not.toBeNull();
    await userEvent.click(button);
    expect(store.openBranchContext).toHaveBeenCalledWith('42');
  });

  it('renders share as a compact action for a completed durable assistant response', async () => {
    const store = createStore();
    store.sessions.value[0].messages = [
      { id: 'u1', role: 'user', content: 'Question', created: 1 },
      {
        id: 'a1',
        role: 'assistant',
        content: 'Answer',
        created: 2,
        durableRowId: 42,
      },
    ];
    store.openShare = vi.fn();

    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const button = screen.getByRole('button', { name: 'Share…' });
    expect(button).toHaveClass('turn-action-btn', 'turn-share-btn');
    expect(button).toHaveAttribute('title', 'Share…');
    await userEvent.click(button);
    expect(store.openShare).toHaveBeenCalledWith(42);
  });

  it('pins a near-tail transcript immediately without smooth scrolling', () => {
    const store = createStore();
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    const viewport = container.querySelector<HTMLElement>('.chat-scroll')!;
    Object.defineProperty(viewport, 'scrollHeight', { configurable: true, value: 800 });
    viewport.scrollTop = 0;
    const animatedScroll = vi.spyOn(viewport, 'scrollTo');
    act(() => {
      const active = store.sessions.value[0];
      store.sessions.value = [
        {
          ...active,
          messages: [
            ...active.messages,
            { id: 'a-next', role: 'assistant', content: 'Next', created: 4 },
          ],
        },
      ];
    });
    expect(viewport.scrollTop).toBe(800);
    expect(animatedScroll).not.toHaveBeenCalled();
  });

  it('restores project and agent choices for a new project-mode chat', async () => {
    localStorage.clear();
    const store = createStore();
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p2', name: 'Zeta', archived: false, available: true, sessions: [] },
      { id: 'p1', name: 'Alpha', archived: false, available: true, sessions: [] },
      { id: 'old', name: 'Archived', archived: true, available: true, sessions: [] },
      { id: 'gone', name: 'Unavailable', archived: false, available: false, sessions: [] },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );

    const project = screen.getByRole('button', { name: 'Choose chat or project' });
    const agent = screen.getByRole('button', { name: 'Choose agent for new chat' });
    expect(project).toHaveTextContent('Chat');
    expect(agent).toHaveTextContent('AgentDefault');
    expect(project.closest('.new-chat-pickers')).toBe(agent.closest('.new-chat-pickers'));

    await userEvent.click(project);
    const projectPopover = screen.getByRole('dialog', { name: 'Choose chat or project' });
    expect(screen.getByRole('listbox', { name: 'Choose chat or project' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'Chat' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('option', { name: 'Chat' })).toHaveFocus();
    expect(projectPopover).toHaveTextContent('Chat');
    expect(projectPopover).toHaveTextContent('Alpha');
    expect(projectPopover).toHaveTextContent('Zeta');
    expect(projectPopover).not.toHaveTextContent('Archived');
    expect(projectPopover).not.toHaveTextContent('Unavailable');
    expect(screen.getByRole('button', { name: 'Add project…' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'Add project…' })).not.toBeInTheDocument();

    await userEvent.keyboard('{ArrowDown}{Enter}');
    expect(store.activeProjectId.value).toBe('p1');
    expect(localStorage.getItem(store.keys.lastProject)).toBe('p1');
    expect(project).toHaveTextContent('ProjectAlpha');
    expect(project).toHaveAttribute('aria-expanded', 'false');
    expect(project).toHaveFocus();

    await userEvent.click(agent);
    await userEvent.click(screen.getByRole('option', { name: 'jarvis' }));
    expect(store.selectedAgent.value).toBe('jarvis');
    expect(localStorage.getItem(store.keys.selectedAgent)).toBe('jarvis');
    expect(agent).toHaveTextContent('Agentjarvis');
  });

  it('opens add project from the project picker and clears a stale assignment target', async () => {
    const store = createStore();
    const staleTarget = store.sessions.value[0];
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', archived: false, available: true, sessions: [] },
    ];
    store.projectTarget.value = staleTarget;

    render(
      <StoreContext.Provider value={store}>
        <Transcript />
        <Modals />
      </StoreContext.Provider>,
    );

    const trigger = screen.getByRole('button', { name: 'Choose chat or project' });
    await userEvent.click(trigger);
    await userEvent.click(screen.getByRole('button', { name: 'Add project…' }));

    expect(
      screen.queryByRole('dialog', { name: 'Choose chat or project' }),
    ).not.toBeInTheDocument();
    expect(store.projectTarget.value).toBeNull();
    const modal = screen.getByRole('dialog', { name: 'Add project' });
    const path = screen.getByRole('textbox', { name: 'Project path' });
    await waitFor(() => expect(path).toHaveFocus());

    fireEvent.keyDown(modal, { key: 'Escape' });
    expect(store.modal.value).toBe('');
    expect(trigger).toHaveFocus();
  });

  it('creates a project from the picker and selects it without losing the draft', async () => {
    const store = createStore();
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.projectsEnabled.value = true;
    store.projects.value = [];
    store.prompt.value = 'Keep this draft';
    store.endpoints.createProject = vi
      .fn()
      .mockResolvedValueOnce({ canonical_dir: '/home/me/new-project', git: true })
      .mockResolvedValueOnce({
        project: {
          id: 'project-new',
          name: 'New project',
          canonical_dir: '/home/me/new-project',
          git: true,
        },
      });
    store.refreshSidebar = vi.fn(async () => {
      store.projects.value = [
        {
          id: 'project-new',
          name: 'New project',
          path: '/home/me/new-project',
          archived: false,
          available: true,
          git: true,
          sessions: [],
        },
      ];
    });

    render(
      <StoreContext.Provider value={store}>
        <Transcript />
        <Modals />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: 'Choose chat or project' }));
    await userEvent.click(screen.getByRole('button', { name: 'Add project…' }));
    await userEvent.type(
      screen.getByRole('textbox', { name: 'Project path' }),
      '/home/me/new-project',
    );
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    expect(await screen.findByText('Git repository ready')).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Add project' }));

    await waitFor(() => expect(store.modal.value).toBe(''));
    expect(store.endpoints.createProject).toHaveBeenNthCalledWith(
      1,
      { path: '/home/me/new-project', name: '' },
      true,
    );
    expect(store.endpoints.createProject).toHaveBeenNthCalledWith(
      2,
      { path: '/home/me/new-project', name: '' },
      false,
    );
    expect(store.refreshSidebar).toHaveBeenCalledOnce();
    expect(store.activeProjectId.value).toBe('project-new');
    expect(store.prompt.value).toBe('Keep this draft');
    expect(screen.getByRole('button', { name: 'Choose chat or project' })).toHaveTextContent(
      'ProjectNew project',
    );
  });

  it('runs chip picker footer actions with keyboard focus restoration', async () => {
    const action = vi.fn();
    render(
      <ChipPicker
        ariaLabel="Picker with action"
        value="one"
        options={[
          { value: 'one', label: 'One' },
          { value: 'two', label: 'Two' },
        ]}
        actions={[{ label: 'Add item…', onSelect: action }]}
        triggerClass="new-chat-project-trigger"
        onChange={vi.fn()}
        renderTrigger={(selected) => <span>{selected.label}</span>}
      />,
    );

    const trigger = screen.getByRole('button', { name: 'Picker with action' });
    await userEvent.click(trigger);
    expect(screen.getAllByRole('option')).toHaveLength(2);
    expect(screen.queryByRole('option', { name: 'Add item…' })).not.toBeInTheDocument();
    await userEvent.keyboard('{ArrowUp}{Enter}');
    expect(action).toHaveBeenCalledOnce();
    expect(screen.queryByRole('dialog', { name: 'Picker with action' })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('filters long shared chip pickers and restores trigger focus on Escape', async () => {
    const options = Array.from({ length: 11 }, (_, index) => ({
      value: `value-${index}`,
      label: index === 10 ? 'Special project' : `Project ${index}`,
    }));
    render(
      <ChipPicker
        ariaLabel="Reusable picker"
        value="value-0"
        options={options}
        actions={[{ label: 'Add project…', onSelect: vi.fn() }]}
        triggerClass="new-chat-project-trigger"
        onChange={vi.fn()}
        renderTrigger={(selected) => <span>{selected.label}</span>}
      />,
    );
    const trigger = screen.getByRole('button', { name: 'Reusable picker' });
    await userEvent.click(trigger);
    const filter = screen.getByRole('searchbox', { name: 'Filter options' });
    expect(filter).toHaveFocus();
    await userEvent.type(filter, 'special');
    expect(screen.getAllByRole('option')).toHaveLength(1);
    expect(screen.getByRole('option', { name: 'Special project' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add project…' })).toBeInTheDocument();
    await userEvent.clear(filter);
    await userEvent.type(filter, 'missing');
    expect(screen.queryAllByRole('option')).toHaveLength(0);
    expect(screen.getByText('No matching options')).toBeVisible();
    const addProject = screen.getByRole('button', { name: 'Add project…' });
    expect(addProject).toBeVisible();
    await userEvent.keyboard('{ArrowDown}');
    expect(addProject).toHaveFocus();
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Reusable picker' })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();

    await userEvent.click(trigger);
    fireEvent.click(screen.getByRole('dialog', { name: 'Reusable picker' }));
    expect(screen.queryByRole('dialog', { name: 'Reusable picker' })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('names the active agent in the idle composer placeholder', () => {
    const store = createStore();
    store.sessions.value = [{ ...store.sessions.value[0], agent: 'widget-builder' }];
    const { rerender } = render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('textbox', { name: 'Message' })).toHaveAttribute(
      'placeholder',
      'Message Widget Builder…',
    );

    store.draftActive.value = true;
    store.selectedAgent.value = 'jarvis';
    rerender(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('textbox', { name: 'Message' })).toHaveAttribute(
      'placeholder',
      'Message Jarvis…',
    );
  });

  it('morphs the send icon into the interjection icon while streaming with a draft', async () => {
    const store = createStore();
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 1,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.runEngine.markResponseTransportActive('s1', 'r1');
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );

    const send = screen.getByRole('button', { name: 'Response is running' });
    const sendIcon = send.querySelector('.arrow')?.innerHTML;
    expect(send).toBeEnabled();
    expect(send).toHaveClass('loading');
    expect(send.querySelector('.spinner')).toBeInTheDocument();
    expect(send).not.toHaveClass('interject');

    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'change course');

    const interject = screen.getByRole('button', { name: 'Interject' });
    expect(interject).toBeEnabled();
    expect(interject).not.toHaveClass('loading');
    expect(interject).toHaveClass('interject');
    expect(interject).toHaveAttribute('title', 'Interject');
    expect(interject.querySelector('.arrow')?.innerHTML).not.toBe(sendIcon);
    expect(interject.querySelector('.arrow path')).toHaveAttribute('d', 'M5 6v7a2 2 0 0 0 2 2h12');
  });

  it('drives send from public composer UI and opens attachment picker behavior', async () => {
    const store = createStore();
    store.send = vi.fn(async () => undefined);
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    const input = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(input, 'hello');
    await userEvent.keyboard('{Enter}');
    expect(store.send).toHaveBeenCalledOnce();
  });

  it('opens /tree locally and clears the command from the composer', async () => {
    const store = createStore();
    store.loadBranchTree = vi.fn(async () => undefined);
    store.send = vi.fn(async () => undefined);
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );

    const input = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(input, '/tree');
    await userEvent.keyboard('{Enter}');

    expect(store.loadBranchTree).toHaveBeenCalledOnce();
    expect(store.send).not.toHaveBeenCalled();
    expect(store.prompt.value).toBe('');
  });

  it('shrinks the composer after sending a multiline prompt', async () => {
    const store = createStore();
    store.send = vi.fn(async () => {
      store.prompt.value = '';
    });
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    const input = screen.getByRole('textbox', { name: 'Message' }) as HTMLTextAreaElement;
    Object.defineProperty(input, 'scrollHeight', {
      configurable: true,
      get: () => (input.value ? 260 : 36),
    });

    fireEvent.input(input, { target: { value: 'one\ntwo\nthree\nfour\nfive' } });
    expect(input.style.height).toBe('200px');
    fireEvent.keyDown(input, { key: 'Enter' });

    await waitFor(() => expect(input.style.height).toBe('auto'));
    expect(store.send).toHaveBeenCalledOnce();
  });

  it('offers slash and mention completions through an accessible listbox', async () => {
    const store = createStore();
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), '/co');
    expect(screen.getByRole('option', { name: /compact/ })).toBeInTheDocument();
  });

  it('opens approval controls from /approvals and shows a changed-mode chip above the goal', async () => {
    const store = createStore();
    store.sessions.value = store.sessions.value.map((entry) => ({
      ...entry,
      approvalDefaultMode: 'auto',
      approvalRequestedMode: 'auto',
      approvalEffectiveMode: 'prompt',
      guardianAvailable: true,
      guardianAutoSuspended: true,
    }));
    store.goal.value = { objective: 'Ship carefully', status: 'active' };
    store.endpoints.approvalPolicy = vi.fn(async () => ({
      default_mode: 'auto' as const,
      requested_mode: 'auto' as const,
      effective_mode: 'prompt' as const,
      guardian_available: true,
      guardian_auto_suspended: true,
    }));
    store.endpoints.setApprovalMode = vi.fn(async () => ({
      default_mode: 'auto' as const,
      requested_mode: 'auto' as const,
      effective_mode: 'auto' as const,
      guardian_available: true,
      guardian_auto_suspended: false,
    }));
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Composer />
        <Modals />
      </StoreContext.Provider>,
    );

    const chips = container.querySelectorAll('.composer-inner > button');
    expect(chips[0]).toHaveClass('approval-mode-chip');
    expect(chips[1]).toHaveClass('goal-chip');
    expect(chips[0]).toHaveTextContent('Guardian paused');

    const input = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(input, '/approvals');
    await userEvent.keyboard('{Enter}');
    expect(await screen.findByRole('heading', { name: 'Tool approvals' })).toBeInTheDocument();
    const autoMode = screen.getByRole('radio', { name: /Auto/ });
    expect(autoMode).toBeChecked();
    await waitFor(() => expect(autoMode).toHaveFocus());
    expect(screen.getByRole('radio', { name: /Prompt/ })).not.toHaveFocus();
    const resume = await screen.findByRole('button', { name: 'Resume Auto' });
    expect(resume).toBeInTheDocument();
    await userEvent.click(resume);
    await waitFor(() => expect(store.endpoints.setApprovalMode).toHaveBeenCalledWith('s1', 'auto'));
    expect(screen.queryByRole('heading', { name: 'Tool approvals' })).not.toBeInTheDocument();
  });

  it('hides and rejects approval controls when the server launched in Yolo', async () => {
    const store = createStore();
    store.config.approvals = false;
    store.toast = vi.fn();
    store.modal.value = '';
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Composer />
        <Modals />
      </StoreContext.Provider>,
    );

    const input = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(input, '/app');
    expect(screen.queryByRole('option', { name: /approvals/ })).not.toBeInTheDocument();
    await userEvent.type(input, 'rovals');
    await userEvent.keyboard('{Enter}');
    store.config.approvals = true;

    expect(store.modal.value).toBe('');
    expect(container.querySelector('.approval-mode-chip')).not.toBeInTheDocument();
    expect(store.toast).toHaveBeenCalledWith(
      'Approval controls are disabled for a Yolo server.',
      'error',
    );
  });

  it('keeps a local approval choice across live policy updates after hydration', async () => {
    const store = createStore();
    let resolvePolicy!: (value: {
      default_mode: 'auto';
      requested_mode: 'auto';
      effective_mode: 'auto';
      guardian_available: boolean;
      guardian_auto_suspended: boolean;
    }) => void;
    store.endpoints.approvalPolicy = vi.fn(
      () =>
        new Promise<Parameters<typeof resolvePolicy>[0]>((resolve) => {
          resolvePolicy = resolve;
        }),
    );
    store.modal.value = 'approvals';
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    await screen.findByRole('heading', { name: 'Tool approvals' });
    const yolo = screen.getByRole('radio', { name: /Yolo/ });
    await act(async () => {
      resolvePolicy({
        default_mode: 'auto',
        requested_mode: 'auto',
        effective_mode: 'auto',
        guardian_available: true,
        guardian_auto_suspended: false,
      });
    });
    await waitFor(() => expect(yolo).toBeEnabled());
    await userEvent.click(yolo);
    act(() => {
      store.sessionStore.patch('s1', {
        approvalRequestedMode: 'prompt',
        approvalEffectiveMode: 'prompt',
      });
    });

    expect(yolo).toBeChecked();
    expect(screen.getByRole('radio', { name: /Auto/ })).not.toBeChecked();
  });

  it('discovers and dispatches the capability-gated shell command locally', async () => {
    const store = createStore();
    store.shellStore.enabled.value = true;
    store.openShell = vi.fn();
    store.send = vi.fn(async () => undefined);
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    const input = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(input, '/sh');
    expect(screen.getByRole('option', { name: /shell/ })).toBeInTheDocument();
    await userEvent.type(input, 'ell');
    await userEvent.keyboard('{Enter}');
    expect(store.openShell).toHaveBeenCalledOnce();
    expect(store.send).not.toHaveBeenCalled();
  });

  it('dispatches restored branch and live skill commands instead of sending them as chat', async () => {
    const branchStore = createStore();
    branchStore.branchCommand = vi.fn(async () => undefined);
    branchStore.send = vi.fn(async () => undefined);
    const first = render(
      <StoreContext.Provider value={branchStore}>
        <Composer />
      </StoreContext.Provider>,
    );
    const branchInput = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(branchInput, '/fork continue here');
    await userEvent.keyboard('{Enter}');
    expect(branchStore.branchCommand).toHaveBeenCalledWith('fork', 'continue here');
    expect(branchStore.send).not.toHaveBeenCalled();
    first.unmount();

    const skillStore = createStore();
    skillStore.skills.value = [
      { name: 'review', description: 'Review', execution: 'isolated', source: 'local' },
    ];
    skillStore.invokeSkill = vi.fn(async () => undefined);
    render(
      <StoreContext.Provider value={skillStore}>
        <Composer />
      </StoreContext.Provider>,
    );
    const skillInput = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(skillInput, '/review src');
    await userEvent.keyboard('{Enter}');
    expect(skillStore.invokeSkill).toHaveBeenCalledWith('review', 'src');
  });

  it('keeps Back to Hub inside the persisted collapsible Agents section', async () => {
    const store = new AppStore({
      ...config,
      hub: { url: '/hub/', nodeId: 'Dev', nodeBasePath: '/ui' },
    });
    store.widgets.value = [{ id: 'usage', name: 'Usage', url: '/widgets/usage' }];
    store.hubAgents.value = [
      { id: 'Dev', name: 'Dev', target: '/node/Dev/', active: true, attention: false },
      {
        id: 'worker',
        name: 'worker',
        target: '/node/worker/',
        active: true,
        attention: false,
      },
      {
        id: 'checklist',
        name: 'checklist',
        target: '/node/checklist/',
        active: false,
        attention: true,
      },
    ];
    const view = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    const { container } = view;
    const actions = container.querySelector('.sidebar-actions');
    expect([...actions!.children].map((element) => element.className)).toEqual([
      'new-chat-btn',
      'widgets-sidebar-btn',
      'session-group hub-agent-group',
    ]);

    const agentsToggle = screen.getByRole('button', { name: 'Agents' });
    const agentGroup = container.querySelector('.hub-agent-group');
    const agentsNav = screen.getByRole('navigation', { name: 'Agents' });
    const backToHub = screen.getByRole('link', { name: 'Back to Hub' });
    expect(agentsToggle).toHaveAttribute('aria-expanded', 'true');
    expect(agentGroup).toContainElement(backToHub);
    expect(agentsNav.nextElementSibling).toBe(backToHub);
    expect(agentsNav).toContainElement(container.querySelector('.hub-agent-link'));
    expect(
      container.querySelector('.hub-agent-link[aria-current="true"] .hub-agent-name'),
    ).toHaveTextContent('Dev');
    expect(
      screen.getByRole('link', { name: 'worker' }).querySelector('.hub-agent-attention'),
    ).toBeNull();
    expect(screen.getByText('Needs attention')).toBeInTheDocument();

    await userEvent.click(agentsToggle);
    expect(agentsToggle).toHaveAttribute('aria-expanded', 'false');
    expect(container.querySelector('.hub-agent-link')).toBeNull();
    expect(screen.queryByRole('link', { name: 'Back to Hub' })).not.toBeInTheDocument();
    expect(screen.getByText('Agents need attention')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Agents' })).toBe(agentsToggle);
    expect(
      readJSON<Record<string, boolean>>(store.storage, store.keys.projectExpansion, {})[
        '__hub_agents__'
      ],
    ).toBe(false);

    view.unmount();
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('button', { name: 'Agents' })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
    expect(screen.queryByRole('link', { name: 'Back to Hub' })).not.toBeInTheDocument();
  });

  it('keeps terminal-origin agent sessions out of the default sidebar', () => {
    const store = createStore();
    store.sessions.value = [
      ...store.sessions.value,
      {
        ...store.sessions.value[0],
        id: 'agent-session',
        title: 'Background agent investigation',
        origin: 'tui',
      },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    expect(screen.getByText('Test')).toBeInTheDocument();
    expect(screen.queryByText('Background agent investigation')).not.toBeInTheDocument();
  });
  it('shows server-observed running state before this tab attaches to the stream', () => {
    const store = createStore();
    store.activeSessionId.value = '';
    store.sessions.value = store.sessions.value.map((entry) => ({
      ...entry,
      activeRun: true,
    }));
    store.services.eventFeedHealthy.value = true;

    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    expect(container.querySelector('.session-row')).toHaveClass('is-active');
    expect(store.runs.value).toEqual({});
  });

  it('lifts an archived session row while its actions menu is open', () => {
    const store = createStore();
    store.sessions.value = store.sessions.value.map((session) => ({
      ...session,
      archived: true,
    }));
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    const row = container.querySelector('.session-row');
    const trigger = screen.getByRole('button', { name: 'Actions for Test' });

    expect(row).toHaveClass('archived');
    expect(row).not.toHaveClass('menu-open');

    fireEvent.click(trigger);
    expect(row).toHaveClass('menu-open');
    expect(screen.getByRole('menuitem', { name: 'Unhide' })).toBeInTheDocument();

    fireEvent.click(trigger);
    expect(row).not.toHaveClass('menu-open');
  });

  it('collapses a hidden session before removing it from the sidebar', async () => {
    vi.useFakeTimers();
    try {
      const store = createStore();
      store.activeSessionId.value = '';
      store.archiveSession = vi.fn(async (session) => {
        store.sessions.value = store.sessions.value.filter((entry) => entry.id !== session.id);
      });
      const { container } = render(
        <StoreContext.Provider value={store}>
          <Sidebar />
        </StoreContext.Provider>,
      );
      const scroller = container.querySelector<HTMLElement>('.sidebar-content');
      if (!scroller) throw new Error('sidebar scroller missing');
      scroller.scrollTop = 180;

      fireEvent.click(screen.getByRole('button', { name: 'Actions for Test' }));
      expect(screen.queryByRole('menuitem', { name: 'Delete' })).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole('menuitem', { name: 'Hide' }));

      const row = container.querySelector('.session-row');
      expect(row).toHaveClass('is-hiding');
      expect(store.archiveSession).not.toHaveBeenCalled();
      expect(scroller.scrollTop).toBe(180);

      const transitionEnd = (propertyName: string) => {
        const event = new Event('transitionend', { bubbles: true });
        Object.defineProperty(event, 'propertyName', { value: propertyName });
        fireEvent(row as Element, event);
      };
      transitionEnd('opacity');
      expect(store.archiveSession).not.toHaveBeenCalled();

      await act(async () => {
        transitionEnd('max-height');
        await Promise.resolve();
      });

      expect(store.archiveSession).toHaveBeenCalledWith(expect.objectContaining({ id: 's1' }));
      expect(container.querySelector('.session-row')).toBeNull();
      expect(scroller.scrollTop).toBe(180);
    } finally {
      vi.useRealTimers();
    }
  });

  it('opens Assign project without submitting an ancestor form', async () => {
    const store = createStore();
    store.setSidebarView('projects');
    store.projectsEnabled.value = true;
    store.endpoints.projectAssignment = vi.fn(async () => ({ candidate: null }));
    const submit = vi.fn((event: SubmitEvent) => event.preventDefault());
    render(
      <StoreContext.Provider value={store}>
        <form onSubmit={submit}>
          <Sidebar />
          <Modals />
        </form>
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: 'Actions for Test' }));
    await userEvent.click(screen.getByRole('menuitem', { name: 'Assign project…' }));

    expect(submit).not.toHaveBeenCalled();
    expect(store.projectTarget.value?.id).toBe('s1');
    expect(screen.getByRole('dialog', { name: 'Assign project' })).toBeInTheDocument();
  });

  it('shows only the leading activity dot while a sidebar conversation is streaming', () => {
    const store = createStore();
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r1',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    store.runEngine.markResponseTransportActive('s1', 'r1');

    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    expect(container.querySelector('.session-row')).toHaveClass('is-active');
    expect(container.querySelector('.session-progress')).not.toBeInTheDocument();
  });

  it('adds a newly created project conversation to the sidebar immediately', () => {
    const store = createStore();
    store.setSidebarView('projects');
    const existing = { ...store.sessions.value[0], projectId: 'p1', projectName: 'Alpha' };
    store.sessions.value = [existing];
    store.projectsEnabled.value = true;
    store.projects.value = [{ id: 'p1', name: 'Alpha', sessions: [existing], has_more: false }];

    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(
      screen.queryByRole('button', { name: 'Brand new conversation' }),
    ).not.toBeInTheDocument();

    act(() => {
      store.sessions.value = [
        {
          ...existing,
          id: 'draft_new',
          title: 'Brand new conversation',
          lastMessageAt: Date.now(),
          messages: [{ id: 'pending_1', role: 'user', content: 'Hello', created: Date.now() }],
        },
        ...store.sessions.value,
      ];
    });

    expect(screen.getByRole('button', { name: 'Brand new conversation' })).toBeVisible();
  });

  it('always shows sidebar message counts and relative activity instead of title previews', () => {
    vi.useFakeTimers();
    try {
      const now = new Date('2026-08-26T12:00:00Z').getTime();
      vi.setSystemTime(now);
      const store = createStore();
      const base = store.sessions.value[0];
      store.sessions.value = [
        {
          ...base,
          title: 'DSA Compliance HTML Mock',
          longTitle: 'DSA Compliance HTML Mock with details that repeat the title',
          messageCount: 10,
          lastMessageAt: now - 5 * 60 * 60 * 1000,
        },
        {
          ...base,
          id: 's2',
          title: 'Earlier this year',
          messageCount: 2,
          lastMessageAt: new Date(2026, 4, 22, 12).getTime(),
        },
        {
          ...base,
          id: 's3',
          title: 'Last year',
          messageCount: 1,
          lastMessageAt: new Date(2025, 4, 22, 12).getTime(),
        },
      ];
      render(
        <StoreContext.Provider value={store}>
          <Sidebar />
        </StoreContext.Provider>,
      );

      expect(screen.getByText('10 messages · 5h ago')).toBeVisible();
      expect(screen.getByText('2 messages · 22 May')).toBeVisible();
      expect(screen.getByText('1 message · May 2025')).toBeVisible();
      expect(
        screen.queryByText('DSA Compliance HTML Mock with details that repeat the title'),
      ).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('switches between deduplicated Recent and Projects views and remembers the choice', async () => {
    const store = createStore();
    const pinned = {
      ...store.sessions.value[0],
      id: 'pinned',
      title: 'Pinned work',
      projectId: 'p1',
      projectName: 'Alpha',
      pinned: true,
    };
    const projectChat = {
      ...pinned,
      id: 'project-chat',
      title: 'Project work',
      pinned: false,
    };
    const chat = {
      ...projectChat,
      id: 'chat',
      title: 'Loose chat',
      projectId: undefined,
      projectName: undefined,
    };
    const olderActive = {
      ...projectChat,
      id: 'older-active',
      title: 'Older active work',
      lastMessageAt: 0,
      created: 0,
    };
    store.sessions.value = [pinned, projectChat, chat, olderActive];
    store.recentSessions.value = [pinned, projectChat, chat];
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', sessions: [pinned, projectChat, olderActive] },
    ];
    store.sessionStore.activate(olderActive);

    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('tab', { name: 'Recent' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('tablist', { name: 'Conversation view' })).toHaveAttribute(
      'data-view',
      'recent',
    );
    expect(screen.getByRole('tablist', { name: 'Conversation view' })).toHaveAttribute(
      'data-indicator-cycle',
      'initial',
    );
    expect(
      container
        .querySelector('.sidebar-content')!
        .compareDocumentPosition(container.querySelector('.sidebar-view-footer')!),
    ).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(screen.queryByRole('heading', { name: 'Projects' })).not.toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Pinned work' })).toHaveLength(1);
    expect(screen.getAllByRole('button', { name: 'Project work' })).toHaveLength(1);
    expect(screen.getAllByRole('button', { name: 'Loose chat' })).toHaveLength(1);
    expect(screen.getAllByRole('button', { name: 'Older active work' })).toHaveLength(1);
    expect(container.querySelector('.sidebar-recent-groups')).toHaveTextContent('Alpha');
    expect(container.querySelector('.sidebar-recent-groups')).toHaveTextContent('Chat');

    await userEvent.click(screen.getByRole('tab', { name: 'Projects' }));
    expect(screen.getByRole('tablist', { name: 'Conversation view' })).toHaveAttribute(
      'data-view',
      'projects',
    );
    expect(screen.getByRole('heading', { name: 'Projects' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Chat' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Pinned work' })).toHaveLength(1);
    expect(store.storage.getItem(store.keys.sidebarView)).toBe('projects');
    expect(screen.getByRole('tablist', { name: 'Conversation view' })).toHaveAttribute(
      'data-indicator-cycle',
      'alternate',
    );

    await userEvent.click(screen.getByRole('tab', { name: 'Projects' }));
    expect(screen.getByRole('tablist', { name: 'Conversation view' })).toHaveAttribute(
      'data-indicator-cycle',
      'initial',
    );
  });

  it('does not add view controls when projects are disabled', () => {
    const store = createStore();
    store.projectsEnabled.value = false;
    store.sessions.value = [{ ...store.sessions.value[0], pinned: true }];

    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    expect(screen.queryByRole('tablist', { name: 'Conversation view' })).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Pinned' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: store.sessions.value[0].title })).toBeInTheDocument();
  });

  it('keeps non-empty date sections inside project mode groups', () => {
    vi.useFakeTimers();
    try {
      const now = new Date(2026, 7, 26, 12).getTime();
      vi.setSystemTime(now);
      const store = createStore();
      store.setSidebarView('projects');
      const base = { ...store.sessions.value[0], projectId: 'p1', projectName: 'Alpha' };
      const projectSessions = [
        { ...base, lastMessageAt: now - 60 * 60 * 1000 },
        {
          ...base,
          id: 's2',
          title: 'Yesterday chat',
          lastMessageAt: new Date(2026, 7, 25, 12).getTime(),
        },
        {
          ...base,
          id: 's3',
          title: 'Week chat',
          lastMessageAt: new Date(2026, 7, 23, 12).getTime(),
        },
        {
          ...base,
          id: 's4',
          title: 'Older chat',
          lastMessageAt: new Date(2026, 4, 22, 12).getTime(),
        },
      ];
      const ungrouped = {
        ...base,
        id: 's5',
        title: 'Ungrouped yesterday',
        projectId: undefined,
        projectName: undefined,
        lastMessageAt: new Date(2026, 7, 25, 10).getTime(),
      };
      store.sessions.value = [...projectSessions, ungrouped];
      store.projectsEnabled.value = true;
      store.projects.value = [{ id: 'p1', name: 'Alpha', sessions: projectSessions }];

      const { container } = render(
        <StoreContext.Provider value={store}>
          <Sidebar />
        </StoreContext.Provider>,
      );

      expect(
        [...container.querySelectorAll('[data-project-id="p1"] h4')].map((heading) =>
          heading.textContent?.trim(),
        ),
      ).toEqual(['Today', 'Yesterday', 'This week', 'Older']);
      expect(
        [...container.querySelectorAll('.session-ungrouped h4')].map((heading) =>
          heading.textContent?.trim(),
        ),
      ).toEqual(['Yesterday']);
    } finally {
      vi.useRealTimers();
    }
  });

  it('auto-loads older sidebar conversations through the pagination sentinels', async () => {
    const store = createStore();
    store.setSidebarView('projects');
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', sessions: [], has_more: true, next_cursor: 'cursor-1' },
    ];
    store.noProjectCursor.value = 'no-project-cursor';
    store.loadMoreProject = vi.fn(async () => undefined);
    store.loadMoreNoProject = vi.fn(async () => undefined);
    const view = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    const { container } = view;
    expect(screen.getByRole('heading', { name: 'Projects' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Chat' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Project' })).not.toBeInTheDocument();
    expect(container.querySelectorAll('.project-pagination-sentinel')).toHaveLength(2);
    expect(container.querySelector('.sidebar-load-more')).toBeNull();
    await waitFor(() => expect(store.loadMoreProject).toHaveBeenCalledWith('p1'));
    await waitFor(() => expect(store.loadMoreNoProject).toHaveBeenCalled());

    const noProject = screen.getByRole('button', { name: 'Chat' });
    expect(noProject).toHaveAttribute('aria-expanded', 'true');
    await userEvent.click(noProject);
    expect(noProject).toHaveAttribute('aria-expanded', 'false');
    expect(container.querySelectorAll('.session-ungrouped .session-row')).toHaveLength(1);
    expect(container.querySelector('.session-ungrouped .session-row')).toHaveTextContent('Test');
    expect(container.querySelectorAll('.project-pagination-sentinel')).toHaveLength(1);
    expect(
      readJSON<Record<string, boolean>>(store.storage, store.keys.projectExpansion, {})[
        '__no_project__'
      ],
    ).toBe(false);

    view.unmount();
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('button', { name: 'Chat' })).toHaveAttribute('aria-expanded', 'false');
  });

  it('persists the Projects section collapse while keeping its active conversation visible', async () => {
    const store = createStore();
    store.setSidebarView('projects');
    const active = { ...store.sessions.value[0], projectId: 'p1', projectName: 'Alpha' };
    store.sessions.value = [active];
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', sessions: [active], has_more: false },
      { id: 'p2', name: 'Beta', sessions: [], has_more: false },
    ];

    const view = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    const { container } = view;
    const projects = screen.getByRole('button', { name: 'Projects' });
    expect(projects).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('button', { name: 'Alpha' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Beta' })).toBeInTheDocument();

    await userEvent.click(projects);
    expect(projects).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('button', { name: 'Alpha' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Beta' })).not.toBeInTheDocument();
    expect(container.querySelectorAll('.sidebar-project-groups .session-row')).toHaveLength(1);
    expect(container.querySelector('.sidebar-project-groups .session-row')).toHaveTextContent(
      'Test',
    );
    expect(
      readJSON<Record<string, boolean>>(store.storage, store.keys.projectExpansion, {})[
        '__projects__'
      ],
    ).toBe(false);

    view.unmount();
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('button', { name: 'Projects' })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
    expect(screen.queryByRole('button', { name: 'Alpha' })).not.toBeInTheDocument();
  });

  it('preserves sibling sidebar expansion choices when toggles are changed in sequence', async () => {
    const store = createStore();
    store.setSidebarView('projects');
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', sessions: [] },
      { id: 'p2', name: 'Beta', sessions: [] },
    ];
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: 'Alpha' }));
    await userEvent.click(screen.getByRole('button', { name: 'Beta' }));
    await userEvent.click(screen.getByRole('button', { name: 'Chat' }));
    await userEvent.click(screen.getByRole('button', { name: 'Projects' }));

    expect(
      readJSON<Record<string, boolean>>(store.storage, store.keys.projectExpansion, {}),
    ).toMatchObject({ p1: false, p2: false, __no_project__: false, __projects__: false });
  });

  it('lifts pinned project conversations into the global pinned section', () => {
    const store = createStore();
    store.setSidebarView('projects');
    const pinned = { ...store.sessions.value[0], projectId: 'p1', pinned: true };
    const regular = {
      ...pinned,
      id: 's2',
      title: 'Project conversation',
      pinned: false,
    };
    store.sessions.value = [pinned, regular];
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', sessions: [pinned, regular], has_more: false },
    ];

    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    const pinnedGroup = container.querySelector('.sidebar-pinned-group')!;
    const projectsGroup = container.querySelector('.sidebar-project-groups')!;
    const project = container.querySelector('[data-project-id="p1"]')!;
    expect(pinnedGroup).toHaveTextContent('Test');
    expect(project).not.toHaveTextContent('Test');
    expect(project).toHaveTextContent('Project conversation');
    expect(pinnedGroup.compareDocumentPosition(projectsGroup)).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
  });

  it('keeps the active conversation visible when its project is collapsed', async () => {
    const store = createStore();
    store.setSidebarView('projects');
    const active = { ...store.sessions.value[0], projectId: 'p1' };
    const other = { ...active, id: 's2', title: 'Other conversation' };
    store.sessions.value = [active, other];
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', sessions: [active, other], has_more: false },
    ];

    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    const project = screen.getByRole('button', { name: 'Alpha' });
    await userEvent.click(project);

    expect(project).toHaveAttribute('aria-expanded', 'false');
    const group = container.querySelector('[data-project-id="p1"]')!;
    expect(group.querySelectorAll('.session-row')).toHaveLength(1);
    expect(group).toHaveTextContent('Test');
    expect(group).not.toHaveTextContent('Other conversation');
  });

  it('debounces project mentions with session/worktree context and keeps agent matches', async () => {
    vi.useFakeTimers();
    try {
      const store = createStore();
      store.sessions.value = [
        { ...store.sessions.value[0], projectId: 'project-1', worktreeDir: '/tmp/tree' },
      ];
      store.projectsEnabled.value = true;
      const search = vi.fn(async (_body: unknown, _sessionId?: string, _signal?: AbortSignal) => ({
        active: true,
        token: { start_utf16: 0, end_utf16: 3, query: 'ja' },
        items: [
          {
            path: 'jar.go',
            kind: 'file' as const,
            insert_text: '@jar.go',
            segments: [{ text: 'jar', matched: true }, { text: '.go' }],
          },
        ],
      }));
      store.endpoints.mentionSearch = search;
      render(
        <StoreContext.Provider value={store}>
          <Composer />
        </StoreContext.Provider>,
      );
      const input = screen.getByRole('textbox', { name: 'Message' }) as HTMLTextAreaElement;
      fireEvent.input(input, { target: { value: '@ja', selectionStart: 3, selectionEnd: 3 } });
      expect(search).not.toHaveBeenCalled();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(50);
      });
      expect(search).toHaveBeenCalledWith(
        expect.objectContaining({
          text: '@ja',
          cursor_utf16: 3,
          limit: 10,
          project_id: 'project-1',
          worktree_dir: '/tmp/tree',
        }),
        's1',
        expect.any(AbortSignal),
      );
      await act(async () => Promise.resolve());
      expect(screen.getByRole('option', { name: /@jarvisAgent/ })).toBeInTheDocument();
      expect(screen.getByRole('option', { name: /jar\.gofile/ })).toBeInTheDocument();
      const firstSignal = search.mock.calls[0][2] as AbortSignal;
      fireEvent.input(input, { target: { value: '@jab', selectionStart: 4, selectionEnd: 4 } });
      expect(firstSignal.aborted).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('dismisses completions with Escape without mutating the prompt', async () => {
    const store = createStore();
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    const input = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(input, '@ja');
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    await userEvent.keyboard('{Escape}');
    expect(input).toHaveValue('@ja');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  });

  it('restores manual rename saving without submitting an ancestor form', async () => {
    const store = createStore();
    store.renameTarget.value = { ...store.sessions.value[0], name: 'Custom label' };
    store.modal.value = 'rename';
    store.renameSession = vi
      .fn()
      .mockRejectedValueOnce(new Error('Could not rename this session'))
      .mockResolvedValue(undefined);
    const submit = vi.fn((event: SubmitEvent) => event.preventDefault());
    render(
      <StoreContext.Provider value={store}>
        <form onSubmit={submit}>
          <Modals />
        </form>
      </StoreContext.Provider>,
    );

    const input = screen.getByRole('textbox', { name: 'Session name' });
    expect(input).toHaveValue('Custom label');
    await userEvent.clear(input);
    await userEvent.type(input, 'New label');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(submit).not.toHaveBeenCalled();
    expect(store.renameSession).toHaveBeenCalledWith({ name: 'New label' });
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not rename this session');
    expect(screen.getByRole('dialog', { name: 'Rename session' })).toBeInTheDocument();
  });

  it('restores editable AI title previews in the rename dialog', async () => {
    const store = createStore();
    store.renameTarget.value = store.sessions.value[0];
    store.modal.value = 'rename';
    store.improveTitle = vi.fn(async () => ({
      title: 'Suggested title',
      detail: 'Suggested detail',
    }));
    store.renameSession = vi.fn(async () => undefined);
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('textbox', { name: 'Session name' })).toHaveValue('Test');
    await userEvent.click(screen.getByRole('button', { name: 'Improve title with AI' }));
    const title = await screen.findByRole('textbox', { name: 'Title' });
    const detail = screen.getByRole('textbox', { name: 'Detail' });
    expect(title).toHaveValue('Suggested title');
    expect(detail).toHaveValue('Suggested detail');
    expect(screen.getByRole('button', { name: 'Try again with AI' })).toBeInTheDocument();
    await userEvent.clear(title);
    await userEvent.type(title, 'Edited suggestion');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(store.renameSession).toHaveBeenCalledWith({
      generatedShortTitle: 'Edited suggestion',
      generatedLongTitle: 'Suggested detail',
    });
  });

  it('keeps the fallback title editable when AI refinement abstains', async () => {
    const store = createStore();
    store.renameTarget.value = store.sessions.value[0];
    store.modal.value = 'rename';
    store.improveTitle = vi.fn(async () => ({
      title: 'Test',
      detail: 'Existing detail',
      abstained: true,
    }));
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: 'Improve title with AI' }));

    expect(await screen.findByRole('status')).toHaveTextContent(
      "AI couldn't produce a specific title from this session",
    );
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Title' })).toHaveValue('Test');
    expect(screen.getByRole('textbox', { name: 'Detail' })).toHaveValue('Existing detail');
  });

  it('only exposes the widget sidebar launcher when widgets are discovered', () => {
    const store = createStore();

    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );

    expect(screen.queryByRole('button', { name: 'Widgets' })).not.toBeInTheDocument();

    act(() => {
      store.widgets.value = [{ id: 'usage', name: 'Usage', url: '/ui/widgets/usage/' }];
    });
    expect(screen.getByRole('button', { name: 'Widgets' })).toBeInTheDocument();

    act(() => {
      store.showWidgets.value = false;
    });
    expect(screen.queryByRole('button', { name: 'Widgets' })).not.toBeInTheDocument();
  });

  it('renders widgets as a bounded, searchable launcher with useful status details', () => {
    const store = createStore();
    store.modal.value = 'widgets';
    store.widgets.value = [
      {
        id: 'usage',
        name: 'Usage dashboard',
        url: '/ui/widgets/usage/',
        mount: 'usage',
        description: 'Track token usage and cost.',
        state: 'running',
      },
      {
        id: 'discourse',
        name: 'Discourse SQL Lab',
        url: '/ui/widgets/discourse/',
        mount: 'discourse',
        description: 'Query community data.',
        state: 'error',
        error: 'DISCOURSE_API_KEY is missing',
      },
      ...Array.from({ length: 6 }, (_, index) => ({
        id: `tool-${index}`,
        name: `Local tool ${index + 1}`,
        url: `/ui/widgets/tool-${index}/`,
        mount: `tool-${index}`,
        description: `Local utility ${index + 1}`,
        state: 'stopped',
      })),
    ];

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('dialog', { name: 'Widgets' })).toHaveClass('widgets-modal');
    expect(screen.getByText('Open a local tool without leaving your workspace.')).toBeVisible();
    expect(screen.getByLabelText('8 widgets available')).toHaveTextContent('8 available');
    expect(screen.getByRole('link', { name: 'Open Usage dashboard, Running' })).toHaveAttribute(
      'href',
      '/ui/widgets/usage/',
    );
    expect(screen.getByText('Track token usage and cost.')).toBeVisible();
    expect(screen.getByText('Unavailable')).toBeVisible();
    expect(screen.getByText('DISCOURSE_API_KEY is missing')).toBeVisible();
    expect(screen.getAllByRole('link')[0]).toHaveAccessibleName(
      'Open Discourse SQL Lab, Unavailable',
    );

    const filter = screen.getByRole('searchbox', { name: 'Filter widgets' });
    fireEvent.input(filter, { target: { value: 'discourse' } });
    expect(screen.getByRole('link', { name: 'Open Discourse SQL Lab, Unavailable' })).toBeVisible();
    expect(
      screen.queryByRole('link', { name: 'Open Usage dashboard, Running' }),
    ).not.toBeInTheDocument();

    fireEvent.input(filter, { target: { value: 'no such widget' } });
    expect(screen.getByText('No matching widgets')).toBeVisible();
  });

  it('stops a running widget without opening it', async () => {
    const store = createStore();
    store.modal.value = 'widgets';
    store.widgets.value = [
      {
        id: 'usage',
        name: 'Usage dashboard',
        url: '/ui/widgets/usage/',
        mount: 'usage',
        state: 'running',
      },
    ];
    store.endpoints.stopWidget = vi.fn(async () => ({ ok: true }));

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Stop Usage dashboard' }));

    await waitFor(() => expect(store.endpoints.stopWidget).toHaveBeenCalledWith('usage'));
    await waitFor(() =>
      expect(
        screen.queryByRole('button', { name: 'Stop Usage dashboard' }),
      ).not.toBeInTheDocument(),
    );
    expect(screen.getByRole('link', { name: 'Open Usage dashboard' })).toHaveAttribute(
      'href',
      '/ui/widgets/usage/',
    );
  });

  it('restores the rich, searchable MCP server picker', async () => {
    const store = createStore();
    store.modal.value = 'mcp';
    store.mcp.value = {
      servers: [
        {
          name: 'github',
          configured: true,
          enabled: true,
          status: 'ready',
          error: '',
          refreshWarning: '',
          tools: 12,
          active: 4,
          deferred: 8,
          loadingMode: 'dynamic',
        },
        {
          name: 'discourse',
          configured: true,
          enabled: false,
          status: 'failed',
          error: 'Missing DISCOURSE_API_KEY',
          refreshWarning: '',
          tools: 0,
          active: 0,
          deferred: 0,
          loadingMode: '',
        },
      ],
      enabled: ['github'],
      loading: false,
      pending: '',
      error: '',
    };
    store.toggleMCP = vi.fn(async () => undefined);
    store.loadMCP = vi.fn(async () => undefined);

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('dialog', { name: 'MCP servers' })).toHaveClass('mcp-modal');
    expect(screen.getByText('Turn on servers to add their tools.')).toBeVisible();
    expect(screen.queryByText(/Changes save immediately/)).not.toBeInTheDocument();
    expect(screen.queryByText('Tools load when enabled')).not.toBeInTheDocument();
    expect(screen.getByLabelText('1 server enabled')).toHaveTextContent('1 of 2 on');
    expect(screen.getByText('12 tools · 4 active, 8 deferred')).toBeVisible();
    expect(screen.getByRole('alert')).toHaveTextContent('MCP server error');
    expect(screen.getByRole('region', { name: 'MCP server error details' })).toHaveClass(
      'mcp-error-details',
    );
    expect(screen.getByText('Missing DISCOURSE_API_KEY')).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(store.loadMCP).toHaveBeenCalledOnce();
    expect(screen.getByRole('checkbox', { name: 'Disable github' })).toBeChecked();

    const filter = screen.getByRole('searchbox', { name: 'Filter MCP servers' });
    fireEvent.input(filter, { target: { value: 'git' } });
    expect(screen.getByText('github')).toBeVisible();
    expect(screen.queryByRole('checkbox', { name: 'Enable discourse' })).not.toBeInTheDocument();
    fireEvent.input(filter, { target: { value: 'missing-name' } });
    expect(screen.getByText('No matching servers')).toBeVisible();
    expect(screen.getByLabelText('1 server enabled')).toHaveTextContent('1 of 2 on');

    fireEvent.input(filter, { target: { value: '' } });
    await userEvent.click(screen.getByRole('checkbox', { name: 'Enable discourse' }));
    expect(store.toggleMCP).toHaveBeenCalledWith('discourse');
  });

  it('separates MCP enablement from OAuth sign-in actions', async () => {
    const store = createStore();
    store.modal.value = 'mcp';
    store.mcp.value = {
      servers: [
        {
          name: 'protected',
          configured: true,
          enabled: true,
          status: 'auth_required',
          error: '',
          refreshWarning: '',
          tools: 0,
          active: 0,
          deferred: 0,
          loadingMode: '',
          authState: 'needs_sign_in',
          authIssuer: 'https://auth.example',
          authScopes: ['read'],
          authExpiresAt: '',
          canSignIn: true,
          canSignOut: false,
        },
      ],
      enabled: ['protected'],
      loading: false,
      pending: '',
      error: '',
      oauth: {},
    };
    store.startMCPOAuth = vi.fn(async () => undefined);

    const { rerender } = render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    expect(screen.getByText('Server enabled · sign-in required')).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Sign in again' }));
    expect(store.startMCPOAuth).toHaveBeenCalledWith('protected', true);
    expect(screen.getByRole('checkbox', { name: 'Disable protected' })).toBeChecked();

    store.mcp.value = {
      ...store.mcp.value,
      oauth: {
        protected: {
          flowId: 'flow-safe-id',
          authorizationURL: 'https://auth.example/authorize',
          state: 'pending',
          error: '',
          popupBlocked: true,
        },
      },
    };
    store.copyMCPOAuthLink = vi.fn(async () => undefined);
    store.cancelMCPOAuth = vi.fn(async () => undefined);
    rerender(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    await waitFor(() =>
      expect(screen.getByText('Popup blocked — copy the sign-in link.')).toBeVisible(),
    );
    await userEvent.click(screen.getByRole('button', { name: 'Copy link' }));
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(store.copyMCPOAuthLink).toHaveBeenCalledWith('protected');
    expect(store.cancelMCPOAuth).toHaveBeenCalledWith('protected');
  });

  it('does not claim the MCP config is empty when loading fails', () => {
    const store = createStore();
    store.modal.value = 'mcp';
    store.mcp.value = {
      servers: [],
      enabled: [],
      loading: false,
      pending: '',
      error: 'session is temporarily busy',
    };

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByText('Unable to load MCP servers')).toBeVisible();
    expect(screen.queryByText('No MCP servers configured')).not.toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('session is temporarily busy');
  });

  it('creates a point-in-time share with provider capabilities and explicit visibility', async () => {
    const store = createStore();
    store.shareTarget.value = { sessionId: store.sessions.value[0].id, anchorMessageId: 42 };
    store.modal.value = 'share';
    store.endpoints.sharingCapabilities = vi.fn(async () => ({
      enabled: true,
      provider: { id: 'github', name: 'GitHub Gist' },
      operations: ['create', 'update'] as Array<'create' | 'update'>,
      visibilities: ['unlisted', 'public'] as Array<'unlisted' | 'public'>,
      default_visibility: 'unlisted' as const,
      help: 'Use the active GitHub account.',
      notes: ['Unlisted Gists are not private.'],
    }));
    let resolveShare!: (value: SessionShareResponse) => void;
    store.endpoints.createSessionShare = vi.fn(
      () =>
        new Promise<SessionShareResponse>((resolve) => {
          resolveShare = resolve;
        }),
    );
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(await screen.findByRole('heading', { name: 'Share transcript' })).toBeInTheDocument();
    expect(await screen.findByRole('radio', { name: /This response/ })).toBeChecked();
    expect(screen.getByRole('radio', { name: /Unlisted/ })).toBeChecked();

    await userEvent.click(screen.getByRole('radio', { name: /Conversation up to here/ }));
    await userEvent.click(screen.getByRole('radio', { name: /Public/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Create share' }));

    expect(store.endpoints.createSessionShare).toHaveBeenCalledWith(store.sessions.value[0].id, {
      anchor_message_id: 42,
      scope: 'conversation',
      visibility: 'public',
    });
    expect(screen.getByText('Creating share…')).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Close Share transcript' }),
    ).not.toBeInTheDocument();
    const backdrop = container.querySelector('.modal-overlay') as HTMLElement;
    fireEvent.pointerDown(backdrop, { pointerId: 9 });
    fireEvent.pointerUp(backdrop, { pointerId: 9 });
    expect(store.modal.value).toBe('share');

    await act(async () =>
      resolveShare({
        provider: 'github',
        id: 'abc123',
        url: 'https://share.example/abc123',
        source_url: 'https://source.example/abc123',
        visibility: 'public',
        ready: true,
        scope: 'conversation',
      }),
    );
    expect(await screen.findByRole('heading', { name: 'Share created' })).toBeVisible();
    expect(screen.getByRole('link', { name: 'Open link' })).toHaveAttribute(
      'href',
      'https://share.example/abc123',
    );
    expect(screen.getByRole('link', { name: 'View source ↗' })).toHaveAttribute(
      'href',
      'https://source.example/abc123',
    );
  });

  it('uses generic wording and a statement for a command provider with one visibility', async () => {
    const store = createStore();
    store.shareTarget.value = { sessionId: store.sessions.value[0].id, anchorMessageId: 42 };
    store.modal.value = 'share';
    store.endpoints.sharingCapabilities = vi.fn(async () => ({
      enabled: true,
      provider: { id: 'acme', name: 'Acme Vault' },
      operations: ['create'] as Array<'create'>,
      visibilities: ['private'] as Array<'private'>,
      default_visibility: 'private' as const,
      notes: ['Only invited teammates can open this link.'],
    }));
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(await screen.findByText('Create a share with Acme Vault.')).toBeVisible();
    expect(screen.getByText('Private')).toBeVisible();
    expect(screen.queryByRole('radio', { name: /Private/ })).not.toBeInTheDocument();
    expect(container).not.toHaveTextContent(/GitHub|Gist|\bgh\b|gisthost/i);
  });

  it('preserves share choices and shows a curated generic provider failure', async () => {
    const store = createStore();
    store.shareTarget.value = { sessionId: store.sessions.value[0].id, anchorMessageId: 42 };
    store.modal.value = 'share';
    store.endpoints.sharingCapabilities = vi.fn(async () => ({
      enabled: true,
      provider: { id: 'acme', name: 'Acme Vault' },
      operations: ['create'] as Array<'create'>,
      visibilities: ['private'] as Array<'private'>,
      default_visibility: 'private' as const,
      help: 'Sign in to Acme Vault and try again.',
    }));
    store.endpoints.createSessionShare = vi.fn(async () => {
      throw new Error('Authentication is required by the sharing provider.');
    });
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    await screen.findByRole('radio', { name: /Conversation up to here/ });
    await userEvent.click(screen.getByRole('radio', { name: /Conversation up to here/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Create share' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Authentication is required by the sharing provider.',
    );
    expect(screen.getByRole('radio', { name: /Conversation up to here/ })).toBeChecked();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeEnabled();
    expect(container).not.toHaveTextContent(/GitHub|Gist|\bgh\b|gisthost/i);
  });
  it('offers legacy branch-context choices before creating a path', async () => {
    const store = createStore();
    store.branchTarget.value = '42';
    store.modal.value = 'branch-context';
    store.branchFrom = vi.fn(async () => true);
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('heading', { name: 'Start a conversation path' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Bring concise notes/ }));
    expect(store.branchFrom).toHaveBeenCalledWith('42', 'notes', '');
  });

  it('shows branch progress and a visible failure while blocking double-clicks', async () => {
    const store = createStore();
    store.branchTarget.value = '42';
    store.modal.value = 'branch-context';
    let rejectRequest!: (reason?: unknown) => void;
    const request = new Promise<Record<string, unknown>>((_resolve, reject) => {
      rejectRequest = reject;
    });
    store.endpoints.branch = vi.fn(() => request);
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    const notes = screen.getByRole('button', { name: /Bring concise notes/ });
    await userEvent.dblClick(notes);

    expect(store.endpoints.branch).toHaveBeenCalledOnce();
    expect(notes).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Close Start a conversation path' })).toBeDisabled();
    expect(screen.getByRole('status')).toHaveTextContent('Creating path…');

    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    const backdrop = container.querySelector('.modal-overlay') as HTMLElement;
    fireEvent.pointerDown(backdrop, { pointerId: 7 });
    fireEvent.pointerUp(backdrop, { pointerId: 7 });
    expect(store.modal.value).toBe('branch-context');

    await act(async () => rejectRequest(new Error('branch service unavailable')));
    expect(await screen.findByRole('alert')).toHaveTextContent('branch service unavailable');
    expect(notes).toBeEnabled();
  });

  it('opens a conversation path from anywhere on its row', async () => {
    const store = createStore();
    store.sessions.value = [
      store.sessions.value[0],
      { ...store.sessions.value[0], id: 's2', title: 'Focused branch', number: 8 },
    ];
    store.selectSession = vi.fn(async () => undefined);
    store.branchTree.value = {
      root_session_id: 's1',
      active_session_id: 's2',
      path_count: 2,
      nodes: [
        { session_id: 's1', title: 'Original path', session_number: 7 },
        {
          session_id: 's2',
          title: 'Focused branch',
          session_number: 8,
          anchor_preview: 'Earlier answer',
          created_at: '2026-08-28T12:00:00Z',
        },
      ],
    };
    store.modal.value = 'branch';

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    const dialog = screen.getByRole('dialog', { name: 'Conversation paths' });
    expect(dialog).toHaveClass('branch-tree-modal');
    expect(dialog).not.toHaveClass('wide-modal');
    expect(screen.getByText('Original path')).toBeVisible();
    expect(screen.getByText('Focused branch')).toBeVisible();
    expect(screen.getByText('After “Earlier answer”')).toBeVisible();
    expect(screen.getByText('Origin')).toBeVisible();
    const currentPath = screen.getByText('Current').closest('.branch-tree-item');
    expect(currentPath).toHaveAttribute('aria-current', 'true');
    expect(screen.queryByRole('button', { name: 'Open' })).not.toBeInTheDocument();
    const originalPath = screen.getByRole('button', { name: /Original path/ });
    expect(originalPath).toHaveClass('branch-tree-item');
    await userEvent.click(originalPath);
    expect(store.selectSession).toHaveBeenCalledWith(store.sessions.value[0]);
    expect(store.modal.value).toBe('');
    expect(
      screen.queryByText(/Switch between independently resumable paths/),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/Conversation #/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Create a new path from/)).not.toBeInTheDocument();
  });

  it('offers editable turns from the conversation tree', async () => {
    const store = createStore();
    store.branchTree.value = {
      root_session_id: 's1',
      active_session_id: 's1',
      path_count: 1,
      nodes: [{ session_id: 's1', title: 'Original path' }],
      branch_points: [
        {
          message_id: 11,
          anchor_message_id: 0,
          sequence: 0,
          role: 'user',
          preview: 'First question',
          prefill: 'First question',
          later_message_count: 2,
        },
      ],
    };
    store.modal.value = 'branch';

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByText('Existing paths')).toBeVisible();
    expect(screen.getByText('Branch from a message')).toBeVisible();
    const point = screen.getByRole('button', {
      name: 'Branch from message 1: First question',
    });
    expect(point).toHaveTextContent('First question');
    expect(point).not.toHaveTextContent('Edit:');
    expect(point).toHaveTextContent('Message 1 · 2 later messages');
    await userEvent.click(point);
    expect(store.branchTarget.value).toBe('0');
    expect(store.branchPrefill.value).toBe('First question');
    expect(screen.getByRole('dialog', { name: 'Start a conversation path' })).toBeVisible();
  });

  it('makes large conversation trees searchable and reveals older messages in batches', async () => {
    const store = createStore();
    store.branchTree.value = {
      root_session_id: 's1',
      active_session_id: 's1',
      path_count: 1,
      nodes: [{ session_id: 's1', title: 'Original path' }],
      branch_points: Array.from({ length: 55 }, (_, index) => ({
        message_id: index + 1,
        anchor_message_id: index,
        sequence: index,
        role: 'user',
        preview: index === 2 ? 'Deployment concern' : `Question ${index + 1}`,
        prefill: index === 2 ? 'Deployment concern' : `Question ${index + 1}`,
        later_message_count: 54 - index,
      })),
    };
    store.modal.value = 'branch';

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    const initialPoints = screen.getAllByRole('button', { name: /Branch from message/ });
    expect(initialPoints).toHaveLength(50);
    expect(initialPoints[0]).toHaveAccessibleName('Branch from message 55: Question 55');
    expect(
      screen.queryByRole('button', { name: 'Branch from message 3: Deployment concern' }),
    ).not.toBeInTheDocument();

    const filter = screen.getByRole('searchbox', {
      name: 'Filter conversation paths and messages',
    });
    await userEvent.type(filter, 'deployment concern');
    expect(
      screen.getByRole('button', { name: 'Branch from message 3: Deployment concern' }),
    ).toBeVisible();
    expect(screen.queryByText('Original path')).not.toBeInTheDocument();

    await userEvent.keyboard('{Escape}');
    expect(filter).toHaveValue('');
    expect(screen.getByRole('dialog', { name: 'Conversation paths' })).toBeVisible();
    expect(screen.getAllByRole('button', { name: /Branch from message/ })).toHaveLength(50);

    await userEvent.click(screen.getByRole('button', { name: 'Show 5 older messages' }));
    expect(screen.getAllByRole('button', { name: /Branch from message/ })).toHaveLength(55);
    expect(screen.getByRole('button', { name: 'Branch from message 1: Question 1' })).toBeVisible();
  });

  it('prioritizes approval and ask-user prompts without losing the underlying modal', () => {
    const store = createStore();
    store.activeSessionId.value = 's1';
    store.modal.value = 'settings';
    store.askUser.value = {
      sessionId: 's1',
      callId: 'ask-1',
      questions: [{ question: 'Question?', options: [] }],
    };
    store.approval.value = {
      sessionId: 's1',
      id: 'approval-1',
      title: 'Approval first',
      options: [
        { index: 0, choice: 'allow', label: 'Allow' },
        { index: 1, choice: 'deny', label: 'Deny' },
      ],
    };
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('heading', { name: 'Approval first' })).toBeInTheDocument();
    act(() => {
      store.approval.value = null;
    });
    expect(screen.getByRole('heading', { name: 'Answer question' })).toBeInTheDocument();
    act(() => {
      store.askUser.value = null;
    });
    expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument();
  });

  it('never renders a background-session interaction over the active conversation', () => {
    const store = createStore();
    store.activeSessionId.value = 's2';
    store.modal.value = 'settings';
    store.interactionStore.upsert('ask-user', 's1', 'r1', 'ask-1', {
      sessionId: 's1',
      callId: 'ask-1',
      questions: [{ question: 'Background question?', options: [] }],
    });

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument();
    expect(screen.queryByText('Background question?')).not.toBeInTheDocument();
  });

  it('creates clean worktrees by default and only offers the choice for a dirty root', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true, dirty_files: 6 },
    ];
    store.createWorktree = vi.fn(async () => {});

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    const clean = screen.getByRole('checkbox', { name: /Start clean/ });
    const name = screen.getByLabelText('New worktree name');
    expect(clean).toBeChecked();
    expectPasswordManagersIgnored(name);
    expect(screen.getByText(/Leave 6 changed files in the root checkout/)).toBeVisible();

    fireEvent.input(name, {
      target: { value: 'clean-tree' },
    });
    await userEvent.click(screen.getByRole('button', { name: 'Create' }));
    expect(store.createWorktree).toHaveBeenCalledWith('clean-tree', true);

    act(() => {
      store.worktrees.value = [
        { name: 'root', dir: '/repo', repo_root: '/repo', root: true, dirty_files: 0 },
      ];
    });
    expect(screen.queryByRole('checkbox', { name: /Start clean/ })).not.toBeInTheDocument();

    store.createWorktree = vi.fn(async () => {
      throw new Error('creation blocked');
    });
    fireEvent.input(screen.getByLabelText('New worktree name'), {
      target: { value: 'blocked-tree' },
    });
    await userEvent.click(screen.getByRole('button', { name: 'Create' }));
    expect(await screen.findByText('Worktree operation failed')).toBeVisible();
    expect(screen.getByText('creation blocked')).toBeVisible();
    expect(screen.queryByText('Couldn’t load worktrees')).not.toBeInTheDocument();
  });

  it('renders worktrees as a compact draft picker without duplicating the root checkout', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.draftActive.value = true;
    store.activeSessionId.value = '';
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true },
      {
        name: 'feature-polish',
        dir: '/worktrees/feature-polish',
        repo_root: '/repo',
        branch: 'feature-polish',
        dirty_files: 3,
      },
    ];

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(screen.getByRole('dialog', { name: 'Worktrees' })).toHaveClass('worktree-modal');
    expect(screen.getAllByText('root checkout')).toHaveLength(1);
    expect(screen.getByLabelText('3 changed files')).toBeVisible();
    expect(screen.getByRole('radio', { name: /root checkout/ })).toHaveAttribute(
      'aria-checked',
      'true',
    );

    await userEvent.click(screen.getByRole('button', { name: 'Manage feature-polish' }));
    expect(screen.getByRole('heading', { name: 'feature-polish' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Run this conversation here' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Remove…' })).toBeVisible();
    expect(store.selectedDraftWorktree.value).toBe('');
    expect(store.modal.value).toBe('worktrees');

    await userEvent.click(screen.getByRole('button', { name: 'All worktrees' }));
    await userEvent.click(screen.getByRole('radio', { name: /feature-polish/ }));
    expect(store.selectedDraftWorktree.value).toBe('/worktrees/feature-polish');
    expect(store.modal.value).toBe('');
  });

  it('switches existing conversations and opens worktree changes in the rich diff panel', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        projectId: 'project-1',
        projectName: 'Term LLM',
        workingDir: '/repo',
        worktreeDir: '',
      },
    ];
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true },
      { name: 'feature-polish', dir: '/worktrees/feature-polish', dirty_files: 2 },
    ];
    store.switchWorktree = vi.fn(async () => undefined);
    store.openWorktreeDiff = vi.fn(async () => undefined);

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    await userEvent.click(screen.getByRole('button', { name: /^feature-polish/ }));
    expect(store.switchWorktree).toHaveBeenCalledWith('/worktrees/feature-polish');

    await userEvent.click(screen.getByRole('button', { name: 'Manage feature-polish' }));
    expect(screen.getByRole('button', { name: 'Switch conversation here' })).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Show changes' }));
    expect(store.openWorktreeDiff).toHaveBeenCalledWith(
      '/worktrees/feature-polish',
      'feature-polish',
    );
    expect(store.modal.value).toBe('');
  });

  it('returns to the list after branch promotion removes the old checkout', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        projectId: 'project-1',
        projectName: 'Term LLM',
        worktreeDir: '/worktrees/feature-polish',
      },
    ];
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true },
      { name: 'feature-polish', dir: '/worktrees/feature-polish', dirty_files: 2 },
    ];
    store.promoteWorktree = vi.fn(async () => ({ cleanup: { removed: true } }));

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(await screen.findByRole('heading', { name: 'feature-polish' })).toBeInTheDocument();
    const branch = screen.getByLabelText('Branch name');
    expectPasswordManagersIgnored(branch);
    fireEvent.input(branch, {
      target: { value: 'feature/promoted' },
    });
    await userEvent.click(screen.getByRole('button', { name: 'Promote' }));

    expect(store.promoteWorktree).toHaveBeenCalledWith(
      '/worktrees/feature-polish',
      'feature/promoted',
    );
    expect(screen.getByText('Project checkouts')).toBeVisible();
    expect(screen.queryByRole('heading', { name: 'feature-polish' })).not.toBeInTheDocument();
  });

  it('offers shared assisted recovery after a promotion conflict and runs it on confirmation', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        projectId: 'project-1',
        projectName: 'Term LLM',
        worktreeDir: '/worktrees/feature-polish',
      },
    ];
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true },
      { name: 'feature-polish', dir: '/worktrees/feature-polish', branch: 'feature-polish' },
    ];
    const recovery = {
      kind: 'conflict',
      title: 'Assisted Worktree Recovery',
      question: 'Would you like me to resolve it directly in the root checkout?',
      yes_label: 'Yes — start assisted recovery',
      no_label: 'No — leave everything unchanged',
      details: 'Source: /worktrees/feature-polish\nRoot: /repo\nConflicts: file.txt',
      available: true,
      decline_message: 'Okay — leaving the root checkout clean.',
    };
    store.mergeWorktree = vi.fn(async () => {
      throw new APIError(
        'promotion conflicts',
        409,
        JSON.stringify({ error: 'conflicts', recovery }),
      );
    });
    store.recoverWorktree = vi.fn(async () => ({}));

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    await userEvent.click(await screen.findByRole('button', { name: 'Merge into root' }));
    expect(await screen.findByText('Assisted Worktree Recovery')).toBeVisible();
    expect(screen.getByText(/resolve it directly in the root checkout/)).toBeVisible();
    expect(screen.getByText(/Conflicts: file.txt/)).toBeVisible();

    const confirmRecovery = screen.getByRole('button', { name: recovery.yes_label });
    await waitFor(() => expect(confirmRecovery).toHaveFocus());
    await userEvent.click(confirmRecovery);
    expect(store.recoverWorktree).toHaveBeenCalledWith('/worktrees/feature-polish');
  });

  it('keeps the checkout unchanged when assisted recovery is declined', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        projectId: 'project-1',
        worktreeDir: '/worktrees/feature-polish',
      },
    ];
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true },
      { name: 'feature-polish', dir: '/worktrees/feature-polish' },
    ];
    store.mergeWorktree = vi.fn(async () => {
      throw new APIError(
        'promotion conflicts',
        409,
        JSON.stringify({
          recovery: {
            kind: 'conflict',
            title: 'Assisted Worktree Recovery',
            question: 'Run recovery?',
            yes_label: 'Yes — start assisted recovery',
            no_label: 'No — leave everything unchanged',
            available: true,
            decline_message: 'Okay — leaving the root checkout clean.',
          },
        }),
      );
    });
    store.recoverWorktree = vi.fn(async () => ({}));

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    await userEvent.click(await screen.findByRole('button', { name: 'Merge into root' }));
    await userEvent.click(
      await screen.findByRole('button', { name: 'No — leave everything unchanged' }),
    );
    expect(store.recoverWorktree).not.toHaveBeenCalled();
    expect(screen.getByRole('status')).toHaveTextContent('leaving the root checkout clean');
  });

  it('merges immediately even when the root checkout may have an active run', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        projectId: 'project-1',
        projectName: 'Term LLM',
        worktreeDir: '/worktrees/feature-polish',
      },
    ];
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true },
      {
        name: 'feature-polish',
        dir: '/worktrees/feature-polish',
        branch: 'feature-polish',
      },
    ];
    store.mergeWorktree = vi.fn().mockResolvedValue({ cleanup: { removed: false } });

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    await userEvent.click(await screen.findByRole('button', { name: 'Merge into root' }));
    expect(store.mergeWorktree).toHaveBeenCalledOnce();
    expect(store.mergeWorktree).toHaveBeenCalledWith('/worktrees/feature-polish', true);
    expect(screen.queryByText('The root checkout is in use')).not.toBeInTheDocument();
  });

  it('opens the bound worktree detail and escalates removal inline without confirm()', async () => {
    const store = createStore();
    store.modal.value = 'worktrees';
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        projectId: 'project-1',
        projectName: 'Term LLM',
        worktreeDir: '/worktrees/feature-polish',
      },
    ];
    store.worktrees.value = [
      { name: 'root', dir: '/repo', repo_root: '/repo', root: true },
      {
        name: 'feature-polish',
        dir: '/worktrees/feature-polish',
        branch: 'feature-polish',
        dirty_files: 2,
        in_use: [{ id: 's1', name: 'Polish worktrees' }],
      },
    ];
    store.removeWorktree = vi.fn(async () => {
      throw new APIError(
        'worktree_in_use',
        409,
        JSON.stringify({
          error: 'worktree_in_use',
          message: 'This worktree is still in use.',
          in_use: [{ id: 's1', name: 'Polish worktrees' }],
        }),
      );
    });

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(await screen.findByRole('heading', { name: 'feature-polish' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Merge into root' })).toBeVisible();
    expect(screen.getByLabelText('2 changed files')).toBeVisible();

    await userEvent.click(screen.getByRole('button', { name: 'Remove…' }));
    expect(screen.getByRole('button', { name: 'Confirm remove' })).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Confirm remove' }));
    expect(store.removeWorktree).toHaveBeenCalledWith('/worktrees/feature-polish', false);
    expect(await screen.findByRole('button', { name: 'Force remove' })).toBeVisible();
    expect(screen.getByRole('status')).toHaveTextContent('In use by Polish worktrees.');

    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Worktrees' }), { key: 'Escape' });
    expect(screen.getByText('Project checkouts')).toBeVisible();
    expect(store.modal.value).toBe('worktrees');
  });

  it('restores workspace-aware project assignment with recommendations and counts', async () => {
    const store = createStore();
    const target = store.sessions.value[0];
    store.projectTarget.value = target;
    store.modal.value = 'project';
    store.projects.value = [
      {
        id: 'project-current',
        name: 'Current project',
        path: '/home/me/repo',
        archived: false,
        available: true,
        git: true,
        sessionCount: 3,
      },
      {
        id: 'project-other',
        name: 'Other project',
        path: '/home/me/other',
        archived: false,
        available: true,
        sessionCount: 1,
      },
    ];
    store.endpoints.projectAssignment = vi.fn(async () => ({
      candidate: {
        canonical_dir: '/home/me/repo',
        default_name: 'repo',
        git: true,
        existing_project_id: 'project-current',
        existing_name: 'Current project',
        matching_conversation_count: 2,
      },
    }));
    store.endpoints.setProject = vi
      .fn()
      .mockRejectedValueOnce(new Error('Could not assign this conversation. Retry.'))
      .mockResolvedValue({ assigned_conversation_count: 2 });
    store.refreshSidebar = vi.fn(async () => undefined);

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    expect(await screen.findByRole('dialog', { name: 'Assign project' })).toHaveClass(
      'project-assign-modal',
    );
    expect(screen.queryByRole('textbox', { name: 'Project path' })).not.toBeInTheDocument();
    expect(screen.queryByText('No project')).not.toBeInTheDocument();
    expect(screen.queryByText('Grouping only')).not.toBeInTheDocument();
    expect(
      screen.getByText('Choose a sidebar group. Files and workspace stay unchanged.'),
    ).toBeVisible();
    const recommended = await screen.findByRole('radio', { name: /Current project/ });
    await waitFor(() => expect(recommended).toHaveAttribute('aria-checked', 'true'));
    expect(recommended).not.toHaveTextContent('3 conversations');
    expect(recommended).toHaveTextContent(
      '2 conversations from this workspace will be grouped here.',
    );

    await userEvent.click(screen.getByRole('button', { name: 'Assign project' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Retry');
    expect(screen.getByRole('dialog', { name: 'Assign project' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Assign project' }));
    expect(store.endpoints.setProject).toHaveBeenLastCalledWith(target.id, {
      project_id: 'project-current',
    });
    expect(store.endpoints.setProject).toHaveBeenCalledTimes(2);
    expect(store.refreshSidebar).toHaveBeenCalledOnce();
  });

  it('prefills the current folder name and creates and assigns it in one step', async () => {
    const store = createStore();
    const target = store.sessions.value[0];
    store.projectTarget.value = target;
    store.modal.value = 'project';
    store.projects.value = [];
    store.endpoints.projectAssignment = vi.fn(async () => ({
      candidate: {
        canonical_dir: '/home/me/new-repo',
        default_name: 'new-repo',
        git: true,
        matching_conversation_count: 1,
      },
    }));
    store.endpoints.setProject = vi.fn(async () => ({ assigned_conversation_count: 1 }));
    store.refreshSidebar = vi.fn(async () => undefined);

    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );

    const name = await screen.findByRole('textbox', { name: 'New project display name' });
    expect(name).toHaveValue('new-repo');
    expectPasswordManagersIgnored(name);
    expect(
      screen.getByText('1 conversation from this workspace will be grouped here.'),
    ).toBeVisible();
    fireEvent.input(name, { target: { value: 'Frontend' } });
    await userEvent.click(screen.getByRole('button', { name: 'Create & assign' }));
    expect(store.endpoints.setProject).toHaveBeenCalledWith(target.id, {
      create_from_workspace: true,
      name: 'Frontend',
    });
  });

  it('browses server folders with the real path and hidden-directory contract', async () => {
    const store = createStore();
    store.modal.value = 'project';
    const lookup = vi.fn(async (path = '', hidden = false, _signal?: AbortSignal) => ({
      path: path || '/home/me',
      parent: '/home',
      home: '/home/me',
      breadcrumbs: [{ label: 'me', path: '/home/me' }],
      entries: hidden
        ? [{ name: '.config', path: '/home/me/.config' }]
        : [{ name: 'src', path: '/home/me/src', git: true }],
    }));
    store.endpoints.projectDirectories = lookup;
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    fireEvent.input(screen.getByRole('textbox', { name: 'Project path' }), {
      target: { value: '/home/me' },
    });
    await userEvent.click(screen.getByRole('button', { name: 'Browse' }));
    expect(lookup).toHaveBeenCalledWith('/home/me', false, expect.any(AbortSignal));
    expect(screen.getByRole('option', { name: /src/ })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('checkbox', { name: /Hidden/ }));
    await vi.waitFor(() => expect(lookup).toHaveBeenLastCalledWith('/home/me', true));
    expect(screen.getByRole('option', { name: /.config/ })).toBeInTheDocument();
  });

  it('restores code-block copy controls with transient feedback', async () => {
    vi.useFakeTimers();
    const clipboardDescriptor = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    try {
      render(<Markdown value={'```sh\necho "hello"\n```'} />);
      const button = screen.getByRole('button', { name: 'Copy code' });
      await act(async () => {
        fireEvent.click(button);
        await Promise.resolve();
      });
      expect(writeText).toHaveBeenCalledWith('echo "hello"\n');
      expect(button).toHaveClass('copied');
      expect(button).toHaveAttribute('aria-label', 'Copied');
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_500);
      });
      expect(button).not.toHaveClass('copied');
      expect(button).toHaveAttribute('aria-label', 'Copy code');
    } finally {
      if (clipboardDescriptor) Object.defineProperty(navigator, 'clipboard', clipboardDescriptor);
      else Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
      vi.useRealTimers();
    }
  });

  it('does not add code-copy controls while a markdown fence is open', () => {
    render(<Markdown value={'```sh\necho "hello"'} streaming />);
    expect(screen.queryByRole('button', { name: 'Copy code' })).not.toBeInTheDocument();
  });

  it('drains a streaming burst over multiple presentation frames', async () => {
    vi.useFakeTimers();
    try {
      const target = `first ${'second '.repeat(20)}`;
      const { container, rerender } = render(<Markdown value="first" streaming />);
      rerender(<Markdown value={target} streaming />);
      expect(container).toHaveTextContent('first');
      await act(async () => {
        await vi.advanceTimersByTimeAsync(33);
      });
      expect(container.textContent).not.toBe(target);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_000);
      });
      expect(container).toHaveTextContent(target.trim());
    } finally {
      vi.useRealTimers();
    }
  });

  it('flushes the latest source immediately when streaming completes', async () => {
    vi.useFakeTimers();
    try {
      const target = `start ${'queued '.repeat(100)}`;
      const { container, rerender } = render(<Markdown value="start" streaming />);
      rerender(<Markdown value={target} streaming />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(33);
      });
      expect(container.textContent).not.toBe(target);
      rerender(<Markdown value={target} />);
      expect(container).toHaveTextContent(target.trim());
      await act(async () => {
        await vi.runAllTimersAsync();
      });
      expect(container).toHaveTextContent(target.trim());
    } finally {
      vi.useRealTimers();
    }
  });

  it('does no streaming presentation work while the tab is hidden', async () => {
    vi.useFakeTimers();
    const hidden = Object.getOwnPropertyDescriptor(document, 'hidden');
    try {
      const { container, rerender } = render(<Markdown value="visible" streaming />);
      Object.defineProperty(document, 'hidden', { configurable: true, value: true });
      document.dispatchEvent(new Event('visibilitychange'));

      const target = `visible ${'hidden '.repeat(100)}`;
      rerender(<Markdown value={target} streaming />);
      await act(async () => {
        await vi.runAllTimersAsync();
      });
      expect(container).toHaveTextContent('visible');
      expect(container.textContent).not.toBe(target);

      Object.defineProperty(document, 'hidden', { configurable: true, value: false });
      await act(async () => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      expect(container).toHaveTextContent(target.trim());
    } finally {
      if (hidden) Object.defineProperty(document, 'hidden', hidden);
      else Reflect.deleteProperty(document, 'hidden');
      vi.useRealTimers();
    }
  });

  it('rebases finalized assets without rebuilding their DOM', () => {
    const source = '![artifact](/artifact.png)';
    const { container, rerender } = render(<Markdown value={source} />);
    const image = container.querySelector('img');
    expect(image).not.toBeNull();

    rerender(<Markdown value={source} rebase={(value) => `/chat${value}`} />);
    expect(container.querySelector('img')).toBe(image);
    expect(image).toHaveAttribute('src', '/chat/artifact.png');
  });

  it('keeps one code node while an open fence grows and commits it once', async () => {
    vi.useFakeTimers();
    try {
      const first = 'intro\n\n```ts\nconst value = 1';
      const { container, rerender } = render(<Markdown value={first} streaming />);
      const pre = container.querySelector('pre');
      const code = container.querySelector('pre code');
      expect(pre).not.toBeNull();
      expect(code).toHaveTextContent('const value = 1');
      expect(code).not.toHaveAttribute('data-highlighted');

      const second = `${first};\nconsole.log(value);`;
      rerender(<Markdown value={second} streaming />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_000);
      });
      expect(container.querySelector('pre')).toBe(pre);
      expect(container.querySelector('pre code')).toBe(code);
      expect(code).toHaveTextContent('console.log(value);');
      expect(screen.queryByRole('button', { name: 'Copy code' })).not.toBeInTheDocument();

      const closed = `${second}\n\`\`\`\ntrailing prose`;
      rerender(<Markdown value={closed} streaming />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_000);
      });
      expect(container.querySelector('pre')).toBe(pre);
      expect(container.querySelector('pre code')).toBe(code);
      expect(screen.getAllByRole('button', { name: 'Copy code' })).toHaveLength(1);
      expect(container).toHaveTextContent('trailing prose');

      rerender(<Markdown value={closed} />);
      await act(async () => {
        await Promise.resolve();
      });
      expect(container.querySelector('pre')).toBe(pre);
      expect(container.querySelector('pre code')).toBe(code);
      expect(code).toHaveAttribute('data-highlighted', 'yes');
      const highlighted = code?.innerHTML;
      expect(screen.getAllByRole('button', { name: 'Copy code' })).toHaveLength(1);

      rerender(<Markdown value={closed} rebase={(value) => value} />);
      expect(container.querySelector('pre')).toBe(pre);
      expect(container.querySelector('pre code')).toBe(code);
      expect(code?.innerHTML).toBe(highlighted);
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps committed code immutable when a nested list arrives later', async () => {
    vi.useFakeTimers();
    try {
      const codeSource = '```js\nconst stable = true;\n```\n\n';
      const { container, rerender } = render(<Markdown value={codeSource} streaming />);
      const pre = container.querySelector('pre');
      const code = container.querySelector('pre code');
      expect(pre).not.toBeNull();
      await act(async () => {
        await Promise.resolve();
      });
      expect(code).toHaveAttribute('data-highlighted', 'yes');

      rerender(<Markdown value={`${codeSource}- outer\n    - deeply nested\n`} streaming />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_000);
      });
      expect(container.querySelector('pre')).toBe(pre);
      expect(container.querySelector('pre code')).toBe(code);
      expect(code).toHaveAttribute('data-highlighted', 'yes');
      expect(container.querySelector('.streaming-markdown')).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('preserves completed fence nodes across every single-character chunk boundary', async () => {
    vi.useFakeTimers();
    try {
      const source = 'before\n\n```js\nconst one = 1;\n```\nbetween\n\n~~~py\nprint(2)\n~~~\nafter';
      const { container, rerender } = render(<Markdown value="" streaming />);
      let firstBlock: HTMLPreElement | null = null;
      let secondBlock: HTMLPreElement | null = null;
      for (let index = 1; index <= source.length; index += 1) {
        rerender(<Markdown value={source.slice(0, index)} streaming />);
        await act(async () => {
          await vi.advanceTimersByTimeAsync(33);
        });
        const first =
          container.querySelector('.streaming-stable > pre > code.language-js')?.closest('pre') ||
          null;
        const second =
          container.querySelector('.streaming-stable > pre > code.language-py')?.closest('pre') ||
          null;
        if (first) {
          if (!firstBlock) firstBlock = first;
          else expect(first).toBe(firstBlock);
        }
        if (second) {
          if (!secondBlock) secondBlock = second;
          else expect(second).toBe(secondBlock);
        }
      }
      const seen = [firstBlock, secondBlock] as HTMLPreElement[];
      expect(seen.every(Boolean)).toBe(true);
      expect(container.querySelectorAll('.code-copy-btn')).toHaveLength(2);
      rerender(<Markdown value={source} />);
      expect([...container.querySelectorAll('pre')]).toEqual(seen);
      expect(container).toHaveTextContent('before');
      expect(container).toHaveTextContent('after');
    } finally {
      vi.useRealTimers();
    }
  });

  it('opens ready composer images as an isolated lightbox gallery without taking URL ownership', async () => {
    const store = createStore();
    store.attachments.value = [
      {
        id: 'image-one',
        name: 'one.png',
        type: 'image/png',
        previewURL: 'blob:image-one',
        status: 'ready',
      },
      {
        id: 'notes',
        name: 'notes.txt',
        type: 'text/plain',
        dataURL: 'data:text/plain,notes',
        status: 'ready',
      },
      {
        id: 'image-two',
        name: 'two.png',
        type: 'image/png',
        previewURL: 'blob:image-two',
        status: 'ready',
      },
      {
        id: 'preparing-image',
        name: 'preparing.png',
        type: 'image/png',
        previewURL: 'blob:preparing-image',
        status: 'preparing',
        progress: 0.5,
      },
      {
        id: 'failed-image',
        name: 'failed.png',
        type: 'image/png',
        previewURL: 'blob:failed-image',
        status: 'error',
        error: 'Could not prepare image',
      },
    ];
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    try {
      render(
        <StoreContext.Provider value={store}>
          <Composer />
          <Lightbox />
        </StoreContext.Provider>,
      );

      const preview = screen.getByRole('button', { name: 'Preview two.png' });
      expect(screen.queryByRole('button', { name: 'Preview preparing.png' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Preview failed.png' })).toBeNull();
      await userEvent.click(preview);

      expect(store.lightbox.value?.items?.map((item) => item.key)).toEqual([
        'image-one',
        'image-two',
      ]);
      expect(store.lightbox.value?.index).toBe(1);
      expect(store.lightbox.value?.items?.every((item) => !item.ownsObjectURL)).toBe(true);
      expect(screen.getByRole('img', { name: 'two.png' })).toHaveAttribute('src', 'blob:image-two');
      expect(screen.getByText('2 / 2')).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Copy URL' })).toBeNull();

      fireEvent.keyDown(screen.getByRole('dialog', { name: 'Media preview' }), {
        key: 'ArrowLeft',
      });
      expect(screen.getByRole('img', { name: 'one.png' })).toHaveAttribute('src', 'blob:image-one');

      await userEvent.click(screen.getByRole('button', { name: 'Close' }));
      await waitFor(() => expect(store.lightbox.value).toBeNull());
      expect(revoke).not.toHaveBeenCalled();
      await waitFor(() => expect(preview).toHaveFocus());
    } finally {
      revoke.mockRestore();
    }
  });

  it('removes attachments directly and while reviewing the draft gallery', async () => {
    const store = createStore();
    store.attachments.value = [
      {
        id: 'direct-image',
        name: 'direct.png',
        type: 'image/png',
        previewURL: 'blob:direct-image',
        status: 'ready',
      },
      {
        id: 'image-one',
        name: 'one.png',
        type: 'image/png',
        previewURL: 'blob:image-one',
        status: 'ready',
      },
      {
        id: 'image-two',
        name: 'two.png',
        type: 'image/png',
        previewURL: 'blob:image-two',
        status: 'ready',
      },
    ];
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    try {
      render(
        <StoreContext.Provider value={store}>
          <Composer />
          <Lightbox />
        </StoreContext.Provider>,
      );

      await userEvent.click(screen.getByRole('button', { name: 'Remove direct.png' }));
      expect(store.attachments.value.map((attachment) => attachment.id)).toEqual([
        'image-one',
        'image-two',
      ]);
      expect(revoke).toHaveBeenCalledWith('blob:direct-image');

      await userEvent.click(screen.getByRole('button', { name: 'Preview two.png' }));
      const dialog = screen.getByRole('dialog', { name: 'Media preview' });
      await userEvent.click(within(dialog).getByRole('button', { name: 'Remove two.png' }));
      expect(store.attachments.value.map((attachment) => attachment.id)).toEqual(['image-one']);
      expect(revoke).toHaveBeenCalledWith('blob:image-two');
      expect(screen.getByRole('img', { name: 'one.png' })).toHaveAttribute('src', 'blob:image-one');
      expect(screen.getByText('1 / 1')).toBeInTheDocument();

      await userEvent.click(within(dialog).getByRole('button', { name: 'Remove one.png' }));
      await waitFor(() => expect(store.lightbox.value).toBeNull());
      expect(store.attachments.value).toEqual([]);
      expect(revoke).toHaveBeenCalledWith('blob:image-one');
      await waitFor(() => expect(screen.getByRole('textbox', { name: 'Message' })).toHaveFocus());
    } finally {
      revoke.mockRestore();
    }
  });

  it('pauses and clears owned video resources when the lightbox closes', async () => {
    const store = createStore();
    const trigger = document.createElement('button');
    document.body.append(trigger);
    trigger.focus();
    const pause = vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => undefined);
    const load = vi.spyOn(HTMLMediaElement.prototype, 'load').mockImplementation(() => undefined);
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    store.lightbox.value = { src: 'blob:owned-video', type: 'video', ownsObjectURL: true };
    render(
      <StoreContext.Provider value={store}>
        <Lightbox />
      </StoreContext.Provider>,
    );

    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Media preview' }), { key: 'Escape' });

    await waitFor(() => expect(store.lightbox.value).toBeNull());
    expect(pause).toHaveBeenCalled();
    expect(load).toHaveBeenCalled();
    expect(revoke).toHaveBeenCalledWith('blob:owned-video');
    await waitFor(() => expect(trigger).toHaveFocus());
    trigger.remove();
  });

  it('appends safe plain streaming text without replacing its text node', async () => {
    vi.useFakeTimers();
    try {
      const { container, rerender } = render(<Markdown value="first" streaming />);
      const tail = container.querySelector('.streaming-tail');
      const text = tail?.firstChild;
      rerender(<Markdown value="first second" streaming />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(500);
      });
      expect(container.querySelector('.streaming-tail')?.firstChild).toBe(text);
      expect(text).toHaveTextContent('first second');
    } finally {
      vi.useRealTimers();
    }
  });

  it('rebuilds canonically after a replacement streaming snapshot', async () => {
    vi.useFakeTimers();
    try {
      const { container, rerender } = render(<Markdown value={'```ts\nold'} streaming />);
      const oldCode = container.querySelector('code');
      rerender(<Markdown value={'```py\nnew'} streaming />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(33);
      });
      expect(container.querySelector('code')).not.toBe(oldCode);
      expect(container).toHaveTextContent('new');
      expect(container).not.toHaveTextContent('old');
    } finally {
      vi.useRealTimers();
    }
  });

  it('uses a bounded plain-text fallback for over-budget streaming markdown', () => {
    const value = 'x'.repeat(70_000);
    const { container } = render(<Markdown value={value} streaming />);
    expect(container.querySelector('[data-streaming-fallback="plain"]')).toHaveTextContent(value);
  });
});
