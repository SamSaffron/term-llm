import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { Transcript } from './Transcript';
import { Composer } from './Composer';
import { Markdown } from './Markdown';
import { Modals } from './Modals';
import { Sidebar } from './Sidebar';
import { Header } from './Header';
import { DiffSidebar } from './Panels';
import type { AppConfig } from '../app/config';

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
  return store;
};

describe('Preact-owned chat surfaces', () => {
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
    const trigger = screen.getByRole('button', { name: 'Runtime settings' });
    expect(trigger).toHaveTextContent('gpt-5');
    expect(trigger.querySelectorAll('.effort-meter-bar')).toHaveLength(4);
    expect(trigger).toHaveAttribute('data-effort-level', 'medium');
    await userEvent.click(trigger);
    const dialog = screen.getByRole('dialog', { name: 'Runtime settings' });
    expect(dialog.tagName).toBe('DIALOG');
    expect(dialog).toHaveAttribute('open');
    expect(dialog).toHaveTextContent('Provider, model, and effort for the next reply');
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

  it('ports the tiny legacy diff actions and transient copied state', async () => {
    vi.useFakeTimers();
    const descriptor = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    try {
      const store = createStore();
      store.diff.value = {
        ...store.diff.value,
        open: true,
        sessionId: 's1',
        files: [
          {
            path: '/home/sam/project/main.go',
            status: 'create',
            additions: 1,
            deletions: 0,
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
      expect(row.querySelector('.diff-file-base')).toHaveTextContent('main.go');
      expect(row.querySelector('.diff-file-dir')).toHaveTextContent('home/sam/project');
      expect(row).not.toHaveTextContent('/home/sam/project/main.go');
      const path = screen.getByRole('button', { name: 'Copy path /home/sam/project/main.go' });
      const patch = screen.getByRole('button', { name: 'Copy diff for /home/sam/project/main.go' });
      expect(path).toHaveTextContent('⧉');
      expect(path.querySelector('svg')).toBeNull();
      expect(patch).toHaveTextContent('±');
      expect(screen.queryByText('Patch')).not.toBeInTheDocument();
      await act(async () => {
        fireEvent.click(path);
        await Promise.resolve();
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(writeText).toHaveBeenCalledWith('/home/sam/project/main.go');
      expect(path).toHaveClass('copied');
      await act(async () => {
        fireEvent.click(patch);
        await Promise.resolve();
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(writeText).toHaveBeenLastCalledWith(
        '--- a//home/sam/project/main.go\n+++ b//home/sam/project/main.go\n@@ -1 +1 @@\n+package main\n',
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
    expect(screen.getByText(/"path": "x"/)).toBeInTheDocument();
    expect(screen.getByText('ok')).toBeInTheDocument();
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
        { id: 'a1', role: 'assistant', content: 'First segment', created: 2 },
        { id: 'a2', role: 'assistant', content: 'Final segment', created: 3 },
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
      expect(writeText).toHaveBeenCalledWith(expect.stringContaining('Final segment'));
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

  it('keeps Hub agent rows inside the shared action alignment gutter', () => {
    const store = new AppStore({
      ...config,
      hub: { url: '/hub/', nodeId: 'Dev', nodeBasePath: '/ui' },
    });
    store.hubAgents.value = [
      { id: 'Dev', name: 'Dev', target: '/node/Dev/', active: true, attention: false },
    ];
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(container.querySelector('.sidebar-actions > .hub-agent-links')).not.toBeNull();
    expect(
      container.querySelector('.hub-agent-link[aria-current="true"] .hub-agent-name'),
    ).toHaveTextContent('Dev');
  });

  it('auto-loads older sidebar conversations through the pagination sentinels', async () => {
    const store = createStore();
    store.projectsEnabled.value = true;
    store.projects.value = [
      { id: 'p1', name: 'Alpha', sessions: [], has_more: true, next_cursor: 'cursor-1' },
    ];
    store.noProjectCursor.value = 'no-project-cursor';
    store.loadMoreProject = vi.fn(async () => undefined);
    store.loadMoreNoProject = vi.fn(async () => undefined);
    const { container } = render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(container.querySelectorAll('.project-pagination-sentinel')).toHaveLength(2);
    expect(container.querySelector('.sidebar-load-more')).toBeNull();
    await waitFor(() => expect(store.loadMoreProject).toHaveBeenCalledWith('p1'));
    await waitFor(() => expect(store.loadMoreNoProject).toHaveBeenCalled());
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

  it('offers legacy branch-context choices before creating a path', async () => {
    const store = createStore();
    store.branchTarget.value = '42';
    store.modal.value = 'branch-context';
    store.branchFrom = vi.fn(async () => undefined);
    render(
      <StoreContext.Provider value={store}>
        <Modals />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('heading', { name: 'Start a conversation path' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Bring concise notes/ }));
    expect(store.branchFrom).toHaveBeenCalledWith('42', 'notes', '');
  });

  it('prioritizes approval and ask-user prompts without losing the underlying modal', () => {
    const store = createStore();
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

  it('throttles streaming markdown updates at the adaptive cadence', async () => {
    vi.useFakeTimers();
    try {
      const { container, rerender } = render(<Markdown value="first" streaming />);
      rerender(<Markdown value="second" streaming />);
      expect(container).toHaveTextContent('first');
      await act(async () => {
        await vi.advanceTimersByTimeAsync(33);
      });
      expect(container).toHaveTextContent('second');
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
