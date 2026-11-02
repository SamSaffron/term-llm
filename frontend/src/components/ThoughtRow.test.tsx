import { initialProjection } from '../domain/response';
import { act, fireEvent, render, screen, within } from '@testing-library/preact';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { DEFAULT_THINKING } from '../domain/thinking';
import type { Message } from '../domain/types';
import { ThoughtRow } from './ThoughtRow';
import { Composer } from './Composer';
import { Transcript } from './Transcript';
import { overlayManager } from '../platform/overlay-manager';
import { requestTranscriptScrollToDurable } from './transcript-scroll';
import { handleThinkingShortcut } from '../hooks/use-thinking-shortcut';

const thought: Message = {
  id: 'thought',
  role: 'reasoning',
  content: 'A **safe** body',
  reasoningKind: 'summary',
  reasoningTitle: 'Checking the resume path',
  created: 1,
};
const createStore = () => {
  const store = new AppStore(testConfig);
  // Most cases exercise the opt-in visible mode; the store tests cover off by default.
  store.thinking.applyCapabilities({ ...DEFAULT_THINKING, display: 'collapsed' });
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.startupDone.value = true;
  return store;
};
beforeEach(() => {
  localStorage.clear();
  overlayManager.reset();
});

describe('ThoughtRow', () => {
  it('renders a muted accessible header, toggles only its block and sanitises expanded markdown', async () => {
    const store = createStore();
    const view = render(
      <StoreContext.Provider value={store}>
        <ThoughtRow message={thought} live />
        <ThoughtRow message={{ ...thought, id: 'second' }} />
      </StoreContext.Provider>,
    );
    const buttons = screen.getAllByRole('button', { name: 'Thought: Checking the resume path' });
    expect(buttons[0]).toHaveAttribute('aria-expanded', 'false');
    // aria-controls only names the body while it is mounted.
    expect(buttons[0]).not.toHaveAttribute('aria-controls');
    expect(view.container.querySelector('.thought.live')).toBeTruthy();
    expect(view.container.querySelector('[aria-live]')).toBeNull();
    fireEvent.click(buttons[0]);
    expect(buttons[0]).toHaveAttribute('aria-expanded', 'true');
    expect(document.getElementById(buttons[0].getAttribute('aria-controls') || '')).toBeTruthy();
    expect(buttons[1]).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByText('safe')).toBeTruthy();
    fireEvent.click(buttons[0]);
    expect(buttons[0]).toHaveAttribute('aria-expanded', 'false');
    await act(() => {
      store.thinking.command('expanded');
    });
    expect(buttons[0]).toHaveAttribute('aria-expanded', 'true');
    expect(buttons[1]).toHaveAttribute('aria-expanded', 'true');
    view.unmount();
    store.dispose();
  });
  it.each(['off', 'status'])('hides mode %s', (mode) => {
    const store = createStore();
    store.thinking.command(mode);
    const view = render(
      <StoreContext.Provider value={store}>
        <ThoughtRow message={thought} />
      </StoreContext.Provider>,
    );
    expect(view.container).toBeEmptyDOMElement();
    view.unmount();
    store.dispose();
  });
  it.each(['none', 'transcript_only'])('hides history %s', (history) => {
    const store = createStore();
    store.thinking.applyCapabilities({ ...DEFAULT_THINKING, history });
    const view = render(
      <StoreContext.Provider value={store}>
        <ThoughtRow message={thought} />
      </StoreContext.Provider>,
    );
    expect(view.container).toBeEmptyDOMElement();
    view.unmount();
    store.dispose();
  });
  it('uses the hidden label for untitled thoughts and Raw thinking only in raw mode', async () => {
    const store = createStore();
    const view = render(
      <StoreContext.Provider value={store}>
        <ThoughtRow message={{ ...thought, reasoningKind: 'raw', reasoningTitle: '' }} />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('button', { name: 'Thinking...' })).toBeTruthy();
    await act(() => {
      store.thinking.applyCapabilities({ ...DEFAULT_THINKING, raw: true });
      store.thinking.command('raw');
    });
    expect(screen.getByRole('button', { name: 'Raw thinking' })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    expect(view.container.querySelector('.thought-raw')).toBeTruthy();
    view.unmount();
    store.dispose();
  });
});

describe('live thought status', () => {
  it('groups tools across hidden thoughts and restores boundaries when thoughts become visible', async () => {
    const store = createStore();
    const messages: Message[] = [
      { id: 'user', role: 'user', content: 'Question', created: 1 },
      {
        id: 'first',
        durableRowId: 10,
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r',
        tools: [{ id: 'read', name: 'read_file', status: 'done' }],
      },
      thought,
      {
        id: 'second',
        durableRowId: 20,
        role: 'tool-group',
        content: '',
        created: 1,
        responseId: 'r',
        tools: [{ id: 'search', name: 'grep', status: 'done' }],
      },
    ];
    store.sessionStore.update('s1', (session) => ({ ...session, messages }));
    store.thinking.command('off');
    const view = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    for (const mode of ['off', 'collapsed', 'status', 'expanded', 'off']) {
      await act(() => {
        store.thinking.command(mode);
      });
      const visible = mode === 'collapsed' || mode === 'expanded';
      expect(view.container.querySelectorAll('.tool-group')).toHaveLength(visible ? 2 : 1);
      expect(view.container.querySelectorAll('.thought')).toHaveLength(visible ? 1 : 0);
      if (!visible)
        expect(view.container.querySelector('.tool-group-summary')).toHaveTextContent(
          '2 tool calls',
        );
    }
    expect(store.activeSession.value?.messages).toBe(messages);
    const group = view.container.querySelector<HTMLElement>('.tool-group')!;
    expect(group).toHaveAttribute('data-durable-ids', '10 20');
    const bounds = vi
      .spyOn(group, 'getBoundingClientRect')
      .mockReturnValue(new DOMRect(0, 100, 100, 100));
    const scroll = view.container.querySelector<HTMLElement>('#chatScroll')!;
    scroll.scrollTop = 0;
    requestTranscriptScrollToDurable(20);
    expect(scroll.scrollTop).toBe(84);
    bounds.mockRestore();
    view.unmount();
    store.dispose();
  });
  it('shows one thinking label: the row when rows are shown, the status line otherwise', async () => {
    const store = createStore();
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'r',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 0,
          startedRev: 0,
          reconnects: 0,
        }),
        messages: [{ ...thought, reasoningKind: 'raw', reasoningTitle: '', responseId: 'r' }],
      },
    };
    store.runEngine.markResponseTransportActive('s1', 'r');
    const view = render(
      <StoreContext.Provider value={store}>
        <Transcript />
      </StoreContext.Provider>,
    );
    expect(screen.getByRole('button', { name: 'Thinking...' })).toBeTruthy();
    expect(view.container.querySelector('.streaming-indicator')).toBeNull();
    for (const mode of ['status', 'off']) {
      await act(() => {
        store.thinking.command(mode);
      });
      expect(screen.queryByRole('button', { name: 'Thinking...' })).toBeNull();
      expect(view.container.querySelector('.streaming-indicator')).toHaveTextContent('Thinking');
    }
    view.unmount();
    store.dispose();
  });
});

describe('thinking composer command', () => {
  it.each([
    '/thinking',
    '/thinking expanded',
    '/reasoning status',
    '/THINKING AUTO',
    '/thinking bad',
    '/thinking raw',
  ])('runs %s with Enter, clearing the prompt only on success', async (command) => {
    const store = createStore();
    store.runs.value = {
      s1: initialProjection({
        responseId: 'r',
        sessionId: 's1',
        epoch: 1,
        status: 'streaming',
        lastSequence: 0,
        startedRev: 0,
        reconnects: 0,
      }),
    };
    const toast = vi.spyOn(store, 'toast');
    const send = vi.spyOn(store, 'send');
    const view = render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    const input = screen.getByRole('textbox', { name: 'Message' });
    fireEvent.input(input, { target: { value: command, selectionStart: command.length } });
    if (command === '/thinking') {
      const menu = screen.getByRole('listbox', { name: 'Composer completions' });
      expect(within(menu).getByRole('option', { name: /\/thinking expanded/ })).toHaveAttribute(
        'aria-selected',
        'true',
      );
      expect(within(menu).getByText('current')).toBeTruthy();
      expect(within(menu).getByText('disabled in config')).toBeTruthy();
      expect(within(menu).getByText(/alone cycles/)).toBeTruthy();
    }
    fireEvent.keyDown(input, { key: 'Enter' });
    const rejected = command.endsWith('bad') || command.endsWith('raw');
    // As in the TUI, a rejected command stays in the composer to be corrected.
    expect(store.prompt.value).toBe(rejected ? command : '');
    expect(send).not.toHaveBeenCalled();
    expect(toast).toHaveBeenCalledOnce();
    expect(toast.mock.calls[0][1]).toBe(rejected ? 'error' : 'success');
    view.unmount();
    store.dispose();
  });
  it('picks the highlighted mode on Enter once the highlight has moved', () => {
    const store = createStore();
    const toast = vi.spyOn(store, 'toast');
    const view = render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    const input = screen.getByRole('textbox', { name: 'Message' });
    fireEvent.input(input, { target: { value: '/thinking', selectionStart: 9 } });
    const menu = screen.getByRole('listbox', { name: 'Composer completions' });
    // The highlight starts on the cycle target; ArrowDown moves it from expanded to raw.
    fireEvent.keyDown(input, { key: 'ArrowDown' });
    expect(within(menu).getByRole('option', { name: /\/thinking raw/ })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(store.prompt.value).toBe('/thinking raw ');
    expect(toast).not.toHaveBeenCalled();
    expect(store.thinking.display.value).toBe('collapsed');
    view.unmount();
    store.dispose();
  });
});

describe('Ctrl+E', () => {
  it('handles empty composer and non-editable focus, preserves all normal editing and overlay cases', () => {
    const thinking = createStore().thinking;
    const input = document.createElement('textarea');
    input.id = 'promptInput';
    document.body.append(input);
    input.focus();
    const key = (extra: KeyboardEventInit = {}) =>
      new KeyboardEvent('keydown', { key: 'e', ctrlKey: true, cancelable: true, ...extra });
    let event = key();
    expect(handleThinkingShortcut(event, thinking)).toBe(true);
    expect(event.defaultPrevented).toBe(true);
    input.value = 'not empty';
    expect(handleThinkingShortcut(key(), thinking)).toBe(false);
    input.value = '';
    for (const extra of [
      { metaKey: true },
      { altKey: true },
      { shiftKey: true },
      { isComposing: true },
      { ctrlKey: false },
    ])
      expect(handleThinkingShortcut(key(extra), thinking)).toBe(false);
    event = key();
    event.preventDefault();
    expect(handleThinkingShortcut(event, thinking)).toBe(false);
    const token = overlayManager.acquire();
    expect(handleThinkingShortcut(key(), thinking)).toBe(false);
    overlayManager.release(token);
    const other = document.createElement('input');
    document.body.append(other);
    other.focus();
    expect(handleThinkingShortcut(key(), thinking)).toBe(false);
    other.remove();
    const shell = document.createElement('div');
    shell.className = 'shell-overlay';
    const shellButton = document.createElement('button');
    shell.append(shellButton);
    document.body.append(shell);
    shellButton.focus();
    expect(handleThinkingShortcut(key(), thinking)).toBe(false);
    shell.remove();
    input.remove();
    expect(handleThinkingShortcut(key(), thinking)).toBe(true);
  });
  it.each([1, 2])(
    'expands %i tools and thoughts together, clears overrides, and permits local clicks afterwards',
    async (toolCount) => {
      const store = createStore();
      store.sessionStore.update('s1', (session) => ({
        ...session,
        messages: [
          { id: 'user', role: 'user', content: 'Question', created: 1 },
          thought,
          {
            id: 'tools',
            role: 'tool-group',
            content: '',
            created: 1,
            tools: Array.from({ length: toolCount }, (_, index) => ({
              id: `read-${index}`,
              name: 'read_file',
              status: 'done' as const,
              arguments: '{"path":"README.md"}',
            })),
          },
        ],
      }));
      const view = render(
        <StoreContext.Provider value={store}>
          <Transcript />
          <Composer />
        </StoreContext.Provider>,
      );
      const thoughtButton = screen.getByRole('button', {
        name: 'Thought: Checking the resume path',
      });
      fireEvent.click(thoughtButton);
      expect(store.thinking.overrides.value.thought).toBe(true);
      const composer = screen.getByRole('textbox', { name: 'Message' });
      composer.focus();
      fireEvent.keyDown(composer, { key: 'e', ctrlKey: true });
      expect(thoughtButton).toHaveAttribute('aria-expanded', 'true');
      expect(store.thinking.overrides.value).toEqual({});
      expect(view.container.querySelector('.tool-group-toggle, .tool-toggle')).toHaveAttribute(
        'aria-expanded',
        'true',
      );
      fireEvent.keyDown(composer, { key: 'e', ctrlKey: true });
      expect(thoughtButton).toHaveAttribute('aria-expanded', 'false');
      expect(view.container.querySelector('.tool-group-toggle, .tool-toggle')).toHaveAttribute(
        'aria-expanded',
        'false',
      );
      fireEvent.click(thoughtButton);
      expect(thoughtButton).toHaveAttribute('aria-expanded', 'true');
      view.unmount();
      store.dispose();
    },
  );
  it('keeps the shortcut installed for a delegated read-only conversation', () => {
    const store = createStore();
    store.sessionStore.update('s1', (session) => ({ ...session, delegated: true }));
    const view = render(
      <StoreContext.Provider value={store}>
        <Composer />
      </StoreContext.Provider>,
    );
    expect(screen.queryByRole('textbox', { name: 'Message' })).toBeNull();
    fireEvent.keyDown(document.body, { key: 'e', ctrlKey: true });
    expect(store.thinking.detailsExpanded.value).toBe(true);
    view.unmount();
    store.dispose();
  });
});
