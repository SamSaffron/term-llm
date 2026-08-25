import { act, fireEvent, render, screen } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { Transcript } from './Transcript';
import { Composer } from './Composer';
import { Markdown } from './Markdown';
import { Modals } from './Modals';
import type { AppConfig } from '../app/config';

const config: AppConfig = { prefix: '/ui', version: 'v1', sidebarCategories: ['all'], agentName: '', agentNames: ['jarvis'], title: '', locationSharing: true, worktrees: true, hub: null, vapidKey: '', webRTC: false, signalingURL: '' };
const createStore = () => {
  const store = new AppStore(config);
  store.sessions.value = [{ id: 's1', title: 'Test', name: '', mode: 'chat', origin: 'web', archived: false, pinned: false, created: 1, lastMessageAt: 1, messages: [
    { id: 'u1', role: 'user', content: 'Question', created: 1 },
    { id: 'a1', role: 'assistant', content: '**Answer**', created: 2 },
    { id: 't1', role: 'tool-group', content: '', created: 3, tools: [{ id: 'call', name: 'read_file', arguments: '{"path":"x"}', status: 'done', result: 'ok' }] },
  ] }];
  store.activeSessionId.value = 's1'; store.draftActive.value = false;
  return store;
};

describe('Preact-owned chat surfaces', () => {
  it('renders keyed messages, sanitized markdown and expandable tool details', async () => {
    const store = createStore(); render(<StoreContext.Provider value={store}><Transcript /></StoreContext.Provider>);
    expect(screen.getByText('Question')).toBeInTheDocument(); expect(screen.getByText('Answer').tagName).toBe('STRONG');
    await userEvent.click(screen.getByRole('button', { name: /read_file/ }));
    expect(screen.getByText(/"path": "x"/)).toBeInTheDocument(); expect(screen.getByText('ok')).toBeInTheDocument();
  });

  it('drives send from public composer UI and opens attachment picker behavior', async () => {
    const store = createStore(); store.send = vi.fn(async () => undefined);
    render(<StoreContext.Provider value={store}><Composer /></StoreContext.Provider>);
    const input = screen.getByRole('textbox', { name: 'Message' }); await userEvent.type(input, 'hello'); await userEvent.keyboard('{Enter}');
    expect(store.send).toHaveBeenCalledOnce();
  });

  it('offers slash and mention completions through an accessible listbox', async () => {
    const store = createStore(); render(<StoreContext.Provider value={store}><Composer /></StoreContext.Provider>);
    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), '/co');
    expect(screen.getByRole('option', { name: /compact/ })).toBeInTheDocument();
  });

  it('dispatches restored branch and live skill commands instead of sending them as chat', async () => {
    const branchStore = createStore(); branchStore.branchCommand = vi.fn(async () => undefined); branchStore.send = vi.fn(async () => undefined);
    const first = render(<StoreContext.Provider value={branchStore}><Composer /></StoreContext.Provider>); const branchInput = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(branchInput, '/fork continue here'); await userEvent.keyboard('{Enter}');
    expect(branchStore.branchCommand).toHaveBeenCalledWith('fork', 'continue here'); expect(branchStore.send).not.toHaveBeenCalled(); first.unmount();

    const skillStore = createStore(); skillStore.skills.value = [{ name: 'review', description: 'Review', execution: 'isolated', source: 'local' }]; skillStore.invokeSkill = vi.fn(async () => undefined);
    render(<StoreContext.Provider value={skillStore}><Composer /></StoreContext.Provider>); const skillInput = screen.getByRole('textbox', { name: 'Message' });
    await userEvent.type(skillInput, '/review src'); await userEvent.keyboard('{Enter}'); expect(skillStore.invokeSkill).toHaveBeenCalledWith('review', 'src');
  });

  it('debounces project mentions with session/worktree context and keeps agent matches', async () => {
    vi.useFakeTimers();
    try {
      const store = createStore(); store.sessions.value = [{ ...store.sessions.value[0], projectId: 'project-1', worktreeDir: '/tmp/tree' }]; store.projectsEnabled.value = true;
      const search = vi.fn(async (_body: unknown, _sessionId?: string, _signal?: AbortSignal) => ({ active: true, token: { start_utf16: 0, end_utf16: 3, query: 'ja' }, items: [{ path: 'jar.go', kind: 'file' as const, insert_text: '@jar.go', segments: [{ text: 'jar', matched: true }, { text: '.go' }] }] }));
      store.endpoints.mentionSearch = search;
      render(<StoreContext.Provider value={store}><Composer /></StoreContext.Provider>);
      const input = screen.getByRole('textbox', { name: 'Message' }) as HTMLTextAreaElement;
      fireEvent.input(input, { target: { value: '@ja', selectionStart: 3, selectionEnd: 3 } });
      expect(search).not.toHaveBeenCalled();
      await act(async () => { await vi.advanceTimersByTimeAsync(50); });
      expect(search).toHaveBeenCalledWith(expect.objectContaining({ text: '@ja', cursor_utf16: 3, limit: 10, project_id: 'project-1', worktree_dir: '/tmp/tree' }), 's1', expect.any(AbortSignal));
      await act(async () => Promise.resolve());
      expect(screen.getByRole('option', { name: /@jarvisAgent/ })).toBeInTheDocument();
      expect(screen.getByRole('option', { name: /jar\.gofile/ })).toBeInTheDocument();
      const firstSignal = search.mock.calls[0][2] as AbortSignal;
      fireEvent.input(input, { target: { value: '@jab', selectionStart: 4, selectionEnd: 4 } });
      expect(firstSignal.aborted).toBe(true);
    } finally { vi.useRealTimers(); }
  });

  it('dismisses completions with Escape without mutating the prompt', async () => {
    const store = createStore(); render(<StoreContext.Provider value={store}><Composer /></StoreContext.Provider>);
    const input = screen.getByRole('textbox', { name: 'Message' }); await userEvent.type(input, '@ja');
    expect(screen.getByRole('listbox')).toBeInTheDocument(); await userEvent.keyboard('{Escape}');
    expect(input).toHaveValue('@ja'); expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  });

  it('prioritizes approval and ask-user prompts without losing the underlying modal', () => {
    const store = createStore(); store.modal.value = 'settings';
    store.askUser.value = { sessionId: 's1', callId: 'ask-1', questions: [{ question: 'Question?', options: [] }] };
    store.approval.value = { sessionId: 's1', id: 'approval-1', title: 'Approval first', options: [{ index: 0, choice: 'allow', label: 'Allow' }, { index: 1, choice: 'deny', label: 'Deny' }] };
    render(<StoreContext.Provider value={store}><Modals /></StoreContext.Provider>);
    expect(screen.getByRole('heading', { name: 'Approval first' })).toBeInTheDocument();
    act(() => { store.approval.value = null; }); expect(screen.getByRole('heading', { name: 'Answer question' })).toBeInTheDocument();
    act(() => { store.askUser.value = null; }); expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument();
  });

  it('debounces and aborts stale project-directory lookups', async () => {
    vi.useFakeTimers();
    try {
      const store = createStore(); store.modal.value = 'project';
      const signals: AbortSignal[] = []; const lookup = vi.fn((_query: string, signal?: AbortSignal) => { if (signal) signals.push(signal); return new Promise<Record<string, unknown>>(() => undefined); });
      store.endpoints.projectDirectories = lookup;
      render(<StoreContext.Provider value={store}><Modals /></StoreContext.Provider>);
      const input = screen.getByRole('textbox', { name: 'Project path' });
      fireEvent.input(input, { target: { value: '/one' } }); await act(async () => { await vi.advanceTimersByTimeAsync(150); });
      expect(lookup).toHaveBeenCalledWith('/one', expect.any(AbortSignal));
      fireEvent.input(input, { target: { value: '/two' } });
      expect(signals[0].aborted).toBe(true); await act(async () => { await vi.advanceTimersByTimeAsync(150); });
      expect(lookup).toHaveBeenLastCalledWith('/two', expect.any(AbortSignal));
    } finally { vi.useRealTimers(); }
  });

  it('throttles streaming markdown updates at the adaptive cadence', async () => {
    vi.useFakeTimers();
    try {
      const { container, rerender } = render(<Markdown value="first" streaming />); rerender(<Markdown value="second" streaming />);
      expect(container).toHaveTextContent('first'); await act(async () => { await vi.advanceTimersByTimeAsync(33); }); expect(container).toHaveTextContent('second');
    } finally { vi.useRealTimers(); }
  });

  it('uses a bounded plain-text fallback for over-budget streaming markdown', () => {
    const value = 'x'.repeat(70_000); const { container } = render(<Markdown value={value} streaming />);
    expect(container.querySelector('[data-streaming-fallback="plain"]')).toHaveTextContent(value);
  });
});
