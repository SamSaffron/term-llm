import { fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { Facepile } from './AuthorSidebar';
import { AuthorFilter, IdentityCard } from './AuthorViewer';
import { AuthorByline, PendingAuthor } from './AuthorByline';
import type { Author, Session } from '../domain/types';

const alice: Author = { id: 'alice', name: 'Alice Example', email: 'alice@example.com' };
const bob: Author = { id: 'bob', name: 'Bob Example' };
let store: AppStore;
beforeEach(() => {
  localStorage.clear();
  store = new AppStore(testConfig);
});
afterEach(() => store.dispose());
function show(children: preact.ComponentChildren) {
  return render(<StoreContext.Provider value={store}>{children}</StoreContext.Provider>);
}

describe('author presentation', () => {
  it('renders nothing without authors or a viewer', () => {
    const { container } = show(
      <>
        <AuthorFilter />
        <IdentityCard />
        <AuthorByline />
        <Facepile session={{ participants: [] } as unknown as Session} />
      </>,
    );
    expect(container).toBeEmptyDOMElement();
  });
  it('shows You, title metadata, Unknown, and pending author names', () => {
    store.viewer.value = alice;
    show(
      <>
        <AuthorByline author={alice} />
        <AuthorByline author={null} />
        <PendingAuthor author={bob} />
      </>,
    );
    expect(screen.getByText('You').parentElement).toHaveAttribute(
      'title',
      'Alice Example <alice@example.com>',
    );
    expect(screen.getByText('Unknown')).toBeInTheDocument();
    expect(screen.getByText('Bob:')).toBeInTheDocument();
  });
  it('bounds facepiles and announces participant names', () => {
    const participants = [
      alice,
      bob,
      { id: 'c', name: 'Carol Example' },
      { id: 'd', name: 'Diego Example' },
    ];
    const { container } = show(
      <Facepile
        session={{ participants, participantCount: 6 } as unknown as Session}
        viewer={alice}
      />,
    );
    expect(container.querySelectorAll('.author-avatar')).toHaveLength(3);
    expect(screen.getByText('+3')).toBeInTheDocument();
    expect(screen.getByRole('img')).toHaveAccessibleName(
      /Alice Example \(you\), Bob Example, Carol Example, Diego Example/,
    );
  });
  it('persists the filter, forwards it to sidebar requests, and supports arrow keys', async () => {
    store.viewer.value = alice;
    store.projectsEnabled.value = true;
    const sidebar = vi
      .spyOn(store.endpoints, 'sidebar')
      .mockResolvedValue({ groups: [], recent_sessions: [] });
    show(
      <>
        <AuthorFilter />
        <IdentityCard />
      </>,
    );
    const everyone = screen.getByRole('radio', { name: 'Everyone' });
    everyone.focus();
    fireEvent.click(screen.getByRole('radio', { name: 'Started by me' }));
    await waitFor(() => expect(sidebar).toHaveBeenCalledWith(false, 'me'));
    expect(screen.getByRole('radio', { name: 'Started by me' })).toBeChecked();
    expect(store.storage.getItem(store.keys.authorFilter)).toBe('me');
    expect(screen.getByText(/via proxy/)).toBeInTheDocument();
    store.linkedDevicesMode.value = true;
    await waitFor(() => expect(screen.getByText(/via linked device/)).toBeInTheDocument());
  });
  it('does not retain Everyone pages inside a filtered Recent or project page', async () => {
    store.viewer.value = alice;
    store.projectsEnabled.value = true;
    const row = (id: string, author: Author) => ({ id, created_by: author, project_id: 'p' });
    const group = (rows: unknown[], cursor: string) => ({
      project: { id: 'p', name: 'Project' },
      sessions: rows,
      next_cursor: cursor,
    });
    store.sessionStore.applySidebar({
      groups: [group([row('a', alice), row('b', bob)], 'old-project-cursor')],
      recent_sessions: [row('a', alice), row('b', bob)],
      recent_next_cursor: 'old-recent-cursor',
    });
    vi.spyOn(store.endpoints, 'sidebar').mockResolvedValue({
      groups: [group([row('a', alice)], 'new-project-cursor')],
      recent_sessions: [row('a', alice)],
      recent_next_cursor: 'new-recent-cursor',
    });
    await store.sessionStore.authors.setFilter('me');
    expect(store.recentSessions.value.map((row) => row.id)).toEqual(['a']);
    expect(store.projects.value[0].sessions?.map((row) => row.id)).toEqual(['a']);
    expect(store.recentCursor.value).toBe('new-recent-cursor');
  });
});
