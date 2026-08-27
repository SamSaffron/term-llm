import { describe, expect, it } from 'vitest';
import { responseActivity } from './activity';
import type { Message } from './types';

const projection = (tools: Message['tools'] = [], retry = false) => ({
  messages: tools.length
    ? [
        {
          id: 'tools',
          role: 'tool-group' as const,
          content: '',
          created: 1,
          tools,
        },
      ]
    : [],
  retry: retry ? { attempt: 2, delayMs: 100, error: 'busy' } : undefined,
});

const running = (id: string, name: string) => ({ id, name, status: 'running' as const });

describe('responseActivity', () => {
  it('keeps the active plan step stable while tools run', () => {
    expect(
      responseActivity(
        projection([running('read', 'read_file')]),
        {
          plan: [
            { step: 'Inspect the existing flow', status: 'completed' },
            { step: 'Build the semantic indicator', status: 'in_progress' },
          ],
        },
        'streaming',
      ),
    ).toEqual({ text: 'Build the semantic indicator', kind: 'plan' });
  });

  it('uses concise tool-aware fallbacks when there is no plan', () => {
    expect(responseActivity(projection([running('read', 'read_file')]), null, 'streaming')).toEqual(
      { text: 'Reading files', kind: 'tool' },
    );
    expect(
      responseActivity(
        projection([running('read', 'read_file'), running('edit', 'edit_file')]),
        null,
        'streaming',
      ),
    ).toEqual({ text: 'Running 2 tools', kind: 'tool' });
    expect(
      responseActivity(
        projection([running('read', 'read_file')]),
        { plan: [{ step: 'Old task', status: 'completed' }] },
        'streaming',
      ),
    ).toEqual({ text: 'Reading files', kind: 'tool' });
  });

  it('falls back cleanly between explicit response states', () => {
    expect(responseActivity(projection([], true), null, 'streaming')).toEqual({
      text: 'Retrying provider · attempt 2',
      kind: 'retrying',
    });
    expect(responseActivity(projection(), null, 'streaming')).toEqual({
      text: 'Working',
      kind: 'working',
    });
    expect(responseActivity(projection(), null, 'cancelling')).toEqual({
      text: 'Stopping',
      kind: 'stopping',
    });
  });
});
