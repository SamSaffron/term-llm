import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  AUTHOR_COLOR_COUNT,
  authorColor,
  authorInitials,
  bylineFor,
  participantNames,
} from './authors';
import { convertServerMessages, sanitizeSession } from './transcript';
import { createTranscriptIndexes } from './transcript-indexes';
import { initialProjection, reduceResponse } from './response';
import type { Author, Message } from './types';

const alice: Author = { id: 'alice', name: 'Alice Example', email: 'alice@example.com' };
const bob: Author = { id: 'bob', name: 'Bob Example' };
const message = (id: string, author?: Author, role: Message['role'] = 'user'): Message => ({
  id,
  role,
  author,
  content: id,
  created: 1,
});

describe('display-only authors', () => {
  it('derives initials and stable palette colors without network images', () => {
    expect(authorInitials(alice)).toBe('AE');
    expect(authorInitials({ id: 'user', name: '' })).toBe('US');
    expect(authorInitials({ id: 'different-id', name: 'Priya' })).toBe('PR');
    expect(authorInitials({ id: 'different-id', name: ' Åsa ' })).toBe('ÅS');
    expect(authorColor(alice)).toBe(authorColor({ ...alice, name: 'Renamed' }));
    expect(authorColor(alice)).toBeLessThan(AUTHOR_COLOR_COUNT);
  });
  it('styles every palette slot with a fill that keeps white initials legible', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/styles/features/authors.css'), 'utf8');
    const fills = [...css.matchAll(/\.author-color-(\d+)\s*\{\s*--author-bg:\s*(#[0-9a-f]{6})/g)];
    expect(fills.map((match) => Number(match[1]))).toEqual(
      Array.from({ length: AUTHOR_COLOR_COUNT }, (_, index) => index),
    );
    const luminance = (hex: string) =>
      [1, 3, 5]
        .map((at) => parseInt(hex.slice(at, at + 2), 16) / 255)
        .map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4))
        .reduce((sum, v, index) => sum + v * [0.2126, 0.7152, 0.0722][index], 0);
    for (const [, , fill] of fills)
      expect(1.05 / (luminance(fill) + 0.05)).toBeGreaterThanOrEqual(4.5);
  });
  it('groups consecutive authors and marks only immediate anonymous transitions Unknown', () => {
    const rows = [
      message('a', alice),
      message('a2', alice),
      message('b', bob),
      message('anon'),
      message('anon2'),
      message('reply', undefined, 'assistant'),
      message('b2', bob),
    ];
    const index = createTranscriptIndexes((u) => u)(rows);
    expect(rows.map((m) => index.bylines.get(m))).toEqual([
      alice,
      undefined,
      bob,
      null,
      undefined,
      undefined,
      bob,
    ]);
    expect(bylineFor(rows[1], rows[0], true)).toBe(alice);
    expect(bylineFor(rows[3], rows[2], true)).toBeUndefined();
  });
  it('converts user authors but keeps assistant rows unchanged', () => {
    const messages = convertServerMessages([
      { id: 1, role: 'user', author: alice, parts: [{ type: 'text', text: 'hello' }] },
      { id: 2, role: 'assistant', author: bob, parts: [{ type: 'text', text: 'answer' }] },
    ]);
    expect(messages[0].author).toEqual(alice);
    expect(messages[1].author).toBeUndefined();
  });
  it('preserves unknown session metadata when merging absent snapshot fields', () => {
    const session = sanitizeSession({
      id: 's',
      created_by: alice,
      participants: [alice, bob],
      participant_count: 2,
    });
    const snapshot = sanitizeSession({ id: 's' });
    for (const invalid of ['alice', null, {}, { id: 'alice' }, { id: '', name: 'Alice' }]) {
      const incoming = sanitizeSession({ id: 's', created_by: invalid, created_by_id: 'alice' });
      expect(Object.hasOwn(incoming, 'createdBy')).toBe(false);
      expect({ ...session, ...incoming }.createdBy).toEqual(alice);
    }
    expect({ ...session, ...snapshot }).toMatchObject({
      createdBy: alice,
      participants: [alice, bob],
      participantCount: 2,
    });
    expect(participantNames(session, alice)).toBe('Alice Example (you), Bob Example');
    expect(
      { ...session, ...sanitizeSession({ id: 's', participant_count: 0 }) }.participants,
    ).toEqual([]);
  });
  it('projects live steering authors', () => {
    const projection = initialProjection({
      sessionId: 's',
      responseId: 'r',
      epoch: 1,
      status: 'streaming',
      lastSequence: 0,
      startedRev: 0,
      reconnects: 0,
    });
    const next = reduceResponse(projection, {
      type: 'response.steering',
      response_id: 'r',
      run_epoch: 1,
      sequence_number: 1,
      client_message_id: 'intent',
      text: 'check this',
      author: bob,
    });
    expect(next.messages.at(-1)?.author).toEqual(bob);
  });
});
