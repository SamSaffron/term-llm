import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { observeRenders } from '../test/render-counts';
import { Header } from './Header';
import { Transcript } from './Transcript';
import { Sidebar } from './Sidebar';
import { DiffSidebar } from './Panels';
import { SideQuestion } from './Modals';
import { Markdown } from './Markdown';
import { markdownDocumentBlocks } from '../domain/markdown-document';
import * as markdown from '../domain/markdown';
import type { DiffFile } from '../domain/types';

const stores: AppStore[] = [];
let counts: ReturnType<typeof observeRenders> | undefined;
afterEach(() => {
  vi.useRealTimers();
  counts?.dispose();
  counts = undefined;
  stores.splice(0).forEach((store) => store.dispose());
});
const observe = () => (counts = observeRenders());
function createStore() {
  const store = new AppStore(testConfig);
  stores.push(store);
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.endpoints.models = vi.fn(async () => ({ models: [] }));
  store.endpoints.skills = vi.fn(async () => ({ skills: [] }));
  store.endpoints.tree = vi.fn(async () => ({ path_count: 1 }));
  store.endpoints.sessionState = vi.fn(async () => ({}));
  store.endpoints.selectedSession = vi.fn(async () => ({ selected_session: { id: 's1' } }));
  return store;
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe('second-pass rendering and atomic presentation', () => {
  it('uses a pending runtime boundary, then cached provider-specific capabilities', async () => {
    const store = createStore();
    store.providers.value = [
      { id: 'a', name: 'A', models: ['shared'] },
      { id: 'b', name: 'B', models: ['shared'] },
    ];
    const pending = deferred<Record<string, unknown>>();
    store.endpoints.models = vi.fn(() => pending.promise);
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );
    act(() => {
      store.setPreference('provider', 'a');
      store.setPreference('model', 'shared');
    });
    expect(
      screen.getByRole('button', { name: /Runtime settings: Loading runtime/ }),
    ).toHaveAttribute('aria-busy', 'true');
    await act(async () => {
      pending.resolve({ models: [{ id: 'shared', reasoning_efforts: ['high'] }] });
      await store.runtime.whenModelsReady();
    });
    expect(store.runtime.modelFor('a', 'shared')?.efforts).toEqual(['high']);
    const refresh = deferred<Record<string, unknown>>();
    store.endpoints.models = vi.fn(() => refresh.promise);
    act(() => {
      void store.loadModels('a');
    });
    expect(screen.queryByRole('button', { name: /Loading runtime/ })).toBeNull();
    expect(store.runtime.modelFor('b', 'shared')).toBeUndefined();
    await act(async () => {
      refresh.resolve({ models: [{ id: 'shared', reasoning_efforts: ['high'] }] });
      await store.runtime.whenModelsReady();
    });
  });

  it('reveals plan and path controls together without waiting for skills', async () => {
    const store = createStore();
    const tree = deferred<Record<string, unknown>>();
    const skills = deferred<Record<string, unknown>>();
    store.endpoints.tree = vi.fn(() => tree.promise);
    store.endpoints.skills = vi.fn(() => skills.promise);
    store.endpoints.sessionState = vi.fn(async () => ({
      current_plan: { plan: [{ step: 'Test it', status: 'in_progress' }] },
    }));
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );
    let selection!: Promise<void>;
    await act(async () => {
      selection = store.selectSession(store.sessions.peek()[0]);
      await Promise.resolve();
    });
    await waitFor(() => expect(store.currentPlan.peek()).not.toBeNull());
    expect(screen.getByLabelText('Loading session controls')).toBeInTheDocument();
    expect(document.querySelector('#planToggleBtn')).toBeNull();
    await act(async () => {
      tree.resolve({ path_count: 3 });
    });
    await waitFor(() => expect(screen.queryByLabelText('Loading session controls')).toBeNull());
    expect(document.querySelector('#planToggleBtn')).toBeInTheDocument();
    expect(document.querySelector('#branchTreeBtn')).toHaveTextContent('3 paths');
    await act(async () => {
      skills.resolve({ skills: [] });
      await selection;
    });
  });

  it('does not let late selection readiness affect a new draft', async () => {
    const store = createStore();
    const tree = deferred<Record<string, unknown>>();
    store.endpoints.tree = vi.fn(() => tree.promise);
    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );
    await act(async () => {
      await store.selectSession(store.sessions.peek()[0]);
    });
    expect(store.selectionStore.headerLoading.value).toBe(true);
    act(() => store.newChat());
    expect(store.selectionStore.headerLoading.value).toBe(false);
    await act(async () => {
      tree.resolve({ path_count: 9 });
    });
    expect(document.querySelector('#branchTreeBtn')).toBeNull();
    expect(screen.queryByLabelText('Loading session controls')).toBeNull();
  });

  it('publishes completed code and math together from detached content', async () => {
    const gate = deferred<void>();
    const decorate = markdown.decorateRichContent;
    vi.spyOn(markdown, 'decorateRichContent').mockImplementation(async (root, source, current) => {
      expect(root.isConnected).toBe(false);
      await gate.promise;
      await decorate(root, source, current);
    });
    const { container } = render(<Markdown value={'```js\nconst answer = 42;\n```\n\\(x^2\\)'} />);
    expect(container.querySelector('code')).toBeNull();
    expect(container.querySelector('[aria-busy="true"]')).toBeInTheDocument();
    await act(async () => {
      gate.resolve();
    });
    await waitFor(() => expect(container.querySelector('.katex')).toBeInTheDocument());
    expect(container.querySelector('.hljs-keyword')).toHaveTextContent('const');
    expect(container.querySelector('[aria-busy="true"]')).toBeNull();
  });

  it('rejects late rich-content publication after replacement and unmount', async () => {
    const gate = deferred<void>();
    const decorate = markdown.decorateRichContent;
    vi.spyOn(markdown, 'decorateRichContent').mockImplementation(async (root, source, current) => {
      await gate.promise;
      await decorate(root, source, current);
    });
    const view = render(<Markdown value={'```js\nconst old = 1;\n```'} />);
    view.rerender(<Markdown value="Current plain text" />);
    await act(async () => {
      gate.resolve();
    });
    expect(view.container).toHaveTextContent('Current plain text');
    expect(view.container.querySelector('code')).toBeNull();
    view.unmount();
  });

  it('publishes plain content after a stalled enhancement and ignores its late result', async () => {
    vi.useFakeTimers();
    const gate = deferred<void>();
    vi.spyOn(markdown, 'decorateRichContent').mockImplementation(async (root, _source, current) => {
      await gate.promise;
      if (current?.()) root.textContent = 'Late decoration';
    });
    const view = render(<Markdown value={'```js\nconst answer = 42;\n```'} />);
    expect(view.container.querySelector('[aria-busy="true"]')).not.toBeNull();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(250);
    });
    expect(view.container.querySelector('code')).toHaveTextContent('const answer = 42;');
    expect(view.container.querySelector('[aria-busy="true"]')).toBeNull();
    await act(async () => {
      gate.resolve();
    });
    expect(view.container).not.toHaveTextContent('Late decoration');
  });

  it('preserves fallback whitespace until completed rich content is published', async () => {
    vi.useFakeTimers();
    vi.spyOn(markdown, 'decorateRichContent').mockImplementation(() => new Promise(() => {}));
    const source = 'x'.repeat(70_000) + '\n\n```js\nconst answer = 42;\n```';
    const view = render(<Markdown value={source} streaming />);
    const root = view.container.querySelector<HTMLElement>('[data-streaming-fallback="plain"]')!;
    expect(root.style.whiteSpace).toBe('pre-wrap');
    view.rerender(<Markdown value={source} />);
    expect(root.style.whiteSpace).toBe('pre-wrap');
    await act(async () => {
      await vi.advanceTimersByTimeAsync(250);
    });
    expect(root.style.whiteSpace).toBe('');
    expect(root.querySelector('code')).toHaveTextContent('const answer = 42;');
  });

  it('keeps committed streaming decoration live across subsequent tokens', async () => {
    const gate = deferred<void>();
    vi.spyOn(markdown, 'decorateRichContent').mockImplementation(async (root, _source, current) => {
      await gate.promise;
      if (current?.()) root.setAttribute('data-decorated', 'yes');
    });
    const source = '```js\nconst answer = 42;\n```\n\n';
    const view = render(<Markdown value={source} streaming />);
    await act(async () => {});
    view.rerender(<Markdown value={source + 'More text'} streaming />);
    await act(async () => {
      gate.resolve();
    });
    await waitFor(() =>
      expect(view.container.querySelector('[data-decorated="yes"]')).not.toBeNull(),
    );
  });

  it('reveals known header controls when a branch request stalls', async () => {
    vi.useFakeTimers();
    const store = createStore();
    store.endpoints.tree = vi.fn(() => new Promise<Record<string, unknown>>(() => {}));
    await act(async () => {
      await store.selectSession(store.sessions.peek()[0]);
    });
    expect(store.selectionStore.headerLoading.peek()).toBe(true);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(store.selectionStore.headerLoading.peek()).toBe(false);
  });

  it('does not wait for an unrelated provider catalog when selected metadata is cached', async () => {
    const store = createStore();
    store.runtime.modelCatalogs.value = { '': [] };
    store.endpoints.models = vi.fn(() => new Promise<Record<string, unknown>>(() => {}));
    void store.loadModels('other');
    await store.runtime.whenModelsReady('');
    expect(store.endpoints.models).toHaveBeenCalledTimes(1);
  });

  it('mounts tool children on first expansion and only rerenders a changing tool', async () => {
    const store = createStore();
    store.sessions.value = [
      testSession({
        messages: [
          {
            id: 'group',
            role: 'tool-group',
            content: '',
            created: 1,
            tools: [
              { id: 'one', name: 'shell', status: 'done', result: 'old' },
              { id: 'two', name: 'read_file', status: 'running' },
            ],
          },
        ],
      }),
    ];
    const renders = observe();
    render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(renders.count('Tool')).toBe(0);
    fireEvent.click(screen.getByRole('button', { name: /2 tool calls/ }));
    expect(renders.count('Tool')).toBe(2);
    renders.clear();
    await act(() => {
      const session = store.sessions.peek()[0];
      const group = session.messages[0];
      store.sessionStore.patch('s1', {
        messages: [
          { ...group, tools: [group.tools![0], { ...group.tools![1], arguments: '{"path":"x"}' }] },
        ],
      });
    });
    expect(renders.count('Tool')).toBe(1);
  });

  it('keeps rendered Markdown blocks isolated while editing and submits current draft text', async () => {
    const store = createStore();
    const source = 'First paragraph\n\nSecond paragraph\n\nThird paragraph';
    const file: DiffFile = {
      path: 'notes.md',
      expanded: true,
      lines: [],
      markdownPreview: {
        view: 'rendered',
        side: 'after',
        source,
        blocks: markdownDocumentBlocks(source),
        sequence: 1,
        snapshotSeq: 1,
        scope: 'uncommitted',
      },
    };
    store.diff.value = {
      ...store.diff.peek(),
      open: true,
      sessionId: 's1',
      scope: 'uncommitted',
      files: [file],
    };
    store.sendDiffComment = vi.fn(async () => undefined);
    const renders = observe();
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    await waitFor(() => expect(document.querySelectorAll('.diff-markdown-block')).toHaveLength(3));
    expect(renders.count('PreviewBlock')).toBeGreaterThan(0);
    fireEvent.click(screen.getAllByRole('button', { name: /Comment on Paragraph/ })[0]);
    renders.clear();
    fireEvent.input(screen.getByRole('textbox', { name: 'Inline comment' }), {
      target: { value: 'Only this block' },
    });
    expect(renders.count('PreviewBlock')).toBe(1);
    fireEvent.click(screen.getByRole('button', { name: 'Send now' }));
    expect(store.sendDiffComment).toHaveBeenCalledWith(
      expect.objectContaining({ body: 'Only this block', line: 1 }),
    );
  });

  it('keeps side-question history out of draft and streaming renders', async () => {
    const store = createStore();
    store.sideQuestion.value = {
      ...store.sideQuestion.peek(),
      sessionId: 's1',
      history: [{ question: 'Earlier', response: '**Answer**' }],
    };
    const renders = observe();
    render(
      <StoreContext.Provider value={store}>
        <SideQuestion />
      </StoreContext.Provider>,
    );
    expect(renders.count('SideQuestionHistory')).toBe(1);
    renders.clear();
    await act(() => {
      store.setSideQuestionDraft('Next');
    });
    expect(renders.count('SideQuestionHistory')).toBe(0);
    await act(() => {
      store.sideQuestion.value = {
        ...store.sideQuestion.peek(),
        question: 'Next',
        response: 'Streaming',
        running: true,
      };
    });
    expect(renders.count('SideQuestionHistory')).toBe(0);
    expect(screen.getByText('Answer')).toBeInTheDocument();
  });

  it('keeps project groups independent of transcript-only session changes', async () => {
    const store = createStore();
    store.projectsEnabled.value = true;
    store.sidebarView.value = 'projects';
    store.sessionStore.patch('s1', { projectId: 'p1' });
    store.projects.value = [
      { id: 'p1', name: 'Project', path: '/project', sessions: [store.sessions.peek()[0]] },
    ];
    const renders = observe();
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(renders.count('ProjectGroup')).toBeGreaterThan(0);
    renders.clear();
    await act(() =>
      store.sessionStore.patch('s1', {
        transcriptRev: 2,
        messages: [{ id: 'tool', role: 'tool-group', content: '', created: 1 }],
      }),
    );
    expect(renders.count('ProjectGroup')).toBe(0);
  });

  it('preserves the selected terminal session in a collapsed project', () => {
    const store = createStore();
    store.projectsEnabled.value = true;
    store.sidebarView.value = 'projects';
    store.sessionStore.patch('s1', { projectId: 'p1', origin: 'tui', name: 'Selected terminal' });
    store.projects.value = [{ id: 'p1', name: 'Project', path: '/project', sessions: [] }];
    store.storage.setItem(store.keys.projectExpansion, JSON.stringify({ p1: false }));
    render(
      <StoreContext.Provider value={store}>
        <Sidebar />
      </StoreContext.Provider>,
    );
    expect(document.querySelector('[data-project-id="p1"] .session-row')).not.toBeNull();
  });

  it('does not republish diff state when a refreshed file is already followed', async () => {
    const store = createStore();
    const file: DiffFile = {
      path: 'main.go',
      expanded: true,
      lines: [{ kind: 'add', content: 'x', newLine: 1 }],
    };
    store.runEngine.currentActivityFile.value = file.path;
    const state = {
      ...store.diff.peek(),
      open: true,
      followCurrentFile: true,
      selectedPath: file.path,
      files: [file],
    };
    store.diff.value = state;
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    await act(async () => {});
    expect(store.diff.peek()).toBe(state);
    const next = { ...state, files: [{ ...file, additions: 1 }] };
    await act(() => {
      store.diff.value = next;
    });
    expect(store.diff.peek()).toBe(next);
  });

  it('renders changed diff code from current content without a stale highlight frame', async () => {
    const store = createStore();
    const file: DiffFile = {
      path: 'main.js',
      expanded: true,
      lang: 'js',
      lines: [{ kind: 'add', content: 'const oldName = 1;', newLine: 1 }],
    };
    store.diff.value = { ...store.diff.peek(), open: true, files: [file] };
    render(
      <StoreContext.Provider value={store}>
        <DiffSidebar />
      </StoreContext.Provider>,
    );
    await waitFor(() =>
      expect(document.querySelector('.diff-code .hljs-keyword')).toHaveTextContent('const'),
    );
    act(() => {
      store.diff.value = {
        ...store.diff.peek(),
        files: [{ ...file, lines: [{ kind: 'add', content: 'let newName = 2;', newLine: 1 }] }],
      };
    });
    expect(document.querySelector('.diff-code')).toHaveTextContent('let newName = 2;');
    expect(document.querySelector('.diff-code')).not.toHaveTextContent('oldName');
  });
});
