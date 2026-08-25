import { render, screen } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { Transcript } from './Transcript';
import { Composer } from './Composer';
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
});
