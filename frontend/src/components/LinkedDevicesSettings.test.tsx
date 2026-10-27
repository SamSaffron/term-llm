import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as linkedDevicesAPI from '../api/linked-devices';
import { StoreContext } from '../app/context';
import * as clipboard from '../platform/clipboard';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { LINKED_DEVICES_POLL_MS, LinkedDevicesSettings } from './LinkedDevicesSettings';

vi.mock('../api/linked-devices', async (loadOriginal) => {
  const original = await loadOriginal<typeof import('../api/linked-devices')>();
  return {
    ...original,
    getLinkedDevices: vi.fn(),
    createLinkedDevicesInvitation: vi.fn(),
    cancelLinkedDevicesInvitation: vi.fn(),
    approveLinkedDevicesRequest: vi.fn(),
    denyLinkedDevicesRequest: vi.fn(),
    revokeLinkedDevicesDevice: vi.fn(),
    stopLinkedDevices: vi.fn(),
    resumeLinkedDevices: vi.fn(),
    resetLinkedDevices: vi.fn(),
  };
});

const offStatus: linkedDevicesAPI.LinkedDevicesStatus = {
  state: 'off',
  enabled: false,
  broker_connected: false,
  authority: {
    agent_name: 'test-agent',
    description: 'can see every conversation this server shows',
    same_uid_note: 'The agent and sharing runtime use the same UID.',
  },
  pending_requests: [],
  devices: [],
};

function view(active = true) {
  const store = new AppStore(testConfig);
  const result = render(
    <StoreContext.Provider value={store}>
      <LinkedDevicesSettings active={active} />
    </StoreContext.Provider>,
  );
  return { store, ...result };
}

beforeEach(() => {
  vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue(offStatus);
  vi.mocked(linkedDevicesAPI.createLinkedDevicesInvitation).mockResolvedValue(offStatus);
  for (const operation of [
    linkedDevicesAPI.cancelLinkedDevicesInvitation,
    linkedDevicesAPI.approveLinkedDevicesRequest,
    linkedDevicesAPI.denyLinkedDevicesRequest,
    linkedDevicesAPI.revokeLinkedDevicesDevice,
    linkedDevicesAPI.stopLinkedDevices,
    linkedDevicesAPI.resumeLinkedDevices,
    linkedDevicesAPI.resetLinkedDevices,
  ]) {
    vi.mocked(operation).mockResolvedValue(undefined);
  }
});

afterEach(() => {
  vi.clearAllMocks();
  vi.useRealTimers();
});

describe('LinkedDevicesSettings', () => {
  it('polls only while its settings tab is active', async () => {
    vi.useFakeTimers();
    const { store, rerender } = view(false);
    try {
      await act(async () => {});
      expect(linkedDevicesAPI.getLinkedDevices).not.toHaveBeenCalled();

      rerender(
        <StoreContext.Provider value={store}>
          <LinkedDevicesSettings active />
        </StoreContext.Provider>,
      );
      await act(async () => {});
      expect(linkedDevicesAPI.getLinkedDevices).toHaveBeenCalledOnce();

      rerender(
        <StoreContext.Provider value={store}>
          <LinkedDevicesSettings active={false} />
        </StoreContext.Provider>,
      );
      await act(async () => {
        await vi.advanceTimersByTimeAsync(LINKED_DEVICES_POLL_MS * 2);
      });
      expect(linkedDevicesAPI.getLinkedDevices).toHaveBeenCalledOnce();
    } finally {
      store.dispose();
    }
  });

  it.each([
    ['unavailable', 'Unavailable'],
    ['off', 'Off'],
    ['inviting', 'Invitation open'],
    ['pending', 'Approval pending'],
    ['sharing', 'Sharing'],
    ['reconnecting', 'Reconnecting'],
    ['stopped', 'Stopped'],
  ] as const)('renders the honest %s state from the server', async (state, label) => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state,
      reason: state === 'unavailable' ? 'Not supported by this server.' : undefined,
    });
    const { store } = view();
    try {
      expect(
        await screen.findByText(label, { selector: '.linked-devices-state strong' }),
      ).toBeVisible();
      expect(screen.queryByText(/same UID/i)).toBeNull();
      if (state === 'unavailable')
        expect(screen.getByText('Not supported by this server.')).toBeVisible();
    } finally {
      store.dispose();
    }
  });

  it('uses the backend QR and submits the confirmation code', async () => {
    const status: linkedDevicesAPI.LinkedDevicesStatus = {
      ...offStatus,
      state: 'pending',
      enabled: true,
      invitation: {
        url: 'https://example.test/invite/secret',
        qr_data_url: 'data:image/png;base64,cXItYnl0ZXM=',
        expires_at: '2099-01-01T00:00:00Z',
      },
      pending_requests: [{ id: 'request-1', name: 'Sam’s client' }],
      devices: [],
    };
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue(status);
    const copy = vi.spyOn(clipboard, 'copyText').mockResolvedValue(undefined);
    const { store } = view();
    try {
      expect(await screen.findByRole('img', { name: /QR code/ })).toHaveAttribute(
        'src',
        status.invitation!.qr_data_url,
      );
      expect(screen.getByRole('note')).toHaveTextContent(
        'Keep the invitation link private until it expires.',
      );
      fireEvent.click(screen.getByRole('button', { name: 'Copy invitation link' }));
      await waitFor(() => expect(copy).toHaveBeenCalledWith(status.invitation!.url));

      const approve = screen.getByRole('button', { name: 'Approve' });
      expect(approve).toBeDisabled();
      expect(
        screen.getByText('This device can read conversations and use this agent’s tools.'),
      ).toBeVisible();
      expect(screen.getByText('Access details').closest('details')).not.toHaveAttribute('open');
      fireEvent.input(screen.getByLabelText('Confirmation code'), {
        target: { value: 'ab12-cd34' },
      });
      expect(approve).toBeEnabled();
      fireEvent.click(approve);
      await waitFor(() =>
        expect(linkedDevicesAPI.approveLinkedDevicesRequest).toHaveBeenCalledWith(
          store.api,
          'request-1',
          'AB12-CD34',
        ),
      );
    } finally {
      copy.mockRestore();
      store.dispose();
    }
  });

  it('offers recovery while reconnecting and when unavailable state requires reset', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'reconnecting',
      enabled: true,
      warnings: ['rendezvous disconnected; reconnecting'],
    });
    const reconnecting = view();
    try {
      expect(await screen.findByText('rendezvous disconnected; reconnecting')).toBeVisible();
      fireEvent.click(reconnecting.getByRole('button', { name: 'Resume' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.resumeLinkedDevices).toHaveBeenCalledWith(reconnecting.store.api),
      );
    } finally {
      reconnecting.unmount();
      reconnecting.store.dispose();
    }

    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'unavailable',
      reason: 'session store changed; reset is required',
      reset_required: true,
    });
    const reset = view();
    try {
      await screen.findByText('Unavailable', { selector: '.linked-devices-state strong' });
      expect(screen.getByRole('button', { name: 'Forget all' })).toBeVisible();
    } finally {
      reset.store.dispose();
    }
  });

  it('shows typed recovery actions for a broker configuration rejection only', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'unavailable',
      reason: 'rendezvous rejected linked devices registration',
      can_resume: true,
      can_reset: true,
    });
    const recoverable = view();
    try {
      await screen.findByText('Unavailable', { selector: '.linked-devices-state strong' });
      fireEvent.click(screen.getByRole('button', { name: 'Resume' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.resumeLinkedDevices).toHaveBeenCalledWith(recoverable.store.api),
      );
      expect(screen.getByRole('button', { name: 'Forget all' })).toBeVisible();
    } finally {
      recoverable.unmount();
      recoverable.store.dispose();
    }

    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'unavailable',
      reason: 'no rendezvous service is configured in this build',
    });
    const disabled = view();
    try {
      await screen.findByText('Unavailable', { selector: '.linked-devices-state strong' });
      expect(screen.queryByRole('button', { name: 'Resume' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Forget all' })).toBeNull();
    } finally {
      disabled.store.dispose();
    }
  });

  it('revokes with confirmation, stops and resumes directly, and confirms forget all', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'sharing',
      enabled: true,
      devices: [{ id: 'device-1', name: 'Phone', connection: 'direct' }],
    });
    const { store } = view();
    try {
      await screen.findByText('Phone');
      fireEvent.click(screen.getByRole('button', { name: 'Revoke' }));
      expect(linkedDevicesAPI.revokeLinkedDevicesDevice).not.toHaveBeenCalled();
      fireEvent.click(
        within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Revoke' }),
      );
      await waitFor(() =>
        expect(linkedDevicesAPI.revokeLinkedDevicesDevice).toHaveBeenCalledWith(
          store.api,
          'device-1',
        ),
      );

      fireEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.stopLinkedDevices).toHaveBeenCalledWith(store.api),
      );

      fireEvent.click(screen.getByRole('button', { name: 'Forget all' }));
      expect(
        screen.getByText(
          'All invitations, pending requests, and approved devices will be removed.',
        ),
      ).toBeVisible();
      fireEvent.click(
        within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Forget all' }),
      );
      await waitFor(() =>
        expect(linkedDevicesAPI.resetLinkedDevices).toHaveBeenCalledWith(store.api),
      );
    } finally {
      store.dispose();
    }

    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'stopped',
    });
    const resumed = view();
    try {
      await screen.findByText('Stopped', { selector: '.linked-devices-state strong' });
      fireEvent.click(screen.getByRole('button', { name: 'Resume' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.resumeLinkedDevices).toHaveBeenCalledWith(resumed.store.api),
      );
    } finally {
      resumed.store.dispose();
    }
  });
});
