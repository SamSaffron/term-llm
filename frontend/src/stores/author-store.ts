import { errorMessage } from '../domain/text';
import { signal } from '@preact/signals';
import type { Author } from '../domain/types';
import type { SessionStore } from './session-store';
import type { AppStoreServices } from './app-store-services';

/** Viewer identity is request metadata, never persisted as an auth decision. */
export class AuthorStore {
  readonly viewer = signal<Author | undefined>(undefined);
  readonly filter = signal<'everyone' | 'me'>('everyone');
  constructor(
    private services: AppStoreServices,
    private refresh: () => Promise<void>,
  ) {
    if (services.storage.getItem(services.keys.authorFilter) === 'me') this.filter.value = 'me';
  }
  get createdBy(): string {
    return this.viewer.value && this.filter.value === 'me' ? 'me' : '';
  }
  get query(): string {
    return this.createdBy ? '&created_by=me' : '';
  }
  async setFilter(value: 'everyone' | 'me'): Promise<void> {
    this.filter.value = value;
    this.services.storage.setItem(this.services.keys.authorFilter, value);
    try {
      await this.refresh();
    } catch (error) {
      this.services.toast(errorMessage(error), 'error');
    }
  }
}

/** Paging cursors and retained tails belong to the previous author-filter scope. */
export function resetAuthorPages(store: SessionStore): void {
  store.recentSessions.value = [];
  store.recentCursor.value = '';
  store.noProjectCursor.value = '';
  store.projects.value = store.projects
    .peek()
    .map((project) => ({ ...project, sessions: [], next_cursor: '', has_more: false }));
}
