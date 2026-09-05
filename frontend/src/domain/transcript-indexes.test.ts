import { describe, expect, it, vi } from 'vitest';
import { createTranscriptIndexes } from './transcript-indexes';
import { indexTranscriptTurns } from './transcript';
import type { Message } from './types';

const message = (id: string, role: Message['role'] = 'assistant'): Message => ({
  id,
  role,
  content: id,
  created: 1,
});
describe('incremental transcript indexes', () => {
  it('only rebuilds the changed turn and reuses historical contexts', () => {
    const text = vi.fn((entry: Message) => entry.content);
    const index = createTranscriptIndexes((url) => url, text);
    const history = [message('u1', 'user'), message('a1'), message('u2', 'user'), message('a2')];
    const first = index(history);
    const oldContext = first.contexts.get(history[1]);
    text.mockClear();
    const next = [...history.slice(0, 3), { ...history[3], content: 'continued' }];
    const updated = index(next);
    expect(text.mock.calls.map(([entry]) => entry.id)).toEqual(['u2', 'a2']);
    expect(updated.contexts.get(history[1])).toBe(oldContext);
    expect(updated.contexts).toEqual(indexTranscriptTurns(next, (entry) => entry.content));
    text.mockClear();
    index([...next, message('u3', 'user')]);
    expect(text.mock.calls.map(([entry]) => entry.id)).toEqual(['u3']);
  });

  it('matches complete indexing through inserts, role changes, removals and resets', () => {
    const index = createTranscriptIndexes((url) => url);
    let entries: Message[] = [];
    for (let step = 0; step < 160; step += 1) {
      const next = [...entries];
      const at = next.length ? (step * 7) % next.length : 0;
      if (step % 11 === 0) next.splice(0);
      else if (step % 4 === 0) next.splice(at, 1);
      else if (step % 3 === 0 && next.length)
        next[at] = {
          ...next[at],
          role: step % 2 ? 'assistant' : 'user',
          content: `replacement-${step}`,
        };
      else next.splice(at, 0, message(`m-${step}`, step % 3 ? 'assistant' : 'user'));
      entries = next;
      expect(index(entries).contexts, `step ${step}`).toEqual(
        indexTranscriptTurns(entries, (entry) => entry.content),
      );
    }
  });

  it('does not revalidate media on text changes, and handles duplicate removal and new media', () => {
    const url = vi.fn((value: string) => (value.startsWith('/safe/') ? value : ''));
    const index = createTranscriptIndexes(url);
    const first: Message = {
      ...message('tool', 'tool-group'),
      tools: [
        {
          id: 'one',
          name: 'show_media',
          status: 'done',
          media: [
            { reference: 'A', type: 'image/png', url: '/safe/first.png' },
            { reference: 'bad', type: 'image/png', url: 'javascript:alert(1)' },
          ],
        },
      ],
    };
    const second: Message = {
      ...message('tool2', 'tool-group'),
      tools: [
        {
          id: 'two',
          name: 'show_media',
          status: 'done',
          media: [{ reference: 'a', type: 'video/webm', url: '/safe/second.webm' }],
        },
      ],
    };
    const media = index([first, second, message('text')]).media;
    expect(media.size).toBe(1);
    expect(media.get('a')?.url).toBe('/safe/first.png');
    url.mockClear();
    expect(index([first, second, message('more text')]).media).toBe(media);
    expect(url).not.toHaveBeenCalled();
    expect(index([second]).media.get('a')).toEqual({ type: 'video', url: '/safe/second.webm' });
    expect(url).not.toHaveBeenCalled();
    expect(index([]).media.size).toBe(0);
  });
});
