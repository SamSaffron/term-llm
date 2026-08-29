import { describe, expect, it } from 'vitest';
import {
  eventFeedCapability,
  parseServerEvent,
  parseServerEventPollResponse,
  parseServerEventReady,
} from './server-events';

describe('server event protocol', () => {
  const event = {
    v: 1,
    sequence: 7,
    instance_id: 'evt_test',
    type: 'run.finished',
    occurred_at: 100,
    session_id: 's1',
    response_id: 'r1',
    transcript_rev: 9,
    reason: 'completed',
  };

  it('parses a versioned event envelope', () => {
    expect(parseServerEvent(event)).toEqual({
      v: 1,
      sequence: 7,
      instanceId: 'evt_test',
      type: 'run.finished',
      occurredAt: 100,
      sessionId: 's1',
      responseId: 'r1',
      transcriptRev: 9,
      reason: 'completed',
    });
  });

  it('accepts an authoritative snapshot recovery event', () => {
    expect(
      parseServerEvent({
        ...event,
        type: 'snapshot.required',
        session_id: undefined,
        response_id: undefined,
        transcript_rev: undefined,
        reason: 'store_cursor_gap',
      }),
    ).toMatchObject({ type: 'snapshot.required', reason: 'store_cursor_gap' });
  });

  it('rejects unknown versions, types, and invalid cursors', () => {
    expect(parseServerEvent({ ...event, v: 2 })).toBeNull();
    expect(parseServerEvent({ ...event, type: 'response.output_text.delta' })).toBeNull();
    expect(parseServerEvent({ ...event, sequence: -1 })).toBeNull();
    expect(parseServerEvent({ ...event, transcript_rev: 1.5 })).toBeNull();
  });

  it('parses ready controls and filtered long-poll batches', () => {
    expect(
      parseServerEventReady({
        v: 1,
        instance_id: 'evt_test',
        latest_sequence: 8,
        heartbeat_ms: 10_000,
        replay_limit: 2_048,
      }),
    ).toMatchObject({ instanceId: 'evt_test', latestSequence: 8 });
    expect(
      parseServerEventPollResponse({
        object: 'list',
        instance_id: 'evt_test',
        data: [event],
        latest_sequence: 9,
        next_after: 9,
        timed_out: false,
      }),
    ).toMatchObject({ instanceId: 'evt_test', nextAfter: 9, data: [{ sequence: 7 }] });
  });

  it('detects the advertised transport capability', () => {
    expect(eventFeedCapability({ event_feed: { version: 1, sse: true, long_poll: true } })).toBe(
      true,
    );
    expect(eventFeedCapability({})).toBe(false);
  });
});
