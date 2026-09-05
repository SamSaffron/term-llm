import { indexTranscriptTurns, type TranscriptRowContext } from './transcript';
import type { MediaArtifact, Message } from './types';
import type { MarkdownMediaTarget } from './markdown';

/** Session-local derived indexes. Message and media inputs must be immutable.
 * Reference comparisons remain linear, but unchanged history is not reparsed,
 * URL-validated, or concatenated on each foreground streaming update.
 */
export function createTranscriptIndexes(
  mediaURL: (url: string) => string,
  text = (message: Message) => message.content,
) {
  let previous: Message[] = [];
  const contexts = new Map<Message, TranscriptRowContext>();
  let mediaInputs: MediaArtifact[][] = [];
  let media = new Map<string, MarkdownMediaTarget>();
  const mediaCache = new WeakMap<MediaArtifact[], [string, MarkdownMediaTarget][]>();

  return (messages: Message[]) => {
    let changed = 0;
    while (
      changed < previous.length &&
      changed < messages.length &&
      previous[changed] === messages[changed]
    )
      changed += 1;
    if (changed !== previous.length || changed !== messages.length) {
      // Changes can affect the copy target/text of the entire containing turn.
      let start = changed;
      if (
        start === messages.length ||
        messages[start]?.role !== 'user' ||
        (start < previous.length && previous[start]?.role !== 'user')
      ) {
        start = Math.max(0, start - 1);
        while (start > 0 && messages[start].role !== 'user') start -= 1;
      }
      for (let i = start; i < previous.length; i += 1) contexts.delete(previous[i]);
      for (const [message, context] of indexTranscriptTurns(messages.slice(start), text)) {
        contexts.set(message, { ...context, index: context.index + start });
      }
    }
    previous = messages;

    const inputs: MediaArtifact[][] = [];
    for (const message of messages)
      for (const tool of message.tools || []) if (tool.media) inputs.push(tool.media);
    if (
      inputs.length !== mediaInputs.length ||
      inputs.some((input, index) => input !== mediaInputs[index])
    ) {
      const next = new Map<string, MarkdownMediaTarget>();
      for (const input of inputs) {
        let entries = mediaCache.get(input);
        if (!entries) {
          entries = [];
          for (const item of input) {
            const reference = String(item.reference || '')
              .trim()
              .toLowerCase();
            if (!reference) continue;
            const url = mediaURL(item.url);
            if (url)
              entries.push([
                reference,
                { url, type: item.type.startsWith('video/') ? 'video' : 'image' },
              ]);
          }
          mediaCache.set(input, entries);
        }
        for (const [reference, target] of entries)
          if (!next.has(reference)) next.set(reference, target);
      }
      if (
        next.size !== media.size ||
        [...next].some(
          ([key, item]) => media.get(key)?.url !== item.url || media.get(key)?.type !== item.type,
        )
      )
        media = next;
      mediaInputs = inputs;
    }
    return { contexts, media };
  };
}
