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
import { ChipPicker } from './ChipPicker';
import type { AppConfig } from '../app/config';
import { initialProjection } from '../domain/response';
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
    await userEvent.type(screen.getByRole('textbox', { name: 'Inline comment' }), 'Queue this');
    expect(screen.getByRole('button', { name: 'Send now' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'More send options' }));
    await userEvent.click(screen.getByRole('menuitem', { name: /Queue comment/ }));
    expect(store.queueDiffComment).toHaveBeenCalledWith(
      expect.objectContaining({ path: 'main.go', line: 1, body: 'Queue this' }),
    );
    expect(store.sendDiffComment).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole('button', { name: 'Comment on line 2' }));
    await userEvent.type(screen.getByRole('textbox', { name: 'Inline comment' }), 'Send this');
    await userEvent.click(screen.getByRole('button', { name: 'Send now' }));
    expect(store.sendDiffComment).toHaveBeenCalledWith(
      expect.objectContaining({ path: 'main.go', line: 2, body: 'Send this' }),
    );
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
    const argument = document.querySelector('.tool-argument');
    expect(argument?.querySelector('dt')).toHaveTextContent('path');
    expect(argument?.querySelector('dd')).toHaveTextContent('x');
    expect(screen.queryByText(/"path": "x"/)).not.toBeInTheDocument();
    expect(screen.getByText('ok')).toBeInTheDocument();
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
    expect(projectPopover).toHaveTextContent('ChatAlphaZeta');
    expect(projectPopover).not.toHaveTextContent('Archived');
    expect(projectPopover).not.toHaveTextContent('Unavailable');

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
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Reusable picker' })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
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
    render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );

    const send = screen.getByRole('button', { name: 'Send message' });
    const sendIcon = send.querySelector('.arrow')?.innerHTML;
    expect(send).toBeDisabled();
    expect(send).not.toHaveClass('interject');

    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'change course');

    const interject = screen.getByRole('button', { name: 'Interject' });
    expect(interject).toBeEnabled();
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

  it('auto-loads older sidebar conversations through the pagination sentinels', async () => {
    const store = createStore();
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
    expect(screen.getByRole('heading', { name: 'No project' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Project' })).not.toBeInTheDocument();
    expect(container.querySelectorAll('.project-pagination-sentinel')).toHaveLength(2);
    expect(container.querySelector('.sidebar-load-more')).toBeNull();
    await waitFor(() => expect(store.loadMoreProject).toHaveBeenCalledWith('p1'));
    await waitFor(() => expect(store.loadMoreNoProject).toHaveBeenCalled());

    const noProject = screen.getByRole('button', { name: 'No project' });
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
    expect(screen.getByRole('button', { name: 'No project' })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
  });

  it('lifts pinned project conversations into the global pinned section', () => {
    const store = createStore();
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
    await userEvent.type(filter, 'git');
    expect(screen.getByText('github')).toBeVisible();
    expect(screen.queryByRole('checkbox', { name: 'Enable discourse' })).not.toBeInTheDocument();
    fireEvent.input(filter, { target: { value: 'missing-name' } });
    expect(screen.getByText('No matching servers')).toBeVisible();
    expect(screen.getByLabelText('1 server enabled')).toHaveTextContent('1 of 2 on');

    fireEvent.input(filter, { target: { value: '' } });
    await userEvent.click(screen.getByRole('checkbox', { name: 'Enable discourse' }));
    expect(store.toggleMCP).toHaveBeenCalledWith('discourse');
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

  it('does not add code-copy controls while markdown is streaming', () => {
    render(<Markdown value={'```sh\necho "hello"\n```'} streaming />);
    expect(screen.queryByRole('button', { name: 'Copy code' })).not.toBeInTheDocument();
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
