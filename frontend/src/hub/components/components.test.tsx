import { observeRenders } from '../../test/render-counts';
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { HubAPIError, type HubClient } from '../../api/hub-client';
import { LONG_PRESS_MS } from '../../components/useReorderableList';
import { permuteOrder } from '../../stores/saved-order';
import type { HubConfig } from '../config';
import type { HubDelegation, HubNode } from '../domain/types';
import type { PasskeyPlatform } from '../platform/passkeys';
import { AuthStore } from '../stores/auth-store';
import { HubStore } from '../stores/hub-store';
import { AddNodeDialog } from './AddNodeDialog';
import { AuthApp } from './AuthApp';
import { BearerLogin } from './BearerLogin';
import { DelegationsPanel } from './DelegationsPanel';
import { HubApp } from './HubApp';
import { NodeCard, NodeGrid } from './NodeCard';
import { NodeSessions } from './NodeSessions';
import { RegistrationHelp } from './RegistrationHelp';
import { SecurityPanel } from './SecurityPanel';

const dashboardConfig: HubConfig = {
  page: 'dashboard',
  authMode: 'none',
  basePath: '/hub',
  canAddNodes: true,
  passkeyAuth: false,
  invalidToken: false,
  formAction: '/hub/',
};

function store(client: Record<string, unknown> = {}) {
  return new HubStore(client as unknown as HubClient);
}

function DialogFixture({ value }: { value: HubStore }) {
  return value.addDialogOpen.value ? (
    <AddNodeDialog
      config={dashboardConfig}
      store={value}
      clipboard={{ writeText: vi.fn(async () => undefined) }}
    />
  ) : null;
}

describe('Hub components', () => {
  it('keeps closed local node cards independent of operations but disables open menu actions', async () => {
    const value = store();
    const node = {
      id: 'alpha',
      name: 'Alpha',
      source: 'local',
      status: { reachable: true, state: 'ok', latency_ms: 1 },
    } as HubNode;
    const counts = observeRenders();
    try {
      render(<NodeCard node={node} store={value} />);
      expect(counts.count('NodeCard')).toBe(1);
      counts.clear();
      await act(() => {
        value.nodeOperation.value = 'testing';
      });
      expect(counts.count('NodeCard')).toBe(0);
      fireEvent.click(screen.getByRole('button', { name: 'More actions for Alpha' }));
      expect(screen.getByRole('menuitem', { name: 'Remove node' })).toBeDisabled();
      counts.clear();
      await act(() => {
        value.nodeOperation.value = 'idle';
      });
      expect(screen.getByRole('menuitem', { name: 'Remove node' })).toBeEnabled();
      expect(counts.count('NodeCard')).toBe(0);
      expect(counts.count('RemoveNodeAction')).toBeGreaterThan(0);
    } finally {
      counts.dispose();
    }
  });

  it('starts the store and disposes its polling lifecycle on unmount', async () => {
    const listNodes = vi.fn(async () => ({ nodes: [] }));
    const listAttention = vi.fn(async () => ({
      input_required: [],
      inbox: [],
      total_input_required: 0,
      total_unseen: 0,
      has_more: false,
    }));
    const listDelegations = vi.fn(async () => ({ delegations: [] }));
    const clearInterval = vi.fn();
    const value = new HubStore(
      { listNodes, listAttention, listDelegations } as unknown as HubClient,
      undefined,
      {
        setInterval: vi.fn(() => 42) as unknown as typeof window.setInterval,
        clearInterval: clearInterval as unknown as typeof window.clearInterval,
      },
    );
    const view = render(
      <HubApp
        config={dashboardConfig}
        store={value}
        clipboard={{ writeText: vi.fn(async () => undefined) }}
      />,
    );
    await waitFor(() => expect(listNodes).toHaveBeenCalledOnce());
    view.unmount();
    expect(clearInterval).toHaveBeenCalledWith(42);
  });

  it('caps rendered delegations at eight while counting the full active set', () => {
    const value = store();
    value.delegationsVerified.value = true;
    value.delegations.value = Array.from({ length: 9 }, (_, index) => ({
      id: `delegation-${index}`,
      origin_node: 'origin',
      target_node: 'target',
      status: 'running',
      depth: 1,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
    })) satisfies HubDelegation[];
    const { container } = render(<DelegationsPanel config={dashboardConfig} store={value} />);
    expect(container.querySelectorAll('.delegation-row')).toHaveLength(8);
    expect(screen.getByText('9 active · 9 total')).toBeVisible();
  });

  it('keeps full delegation text and labels stale results after a refresh failure', () => {
    const value = store();
    const response = 'Result text\n![chart](/node/target/chart.png)';
    value.initialLoading.value = false;
    value.delegations.value = [
      {
        id: 'delegation',
        origin_node: 'origin',
        target_node: 'target',
        status: 'succeeded',
        depth: 1,
        response,
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      },
    ];
    value.delegationError.value = 'temporarily unavailable';
    const { container } = render(<DelegationsPanel config={dashboardConfig} store={value} />);
    expect(container.querySelector('.delegation-response-text')).toHaveTextContent(response, {
      normalizeWhitespace: false,
    });
    expect(screen.getByRole('status')).toHaveTextContent(
      'Could not refresh delegations: temporarily unavailable',
    );
    expect(screen.getByRole('status')).toHaveTextContent('Showing the last successful result');
  });

  it('traps dialog focus and closes with Escape while restoring the trigger', async () => {
    const trigger = document.createElement('button');
    document.body.append(trigger);
    trigger.focus();
    const value = store();
    value.openAddDialog();
    render(<DialogFixture value={value} />);
    const name = screen.getByLabelText('Name');
    expect(name).toHaveFocus();
    trigger.focus();
    fireEvent.keyDown(document, { key: 'Tab' });
    expect(screen.getByRole('button', { name: 'Private node' })).toHaveFocus();
    const buttons = screen.getAllByRole('button');
    const last = buttons.at(-1)!;
    last.focus();
    fireEvent.keyDown(document, { key: 'Tab' });
    expect(screen.getByRole('button', { name: 'Private node' })).toHaveFocus();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(value.addDialogOpen.value).toBe(false);
    await waitFor(() => expect(trigger).toHaveFocus());
    trigger.remove();
  });

  it('dismisses only a true dialog backdrop click', () => {
    const value = store();
    value.openAddDialog();
    const { container } = render(
      <AddNodeDialog
        config={dashboardConfig}
        store={value}
        clipboard={{ writeText: vi.fn(async () => undefined) }}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('dialog'));
    expect(value.addDialogOpen.value).toBe(true);
    fireEvent.mouseDown(container.querySelector('.modal-overlay')!);
    expect(value.addDialogOpen.value).toBe(false);
  });

  it('cleans up copied-feedback timers when registration help unmounts', async () => {
    const clearTimeout = vi.spyOn(window, 'clearTimeout');
    const clipboard = { writeText: vi.fn(async () => undefined) };
    const value = store();
    value.registrationInfo.value = {
      enabled: true,
      registration_token: 'registration-secret',
    };
    const view = render(
      <RegistrationHelp config={dashboardConfig} store={value} clipboard={clipboard} />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Copy token' }));
    await waitFor(() => {
      expect(clipboard.writeText).toHaveBeenCalledWith('registration-secret');
      expect(screen.getByRole('status')).toHaveTextContent('Copied registration token.');
    });
    view.unmount();
    expect(clearTimeout).toHaveBeenCalled();
    clearTimeout.mockRestore();
  });

  it('renders four node sessions without duplicates or group headings', () => {
    const session = (id: string, extras = {}) => ({
      id,
      short_title: id,
      resume_path: `/hub/node/alpha/chat/${id}`,
      ...extras,
    });
    const node = {
      id: 'alpha',
      name: 'Alpha',
      status: { reachable: true, state: 'ok', latency_ms: 1 },
      sessions: {
        count_label: '5 sessions',
        active: [session('recent-1', { active_run: true })],
        recent: [
          session('pin-1', { pinned: true }),
          session('recent-1', { active_run: true }),
          session('recent-2'),
          session('recent-3'),
        ],
      },
    } as HubNode;

    const view = render(<NodeSessions node={node} />);
    expect(view.container.querySelectorAll('.node-session-row')).toHaveLength(4);
    expect(screen.getAllByLabelText('Pinned')).toHaveLength(1);
    expect(screen.queryByText('Pinned')).not.toBeInTheDocument();
    expect(screen.queryByText('Recent activity')).not.toBeInTheDocument();
    expect(screen.getByText('recent-1').closest('.node-session-row')).toHaveClass('is-active');
  });

  it('renders stale, lost, and unavailable node-attention capability messages', () => {
    const node = {
      id: 'alpha',
      name: 'Alpha',
      source: 'config',
      connection: 'direct',
      url: 'http://node.test',
      base_path: '',
      proxy_path: '/hub/node/alpha/',
      new_session_path: '/hub/node/alpha/?new=1',
      has_token: false,
      status: { reachable: true, state: 'ok', latency_ms: 1 },
      sessions: {
        count_label: '1 session',
        unseen_count: 1,
        attention_capability: 'available',
        attention_last_success_at: 1,
        active: [],
        recent: [],
      },
    } as HubNode;
    const view = render(<NodeSessions node={node} />);
    expect(screen.getByText(/1 ready to review/)).toHaveTextContent('last checked');
    view.rerender(
      <NodeSessions
        node={{ ...node, sessions: { ...node.sessions!, attention_capability: 'lost' } }}
      />,
    );
    expect(screen.getByText(/capability lost/)).toBeVisible();
    view.rerender(
      <NodeSessions
        node={{ ...node, sessions: { ...node.sessions!, attention_capability: 'unavailable' } }}
      />,
    );
    expect(screen.getByText('Terminal attention unavailable')).toBeVisible();
  });

  it('opens Security as a dismissible modal and restores focus', () => {
    const trigger = document.createElement('button');
    document.body.append(trigger);
    trigger.focus();
    const value = store();
    value.securityOpen.value = true;

    const view = render(<SecurityPanel store={value} />);
    expect(screen.getByRole('dialog', { name: 'Security' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Close security' })).toHaveFocus();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Security' })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();

    trigger.focus();
    value.securityOpen.value = true;
    view.rerender(<SecurityPanel store={value} />);
    fireEvent.mouseDown(view.container.querySelector('.modal-overlay')!);
    expect(value.securityOpen.value).toBe(false);
    trigger.remove();
  });

  it('disables removal when only one passkey remains', () => {
    const value = store();
    value.securityOpen.value = true;
    value.credentials.value = [
      {
        record_id: 'primary',
        display_name: 'Primary',
        created_at: '2026-01-01T00:00:00Z',
        last_used_at: '2026-01-01T00:00:00Z',
        transports: [],
      },
    ];
    render(<SecurityPanel store={value} />);
    expect(screen.getByRole('button', { name: 'Remove' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Remove' })).toHaveAttribute(
      'title',
      'At least one passkey is required',
    );
  });

  it('uses keyboard-aware local-node menus and confirms destructive removal', async () => {
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    );
    const removeNode = vi.fn(async () => ({ removed: 'alpha' }));
    const listNodes = vi.fn(async () => ({ nodes: [] }));
    const value = store({ removeNode, listNodes });
    const node = {
      id: 'alpha',
      name: 'Alpha',
      source: 'local',
      proxy_path: '/hub/node/alpha/',
      new_session_path: '/hub/node/alpha/?new=1',
      status: { reachable: true, state: 'ok', latency_ms: 1 },
    } as HubNode;
    render(<NodeCard node={node} store={value} />);
    fireEvent.click(screen.getByRole('button', { name: 'More actions for Alpha' }));
    const remove = await screen.findByRole('menuitem', { name: 'Remove node' });
    await waitFor(() => expect(remove).toHaveFocus());
    fireEvent.keyDown(remove, { key: 'Home' });
    fireEvent.click(remove);
    await waitFor(() => expect(removeNode).toHaveBeenCalledWith('alpha'));
  });

  it('keeps bearer authentication as a native GET form', () => {
    render(
      <BearerLogin
        config={{
          ...dashboardConfig,
          page: 'bearer-login',
          authMode: 'bearer',
          invalidToken: true,
        }}
      />,
    );
    const form = screen.getByRole('button', { name: 'Connect to Hub' }).closest('form')!;
    expect(form.method).toBe('get');
    expect(form.getAttribute('action')).toBe('/hub/');
    expect(screen.getByLabelText('Hub token')).toHaveAttribute('type', 'password');
    expect(screen.getByRole('alert')).toHaveTextContent('not accepted');
  });

  it('labels passkey fields and exposes pending and cancellation states', async () => {
    const client = { beginLogin: vi.fn(async () => ({ publicKey: {} })) } as unknown as HubClient;
    const passkeys = {
      available: () => true,
      get: vi.fn(async () => {
        throw new DOMException('cancelled', 'NotAllowedError');
      }),
    } as unknown as PasskeyPlatform;
    const authStore = new AuthStore(client, passkeys, sessionStorage, vi.fn());
    render(
      <AuthApp
        config={{
          ...dashboardConfig,
          page: 'passkey-auth',
          authMode: 'passkey',
          passkey: {
            mode: 'login',
            title: 'Sign in',
            heading: 'Sign in to Hub',
            description: 'Use a passkey.',
            button: 'Sign in with a passkey',
            needsCode: false,
            needsName: false,
            defaultName: '',
            origin: '',
          },
        }}
        store={authStore}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Sign in with a passkey' }));
    await screen.findByText(/cancelled or timed out/);
  });

  it('links to the configured origin instead of offering a doomed passkey ceremony', () => {
    const authStore = new AuthStore(
      {} as HubClient,
      {} as PasskeyPlatform,
      sessionStorage,
      vi.fn(),
    );
    render(
      <AuthApp
        config={{
          ...dashboardConfig,
          page: 'passkey-auth',
          authMode: 'passkey',
          passkey: {
            mode: 'login',
            title: 'Sign in',
            heading: 'Sign in to term-llm',
            description: 'Use a passkey.',
            button: 'Sign in with a passkey',
            needsCode: false,
            needsName: false,
            defaultName: '',
            origin: 'https://term.example',
          },
        }}
        store={authStore}
        canonicalURL="https://term.example/ui/auth/login"
      />,
    );
    expect(screen.getByRole('alert').textContent).toContain('only work at https://term.example');
    expect(
      screen.getByRole('link', { name: 'Continue at https://term.example' }).getAttribute('href'),
    ).toBe('https://term.example/ui/auth/login');
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('replaces the waiting state after handing a native sign-in to the app', async () => {
    const client = {
      authorizeNative: vi.fn(async () => ({ redirect: 'termllm-auth://callback?code=c&state=s' })),
    } as unknown as HubClient;
    const passkeys = { available: () => true } as unknown as PasskeyPlatform;
    const navigate = vi.fn();
    const authStore = new AuthStore(client, passkeys, sessionStorage, navigate);
    render(
      <AuthApp
        config={{
          ...dashboardConfig,
          page: 'passkey-auth',
          authMode: 'passkey',
          passkey: {
            mode: 'native',
            title: 'Approve app sign-in',
            heading: 'Sign in the term-llm app',
            description: 'Only continue if you just started this sign-in.',
            button: 'Approve sign-in',
            needsCode: false,
            needsName: false,
            defaultName: '',
            challenge: 'A'.repeat(43),
            origin: '',
          },
        }}
        store={authStore}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Approve sign-in' }));
    await screen.findByText(/You can close this window/);
    expect(navigate).toHaveBeenCalledWith('termllm-auth://callback?code=c&state=s');
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.queryByText(/Waiting for your passkey/)).toBeNull();
  });
});

describe('Hub node grid order', () => {
  const gridNode = (id: string, name: string, source = 'config'): HubNode => ({
    id,
    name,
    source,
    connection: 'direct',
    url: `http://${id}.test/chat`,
    base_path: '/chat',
    proxy_path: `/hub/node/${id}/`,
    new_session_path: `/hub/node/${id}/?new=1`,
    has_token: true,
    status: { reachable: true, state: 'ok', latency_ms: 1 },
    sessions: { count_label: '1 session', resume_path: `/hub/node/${id}/chat/s1` },
  });
  const orderStore = (
    reorderNodes?: ReturnType<typeof vi.fn<(ids: string[]) => Promise<{ node_ids: string[] }>>>,
    listed = [
      gridNode('alpha', 'Alpha'),
      gridNode('beta', 'Beta'),
      gridNode('gamma', 'Gamma', 'local'),
    ],
  ) => {
    let saved = listed.map((node) => node.id);
    const send =
      reorderNodes ??
      vi.fn(async (ids: string[]) => {
        saved = permuteOrder(saved, ids);
        return { node_ids: saved };
      });
    const listNodes = vi.fn(async () => ({ nodes: listed }));
    const value = store({ reorderNodes: send, listNodes });
    value.nodes.value = listed;
    value.initialLoading.value = false;
    return { value, reorderNodes: send, listNodes };
  };
  const cardNames = (container: Element) =>
    [...container.querySelectorAll('.node-grid > .node-card .node-name')].map(
      (name) => name.textContent,
    );
  const card = (container: Element, name: string) =>
    [...container.querySelectorAll<HTMLElement>('.node-grid > .node-card')].find(
      (entry) => entry.querySelector('.node-name')?.textContent === name,
    )!;
  const header = (container: Element, name: string) =>
    card(container, name).querySelector<HTMLElement>('.node-card-head')!;
  /**
   * jsdom has no layout: place the cards, in DOM order, in 300×200 cells
   * 20px apart that wrap after `columns` cells, as the dashboard grid does.
   */
  const layoutCards = (container: Element, columns: number) => {
    const cards = [...container.querySelectorAll<HTMLElement>('.node-grid > .node-card')];
    cards.forEach((entry, index) => {
      const left = (index % columns) * 320;
      const top = Math.floor(index / columns) * 220;
      vi.spyOn(entry, 'getBoundingClientRect').mockReturnValue({
        top,
        bottom: top + 200,
        height: 200,
        left,
        right: left + 300,
        width: 300,
        x: left,
        y: top,
        toJSON: () => ({}),
      } as DOMRect);
    });
    return cards;
  };
  const mouse = { pointerId: 1, pointerType: 'mouse', isPrimary: true, button: 0 };
  const touch = { pointerId: 7, pointerType: 'touch', isPrimary: true };

  it('drags a card by its header across grid rows, previewing by sliding the cards it passes', async () => {
    const { value, reorderNodes } = orderStore();
    const { container } = render(<NodeGrid store={value} />);
    expect(cardNames(container)).toEqual(['Alpha', 'Beta', 'Gamma']);
    const [alpha, beta, gamma] = layoutCards(container, 2);

    // Gamma starts alone on the second row.
    fireEvent.pointerDown(header(container, 'Gamma'), { ...mouse, clientX: 150, clientY: 240 });
    // A few pixels of travel, in any direction, is still a click.
    fireEvent.pointerMove(window, { ...mouse, clientX: 153, clientY: 243 });
    expect(gamma).not.toHaveClass('is-dragging');
    // Up and to the right, over Beta's cell: Beta slides to Gamma's cell, on
    // the next row, to open it. Alpha stays. No insertion line is drawn.
    fireEvent.pointerMove(window, { ...mouse, clientX: 480, clientY: 30 });
    expect(gamma).toHaveClass('is-dragging');
    expect(container.querySelector('.node-grid')).toHaveClass('is-reordering');
    expect(gamma.style.transform).toBe('translate(320px, -210px)');
    expect(beta.style.transform).toBe('translate(-320px, 220px)');
    expect(alpha.style.transform).toBe('');
    expect(container.querySelector('.node-grid')!.children).toHaveLength(3);
    // The dragged card stays within the grid.
    fireEvent.pointerMove(window, { ...mouse, clientX: 2_000, clientY: -500 });
    expect(gamma.style.transform).toBe('translate(320px, -220px)');
    fireEvent.pointerUp(window, { ...mouse, clientX: 2_000, clientY: -500 });
    // The click that ends the drag never follows a link it was released on.
    expect(fireEvent.click(card(container, 'Beta').querySelector('a')!, { detail: 1 })).toBe(false);

    await waitFor(() => expect(reorderNodes).toHaveBeenCalledWith(['gamma', 'beta']));
    expect(cardNames(container)).toEqual(['Alpha', 'Gamma', 'Beta']);
    expect(gamma).not.toHaveClass('is-dragging');
    expect(container.querySelector('.node-grid')).not.toHaveClass('is-reordering');
    expect(screen.getByText('Moved Gamma to position 2 of 3.')).toBeInTheDocument();
    // The cards settle into their new cells and keep no drag offsets.
    await waitFor(() =>
      expect([alpha, beta, gamma].map((entry) => entry.style.transform)).toEqual(['', '', '']),
    );
  });

  it('moves cards with Alt+Arrow keys and the card menu, keeping focus on the control used', async () => {
    const { value, reorderNodes } = orderStore();
    const { container } = render(<NodeGrid store={value} />);
    const alphaResume = card(container, 'Alpha').querySelector<HTMLAnchorElement>('a.primary')!;
    expect(alphaResume).toHaveAttribute('aria-keyshortcuts', 'Alt+ArrowUp Alt+ArrowDown');
    alphaResume.focus();
    fireEvent.keyDown(alphaResume, { key: 'ArrowDown', altKey: true });
    expect(cardNames(container)).toEqual(['Beta', 'Alpha', 'Gamma']);
    await waitFor(() => expect(reorderNodes).toHaveBeenLastCalledWith(['beta', 'alpha']));
    await waitFor(() => expect(alphaResume).toHaveFocus());
    expect(screen.getByText('Moved Alpha to position 2 of 3.')).toBeInTheDocument();

    // The first card cannot move earlier, and other modifiers are not moves.
    const betaNew = card(container, 'Beta').querySelector<HTMLAnchorElement>('a.ghost')!;
    fireEvent.keyDown(betaNew, { key: 'ArrowUp', altKey: true });
    fireEvent.keyDown(betaNew, { key: 'ArrowDown', altKey: true, shiftKey: true });
    fireEvent.keyDown(betaNew, { key: 'ArrowDown' });
    expect(cardNames(container)).toEqual(['Beta', 'Alpha', 'Gamma']);
    expect(reorderNodes).toHaveBeenCalledTimes(1);

    // Every card's menu offers the moves it can make; only local nodes can be removed.
    fireEvent.click(screen.getByRole('button', { name: 'More actions for Beta' }));
    expect(screen.queryByRole('menuitem', { name: 'Move earlier' })).not.toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Move later' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Remove node' })).not.toBeInTheDocument();
    // Arrow keys inside the open menu move between its items, not the card.
    fireEvent.keyDown(screen.getByRole('menuitem', { name: 'Move later' }), {
      key: 'ArrowDown',
      altKey: true,
    });
    expect(cardNames(container)).toEqual(['Beta', 'Alpha', 'Gamma']);
    fireEvent.click(screen.getByRole('button', { name: 'More actions for Beta' }));

    const gammaMenu = screen.getByRole('button', { name: 'More actions for Gamma' });
    fireEvent.click(gammaMenu);
    expect(screen.queryByRole('menuitem', { name: 'Move later' })).not.toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Remove node' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('menuitem', { name: 'Move earlier' }));
    expect(cardNames(container)).toEqual(['Beta', 'Gamma', 'Alpha']);
    await waitFor(() => expect(reorderNodes).toHaveBeenLastCalledWith(['gamma', 'alpha']));
    await waitFor(() => expect(gammaMenu).toHaveFocus());
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(screen.getByText('Moved Gamma to position 2 of 3.')).toBeInTheDocument();
  });

  it('lifts a card by a long press on its header on touch; a quick swipe scrolls instead', async () => {
    const { value, reorderNodes } = orderStore();
    const { container } = render(<NodeGrid store={value} />);
    // A phone shows one column of cards.
    const [alpha, beta, gamma] = layoutCards(container, 1);
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      // Sliding before the press has rested is a scroll, not a drag.
      fireEvent.pointerDown(header(container, 'Beta'), { ...touch, clientX: 100, clientY: 250 });
      fireEvent.pointerMove(window, { ...touch, clientX: 100, clientY: 290 });
      act(() => {
        vi.advanceTimersByTime(LONG_PRESS_MS);
      });
      expect(beta).not.toHaveClass('is-dragging');
      fireEvent.pointerUp(window, { ...touch, clientX: 100, clientY: 290 });

      fireEvent.pointerDown(header(container, 'Alpha'), { ...touch, clientX: 100, clientY: 20 });
      act(() => {
        vi.advanceTimersByTime(LONG_PRESS_MS);
      });
      expect(alpha).toHaveClass('is-dragging');
      // Held, the card follows the finger down the column, sideways drift included.
      fireEvent.pointerMove(window, { ...touch, clientX: 60, clientY: 470 });
      expect(alpha.style.transform).toBe('translateY(440px)');
      expect(beta.style.transform).toBe('translateY(-220px)');
      expect(gamma.style.transform).toBe('translateY(-220px)');
      fireEvent.pointerUp(window, { ...touch, clientX: 100, clientY: 20 });
    } finally {
      vi.useRealTimers();
    }
    await waitFor(() => expect(reorderNodes).toHaveBeenCalledWith(['beta', 'gamma', 'alpha']));
    expect(reorderNodes).toHaveBeenCalledOnce();
    expect(cardNames(container)).toEqual(['Beta', 'Gamma', 'Alpha']);
  });

  it('never drags from card controls or a lone card', () => {
    const { value, reorderNodes } = orderStore();
    const { container, unmount } = render(<NodeGrid store={value} />);
    layoutCards(container, 2);
    const alpha = card(container, 'Alpha');
    for (const control of [
      alpha.querySelector<HTMLElement>('a.primary')!,
      screen.getByRole('button', { name: 'More actions for Alpha' }),
    ]) {
      fireEvent.pointerDown(control, { ...mouse, clientX: 100, clientY: 150 });
      fireEvent.pointerMove(window, { ...mouse, clientX: 500, clientY: 150 });
      expect(alpha).not.toHaveClass('is-dragging');
      fireEvent.pointerUp(window, { ...mouse, clientX: 500, clientY: 150 });
    }
    expect(reorderNodes).not.toHaveBeenCalled();
    unmount();

    // A single config node has nothing to reorder, so it has no menu either.
    const lone = orderStore(undefined, [gridNode('solo', 'Solo')]);
    const view = render(<NodeGrid store={lone.value} />);
    const solo = card(view.container, 'Solo');
    expect(solo).not.toHaveClass('is-reorderable');
    expect(screen.queryByRole('button', { name: 'More actions for Solo' })).toBeNull();
    fireEvent.keyDown(solo.querySelector('a.primary')!, { key: 'ArrowDown', altKey: true });
    expect(lone.reorderNodes).not.toHaveBeenCalled();
  });

  it('restores the order, shows the Hub order, and reports a move that cannot be saved', async () => {
    const reorderNodes = vi.fn(async () => {
      throw new HubAPIError(500, 'failed to save the node order');
    });
    const { value, listNodes } = orderStore(reorderNodes);
    const { container } = render(<NodeGrid store={value} />);
    const betaResume = card(container, 'Beta').querySelector<HTMLAnchorElement>('a.primary')!;
    fireEvent.keyDown(betaResume, { key: 'ArrowUp', altKey: true });
    expect(cardNames(container)).toEqual(['Beta', 'Alpha', 'Gamma']);
    await waitFor(() =>
      expect(value.nodeError.value).toBe(
        'Couldn’t save the node order: failed to save the node order',
      ),
    );
    expect(listNodes).toHaveBeenCalledOnce();
    expect(cardNames(container)).toEqual(['Alpha', 'Beta', 'Gamma']);
    expect(screen.queryByText(/^Moved /)).toBeNull();
  });

  it('re-renders only the lifted card when a drag starts', () => {
    const { value } = orderStore();
    const counts = observeRenders();
    try {
      const { container } = render(<NodeGrid store={value} />);
      layoutCards(container, 2);
      counts.clear();
      fireEvent.pointerDown(header(container, 'Alpha'), { ...mouse, clientX: 100, clientY: 20 });
      fireEvent.pointerMove(window, { ...mouse, clientX: 140, clientY: 20 });
      expect(card(container, 'Alpha')).toHaveClass('is-dragging');
      expect(counts.count('NodeCard:alpha')).toBe(1);
      expect(counts.count('NodeCard:beta')).toBe(0);
      expect(counts.count('NodeCard:gamma')).toBe(0);
      fireEvent.keyDown(window, { key: 'Escape' });
      expect(card(container, 'Alpha')).not.toHaveClass('is-dragging');
    } finally {
      counts.dispose();
    }
  });
});
