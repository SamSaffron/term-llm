import type { MarkdownMediaResolver, MarkdownMediaTarget } from './markdown';

/** Track the references actually requested by each row's Markdown renderers.
 * New artifacts invalidate only rows that used those references, not every
 * historical body (which would destroy selections and media playback state).
 * The cache is owned by one transcript/session and contains no DOM nodes.
 */
export function createMessageMediaResolvers() {
  let current: ReadonlyMap<string, MarkdownMediaTarget> = new Map();
  const rows = new Map<
    string,
    {
      used: Map<string, MarkdownMediaTarget | undefined>;
      resolve: MarkdownMediaResolver;
    }
  >();
  return (id: string, media: ReadonlyMap<string, MarkdownMediaTarget>): MarkdownMediaResolver => {
    current = media;
    const previous = rows.get(id);
    if (
      previous &&
      [...previous.used].every(([reference, old]) => {
        const next = media.get(reference);
        return old?.url === next?.url && old?.type === next?.type;
      })
    )
      return previous.resolve;
    const used = new Map<string, MarkdownMediaTarget | undefined>();
    const resolve: MarkdownMediaResolver = (reference) => {
      const key = reference.toLowerCase();
      const target = current.get(key);
      used.set(key, target);
      return target;
    };
    rows.set(id, { used, resolve });
    return resolve;
  };
}
