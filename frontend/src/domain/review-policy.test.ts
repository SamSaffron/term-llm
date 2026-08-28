import { describe, expect, it } from 'vitest';
import {
  REVIEW_LIMITS,
  utf8Bytes,
  validateReviewBatch,
  validateReviewComment,
} from './review-policy';
import type { DiffComment } from './types';

const comment = (patch: Partial<DiffComment> = {}): DiffComment => ({
  id: 'c1',
  path: 'main.go',
  side: 'new',
  line: 1,
  body: 'change this',
  scope: 'last_turn',
  fileChangeSeq: 1,
  ...patch,
});

describe('review queue policy', () => {
  it('counts UTF-8 bytes instead of JavaScript code units', () => {
    expect(utf8Bytes('界')).toBe(3);
    const error = validateReviewComment(comment({ body: '界'.repeat(3000) }));
    expect(error).toMatchObject({ code: 'text_too_large', used: 9000, limit: 8192 });
  });

  it('enforces context and explicit batch count limits', () => {
    expect(
      validateReviewComment(comment({ contextBefore: ['1', '2', '3', '4', '5'] })),
    ).toMatchObject({
      code: 'context_before_too_large',
    });
    expect(
      validateReviewBatch(
        Array.from({ length: REVIEW_LIMITS.commentsPerMessage + 1 }, (_, index) =>
          comment({ id: `c${index}` }),
        ),
      ),
    ).toMatchObject({ code: 'too_many_comments', used: 26, limit: 25 });
  });

  it('reports each mirrored per-comment byte and context-side limit', () => {
    expect(validateReviewComment(comment({ path: '界'.repeat(342) }))).toMatchObject({
      code: 'path_too_large',
    });
    expect(validateReviewComment(comment({ context: 'x'.repeat(8193) }))).toMatchObject({
      code: 'text_too_large',
    });
    expect(
      validateReviewComment(comment({ contextAfter: ['1', '2', '3', '4', '5'] })),
    ).toMatchObject({ code: 'context_after_too_large', used: 5, limit: 4 });
    expect(
      validateReviewComment(
        comment({
          body: 'x'.repeat(8192),
          context: 'y'.repeat(8192),
          contextBefore: ['a'.repeat(8192), 'b'.repeat(8192)],
        }),
      ),
    ).toMatchObject({ code: 'comment_too_large', limit: REVIEW_LIMITS.commentBytes });
  });

  it('rejects aggregate UTF-8 payload size before transport', () => {
    const comments = Array.from({ length: 25 }, (_, index) =>
      comment({
        id: `c${index}`,
        path: `file-${index}.go`,
        body: 'x'.repeat(7000),
        context: 'y'.repeat(4000),
      }),
    );
    expect(validateReviewBatch(comments)).toMatchObject({
      code: 'batch_too_large',
      limit: REVIEW_LIMITS.aggregateBytes,
    });
  });
});
