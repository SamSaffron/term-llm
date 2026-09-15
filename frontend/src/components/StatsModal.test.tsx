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
const childIDs: Array<{ id: string; cost?: number }> = [{ id: 'a' }, { id: 'b' }];
function fixture(data: SessionStats = runtime, fail = false, childSessions = childIDs) {
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
    children: childSessions.map((child) => ({
      ...metrics,
      session_id: child.id,
      cost_usd: child.cost,
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
    for (const text of ['22.4s', '8.9s', '0.5s', '42.0 tokens/s', '≥$0.12'])
      expect(screen.getByText(text)).toBeTruthy();
    for (const title of sections)
      expect(screen.getByRole('heading', { name: title, exact: true })).toBeTruthy();
    const table = screen.getByRole('region', { name: 'Model usage' });
    expect(within(table).getByRole('rowheader', { name: /shared-model/ })).toBeTruthy();
    expect(within(table).getByText('$0.02')).toBeTruthy();
    expect(screen.queryByText('Prompt-bearing agent title')).toBeNull();
    fireEvent.click(screen.getByText('Refresh statistics'));
    expect(store.endpoints.sessionStats).toHaveBeenCalledTimes(2);
  });
  it('keeps durable history separate and does not invent historical cost or active time', async () => {
    fixture({ ...runtime, durable_metrics: { ...metrics, input_tokens: 9999 } });
    await screen.findByText('Recorded parent-session totals');
    expect(screen.getByText('10K')).toBeTruthy();
    expect(screen.getByText(/do not add/)).toBeTruthy();
  });
  it('prices child sessions and omits only the columns they never record', async () => {
    fixture({ metrics, scope: 'durable_history', models: [], sections: [] }, false, [
      { id: 'a', cost: 1.5 },
      { id: 'b', cost: 2.25 },
    ]);
    await screen.findByText('Recorded history');
    const table = screen.getByRole('region', { name: 'Child session usage' });
    expect(within(table).getAllByRole('rowheader')).toHaveLength(1);
    expect(within(table).getByText('2.5K')).toBeTruthy();
    // Delegated spend is real money and is reported per model.
    expect(within(table).getByText('$3.75')).toBeTruthy();
    // Children keep no clock, so those columns are absent rather than dashed.
    expect(within(table).queryAllByText('—')).toHaveLength(0);
    for (const column of ['Time', 'Tools'])
      expect(within(table).queryByRole('columnheader', { name: column })).toBeNull();
    expect(screen.queryByText('3.0s')).toBeNull();
  });
  it("reports the session's own recorded model, not only its children", async () => {
    fixture({
      metrics,
      scope: 'durable_history',
      models: [
        {
          ...metrics,
          input_tokens: 5720,
          cached_input_tokens: 35795091,
          model: 'opus',
          kinds: ['main', 'subagent'],
        },
      ],
      sections: [],
    });
    await screen.findByText('Recorded history');
    const models = screen.getByRole('region', { name: 'Model usage' });
    expect(
      within(models).getByRole('rowheader', { name: /opus\s*\(session, delegated\)/ }),
    ).toBeTruthy();
    const cacheRead = within(models).getByText('35.8M');
    expect(cacheRead.getAttribute('title')).toBe('35,795,091');
    // Recorded history retains no request boundaries, so it must not invent a price.
    expect(within(models).getAllByText('—')).toHaveLength(3);
    // Child counters stay in their own table so delegated tokens are not doubled.
    expect(screen.getByRole('region', { name: 'Child session usage' })).toBeTruthy();
  });
  it('explains why an old session has no per-model rows', async () => {
    fixture({ metrics, scope: 'durable_history', models: [], sections: [] });
    expect(await screen.findByText(/No recorded per-model usage/)).toBeTruthy();
    expect(screen.getByText(/only combined totals exist/)).toBeTruthy();
  });
  it('surfaces recorded tokens that no model row claims', async () => {
    fixture({
      metrics,
      scope: 'durable_history',
      models: [],
      sections: [],
      unattributed: { ...metrics, input_tokens: 955470 },
    });
    const heading = await screen.findByText('Not attributed to a model');
    const section = heading.closest('section');
    expect(section).toBeTruthy();
    expect(within(section!).getByText('955.5K')).toBeTruthy();
    // Turn and tool counts carry no model provenance, so they are not shown as
    // an observed zero.
    expect(within(section!).queryByText('Assistant turns')).toBeNull();
    expect(within(section!).getByText(/already in the session totals/)).toBeTruthy();
  });
  it('drops headline tiles a session cannot report instead of showing dashes', async () => {
    fixture({ metrics, scope: 'durable_history', models: [], sections: [], cost_usd: 12.5 });
    await screen.findByText('Recorded history');
    const terms = screen.queryAllByRole('term').map((node) => node.textContent);
    for (const label of ['Active', 'Model', 'Tools', 'Time to first token', 'Output speed'])
      expect(terms).not.toContain(label);
    // Cost spans every runtime the session ever had, so it is still reported.
    expect(screen.getByText('Est. cost (session)')).toBeTruthy();
    expect(screen.getByText('$12.50')).toBeTruthy();
  });
  it('abbreviates every count and keeps the exact value on hover', async () => {
    fixture({
      ...runtime,
      metrics: { ...metrics, input_tokens: 1234, cached_input_tokens: 79960407 },
      models: [{ ...metrics, model: 'opus', cached_input_tokens: 79960407, cost_usd: 0.004 }],
      sections: [
        {
          title: 'Current Context / Window Pressure',
          rows: [
            { label: 'Current context', value: '173.2K', exact: '173,217' },
            { label: 'Context source', value: 'message estimate' },
          ],
        },
      ],
    });
    const context = await screen.findByText('173.2K');
    expect(context.getAttribute('title')).toBe('173,217');
    // A row that is not a count keeps its text and gains no title.
    expect(screen.getByText('message estimate').getAttribute('title')).toBeNull();
    const table = screen.getByRole('region', { name: 'Model usage' });
    const cacheRead = within(table).getByText('80M');
    expect(cacheRead.getAttribute('title')).toBe('79,960,407');
    // Sub-cent spend is still spend, but fractions of a cent do not help.
    expect(within(table).getByText('<$0.01')).toBeTruthy();
  });
  it('opens without stealing focus for a refresh nobody asked for', async () => {
    fixture();
    await screen.findByText('31.3s');
    const refresh = screen.getByText('Refresh statistics');
    await waitFor(() =>
      expect((document.activeElement as HTMLElement | null)?.getAttribute('role')).toBe('dialog'),
    );
    expect(document.activeElement).not.toBe(refresh);
  });
  it('reads long work times as minutes and hours, exact on hover', async () => {
    fixture({
      ...runtime,
      active_ms: 3_977_300,
      model_ms: 2_247_900,
      tool_ms: 442_000,
      ttft_ms: 400,
    });
    const active = await screen.findByText('1h06m');
    expect(active.getAttribute('title')).toBe('3977.3s');
    expect(screen.getByText('37m27s')).toBeTruthy();
    expect(screen.getByText('7m22s')).toBeTruthy();
    // Sub-second times keep their tenth: 0s would hide a fast first token.
    expect(screen.getByText('0.4s')).toBeTruthy();
  });
  it('shows a dash, not zero, for a child nobody could price', async () => {
    fixture({ metrics, scope: 'durable_history', models: [], sections: [] }, false, [
      { id: 'unpriced' },
    ]);
    await screen.findByText('Recorded history');
    const table = screen.getByRole('region', { name: 'Child session usage' });
    expect(within(table).getByText('—')).toBeTruthy();
    expect(within(table).queryByText('$0.00')).toBeNull();
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
    await screen.findByText(/No observed per-model usage\./);
    expect(screen.queryByText('NaN')).toBeNull();
    // Missing counters render as an omitted section, not a block of zeros.
    expect(screen.queryByText('Token usage')).toBeNull();
    expect(screen.queryByText('0')).toBeNull();
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
