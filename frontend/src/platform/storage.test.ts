import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearDraft,
  draftKey,
  persistDiffComment,
  readDiffCommentQueue,
  readDrafts,
  removeDiffComment,
  saveDraft,
} from './storage';

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
      {
        id: 'legacy_1_b.go_old_4',
        path: 'b.go',
        side: 'old',
        line: 4,
        body: 'x',
        sessionId: 's2',
      },
    ]);
    // Compatibility data remains untouched while independently writable
    // records and a separate migration marker are added.
    expect(JSON.parse(localStorage.getItem(KEY) || 'null')).toMatchObject({ v: 1 });
    expect(localStorage.getItem(`${KEY}:s1:a1`)).not.toBeNull();
    expect(localStorage.getItem(`${KEY}:migration:v2`)).not.toBeNull();
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

  it('keeps legacy comments deleted and preserves complete anchor metadata', () => {
    localStorage.setItem(
      KEY,
      JSON.stringify([
        {
          id: 'c1',
          parentId: 'parent',
          path: 'a.go',
          side: 'new',
          line: 2,
          body: 'keep',
          sessionId: 's1',
          contextBefore: ['before'],
          contextAfter: ['after'],
        },
      ]),
    );
    expect(readDiffCommentQueue(localStorage, KEY)[0]).toMatchObject({
      parentId: 'parent',
      contextBefore: ['before'],
      contextAfter: ['after'],
    });
    removeDiffComment(localStorage, KEY, 's1', 'c1');
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([]);

    const replacement = {
      id: 'c1',
      sessionId: 's1',
      path: 'a.go',
      side: 'new' as const,
      line: 3,
      body: 'replacement',
    };
    persistDiffComment(localStorage, KEY, replacement);
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([
      expect.objectContaining({ line: 3, body: 'replacement' }),
    ]);
  });

  it('tolerates unrecognized shapes without throwing', () => {
    localStorage.setItem(KEY, JSON.stringify({ unexpected: true }));
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([]);
    localStorage.setItem(KEY, 'not json');
    expect(readDiffCommentQueue(localStorage, KEY)).toEqual([]);
  });
});

describe('record-oriented drafts', () => {
  it('writes independent draft keys and detects stale same-record edits', () => {
    saveDraft(localStorage, 'drafts', { sessionId: 's1', content: 'one', updated: 1, rev: 0 });
    saveDraft(localStorage, 'drafts', { sessionId: 's2', content: 'two', updated: 1, rev: 0 });
    expect(localStorage.getItem(draftKey('drafts', 's1'))).not.toBeNull();
    expect(localStorage.getItem(draftKey('drafts', 's2'))).not.toBeNull();

    expect(() =>
      saveDraft(localStorage, 'drafts', {
        sessionId: 's1',
        content: 'stale overwrite',
        updated: 2,
        rev: 0,
      }),
    ).toThrow(/another tab saved a newer revision/i);
    expect(JSON.parse(localStorage.getItem(draftKey('drafts', 's1')) || '{}').content).toBe('one');
  });

  it('keeps retention-evicted legacy drafts tombstoned', () => {
    localStorage.setItem(
      'drafts',
      JSON.stringify(
        Array.from({ length: 11 }, (_, index) => ({
          sessionId: `legacy-${index}`,
          content: `draft ${index}`,
          updated: index + 1,
        })),
      ),
    );

    saveDraft(localStorage, 'drafts', {
      sessionId: 'newest',
      content: 'new',
      updated: 20,
      rev: 0,
    });

    expect(readDrafts(localStorage, 'drafts')).toHaveLength(10);
    expect(readDrafts(localStorage, 'drafts').some((draft) => draft.sessionId === 'legacy-0')).toBe(
      false,
    );
    expect(JSON.parse(localStorage.getItem(draftKey('drafts', 'legacy-0')) || '{}')).toMatchObject({
      deleted: true,
    });
  });

  it('keeps cleared migrated drafts tombstoned while compatibility reads remain', () => {
    localStorage.setItem(
      'drafts',
      JSON.stringify([{ sessionId: 'legacy', content: 'old intent', updated: 1 }]),
    );
    expect(readDrafts(localStorage, 'drafts')).toHaveLength(1);

    clearDraft(localStorage, 'drafts', 'legacy');
    expect(readDrafts(localStorage, 'drafts')).toEqual([]);
    expect(JSON.parse(localStorage.getItem(draftKey('drafts', 'legacy')) || '{}')).toMatchObject({
      sessionId: 'legacy',
      deleted: true,
    });
    expect(JSON.parse(localStorage.getItem('drafts') || '[]')).toHaveLength(1);
  });
});
