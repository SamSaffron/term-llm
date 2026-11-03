import type { Author, Session } from '../domain/types';
import { participantNames } from '../domain/authors';
import { AuthorAvatar } from './AuthorByline';

export function Facepile({ session, viewer }: { session: Session; viewer?: Author }) {
  const participants = session.participants || [];
  if (!participants.length) return null;
  const visible = participants.slice(0, 3);
  const extra = (session.participantCount || participants.length) - 3;
  const names = participantNames(session, viewer);
  return (
    <span class="author-facepile" role="img" aria-label={`Participants: ${names}`} title={names}>
      {visible.map((a) => (
        <AuthorAvatar key={a.id} author={a} />
      ))}
      {extra > 0 && <span class="author-overflow">+{extra}</span>}
    </span>
  );
}
