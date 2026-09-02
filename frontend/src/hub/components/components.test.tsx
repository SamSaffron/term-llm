import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/preact';
import type { HubClient } from '../../api/hub-client';
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
import { NodeCard } from './NodeCard';
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
          },
        }}
        store={authStore}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Sign in with a passkey' }));
    await screen.findByText(/cancelled or timed out/);
  });
});
