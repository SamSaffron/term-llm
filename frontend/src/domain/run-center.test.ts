import { describe, expect, it } from 'vitest';
import { initialProjection } from './response';
import { deriveRunCenter } from './run-center';
import type { ActiveRun, InteractionRecord, Session } from './types';

const session = { id: 's1', title: 'Main', messages: [] } as unknown as Session;
const run = (status: ActiveRun['status'] = 'streaming'): ReturnType<typeof initialProjection> =>
  initialProjection({
    responseId: 'r1',
    sessionId: 's1',
    epoch: 1,
    status,
    lastSequence: 0,
    startedRev: 0,
    reconnects: 0,
  });

describe('deriveRunCenter', () => {
  it('prioritizes decision attention over reconnecting and reasoning', () => {
    const projection = run();
    projection.run.reconnects = 2;
    const interaction: InteractionRecord = {
      key: 'i1',
      sessionId: 's1',
      responseId: 'r1',
      requestId: 'a1',
      kind: 'approval',
      state: 'dismissed',
      order: 0,
      createdAt: 1,
      prompt: { sessionId: 's1', id: 'a1', options: [] },
    };
    const items = deriveRunCenter([session], { s1: projection }, { i1: interaction }, []);
    expect(items[0]).toMatchObject({ phase: 'awaiting decision', attention: true });
  });

  it('reports reconnecting only while a recovered run is still connecting', () => {
    const connecting = run('connecting');
    connecting.run.reconnects = 2;
    expect(deriveRunCenter([session], { s1: connecting }, {}, [])[0].phase).toBe('reconnecting');
    connecting.run.status = 'streaming';
    expect(deriveRunCenter([session], { s1: connecting }, {}, [])[0].phase).toBe('reasoning');
  });

  it('includes terminal child-agent history with authoritative timing', () => {
    const items = deriveRunCenter([], {}, {}, [
      {
        sessionId: 'child',
        parentSessionId: 's1',
        title: 'Reviewer',
        state: 'complete',
        attention: false,
        revision: 2,
        startedAt: 1000,
        endedAt: 2000,
      },
    ]);
    expect(items[0]).toMatchObject({
      child: true,
      phase: 'completed',
      startedAt: 1000,
      endedAt: 2000,
    });
  });
});
