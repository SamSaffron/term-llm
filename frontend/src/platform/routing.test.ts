import { describe, expect, it, vi } from 'vitest';
import { updateSessionRoute } from './routing';
import type { Session } from '../domain/types';

const session: Session = {
  id: 's1',
  number: 7,
  title: 'Test',
  name: '',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: 1,
  lastMessageAt: 1,
  messages: [],
};

describe('session routing', () => {
  it('compares the pathname rather than treating an unrelated query as a new route', () => {
    history.replaceState(null, '', '/ui/chat/7?panel=diff');
    const push = vi.spyOn(history, 'pushState');
    updateSessionRoute('/ui', session);
    expect(push).not.toHaveBeenCalled();
  });
});
