import { describe, expect, it, vi } from 'vitest';
import { parseTabEvent, TabSync } from './tab-sync';

describe('tab synchronization envelopes', () => {
  it('validates versioned envelopes and accepts legacy full invalidation', () => {
    expect(parseTabEvent({ type: 'sessions-changed' })).toBe('legacy');
    expect(parseTabEvent({ v: 2, type: 'run-changed' })).toBeNull();
    expect(
      parseTabEvent({
        v: 1,
        eventId: 'e1',
        originTabId: 't1',
        operationId: 'o1',
        type: 'run-changed',
        sessionId: 's1',
        revision: 2,
        occurredAt: 1,
      }),
    ).toEqual({
      v: 1,
      eventId: 'e1',
      originTabId: 't1',
      operationId: 'o1',
      type: 'run-changed',
      sessionId: 's1',
      revision: 2,
      occurredAt: 1,
    });
  });

  it('ignores self and duplicate events with a bounded cache', () => {
    vi.spyOn(globalThis.crypto, 'randomUUID')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000001')
      .mockReturnValue('00000000-0000-4000-8000-000000000002');
    const tabs = new TabSync(1);
    const peer = (eventId: string) => ({
      v: 1,
      eventId,
      originTabId: 'peer',
      operationId: eventId,
      type: 'draft-changed',
      occurredAt: 1,
    });

    expect(tabs.accept(peer('e1'))).not.toBeNull();
    expect(tabs.accept(peer('e1'))).toBeNull();
    expect(tabs.accept({ ...peer('retry'), operationId: 'e1' })).toBeNull();
    expect(tabs.accept(peer('e2'))).not.toBeNull();
    expect(tabs.accept(peer('e1'))).not.toBeNull();
    expect(tabs.accept({ ...peer('self'), originTabId: tabs.tabId })).toBeNull();
  });
});
