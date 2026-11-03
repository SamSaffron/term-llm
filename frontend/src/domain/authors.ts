import type { Author, Message, Session } from './types';

/** Preserve immutable API objects so unchanged author props stay memoizable. */
export function authorFrom(value: unknown): Author | undefined {
  const author = value as Author | null;
  return author && typeof author.id === 'string' && author.id && typeof author.name === 'string'
    ? author
    : undefined;
}

export function authorInitials(author: Author): string {
  const words = author.name.trim().split(/\s+/).filter(Boolean);
  return (
    words.length > 1
      ? `${Array.from(words[0])[0] || ''}${Array.from(words.at(-1)!)[0] || ''}`
      : Array.from(words[0] || author.id)
          .slice(0, 2)
          .join('')
  ).toUpperCase();
}
/** Number of `.author-color-N` classes in styles/features/authors.css. */
export const AUTHOR_COLOR_COUNT = 8;

export function authorColor(author: Author): number {
  return (
    Array.from(author.id).reduce((hash, char) => (hash * 31 + char.codePointAt(0)!) % 65521, 0) %
    AUTHOR_COLOR_COUNT
  );
}
export function authorTitle(author: Author): string {
  return `${author.name}${author.email ? ` <${author.email}>` : ''}`;
}
export function participantNames(session: Session, viewer?: Author): string {
  return (session.participants || [])
    .map((a) => `${a.name}${a.id === viewer?.id ? ' (you)' : ''}`)
    .join(', ');
}

/** Undefined hides a byline; null explicitly labels an unattributed transition. */
export function bylineFor(
  message: Message,
  previous?: Message,
  gap = false,
): Author | null | undefined {
  if (message.role !== 'user') return;
  if (gap || !previous || previous.role !== 'user') return message.author;
  if (!message.author) return previous.author ? null : undefined;
  return message.author.id === previous.author?.id ? undefined : message.author;
}
