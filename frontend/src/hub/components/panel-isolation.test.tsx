import { act, render, screen, waitFor } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import type { HubClient } from '../../api/hub-client';
import type { HubConfig } from '../config';
import type { HubAttentionInboxItem, HubDelegation, HubInputRequiredItem } from '../domain/types';
import { HubStore } from '../stores/hub-store';
import { AttentionPanels } from './AttentionPanels';
import { DelegationsPanel } from './DelegationsPanel';

const dashboardConfig: HubConfig = {
  page: 'dashboard',
  authMode: 'none',
  basePath: '/hub',
  canAddNodes: true,
  passkeyAuth: false,
  invalidToken: false,
  formAction: '/hub/',
};

/** A node payload that names no interaction kind: reading it throws while the row renders. */
const malformedKinds = [null] as unknown as string[];

/** A node payload that returns a record where the row expects response text. */
const malformedResponse = { summary: 'not text' } as unknown as string;

function store() {
  return new HubStore({} as unknown as HubClient);
}

/**
 * Collects the boundary's diagnostics for a failed row and forwards every other
 * console error, so an unexpected failure still reaches the test output.
 */
function boundaryDiagnostics(): string[] {
  const reported: string[] = [];
  const original = console.error;
  vi.spyOn(console, 'error').mockImplementation((...args: unknown[]) => {
    const [first] = args;
    if (typeof first === 'string' && first.startsWith('[hub] ')) {
      reported.push(first);
      return;
    }
    original(...args);
  });
  return reported;
}

function inputItem(overrides: Partial<HubInputRequiredItem> = {}): HubInputRequiredItem {
  return {
    node_id: 'alpha',
    node_name: 'Alpha',
    session_id: 'blocked',
    title: 'Blocked conversation',
    pending_interaction_count: 1,
    pending_interaction_kinds: ['ask_user'],
    resume_path: '/hub/node/alpha/chat/blocked',
    ...overrides,
  };
}

function inboxItem(overrides: Partial<HubAttentionInboxItem> = {}): HubAttentionInboxItem {
  return {
    node_id: 'alpha',
    node_name: 'Alpha',
    session_id: 'ready',
    title: 'Ready conversation',
    outcome: 'succeeded',
    attention_seq: 1,
    resume_path: '/hub/node/alpha/chat/ready',
    ...overrides,
  };
}

function delegation(overrides: Partial<HubDelegation> = {}): HubDelegation {
  return {
    id: 'delegation',
    origin_node: 'origin',
    target_node: 'target',
    status: 'running',
    depth: 1,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    ...overrides,
  };
}

describe('Hub panel row isolation', () => {
  it('keeps the other blocked conversations when one input row cannot be rendered', async () => {
    const value = store();
    const reported = boundaryDiagnostics();
    value.totalInputRequired.value = 2;
    value.inputRequired.value = [
      inputItem({
        session_id: 'broken',
        title: 'Broken conversation',
        pending_interaction_kinds: malformedKinds,
      }),
      inputItem({ session_id: 'healthy', title: 'Healthy conversation' }),
    ];
    const { container } = render(<AttentionPanels store={value} />);

    expect(screen.getByText('Healthy conversation')).toBeVisible();
    expect(screen.queryByText('Broken conversation')).not.toBeInTheDocument();
    expect(container.querySelectorAll('.attention-list .attention-row')).toHaveLength(1);
    // The list keeps a row per conversation: only the broken row's body is replaced.
    expect(container.querySelectorAll('.attention-list > li')).toHaveLength(2);
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent(
      'Conversation could not be displayed. Other Hub data is still available.',
    );
    expect(container.querySelector('.attention-list [role="alert"]')).toBe(alert);
    expect(screen.getByText('Conversations blocked on a question or approval.')).toBeVisible();
    // The row is reported once, for the operator, without a message per poll.
    expect(reported).toHaveLength(1);
    expect(reported[0]).toContain('Conversation');

    // Rebuilding the list from the same payload keeps the row identity, so the
    // failed row is not retried and the message does not flicker.
    await act(() => {
      value.inputRequired.value = [...value.inputRequired.peek()];
    });
    expect(screen.getByRole('alert')).toBeVisible();
    expect(screen.getByText('Healthy conversation')).toBeVisible();
    expect(reported).toHaveLength(1);

    // A corrected row arrives as a new object, so the row renders again.
    await act(() => {
      value.inputRequired.value = [
        inputItem({
          session_id: 'broken',
          title: 'Recovered conversation',
          pending_interaction_kinds: ['approval'],
        }),
        value.inputRequired.peek()[1],
      ];
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByText('Recovered conversation')).toBeVisible();
    expect(screen.getByText(/Approval waiting/)).toBeVisible();
    expect(container.querySelectorAll('.attention-list .attention-row')).toHaveLength(2);
    value.dispose();
  });

  it('keeps the other ready conversations when one inbox row cannot be rendered', async () => {
    const value = store();
    const reported = boundaryDiagnostics();
    value.attentionVerified.value = true;
    value.totalUnseen.value = 2;
    value.inbox.value = [
      inboxItem({ session_id: 'broken', title: 'Broken review' }),
      inboxItem({ session_id: 'healthy', title: 'Healthy review' }),
    ];
    const { container } = render(<AttentionPanels store={value} />);
    expect(screen.getByText('Healthy review')).toBeVisible();

    // An outcome the row cannot read joins the row's meta text, which throws.
    await act(() => {
      value.inbox.value = [
        inboxItem({
          session_id: 'broken',
          title: 'Broken review',
          outcome: Symbol('broken') as unknown as string,
        }),
        value.inbox.peek()[1],
      ];
    });
    expect(container.querySelectorAll('.attention-list > li')).toHaveLength(2);
    expect(screen.queryByText('Broken review')).not.toBeInTheDocument();
    expect(screen.getByText('Healthy review')).toBeVisible();
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Conversation could not be displayed. Other Hub data is still available.',
    );
    expect(screen.getByRole('button', { name: 'Clear all' })).toBeEnabled();
    expect(reported).toHaveLength(1);
    expect(reported[0]).toContain('Conversation');
    value.dispose();
  });

  it('keeps the other delegations when one response cannot be read', async () => {
    const value = store();
    const reported = boundaryDiagnostics();
    value.delegationsVerified.value = true;
    value.delegations.value = [
      delegation({ id: 'broken', origin_node: 'broken-origin', response: malformedResponse }),
      delegation({ id: 'healthy', origin_node: 'healthy-origin' }),
    ];
    const { container } = render(<DelegationsPanel config={dashboardConfig} store={value} />);

    expect(screen.getByText('healthy-origin')).toBeVisible();
    expect(screen.queryByText('broken-origin')).not.toBeInTheDocument();
    expect(container.querySelectorAll('.delegation-row')).toHaveLength(1);
    expect(container.querySelectorAll('.delegations-list > *')).toHaveLength(2);
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent(
      'Delegation could not be displayed. Other Hub data is still available.',
    );
    expect(container.querySelector('.delegations-list [role="alert"]')).toBe(alert);
    expect(screen.getByText('Cross-node work routed through the Hub.')).toBeVisible();
    expect(screen.getByText('2 active · 2 total')).toBeVisible();

    // The corrected response renders the row's artifact and full text.
    await act(() => {
      value.delegations.value = [
        delegation({
          id: 'broken',
          origin_node: 'broken-origin',
          status: 'succeeded',
          response: 'Result text\n[report](/hub/node/target/report.html)',
        }),
        value.delegations.peek()[1],
      ];
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByText('broken-origin')).toBeVisible();
    expect(screen.getByRole('link', { name: 'report' })).toHaveAttribute(
      'href',
      '/hub/node/target/report.html',
    );
    expect(container.querySelectorAll('.delegation-response-text')[0]).toHaveTextContent(
      'Result text',
      { normalizeWhitespace: false },
    );
    expect(reported).toHaveLength(1);
    expect(reported[0]).toContain('Delegation');
    value.dispose();
  });

  it('recovers a failed row from a corrected poll without retrying an unchanged one', async () => {
    const payload = (kinds: string[]) => ({
      input_required: [
        inputItem({ title: 'Blocked conversation', pending_interaction_kinds: kinds }),
      ],
      inbox: [],
      total_input_required: 1,
      total_unseen: 0,
      has_more: false,
    });
    const listAttention = vi
      .fn()
      .mockResolvedValueOnce(payload(malformedKinds))
      .mockResolvedValueOnce(payload(malformedKinds))
      .mockResolvedValueOnce(payload(['approval']));
    const value = new HubStore({
      listNodes: vi.fn(async () => ({ nodes: [] })),
      listAttention,
      listDelegations: vi.fn(async () => ({ delegations: [] })),
    } as unknown as HubClient);
    const reported = boundaryDiagnostics();
    render(<AttentionPanels store={value} />);

    await act(async () => {
      await value.refresh();
    });
    expect(screen.getByRole('alert')).toBeVisible();
    expect(screen.queryByText('Blocked conversation')).not.toBeInTheDocument();
    expect(reported).toHaveLength(1);

    // An identical payload reuses the row, so the boundary keeps its message.
    await act(async () => {
      await value.refresh();
    });
    expect(screen.getByRole('alert')).toBeVisible();
    expect(reported).toHaveLength(1);

    await act(async () => {
      await value.refresh();
    });
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
    expect(screen.getByText('Blocked conversation')).toBeVisible();
    expect(screen.getByText(/Approval waiting/)).toBeVisible();
    value.dispose();
  });
});
