import { fireEvent, render, screen, waitFor, within } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import type { SessionStats } from '../api/endpoints';
import type { Session } from '../domain/types';
import { SLASH_COMMANDS } from '../domain/completions';
import { StatsModal } from './StatsModal';

const metrics = {
  input_tokens: 1234,
  output_tokens: 12,
  cached_input_tokens: 30,
  cache_write_tokens: 4,
  tool_calls: 2,
  llm_turns: 1,
};
const sections = [
  'Current Context / Window Pressure',
  'Tool Discovery',
  'Private Side-Question Usage',
  'Guardian Usage',
  'Compaction Usage',
  'Cumulative Session Activity',
  'Compactions',
];
const runtime: SessionStats = {
  metrics,
  scope: 'runtime_local',
  active_ms: 31300,
  model_ms: 22400,
  tool_ms: 8900,
  ttft_ms: 500,
  output_tokens_per_second: 42,
  cost_usd: 0.1234,
  cost_partial: true,
  models: [
    { ...metrics, model: 'shared-model', active_ms: 10000, tool_ms: 5000, cost_usd: 0.0234 },
  ],
  sections: sections.map((title) => ({
    title,
    rows: [{ label: `${title} detail`, value: 'retained detail' }],
  })),
};
function fixture(data: SessionStats = runtime, fail = false) {
  const store = new AppStore({
    prefix: '/ui',
    version: 'v1',
    sidebarCategories: ['all'],
    agentName: '',
    agentNames: [],
    title: '',
    locationSharing: false,
    worktrees: false,
    hub: null,
    vapidKey: '',
    webRTC: false,
    signalingURL: '',
  });
  store.sessions.value = [
    { id: 's1', messages: [] },
    { id: 's2', messages: [] },
  ] as unknown as Session[];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.modal.value = 'stats';
  store.endpoints.sessionStats = vi.fn(async () => {
    if (fail) throw new Error('Stats request failed');
    return data;
  });
  store.endpoints.sessionChildren = vi.fn(async () => ({
    children: ['a', 'b'].map((id) => ({
      ...metrics,
      session_id: id,
      title: 'Prompt-bearing agent title',
      model: 'shared-model',
      state: 'complete',
      started_at: 1000,
      ended_at: 4000,
    })),
  }));
  render(
    <StoreContext.Provider value={store}>
      <StatsModal />
    </StoreContext.Provider>,
  );
  return store;
}

describe('web stats', () => {
  it('registers a streaming-safe stats command', () => {
    expect(SLASH_COMMANDS.filter((entry) => entry.command === '/stats')).toEqual([
      {
        command: '/stats',
        description: 'Show session and subagent usage statistics',
        streamingSafe: true,
      },
    ]);
  });
  it('shows all runtime sections, work time, performance and model billing without agent prose', async () => {
    const store = fixture();
    expect(await screen.findByText('31.3s')).toBeTruthy();
    for (const text of ['22.4s', '8.9s', '0.5s', '42.0 tokens/s', '≥$0.1234'])
      expect(screen.getByText(text)).toBeTruthy();
    for (const title of sections)
      expect(screen.getByRole('heading', { name: title, exact: true })).toBeTruthy();
    const table = screen.getByRole('table');
    expect(within(table).getByRole('rowheader', { name: /shared-model/ })).toBeTruthy();
    expect(within(table).getByText('$0.0234')).toBeTruthy();
    expect(screen.queryByText('Prompt-bearing agent title')).toBeNull();
    fireEvent.click(screen.getByText('Refresh statistics'));
    expect(store.endpoints.sessionStats).toHaveBeenCalledTimes(2);
  });
  it('keeps durable history separate and does not invent historical cost or active time', async () => {
    fixture({ ...runtime, durable_metrics: { ...metrics, input_tokens: 9999 } });
    await screen.findByText('Recorded parent-session totals');
    expect(screen.getByText('9,999')).toBeTruthy();
    expect(screen.getByText(/do not add/)).toBeTruthy();
  });
  it('groups historical child counters by model, with missing pricing and timing', async () => {
    fixture({ metrics, scope: 'durable_history', models: [], sections: [] });
    await screen.findByText('Recorded history');
    const table = screen.getByRole('table');
    expect(within(table).getAllByRole('rowheader')).toHaveLength(1);
    expect(within(table).getByText('2,468')).toBeTruthy();
    expect(within(table).getAllByText('—')).toHaveLength(3);
    expect(screen.queryByText('3.0s')).toBeNull();
  });
  it('reports fetch errors and offers retry', async () => {
    fixture(runtime, true);
    expect(await screen.findByRole('alert')).toHaveTextContent('Stats request failed');
    expect(screen.getByText('Refresh statistics')).toBeTruthy();
  });
  it('tolerates partial counters and empty models on refresh', async () => {
    const store = fixture();
    await screen.findByText('31.3s');
    store.endpoints.sessionStats = vi.fn(async () => ({
      metrics: {} as SessionStats['metrics'],
      scope: 'runtime_local' as const,
      models: [],
      sections: [],
    }));
    fireEvent.click(screen.getByText('Refresh statistics'));
    await screen.findByText('No observed subagent usage.');
    expect(screen.queryByText('NaN')).toBeNull();
    expect(screen.getAllByText('0')).toHaveLength(6);
  });
  it('aborts stale requests when the session changes', async () => {
    const store = fixture();
    await screen.findByText('31.3s');
    const signal = vi.mocked(store.endpoints.sessionStats).mock.calls[0][1];
    store.endpoints.sessionStats = vi.fn(async () => ({ ...runtime, active_ms: 1000 }));
    store.activeSessionId.value = 's2';
    await waitFor(() => expect(signal?.aborted).toBe(true));
    await screen.findByText('1.0s');
    expect(screen.queryByText('31.3s')).toBeNull();
  });
});
