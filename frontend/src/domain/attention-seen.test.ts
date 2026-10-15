import { describe, expect, it } from 'vitest';
import { mergeSeenState, parseSeenRev } from './attention-seen';

describe('mergeSeenState', () => {
  it.each([
    [
      'keeps the larger watermark without revisions',
      { seenThroughSeq: 5 },
      { seenThroughSeq: 3 },
      false,
      { seenThroughSeq: 5 },
    ],
    [
      'lowers the watermark for a strictly newer revision',
      { seenThroughSeq: 5, seenRev: 2 },
      { seenThroughSeq: 3, seenRev: 3 },
      false,
      { seenThroughSeq: 3, seenRev: 3 },
    ],
    [
      'ignores a stale revision that would raise the watermark',
      { seenThroughSeq: 3, seenRev: 3 },
      { seenThroughSeq: 5, seenRev: 2 },
      false,
      { seenThroughSeq: 3, seenRev: 3 },
    ],
    [
      'trusts a revisioned incoming watermark over an unrevisioned cache',
      { seenThroughSeq: 5 },
      { seenThroughSeq: 3, seenRev: 4 },
      false,
      { seenThroughSeq: 3, seenRev: 4 },
    ],
    [
      'keeps a revisioned watermark over an unrevisioned incoming one',
      { seenThroughSeq: 3, seenRev: 4 },
      { seenThroughSeq: 5 },
      false,
      { seenThroughSeq: 3, seenRev: 4 },
    ],
    [
      'takes the incoming watermark at an equal revision',
      { seenThroughSeq: 5, seenRev: 4 },
      { seenThroughSeq: 3, seenRev: 4 },
      false,
      { seenThroughSeq: 3, seenRev: 4 },
    ],
    [
      'replaces state from a different store',
      { seenThroughSeq: 9, seenRev: 9 },
      { seenThroughSeq: 1 },
      true,
      { seenThroughSeq: 1 },
    ],
  ])('%s', (_name, existing, incoming, storeReplaced, expected) => {
    expect(mergeSeenState(existing, incoming, storeReplaced)).toEqual(expected);
  });
});

describe('parseSeenRev', () => {
  it.each([
    [undefined, undefined],
    [null, undefined],
    ['', undefined],
    [-1, undefined],
    [1.5, undefined],
    ['nope', undefined],
    [0, 0],
    [7, 7],
    ['8', 8],
  ])('parses %j as %j', (value, expected) => {
    expect(parseSeenRev(value)).toBe(expected);
  });
});
