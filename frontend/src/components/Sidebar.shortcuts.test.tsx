import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Sidebar } from './Sidebar';
import { Overlay } from './Overlay';

let store: AppStore;
afterEach(() => store?.dispose());
function setup() {
  vi.spyOn(navigator, 'platform', 'get').mockReturnValue('MacIntel');
  vi.mocked(window.matchMedia).mockImplementation((query) => ({
    matches: query === '(display-mode: standalone)',
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }));
  store = new AppStore(testConfig);
  store.sessions.value = [
    testSession({ id: 'older', title: 'Older chat', lastMessageAt: 1 }),
    testSession({ id: 'newer', title: 'Newer chat', lastMessageAt: 2 }),
    testSession({ id: 'pinned', title: 'Pinned chat', pinned: true }),
  ];
  store.selectSession = vi.fn(async () => undefined);
  return store;
}
function show() {
  return render(
    <StoreContext.Provider value={store}>
      <Sidebar />
      <input aria-label="Composer" />
    </StoreContext.Provider>,
  );
}
const shortcut = (key: string, options = {}) =>
  fireEvent.keyDown(document.activeElement || document.body, { key, metaKey: true, ...options });

describe('sidebar chat shortcuts', () => {
  it('leaves Cmd-number and hints alone in a normal browser tab', () => {
    setup();
    vi.mocked(window.matchMedia).mockReturnValue({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    } as unknown as MediaQueryList);
    show();
    shortcut('Meta');
    expect(shortcut('1')).toBe(true);
    expect(store.selectSession).not.toHaveBeenCalled();
    expect(screen.getByLabelText('Sessions')).not.toHaveAttribute('data-show-chat-shortcuts');
    expect(screen.getByRole('button', { name: 'Pinned chat' })).not.toHaveAttribute(
      'aria-keyshortcuts',
    );
  });

  it('removes bindings and hints when an app window returns to browser mode', () => {
    setup();
    const media = new EventTarget() as MediaQueryList;
    Object.defineProperty(media, 'matches', { value: true, configurable: true });
    vi.spyOn(window, 'matchMedia').mockImplementation((query) =>
      query === '(display-mode: standalone)'
        ? media
        : ({
            matches: false,
            addEventListener: vi.fn(),
            removeEventListener: vi.fn(),
          } as unknown as MediaQueryList),
    );
    show();
    shortcut('Meta');
    expect(screen.getByLabelText('Sessions')).toHaveAttribute('data-show-chat-shortcuts');
    act(() => {
      Object.defineProperty(media, 'matches', { value: false });
      media.dispatchEvent(new Event('change'));
    });
    expect(screen.getByLabelText('Sessions')).not.toHaveAttribute('data-show-chat-shortcuts');
    expect(screen.getByRole('button', { name: 'Pinned chat' })).not.toHaveAttribute(
      'aria-keyshortcuts',
    );
    expect(shortcut('1')).toBe(true);
    expect(store.selectSession).not.toHaveBeenCalled();
  });

  it('switches immediately in rendered order from an input, without showing hints first', () => {
    setup();
    show();
    screen.getByRole('textbox', { name: 'Composer' }).focus();
    expect(shortcut('1')).toBe(false);
    expect(store.selectSession).toHaveBeenLastCalledWith(expect.objectContaining({ id: 'pinned' }));
    shortcut('2');
    expect(store.selectSession).toHaveBeenLastCalledWith(expect.objectContaining({ id: 'older' }));
    shortcut('3');
    expect(store.selectSession).toHaveBeenLastCalledWith(expect.objectContaining({ id: 'newer' }));
  });

  it('shows matching hints while Cmd is down and clears them on release, blur and visibility change', () => {
    setup();
    show();
    const sidebar = screen.getByLabelText('Sessions');
    expect(sidebar).not.toHaveAttribute('data-show-chat-shortcuts');
    shortcut('Meta');
    expect(sidebar).toHaveAttribute('data-show-chat-shortcuts');
    expect(screen.getByRole('button', { name: 'Pinned chat' })).toHaveAttribute(
      'aria-keyshortcuts',
      'Meta+1',
    );
    expect(screen.getByRole('button', { name: 'Older chat' })).toHaveAttribute(
      'data-chat-shortcut-number',
      '2',
    );
    fireEvent.keyUp(window, { key: 'Meta', metaKey: false });
    expect(sidebar).not.toHaveAttribute('data-show-chat-shortcuts');
    shortcut('Meta');
    fireEvent.blur(window);
    expect(sidebar).not.toHaveAttribute('data-show-chat-shortcuts');
    shortcut('Meta');
    fireEvent(document, new Event('visibilitychange'));
    expect(sidebar).not.toHaveAttribute('data-show-chat-shortcuts');
  });

  it('renumbers expanded groups and skips the active chat retained inside a folded group', async () => {
    setup();
    store.projectsEnabled.value = true;
    store.setSidebarView('projects');
    const projectChat = testSession({ id: 'project', title: 'Project chat', projectId: 'p1' });
    store.sessions.value = [projectChat, testSession({ id: 'loose', title: 'Loose chat' })];
    store.projects.value = [{ id: 'p1', name: 'Alpha', sessions: [projectChat] }];
    store.activeSessionId.value = 'project';
    show();
    shortcut('1');
    expect(store.selectSession).toHaveBeenLastCalledWith(
      expect.objectContaining({ id: 'project' }),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Alpha' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Loose chat' })).toHaveAttribute(
        'aria-keyshortcuts',
        'Meta+1',
      ),
    );
    expect(screen.getByRole('button', { name: 'Project chat' })).not.toHaveAttribute(
      'aria-keyshortcuts',
    );
    shortcut('1');
    expect(store.selectSession).toHaveBeenLastCalledWith(expect.objectContaining({ id: 'loose' }));
    fireEvent.click(screen.getByRole('button', { name: 'Alpha' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Project chat' })).toHaveAttribute(
        'aria-keyshortcuts',
        'Meta+1',
      ),
    );
  });

  it('follows search results and skips archived chats', async () => {
    setup();
    show();
    act(() => {
      store.searchResults.value = [
        testSession({ id: 'hidden', title: 'Hidden chat', archived: true }),
        testSession({ id: 'result', title: 'Result' }),
      ];
    });
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Result' })).toHaveAttribute(
        'aria-keyshortcuts',
        'Meta+1',
      ),
    );
    shortcut('1');
    expect(store.selectSession).toHaveBeenLastCalledWith(expect.objectContaining({ id: 'result' }));
  });

  it('ignores extra modifiers, composition, repeats, unmapped digits, dialogs, terminal input and unmounted sidebars', () => {
    setup();
    const { rerender, unmount } = show();
    for (const options of [
      { ctrlKey: true },
      { altKey: true },
      { shiftKey: true },
      { isComposing: true },
      { repeat: true },
    ])
      shortcut('1', options);
    shortcut('9');
    shortcut('0');
    const shell = document.createElement('div');
    shell.className = 'shell-overlay';
    document.body.append(shell);
    fireEvent.keyDown(shell, { key: '1', metaKey: true });
    shell.remove();
    rerender(
      <StoreContext.Provider value={store}>
        <Sidebar />
        <Overlay title="Settings">Settings content</Overlay>
      </StoreContext.Provider>,
    );
    shortcut('1');
    expect(store.selectSession).not.toHaveBeenCalled();
    unmount();
    shortcut('1');
    expect(store.selectSession).not.toHaveBeenCalled();
  });

  it('numbers only the first nine chats and leaves other platforms alone', () => {
    setup();
    store.sessions.value = Array.from({ length: 10 }, (_, i) =>
      testSession({ id: String(i), title: `Chat ${i}`, lastMessageAt: 10 - i }),
    );
    const { unmount } = show();
    expect(screen.getByRole('button', { name: 'Chat 8' })).toHaveAttribute(
      'aria-keyshortcuts',
      'Meta+9',
    );
    expect(screen.getByRole('button', { name: 'Chat 9' })).not.toHaveAttribute('aria-keyshortcuts');
    shortcut('9');
    expect(store.selectSession).toHaveBeenLastCalledWith(expect.objectContaining({ id: '8' }));
    unmount();
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('Linux x86_64');
    vi.mocked(store.selectSession).mockClear();
    show();
    shortcut('1');
    expect(store.selectSession).not.toHaveBeenCalled();
  });
});
