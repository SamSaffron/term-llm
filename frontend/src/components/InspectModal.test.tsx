import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import type { InspectBlock, SessionInspect, SessionInspectBodies } from '../domain/inspect';
import * as clipboard from '../platform/clipboard';
import { InspectModal } from './InspectModal';

const block = (
  id: string,
  kind: InspectBlock['kind'],
  extra: Partial<InspectBlock> = {},
): InspectBlock => ({
  id,
  kind,
  title: id,
  preview: `Preview ${id}`,
  total_bytes: 20,
  total_lines: 2,
  has_body: true,
  ...extra,
});
const blocks = [
  block('m1.p0.system', 'system'),
  block('m2.p0.user', 'user', { message_id: '2', sequence: 2 }),
  block('m3.p0.reasoning', 'reasoning', { reasoning_kind: 'raw' }),
  block('m3.p1.reasoning', 'reasoning', {
    reasoning_kind: 'encrypted',
    encrypted_bytes: 47,
    has_body: false,
    preview: undefined,
  }),
  block('m4.p0.tool_call', 'tool_call'),
  block('m5.p0.tool_result', 'tool_result', {
    tool: { name: 'shell', is_error: true },
    child_session_ids: ['child'],
  }),
  block('m6.p0.compaction_summary', 'compaction_summary', {
    compaction: { ordinal: 1, active_boundary: true, tail_count: 1 },
  }),
  block('m7.p0.assistant_text', 'assistant_text', {
    compaction_tail: true,
    compaction_summary_id: 'm6.p0.compaction_summary',
  }),
];
const snapshot = (sessionId = 's1', rows = blocks): SessionInspect => ({
  schema_version: 1,
  session_id: sessionId,
  transcript_rev: 42,
  scope: 'persisted_history',
  header: {
    initial_model: { provider: 'openai', model: 'gpt-5', effort: 'high', agent: 'developer' },
    current_model: { provider: 'openai', model: 'gpt-5', effort: 'high', agent: 'developer' },
    provider: 'openai',
    model: 'gpt-5',
    reasoning_effort: 'high',
    agent: 'developer',
    compaction_count: 1,
    compaction_seq: 6,
    runtime: { loaded: false, unavailable_reason: 'runtime_not_loaded' },
  },
  blocks: rows,
});
const stores: AppStore[] = [];
afterEach(() => {
  for (const store of stores.splice(0)) store.dispose();
});

function fixture(data = snapshot()) {
  const store = new AppStore(testConfig);
  stores.push(store);
  store.sessions.value = [testSession(), testSession({ id: 's2' })];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.modal.value = 'inspect';
  store.endpoints.sessionInspect = vi.fn(async (id) => ({ ...data, session_id: id }));
  store.endpoints.sessionInspectBodies = vi.fn(async (_id: string, ids: string[]) => ({
    transcript_rev: 42,
    bodies: ids.map((id) => ({ id, format: 'text' as const, text: `Full ${id}`, total_bytes: 20 })),
    missing: [],
    deferred: [],
  }));
  const Surface = () => (store.modal.value === 'inspect' ? <InspectModal /> : null);
  const view = render(
    <StoreContext.Provider value={store}>
      <Surface />
    </StoreContext.Provider>,
  );
  return { store, ...view };
}
const expand = (id: string) =>
  fireEvent.click(screen.getByRole('button', { name: new RegExp(id.replaceAll('.', '\\.')) }));

describe('web inspect', () => {
  it('shows initial and current models prominently before the controls', async () => {
    const data = snapshot();
    data.header.initial_model = {
      provider: 'claude-bin',
      model: 'opus',
      effort: 'max',
      agent: 'source',
    };
    data.header.current_model = { ...data.header.current_model, reasoning_mode: 'pro' };
    const { container } = fixture(data);
    const panel = await screen.findByRole('region', { name: 'Model' });
    expect(panel).toHaveTextContent('Started with: claude-bin:opus · Thinking: max · @source');
    expect(panel).toHaveTextContent('Now: openai:gpt-5 · Thinking: high · pro · @developer');
    expect(
      container.querySelector('.modal > .inspect-model-panel')?.nextElementSibling,
    ).toHaveClass('inspect-toolbar');
  });

  it('shows defaults and unknown values without inventing history, and omits unchanged Now', async () => {
    const data = snapshot();
    data.header.initial_model = {};
    data.header.current_model = {};
    fixture(data);
    const panel = await screen.findByRole('region', { name: 'Model' });
    expect(panel).toHaveTextContent('Started with: unknown:unknown · Thinking: default');
    expect(within(panel).queryByText(/^Now:/)).toBeNull();
  });

  it.each(['succeeded', 'failed', 'started'])(
    'keeps %s model dividers visible and uncollapsed under every filter',
    async (status) => {
      const data = snapshot('s1', [
        block('m2.p0.model_change', 'model_change', {
          title: 'Model change point',
          compaction_tail: true,
          model_change: {
            from: { provider: 'openai', model: 'gpt-5', effort: 'high' },
            to: { provider: 'claude-bin', model: 'opus', effort: 'max' },
            strategy: 'handover',
            status,
          },
        }),
      ]);
      const { store } = fixture(data);
      const divider = await screen.findByRole('separator', { name: 'Model change point' });
      expect(divider).toHaveTextContent(
        'Model changed (handover): openai:gpt-5 · Thinking: high → claude-bin:opus · Thinking: max',
      );
      for (const name of ['System & developer', 'Reasoning', 'Tools', 'Compaction']) {
        fireEvent.click(screen.getByRole('button', { name, exact: true }));
        expect(divider).toBeVisible();
      }
      expect(within(divider).queryByRole('button')).toBeNull();
      expect(store.endpoints.sessionInspectBodies).not.toHaveBeenCalled();
      if (status === 'failed') {
        expect(divider).toHaveClass('inspect-model-warning');
        expect(divider).toHaveTextContent('failed; restored openai:gpt-5 · Thinking: high');
      }
    },
  );

  it('labels the outgoing model document and synthetic acknowledgement', async () => {
    fixture(
      snapshot('s1', [
        block('m1.p0.handover_document', 'handover_document', {
          title: 'Handover @dev → @reviewer',
          handover: { source: 'dev', target: 'reviewer' },
        }),
        block('m2.p0.assistant_text', 'assistant_text', {
          title: 'Synthetic acknowledgement',
          synthetic: true,
        }),
      ]),
    );
    expect(await screen.findByText('Handover @dev → @reviewer')).toBeVisible();
    expect(screen.getByText('Written by the outgoing model')).toBeVisible();
    expect(screen.getByText('Synthetic — not model output')).toBeVisible();
  });

  it('shows the active model after hidden earlier changes and updates it when revealed', async () => {
    const data = snapshot('s1', [
      block('m1.p0.model_change', 'model_change', {
        model_change: {
          from: { model: 'old' },
          to: { provider: 'claude-bin', model: 'opus', effort: 'max' },
          status: 'succeeded',
        },
      }),
      ...Array.from({ length: 220 }, (_, i) => block(`m${i + 2}.p0.user`, 'user')),
    ]);
    fixture(data);
    expect(
      await screen.findByText('Active model here: claude-bin:opus · Thinking: max'),
    ).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: 'Show 21 earlier (21 hidden)' }));
    expect(screen.queryByText(/^Active model here:/)).toBeNull();
    expect(screen.getByText(/Model changed:/)).toBeVisible();
  });

  it('opens a read-only snapshot with provenance, filters, turn separators and close', async () => {
    const { store, container } = fixture();
    expect(await screen.findByText('Transcript blocks')).toBeVisible();
    expect(screen.getByText(/Persisted history/)).not.toHaveTextContent('Effort:');
    expect(screen.getByText('Tool definitions unavailable: runtime not loaded.')).toBeVisible();
    expect(screen.getByRole('separator', { name: 'User turn' })).toBeVisible();
    expect(screen.getByText(/Compaction 1 · active boundary/)).toBeVisible();
    expect(container.querySelector('.inspect-failed')).toHaveTextContent('failed');
    expect(container.querySelector('.inspect-tail')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Reasoning', exact: true }));
    expect(screen.queryByText('Preview m3.p0.reasoning')).toBeNull();
    expect(screen.getByText('Preview m2.p0.user')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: 'System & developer' }));
    expect(screen.queryByText('Preview m1.p0.system')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Tools', exact: true }));
    expect(screen.queryByText('Preview m4.p0.tool_call')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Compaction', exact: true }));
    expect(screen.getByText('1 of 8 blocks')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: 'Close Inspect' }));
    expect(store.modal.value).toBe('');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(vi.mocked(store.endpoints.sessionInspect).mock.calls[0][1]?.aborted).toBe(true);
  });

  it('batches expansions, renders only escaped preformatted text, and copies the body', async () => {
    const { store, container } = fixture();
    const copy = vi.spyOn(clipboard, 'copyText').mockResolvedValue(undefined);
    store.endpoints.sessionInspectBodies = vi.fn(async (_id: string, ids: string[]) => ({
      transcript_rev: 42,
      bodies: ids.map((id) => ({
        id,
        format: 'json' as const,
        text: '<img src=x onerror=alert(1)>',
        total_bytes: 20,
      })),
    }));
    await screen.findByText('Transcript blocks');
    expand('m1.p0.system');
    expand('m4.p0.tool_call');
    await waitFor(() => expect(container.querySelectorAll('pre')).toHaveLength(2));
    expect(store.endpoints.sessionInspectBodies).toHaveBeenCalledTimes(1);
    expect(store.endpoints.sessionInspectBodies).toHaveBeenCalledWith(
      's1',
      ['m1.p0.system', 'm4.p0.tool_call'],
      expect.any(AbortSignal),
    );
    expect(container.querySelector('img')).toBeNull();
    expect(screen.getByRole('region', { name: 'm1.p0.system body' })).toHaveTextContent(
      '<img src=x onerror=alert(1)>',
    );
    fireEvent.click(screen.getByRole('button', { name: 'Copy m1.p0.system' }));
    await waitFor(() => expect(copy).toHaveBeenCalledWith('<img src=x onerror=alert(1)>'));
  });

  it('stays within 64 IDs and reduces deferred batches to fit the response budget', async () => {
    const rows = Array.from({ length: 68 }, (_, index) => block(`m${index}.p0.user`, 'user'));
    const { store } = fixture(snapshot('s1', rows));
    store.endpoints.sessionInspectBodies = vi.fn(async (_id: string, ids: string[]) => ({
      transcript_rev: 42,
      bodies: ids
        .slice(0, 8)
        .map((id) => ({ id, format: 'text' as const, text: `Full ${id}`, total_bytes: 20 })),
      deferred: ids.slice(8),
    }));
    await screen.findByText('Transcript blocks');
    for (const button of screen
      .getAllByRole('button')
      .filter((button) => button.hasAttribute('aria-expanded')))
      fireEvent.click(button);
    await waitFor(() => expect(screen.getAllByText(/^Full m/)).toHaveLength(68));
    const calls = vi.mocked(store.endpoints.sessionInspectBodies).mock.calls;
    expect(calls[0][1]).toHaveLength(64);
    expect(calls.slice(1).every((call) => call[1].length <= 4)).toBe(true);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('never offers encrypted reasoning for expansion or requests it', async () => {
    const { store } = fixture();
    expect(
      await screen.findByText('Encrypted reasoning (47 bytes); plaintext unavailable.'),
    ).toBeVisible();
    expect(screen.queryByRole('button', { name: /m3.p1.reasoning/ })).toBeNull();
    expect(store.endpoints.sessionInspectBodies).not.toHaveBeenCalled();
  });

  it('re-requests deferred bodies, handles missing ones, retries, and labels truncation', async () => {
    const { store } = fixture();
    store.endpoints.sessionInspectBodies = vi
      .fn()
      .mockResolvedValueOnce({ transcript_rev: 42, bodies: [], deferred: ['m1.p0.system'] })
      .mockResolvedValueOnce({ transcript_rev: 42, bodies: [], missing: ['m1.p0.system'] })
      .mockResolvedValueOnce({
        transcript_rev: 42,
        bodies: [
          {
            id: 'm1.p0.system',
            format: 'text',
            text: 'excerpt',
            truncated: true,
            total_bytes: 500000,
          },
        ],
      });
    await screen.findByText('Transcript blocks');
    expand('m1.p0.system');
    expect(await screen.findByText(/Body unavailable/)).toBeVisible();
    expect(store.endpoints.sessionInspectBodies).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole('button', { name: 'Retry body' }));
    expect(await screen.findByText('excerpt')).toBeVisible();
    expect(screen.getByText(/Truncated:.*500000 bytes/)).toHaveTextContent(
      'Copy copies only this excerpt.',
    );
  });

  it('keeps expansion across Refresh while refreshing cached bodies', async () => {
    const { store } = fixture();
    await screen.findByText('Transcript blocks');
    expand('m1.p0.system');
    await screen.findByText('Full m1.p0.system');
    store.endpoints.sessionInspectBodies = vi.fn(async () => ({
      transcript_rev: 43,
      bodies: [
        { id: 'm1.p0.system', format: 'text' as const, text: 'Updated body', total_bytes: 20 },
      ],
    }));
    fireEvent.click(screen.getByRole('button', { name: 'Refresh', exact: true }));
    expect(await screen.findByText('Updated body')).toBeVisible();
    expect(screen.getByRole('button', { name: /m1.p0.system.*bytes/ })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    expect(store.endpoints.sessionInspect).toHaveBeenCalledTimes(2);
  });

  it('pushes a child session, offers back, and does not change the active chat', async () => {
    const { store } = fixture();
    await screen.findByText('Transcript blocks');
    const childLink = screen.getByRole('button', { name: 'Inspect child session child' });
    childLink.focus();
    fireEvent.click(childLink);
    expect(await screen.findByText(/Session: child/)).toBeVisible();
    expect(screen.getByRole('dialog', { name: 'Inspect' })).toHaveFocus();
    await waitFor(() =>
      expect(store.endpoints.sessionInspect).toHaveBeenCalledWith('child', expect.any(AbortSignal)),
    );
    expect(store.activeSessionId.value).toBe('s1');
    const back = screen.getByRole('button', { name: 'Back to s1' });
    back.focus();
    fireEvent.click(back);
    expect(await screen.findByText(/Session: s1/)).toBeVisible();
    expect(screen.getByRole('dialog', { name: 'Inspect' })).toHaveFocus();
    expect(screen.queryByRole('button', { name: /^Back to/ })).toBeNull();
  });

  it('shows current runtime schemas, prompt comparison and discovery diagnostics honestly', async () => {
    const data = snapshot();
    data.scope = 'runtime_history';
    data.header.runtime = {
      loaded: true,
      system_prompt_block_id: 'runtime.system_prompt',
      system_prompt_matches_persisted: false,
      tools: [
        {
          block_id: 'runtime.tool.0',
          name: 'shell',
          description: 'Run commands',
          schema_bytes: 120,
        },
      ],
      tool_discovery: { ConfiguredMode: 'dynamic', ActiveMCPCount: 2, Recent: [{ Name: 'shell' }] },
    };
    const { store } = fixture(data);
    expect(
      await screen.findByText('Current runtime definitions; historical turns may have differed.'),
    ).toBeVisible();
    expect(screen.getByText('Current system prompt differs from persisted.')).toBeVisible();
    expect(screen.getByText(/Runtime history \(not persisted\)/)).toBeVisible();
    const diagnostics = screen.getByRole('region', { name: 'Tool discovery diagnostics' });
    expect(within(diagnostics).getByText('dynamic')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: /Current system prompt/ }));
    fireEvent.click(screen.getByRole('button', { name: /shell.*120 schema bytes/ }));
    await screen.findByText('Full runtime.tool.0');
    expect(store.endpoints.sessionInspectBodies).toHaveBeenCalledWith(
      's1',
      ['runtime.system_prompt', 'runtime.tool.0'],
      expect.any(AbortSignal),
    );
  });

  it('reports a busy runtime as unavailable rather than tool-less', async () => {
    const data = snapshot();
    data.header.runtime = { loaded: true, unavailable_reason: 'runtime_busy' };
    fixture(data);
    expect(await screen.findByText('Tool definitions unavailable: runtime busy.')).toBeVisible();
    expect(screen.queryByText('No current tool definitions.')).toBeNull();
    expect(screen.queryByText(/Current runtime definitions/)).toBeNull();
  });

  it('windows large sessions, starts at the latest block, and shows earlier on demand', async () => {
    const scroll = vi.fn();
    const descriptor = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
    Object.defineProperty(Element.prototype, 'scrollIntoView', {
      configurable: true,
      value: scroll,
    });
    try {
      const rows = Array.from({ length: 250 }, (_, index) => block(`m${index}.p0.user`, 'user'));
      fixture(snapshot('s1', rows));
      await screen.findByText('Preview m249.p0.user');
      expect(screen.queryByText('Preview m0.p0.user')).toBeNull();
      expect(screen.getAllByRole('article')).toHaveLength(200);
      await waitFor(() => expect(scroll).toHaveBeenCalledWith({ block: 'end' }));
      fireEvent.click(screen.getByRole('button', { name: 'Show 50 earlier (50 hidden)' }));
      expect(screen.getByText('Preview m0.p0.user')).toBeVisible();
      expect(screen.getAllByRole('article')).toHaveLength(250);
    } finally {
      if (descriptor) Object.defineProperty(Element.prototype, 'scrollIntoView', descriptor);
      else Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
    }
  });

  it('aborts pending bodies and starts session-scoped state on active session switch', async () => {
    const { store } = fixture();
    store.endpoints.sessionInspectBodies = vi.fn(() => new Promise<SessionInspectBodies>(() => {}));
    await screen.findByText('Transcript blocks');
    expand('m1.p0.system');
    await waitFor(() => expect(store.endpoints.sessionInspectBodies).toHaveBeenCalledOnce());
    const signal = vi.mocked(store.endpoints.sessionInspectBodies).mock.calls[0][2];
    act(() => {
      store.activeSessionId.value = 's2';
    });
    await screen.findByText(/Session: s2/);
    expect(signal?.aborted).toBe(true);
    expect(screen.getByRole('button', { name: /m1.p0.system.*bytes/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
  });

  it('shows request failures and allows Refresh to recover', async () => {
    const { store } = fixture();
    await screen.findByText('Transcript blocks');
    store.endpoints.sessionInspect = vi
      .fn()
      .mockRejectedValueOnce(new Error('Inspection failed'))
      .mockResolvedValue(snapshot());
    fireEvent.click(screen.getByRole('button', { name: 'Refresh', exact: true }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Inspection failed');
    fireEvent.click(screen.getByRole('button', { name: 'Refresh', exact: true }));
    expect(await screen.findByText('Transcript blocks')).toBeVisible();
  });
});
