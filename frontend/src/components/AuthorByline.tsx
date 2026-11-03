import type { Author } from '../domain/types';
import { authorColor, authorInitials, authorTitle } from '../domain/authors';
import { useStore } from '../app/context';

export function AuthorAvatar({ author }: { author: Author }) {
  return (
    <span class={`author-avatar author-color-${authorColor(author)}`} aria-hidden="true">
      {authorInitials(author)}
    </span>
  );
}

export function AuthorByline({ author }: { author?: Author | null }) {
  const store = useStore();
  if (author === undefined) return null;
  if (author === null) return <div class="author-byline author-unknown">Unknown</div>;
  return (
    <div class="author-byline" title={authorTitle(author)}>
      <span>{author.id === store.viewer.value?.id ? 'You' : author.name}</span>
      <AuthorAvatar author={author} />
    </div>
  );
}

export function PendingAuthor({ author }: { author?: Author }) {
  return author ? (
    <span class="pending-author" title={authorTitle(author)}>
      <AuthorAvatar author={author} />
      <span>{author.name.trim().split(/\s+/)[0]}:</span>
    </span>
  ) : null;
}
