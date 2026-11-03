import { useStore } from '../app/context';
import { AuthorAvatar } from './AuthorByline';

// Native radios provide roving focus and arrow-key navigation without a custom keyboard state machine.
export function AuthorFilter() {
  const store = useStore();
  const authors = store.sessionStore.authors;
  if (!store.viewer.value) return null;
  return (
    <>
      <div class="author-filter" role="group" aria-label="Conversation authors">
        {(['everyone', 'me'] as const).map((value) => (
          <label key={value}>
            <input
              type="radio"
              name="author-filter"
              checked={authors.filter.value === value}
              onChange={() => void authors.setFilter(value)}
            />
            <span>{value === 'me' ? 'Started by me' : 'Everyone'}</span>
          </label>
        ))}
      </div>
      {authors.filter.value === 'me' &&
        !store.sidebarSearch.value &&
        !store.sidebarSessions.value.length && (
          <div class="sidebar-empty">No chats started by you.</div>
        )}
    </>
  );
}

export function IdentityCard() {
  const store = useStore();
  const viewer = store.viewer.value;
  if (!viewer) return null;
  return (
    <div class="author-identity">
      <AuthorAvatar author={viewer} />
      <div>
        <strong>{viewer.name}</strong>
        <div class="author-identity-detail">
          {viewer.email && <>{viewer.email} · </>}via{' '}
          {store.linkedDevicesMode.value ? 'linked device' : 'proxy'}
        </div>
      </div>
    </div>
  );
}
