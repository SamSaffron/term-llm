import { signal } from '@preact/signals';
import { describe, expect, it, vi } from 'vitest';
import { continueRush, type SteeringActions } from './steering-actions';
import type { RushOperation } from '../domain/steering';
import type { PendingSteering } from './store-types';

function fixture(operation: RushOperation) {
  const pending = signal<PendingSteering[]>([
    { id: 'accepted', sessionId: 's', content: 'first', state: 'pending' },
    { id: 'late', sessionId: 's', content: 'unsent draft', state: 'failed' },
    { id: 'elsewhere', sessionId: 'other', content: 'other tab', state: 'pending' },
  ]);
  const actions = {
    steering: pending,
    activeRush: signal<RushOperation | null>(null),
    setSteering: (entries: PendingSteering[]) => {
      pending.value = entries;
    },
    services: {
      endpoints: { rush: vi.fn(async () => operation), rushState: vi.fn(async () => operation) },
      toast: vi.fn(),
    },
    host: { resumeResponse: vi.fn(async () => undefined), reconcile: vi.fn(async () => undefined) },
  };
  return {
    actions,
    run: () =>
      continueRush(actions as unknown as SteeringActions, 's', 'r', {
        request_id: 'r',
        expected_response_id: 'source',
        expected_run_epoch: 1,
      }),
  };
}
const started: RushOperation = {
  rush_id: 'r',
  session_id: 's',
  source_response_id: 'source',
  source_run_epoch: 1,
  status: 'started',
  revision: 3,
  replacement_response_id: 'replacement',
  steering_ids: ['accepted'],
};

describe('server-owned rush handoff', () => {
  it('retires only the admitted snapshot, never failed or later drafts', async () => {
    const f = fixture(started);
    await f.run();
    expect(f.actions.steering.value.map((entry) => entry.id)).toEqual(['late', 'elsewhere']);
    expect(f.actions.host.resumeResponse).toHaveBeenCalledWith('s', 'replacement');
  });
  it('queries the same operation after an ambiguous transport failure', async () => {
    const f = fixture(started);
    f.actions.services.endpoints.rush.mockRejectedValueOnce(new Error('connection lost'));
    await f.run();
    expect(f.actions.services.endpoints.rush).toHaveBeenCalledTimes(1);
    expect(f.actions.services.endpoints.rushState).toHaveBeenCalledWith('s', 'r');
    expect(f.actions.host.resumeResponse).toHaveBeenCalledWith('s', 'replacement');
  });
  it('keeps recoverable input when Stop wins without starting a fallback', async () => {
    const f = fixture({ ...started, status: 'cancelled', reason: 'stopped by user' });
    await f.run();
    expect(f.actions.steering.value).toHaveLength(3);
    expect(f.actions.host.resumeResponse).not.toHaveBeenCalled();
    expect(f.actions.activeRush.value?.status).toBe('cancelled');
  });
});
