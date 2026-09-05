import { describe, expect, it } from 'vitest';
import { normalizeSteering, rushActive } from './steering';
import { legacySteeringBody } from '../api/steering';

describe('steering protocol compatibility', () => {
  it('normalizes old receipts without changing text, IDs or sequence', () => {
    const old = {
      type: 'response.interjection',
      interjection_id: 'original',
      sequence_number: 42,
      text: 'interject literally',
    };
    expect(normalizeSteering(old)).toMatchObject({
      type: 'response.steering',
      steering_id: 'original',
      sequence_number: 42,
      text: old.text,
    });
    expect(old.type).toBe('response.interjection');
  });
  it('normalizes both historical pending shapes at ingress', () => {
    const entry = { id: 'a', text: 'guidance' };
    expect(normalizeSteering({ pending_interjection: entry })).toMatchObject({
      pending_steering: [entry],
    });
    expect(normalizeSteering({ pending_interjections: [entry] })).toMatchObject({
      pending_steering: [entry],
    });
    expect(normalizeSteering({ pending_interjection: null })).toMatchObject({
      pending_steering: [],
    });
  });
  it('projects old-server submission IDs in one adapter', () => {
    const body = {
      steering_id: 'id',
      client_message_id: 'id',
      message: 'do not rewrite interject',
    };
    expect(legacySteeringBody(body)).toEqual({
      interjection_id: 'id',
      client_message_id: 'id',
      message: body.message,
    });
    expect(body.steering_id).toBe('id');
  });
  it('retains terminal operation metadata without disabling input', () => {
    for (const status of ['started', 'blocked', 'cancelled', 'failed', 'noop'])
      expect(
        rushActive({
          rush_id: 'r',
          session_id: 's',
          source_response_id: 'old',
          source_run_epoch: 1,
          status,
          revision: 2,
        }),
      ).toBe(false);
  });
});
