import { beforeEach, describe, expect, it } from 'vitest';
import { readDiffCommentQueue } from './storage';

const KEY = 'term_llm_diff_comment_queue';

beforeEach(() => localStorage.clear());

describe('readDiffCommentQueue', () => {
  it('returns an empty array when nothing is stored', () => {
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([]);
  });

  it('migrates the legacy session-keyed queue format', () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({
        v: 1,
        sessions: {
          s1: {
            mode: 'queue',
            items: [
              {
                id: 'a1',
                path: 'main.go',
                side: 'new',
                line: 12,
                scope: 'last_turn',
                instruction: 'rename this',
                line_text: 'func main() {',
                file_change_seq: 3,
              },
              { id: 'bad', path: '', side: 'new', line: 1, instruction: 'no path' },
            ],
          },
          s2: { mode: 'send', items: [{ path: 'b.go', side: 'old', line: 4, instruction: 'x' }] },
        },
      }),
    );
    const comments = readDiffCommentQueue(localStorage, KEY);
    expect(comments).toEqual([
      {
        id: 'a1',
        path: 'main.go',
        side: 'new',
        line: 12,
        body: 'rename this',
        sessionId: 's1',
        scope: 'last_turn',
        context: 'func main() {',
        fileChangeSeq: 3,
      },
      { path: 'b.go', side: 'old', line: 4, body: 'x', sessionId: 's2' },
    ]);
    // The migrated flat array is written back so future boots read the new format.
    expect(JSON.parse(localStorage.getItem(KEY) || 'null')).toEqual(comments);
  });

  it('keeps valid entries and drops malformed ones from the current format', () => {
    localStorage.setItem(
      KEY,
      JSON.stringify([
        { id: 'c1', path: 'a.go', side: 'new', line: 2, body: 'keep', sessionId: 's1' },
        { id: 'c2', path: 'a.go', side: 'sideways', line: 2, body: 'drop' },
        'garbage',
      ]),
    );
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([
      { id: 'c1', path: 'a.go', side: 'new', line: 2, body: 'keep', sessionId: 's1' },
    ]);
  });

  it('tolerates unrecognized shapes without throwing', () => {
    localStorage.setItem(KEY, JSON.stringify({ unexpected: true }));
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([]);
    localStorage.setItem(KEY, 'not json');
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([]);
  });
});
