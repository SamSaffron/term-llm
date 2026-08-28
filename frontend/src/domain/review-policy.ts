import type { DiffComment } from './types';

export const REVIEW_LIMITS = Object.freeze({
  pathBytes: 1024,
  textBytes: 8 * 1024,
  commentBytes: 32 * 1024,
  contextLinesPerSide: 4,
  commentsPerMessage: 25,
  aggregateBytes: 256 * 1024,
});

const encoder = new TextEncoder();
export const utf8Bytes = (value: string): number => encoder.encode(value).byteLength;

export interface ReviewValidationError {
  code: string;
  message: string;
  used: number;
  limit: number;
}

const violation = (
  code: string,
  label: string,
  used: number,
  limit: number,
): ReviewValidationError => ({
  code,
  message: `${label} uses ${used.toLocaleString()} bytes; the limit is ${limit.toLocaleString()} bytes.`,
  used,
  limit,
});

export function reviewCommentPayload(comment: DiffComment): Record<string, unknown> {
  return {
    id: comment.id || '',
    path: comment.path,
    scope: comment.scope || 'last_turn',
    side: comment.side,
    line: comment.line,
    file_change_seq: ['last_turn', 'last_3_turns'].includes(comment.scope || 'last_turn')
      ? Number(comment.fileChangeSeq) || 0
      : 0,
    line_text: comment.context || '',
    context_before: comment.contextBefore || [],
    context_after: comment.contextAfter || [],
    instruction: comment.body,
  };
}

export function validateReviewComment(comment: DiffComment): ReviewValidationError | null {
  const pathBytes = utf8Bytes(comment.path);
  if (pathBytes > REVIEW_LIMITS.pathBytes)
    return violation('path_too_large', 'Path', pathBytes, REVIEW_LIMITS.pathBytes);
  const values = [
    ['Instruction', comment.body],
    ['Selected line', comment.context || ''],
    ...(comment.contextBefore || []).map((line) => ['Context line', line] as const),
    ...(comment.contextAfter || []).map((line) => ['Context line', line] as const),
  ] as const;
  for (const [label, value] of values) {
    const used = utf8Bytes(value);
    if (used > REVIEW_LIMITS.textBytes)
      return violation('text_too_large', label, used, REVIEW_LIMITS.textBytes);
  }
  if ((comment.contextBefore?.length || 0) > REVIEW_LIMITS.contextLinesPerSide)
    return {
      code: 'context_before_too_large',
      message: `Context before has ${comment.contextBefore?.length} lines; the limit is ${REVIEW_LIMITS.contextLinesPerSide}.`,
      used: comment.contextBefore?.length || 0,
      limit: REVIEW_LIMITS.contextLinesPerSide,
    };
  if ((comment.contextAfter?.length || 0) > REVIEW_LIMITS.contextLinesPerSide)
    return {
      code: 'context_after_too_large',
      message: `Context after has ${comment.contextAfter?.length} lines; the limit is ${REVIEW_LIMITS.contextLinesPerSide}.`,
      used: comment.contextAfter?.length || 0,
      limit: REVIEW_LIMITS.contextLinesPerSide,
    };
  const payloadBytes = utf8Bytes(JSON.stringify(reviewCommentPayload(comment)));
  if (payloadBytes > REVIEW_LIMITS.commentBytes)
    return violation(
      'comment_too_large',
      'Serialized comment',
      payloadBytes,
      REVIEW_LIMITS.commentBytes,
    );
  return null;
}

export function validateReviewBatch(comments: DiffComment[]): ReviewValidationError | null {
  if (comments.length > REVIEW_LIMITS.commentsPerMessage)
    return {
      code: 'too_many_comments',
      message: `This batch has ${comments.length} comments; select at most ${REVIEW_LIMITS.commentsPerMessage}.`,
      used: comments.length,
      limit: REVIEW_LIMITS.commentsPerMessage,
    };
  for (const comment of comments) {
    const error = validateReviewComment(comment);
    if (error) return error;
  }
  const aggregate = comments.reduce(
    (total, comment) => total + utf8Bytes(JSON.stringify(reviewCommentPayload(comment))),
    0,
  );
  if (aggregate > REVIEW_LIMITS.aggregateBytes)
    return violation('batch_too_large', 'Review batch', aggregate, REVIEW_LIMITS.aggregateBytes);
  return null;
}

export function reviewAnchorFingerprint(comment: DiffComment): string {
  const source = JSON.stringify([
    comment.path,
    comment.side,
    comment.line,
    comment.context || '',
    comment.contextBefore || [],
    comment.contextAfter || [],
  ]);
  // Fast deterministic non-cryptographic fingerprint; this only detects
  // accidental source movement and is never used as a security boundary.
  let hash = 2166136261;
  for (let index = 0; index < source.length; index += 1) {
    hash ^= source.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return (hash >>> 0).toString(16).padStart(8, '0');
}
