import { fireEvent, render, screen, waitFor, within } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { activityCell, activityChild, activitySnapshot } from '../test/activity-fixtures';
import { ActivityGlyph, ActivityMeter } from './ActivityMeter';
import { ActivitySheet } from './ActivitySheet';

let store: AppStore;
afterEach(() => {
  store?.dispose();
  vi.restoreAllMocks();
});
function setup() {
  store = new AppStore(testConfig);
  store.childSessionStore.activitySessionId.value = 'parent';
  store.childSessionStore.activity.value = activitySnapshot();
}

function mount(children: preact.ComponentChildren) {
  return render(<StoreContext.Provider value={store}>{children}</StoreContext.Provider>);
}

describe('native ActivityMeter', () => {
  it.each([true, false])('hides script-only activity when scripts are live=%s', (live) => {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [],
      cells: [activityCell({ live, status: live ? 'running' : 'completed' })],
    });
    mount(<ActivityMeter />);
    expect(screen.queryByRole('button', { name: /^Activity:/ })).toBeNull();
  });

  it('counts and shows only agents and jobs without fetching script controls', async () => {
    setup();
    const scripts = vi.spyOn(store.endpoints, 'scriptCells');
    store.childSessionStore.activity.value = activitySnapshot({
      cells: [
        activityCell(),
        activityCell({ cell_id: 'cm_done', live: false, status: 'completed' }),
      ],
      jobs: [{ job_id: 'job', run_id: 'run', title: 'Report', status: 'running' }],
    });
    mount(<ActivityMeter />);
    fireEvent.click(screen.getByRole('button', { name: 'Activity: 2 active' }));
    const dialog = await screen.findByRole('dialog', { name: 'Activity' });
    const agents = within(dialog).getByRole('region', { name: 'Subagents' });
    const jobs = within(dialog).getByRole('region', { name: 'Jobs' });
    expect(within(agents).getByRole('heading')).toHaveTextContent(/^Subagents, 1 active$/);
    expect(within(jobs).getByRole('heading')).toHaveTextContent(/^Jobs, 1 active$/);
    expect(within(dialog).queryByRole('region', { name: 'Scripts' })).toBeNull();
    expect(dialog).not.toHaveTextContent('cm_script');
    expect(dialog).not.toHaveTextContent('cm_done');
    expect(dialog).not.toHaveTextContent('Usage & timing');
    expect(dialog).not.toHaveTextContent('Tokens include');
    expect(scripts).not.toHaveBeenCalled();
  });

  it('keeps bounded-history chats hidden without known activity and terminal history idle', async () => {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({ agents: [], jobs_partial: true });
    mount(<ActivityMeter />);
    expect(screen.queryByRole('button', { name: /^Activity:/ })).toBeNull();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [activityChild({ live: false, state: 'complete' })],
      jobs_partial: true,
    });
    const trigger = await screen.findByRole('button', { name: /^Activity: idle/ });
    expect(trigger).toHaveAttribute('data-state', 'idle');
    expect(trigger).toHaveAccessibleName(/Activity may be incomplete or out of date/);
    fireEvent.click(trigger);
    expect(await screen.findByText(/bounded snapshot/)).toBeVisible();
  });

  it('hides pristine activity and shows one grey idle head without a zero or partial dot', () => {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({ agents: [] });
    const { container } = mount(<ActivityMeter />);
    expect(screen.queryByRole('button', { name: /^Activity:/ })).toBeNull();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [activityChild({ live: false, state: 'complete' })],
      agents_partial: false,
    });
    return waitFor(() => {
      const button = screen.getByRole('button', { name: 'Activity: idle' });
      expect(button).toHaveAttribute('data-state', 'idle');
      expect(button.querySelectorAll('g')).toHaveLength(1);
      expect(button.querySelector('.activity-count')).toBeNull();
      expect(container.querySelector('.activity-stale')).toBeNull();
    });
  });

  it.each([
    [0, 1, 'translate(4.04 1.57)'],
    [1, 1, 'translate(4.04 1.57)'],
    [2, 2, 'translate(2.54 0.57)'],
    [3, 3, 'translate(1.04 -0.43)'],
    [9, 3, 'translate(1.04 -0.43)'],
  ])('centres %i active with %i heads', (count, heads, transform) => {
    const { container } = render(<ActivityGlyph count={count as number} />);
    expect(container.querySelectorAll('g')).toHaveLength(heads as number);
    expect(container.querySelector('g:last-child')?.getAttribute('transform')).toBe(
      `${transform} scale(.58)`,
    );
    expect(container.querySelector('svg')).toHaveAttribute('viewBox', '0 0 22 16');
  });

  it('shows one uncapped active count and opens the lazy accessible sheet with focus restoration', async () => {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: Array.from({ length: 7 }, (_, i) => activityChild({ session_id: `c${i}` })),
    });
    const { container } = mount(
      <header>
        <ActivityMeter />
      </header>,
    );
    const trigger = screen.getByRole('button', { name: 'Activity: 7 active' });
    expect(trigger.querySelectorAll('g')).toHaveLength(3);
    expect(trigger.querySelectorAll('.activity-count')).toHaveLength(1);
    expect(trigger).toHaveTextContent('7');
    expect(trigger.style.color).toBe(''); // Active foreground inherits the native theme.
    expect(trigger.querySelector('g')).toHaveAttribute('fill', 'var(--bg)');
    trigger.focus();
    fireEvent.click(trigger);
    const dialog = await screen.findByRole('dialog', { name: 'Activity' });
    expect(container.contains(dialog)).toBe(false); // Escape the header stacking context.
    expect(within(dialog).getAllByRole('img', { name: 'Running' })).toHaveLength(7);
    fireEvent.keyDown(dialog, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(trigger).toHaveFocus();
  });
});

describe('native ActivitySheet', () => {
  it('shows all active and finished rows, actual models, accessible states and unavailable coverage', () => {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [
        activityChild({ state: 'error', live: false, title: 'Failed work' }),
        activityChild({ session_id: 'waiting', pending_asks: 1, title: 'Review work' }),
      ],
      jobs: [
        {
          job_id: 'j',
          run_id: 'r',
          title: 'Remote report',
          status: 'complete',
          model: 'actual-job-model',
        },
      ],
      jobs_availability: 'unavailable',
      agents_partial: true,
    });
    const { container } = mount(<ActivitySheet onClose={vi.fn()} />);
    expect(
      [...container.querySelectorAll('.activity-label')].map((node) => node.textContent),
    ).toEqual(['developer Review work', 'developer Failed work', 'Remote report']);
    expect(screen.getByRole('img', { name: 'Waiting' })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: 'Failed' })).toBeInTheDocument();
    expect(screen.getByText('actual-job-model · Job')).toBeInTheDocument();
    expect(screen.getByText(/Job activity is unavailable/)).toBeInTheDocument();
    expect(screen.getByText(/bounded snapshot/)).toBeInTheDocument();
    expect(container.querySelector('details')).toBeNull();
    expect(screen.getByRole('button', { name: /Remote report/ })).toBeInTheDocument();
    expect(screen.queryByText(/Advanced/)).toBeNull();
  });

  it("heads the list with this chat's agent and counts each section without a summary sentence", () => {
    setup();
    store.sessions.value = [
      testSession({
        id: 'parent',
        agent: 'developer',
        activeProvider: 'claude-bin',
        activeModel: 'opus-max',
      }),
    ];
    store.activeSessionId.value = 'parent';
    store.draftActive.value = false;
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [
        activityChild({ session_id: 'a', agent: 'codebase', title: 'Find the agent' }),
        activityChild({
          session_id: 'b',
          live: false,
          state: 'complete',
          title: 'You are a worker',
        }),
      ],
    });
    const { container } = mount(<ActivitySheet onClose={vi.fn()} />);
    const root = container.querySelector('.activity-root');
    expect(root).toHaveTextContent('developer this chat');
    expect(root).toHaveTextContent('claude-bin:opus-max');
    expect(screen.getByRole('heading', { name: /Subagents/ })).toHaveTextContent(
      'Subagents, 1 active · 1 done',
    );
    expect(
      [...container.querySelectorAll('.activity-row .activity-label')].map(
        (node) => node.textContent,
      ),
    ).toEqual(['codebase Find the agent', 'developer no summary']);
    expect(container).not.toHaveTextContent(/All finished|children|Developer task/);
    store.draftActive.value = true;
    return waitFor(() => expect(container.querySelector('.activity-root')).toBeNull());
  });

  it('uses native selection only for trusted local sessions from job details', async () => {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [],
      jobs: [
        {
          job_id: 'j',
          run_id: 'r',
          title: 'Local report',
          status: 'running',
          session_id: 'trusted',
        },
      ],
    });
    vi.spyOn(store.endpoints, 'sessionActivityRun').mockResolvedValue({
      job_id: 'j',
      run_id: 'r',
      title: 'Local report',
      status: 'running',
      session_id: 'trusted',
      truncated: false,
    });
    const select = vi.spyOn(store, 'resolveAndSelectSession').mockResolvedValue(null);
    const close = vi.fn();
    mount(<ActivitySheet onClose={close} />);
    fireEvent.click(screen.getByRole('button', { name: /Local report/ }));
    fireEvent.click(await screen.findByRole('button', { name: 'Open conversation' }));
    expect(close).toHaveBeenCalledOnce();
    expect(select).toHaveBeenCalledWith('trusted', false, { newChatOnMiss: false });
  });
});

describe('uncertain activity accessibility', () => {
  it('does not claim idle, finished, or no history on a failed first snapshot or incomplete idle snapshot', async () => {
    setup();
    store.childSessionStore.activity.value = null;
    store.childSessionStore.activityStale.value = true;
    mount(<ActivityMeter />);
    const trigger = screen.getByRole('button', {
      name: /Activity: Status unknown.*incomplete or out of date/,
    });
    expect(trigger.querySelector('.activity-count')).toBeNull();
    fireEvent.click(trigger);
    const dialog = await screen.findByRole('dialog', { name: 'Activity' });
    expect(dialog).not.toHaveTextContent('All finished');
    expect(dialog).not.toHaveTextContent('No related activity in this snapshot');
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [activityChild({ live: false, state: 'complete' })],
      jobs_partial: true,
    });
    await waitFor(() => expect(trigger).toHaveAttribute('data-state', 'unknown'));
    expect(trigger).not.toHaveAccessibleName('Activity: idle');
  });
});

describe('scoped job drilldown lifecycle', () => {
  function jobSetup() {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [],
      jobs: [{ job_id: 'j', run_id: 'run', title: 'Report run', status: 'running' }],
    });
  }

  it('loads the exact scoped run, renders output as safe text, and restores row focus on Back without remote navigation', async () => {
    jobSetup();
    const request = vi.spyOn(store.endpoints, 'sessionActivityRun').mockResolvedValue({
      job_id: 'j',
      run_id: 'run',
      title: 'Report run',
      status: 'complete',
      response: '<script>bad()</script>',
      stdout: '<b>plain output</b>',
      stderr: 'stderr output',
      error: 'run error',
      truncated: true,
    });
    const { container } = mount(<ActivitySheet onClose={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: /Report run/ }));
    expect(await screen.findByText('<script>bad()</script>')).toBeInTheDocument();
    expect(request).toHaveBeenCalledWith('parent', 'run', expect.any(AbortSignal));
    expect(container.querySelector('.activity-output script')).toBeNull();
    expect(container.querySelector('.activity-output b')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Open conversation' })).toBeNull();
    expect(screen.getByText(/Output was truncated/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Back to activity' }));
    expect(screen.getByRole('button', { name: /Report run/ })).toHaveFocus();
  });

  it('cancels old detail generations on Back, scope change and close, ignoring late output', async () => {
    jobSetup();
    const pending: Array<{
      signal: AbortSignal;
      resolve: (detail: import('../api/endpoints').ActivityJobDetail) => void;
    }> = [];
    vi.spyOn(store.endpoints, 'sessionActivityRun').mockImplementation(
      (_scope, _run, signal) =>
        new Promise((resolve) => {
          pending.push({ signal: signal!, resolve });
        }),
    );
    const { unmount } = mount(<ActivitySheet onClose={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: /Report run/ }));
    await waitFor(() => expect(pending).toHaveLength(1));
    fireEvent.click(screen.getByRole('button', { name: 'Back to activity' }));
    expect(pending[0].signal.aborted).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: /Report run/ }));
    await waitFor(() => expect(pending).toHaveLength(2));
    pending[0].resolve({
      job_id: 'j',
      run_id: 'run',
      title: 'Report run',
      status: 'complete',
      response: 'Obsolete output',
      truncated: false,
    });
    await Promise.resolve();
    expect(screen.queryByText('Obsolete output')).toBeNull();
    store.childSessionStore.activitySessionId.value = 'other';
    await waitFor(() => expect(pending[1].signal.aborted).toBe(true));
    fireEvent.click(screen.getByRole('button', { name: /Report run/ }));
    await waitFor(() => expect(pending).toHaveLength(3));
    unmount();
    await waitFor(() => expect(pending[2].signal.aborted).toBe(true));
  });

  it('keeps Back available after a forbidden or missing scoped detail response', async () => {
    jobSetup();
    vi.spyOn(store.endpoints, 'sessionActivityRun').mockRejectedValue(new Error('403'));
    mount(<ActivitySheet onClose={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: /Report run/ }));
    expect(await screen.findByText(/Job details are unavailable/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Back to activity' }));
    expect(screen.getByRole('button', { name: /Report run/ })).toBeInTheDocument();
  });

  it('keeps pristine sessions hidden when optional jobs are unsupported', () => {
    setup();
    store.childSessionStore.activity.value = activitySnapshot({
      agents: [],
      jobs: [],
      jobs_availability: 'unavailable',
    });
    mount(<ActivityMeter />);
    expect(screen.queryByRole('button', { name: /^Activity:/ })).toBeNull();
  });
});
