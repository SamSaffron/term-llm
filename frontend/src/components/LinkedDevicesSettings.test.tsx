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
    ['reconnecting', 'Reconnecting'],
    ['stopped', 'Paused'],
  ] as const)('shows a status banner only when %s needs attention', async (state, label) => {
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

  it.each(['off', 'sharing'] as const)(
    'shows only the device list, with no status banner, when %s',
    async (state) => {
      vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
        ...offStatus,
        state,
        enabled: state === 'sharing',
      });
      const { store } = view();
      try {
        expect(await screen.findByText('No devices yet')).toBeVisible();
        expect(document.querySelector('.linked-devices-state')).toBeNull();
        fireEvent.click(screen.getByRole('button', { name: 'Link a device' }));
        await waitFor(() =>
          expect(linkedDevicesAPI.createLinkedDevicesInvitation).toHaveBeenCalledWith(store.api),
        );
      } finally {
        store.dispose();
      }
    },
  );

  it('walks through scanning and confirming a device', async () => {
    const invitation = {
      url: 'https://example.test/invite/secret',
      qr_data_url: 'data:image/png;base64,cXItYnl0ZXM=',
      expires_at: '2099-01-01T00:00:00Z',
    };
    const scanning: linkedDevicesAPI.LinkedDevicesStatus = {
      ...offStatus,
      state: 'inviting',
      enabled: true,
      invitation,
    };
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue(scanning);
    const copy = vi.spyOn(clipboard, 'copyText').mockResolvedValue(undefined);
    const { store } = view();
    try {
      expect(await screen.findByRole('img', { name: /QR code/ })).toHaveAttribute(
        'src',
        invitation.qr_data_url,
      );
      expect(screen.getByText('1 Scan')).toHaveAttribute('aria-current', 'step');
      expect(screen.getByRole('note')).toHaveTextContent('Keep the link private until it expires.');
      fireEvent.click(screen.getByRole('button', { name: 'Copy link instead' }));
      await waitFor(() => expect(copy).toHaveBeenCalledWith(invitation.url));
      expect(screen.queryByRole('button', { name: 'Link a device' })).toBeNull();

      vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
        ...scanning,
        state: 'pending',
        pending_requests: [{ id: 'request-1', name: 'iPad' }],
      });
      expect(await screen.findByText('iPad wants to connect', {}, { timeout: 3000 })).toBeVisible();
      expect(screen.getByText('2 Confirm')).toHaveAttribute('aria-current', 'step');
      const approve = screen.getByRole('button', { name: 'Approve iPad' });
      expect(approve).toBeDisabled();
      expect(
        screen
          .getByText('It will see your conversations and can use this agent’s tools')
          .closest('details'),
      ).not.toHaveAttribute('open');
      const code = screen.getByRole('group', { name: 'Code shown on iPad' });
      const boxes = within(code).getAllByRole('textbox');
      expect(boxes).toHaveLength(6);
      expect(boxes[0]).toHaveFocus();
      fireEvent.input(boxes[0], { target: { value: '393642' } });
      expect(boxes.map((box) => (box as HTMLInputElement).value).join('')).toBe('393642');
      expect(approve).toBeEnabled();
      fireEvent.click(approve);
      await waitFor(() =>
        expect(linkedDevicesAPI.approveLinkedDevicesRequest).toHaveBeenCalledWith(
          store.api,
          'request-1',
          '393642',
        ),
      );
    } finally {
      copy.mockRestore();
      store.dispose();
    }
  });

  it('shows how many devices are waiting when several ask at once', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'pending',
      enabled: true,
      pending_requests: [
        { id: 'request-1', name: 'iPad' },
        { id: 'request-2', name: 'Pixel' },
      ],
    });
    const { store } = view();
    try {
      expect(await screen.findByText('1 of 2 devices waiting')).toBeVisible();
      expect(screen.getByText('iPad wants to connect')).toBeVisible();
    } finally {
      store.dispose();
    }
  });

  it('keeps a new invitation when an older status poll lands after it', async () => {
    let finishPoll: (status: linkedDevicesAPI.LinkedDevicesStatus) => void = () => {};
    vi.mocked(linkedDevicesAPI.getLinkedDevices)
      .mockResolvedValueOnce({ ...offStatus, state: 'sharing', enabled: true })
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            finishPoll = resolve;
          }),
      );
    const invitation = {
      url: 'https://example.test/invite/secret',
      qr_data_url: 'data:image/png;base64,cXItYnl0ZXM=',
      expires_at: '2099-01-01T00:00:00Z',
    };
    vi.mocked(linkedDevicesAPI.createLinkedDevicesInvitation).mockResolvedValue({
      ...offStatus,
      state: 'inviting',
      enabled: true,
      invitation,
    });
    vi.useFakeTimers();
    const { store } = view();
    try {
      await act(async () => {});
      await act(async () => {
        await vi.advanceTimersByTimeAsync(LINKED_DEVICES_POLL_MS);
      });
      expect(linkedDevicesAPI.getLinkedDevices).toHaveBeenCalledTimes(2);
      await act(async () => {
        fireEvent.click(screen.getByRole('button', { name: 'Link a device' }));
      });
      expect(screen.getByRole('img', { name: /QR code/ })).toBeVisible();
      await act(async () => {
        finishPoll({ ...offStatus, state: 'sharing', enabled: true });
      });
      expect(screen.getByRole('img', { name: /QR code/ })).toBeVisible();
    } finally {
      store.dispose();
    }
  });

  it('keeps the invitation when a status poll fails, before or after it was created', async () => {
    let failPoll: (error: Error) => void = () => {};
    vi.mocked(linkedDevicesAPI.getLinkedDevices)
      .mockResolvedValueOnce({ ...offStatus, state: 'sharing', enabled: true })
      .mockImplementationOnce(
        () =>
          new Promise((_, reject) => {
            failPoll = reject;
          }),
      )
      .mockRejectedValue(new Error('network down'));
    vi.mocked(linkedDevicesAPI.createLinkedDevicesInvitation).mockResolvedValue({
      ...offStatus,
      state: 'inviting',
      enabled: true,
      invitation: {
        url: 'https://example.test/invite/secret',
        qr_data_url: 'data:image/png;base64,cXItYnl0ZXM=',
        expires_at: '2099-01-01T00:00:00Z',
      },
    });
    vi.useFakeTimers();
    const { store } = view();
    try {
      await act(async () => {});
      await act(async () => {
        await vi.advanceTimersByTimeAsync(LINKED_DEVICES_POLL_MS);
      });
      await act(async () => {
        fireEvent.click(screen.getByRole('button', { name: 'Link a device' }));
      });
      await act(async () => failPoll(new Error('stale poll failed')));
      expect(screen.getByRole('img', { name: /QR code/ })).toBeVisible();
      expect(screen.queryByText('stale poll failed')).toBeNull();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(LINKED_DEVICES_POLL_MS);
      });
      expect(screen.getByRole('img', { name: /QR code/ })).toBeVisible();
      expect(screen.getByRole('alert')).toHaveTextContent('network down');
    } finally {
      store.dispose();
    }
  });

  it('shows the failure screen when the first status request fails', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockRejectedValueOnce(new Error('offline'));
    const { store } = view();
    try {
      expect(await screen.findByText('Linked devices unavailable')).toBeVisible();
      expect(screen.getByText('offline')).toBeVisible();
      fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(await screen.findByText('No devices yet')).toBeVisible();
    } finally {
      store.dispose();
    }
  });

  it('resets the copied label for a new invitation', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'sharing',
      enabled: true,
    });
    const scanning = (url: string): linkedDevicesAPI.LinkedDevicesStatus => ({
      ...offStatus,
      state: 'inviting',
      enabled: true,
      invitation: {
        url,
        qr_data_url: 'data:image/png;base64,cXI=',
        expires_at: '2099-01-01T00:00:00Z',
      },
    });
    vi.mocked(linkedDevicesAPI.createLinkedDevicesInvitation)
      .mockResolvedValueOnce(scanning('https://example.test/a'))
      .mockResolvedValueOnce(scanning('https://example.test/b'));
    const copy = vi.spyOn(clipboard, 'copyText').mockResolvedValue(undefined);
    const { store } = view();
    try {
      fireEvent.click(await screen.findByRole('button', { name: 'Link a device' }));
      fireEvent.click(await screen.findByRole('button', { name: 'Copy link instead' }));
      expect(await screen.findByRole('button', { name: 'Link copied' })).toBeVisible();
      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      fireEvent.click(await screen.findByRole('button', { name: 'Link a device' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.createLinkedDevicesInvitation).toHaveBeenCalledTimes(2),
      );
      expect(await screen.findByRole('button', { name: 'Copy link instead' })).toBeVisible();
    } finally {
      copy.mockRestore();
      store.dispose();
    }
  });

  it('starts over when an invitation code can no longer be shown', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'inviting',
      enabled: true,
    });
    const { store } = view();
    try {
      fireEvent.click(await screen.findByRole('button', { name: 'Start over' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.createLinkedDevicesInvitation).toHaveBeenCalledWith(store.api),
      );
      expect(linkedDevicesAPI.cancelLinkedDevicesInvitation).toHaveBeenCalledWith(store.api);
    } finally {
      store.dispose();
    }
  });

  it('offers recovery while reconnecting and when unavailable state requires reset', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'reconnecting',
      enabled: true,
      warnings: [
        'Lost connection to the linked-devices service at broker.term-llm.com. Reconnecting…',
      ],
    });
    const reconnecting = view();
    try {
      expect(
        await screen.findByText(
          'Lost connection to the linked-devices service at broker.term-llm.com. Reconnecting…',
        ),
      ).toBeVisible();
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
      expect(screen.getByRole('button', { name: 'Reset linked devices…' })).toBeVisible();
    } finally {
      reset.store.dispose();
    }
  });

  it('shows typed recovery actions for a broker configuration rejection only', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'unavailable',
      reason:
        "The linked-devices service at broker.term-llm.com rejected this server's settings: bad_request. Fix the configuration, then select Resume.",
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
      expect(screen.getByRole('button', { name: 'Reset linked devices…' })).toBeVisible();
    } finally {
      recoverable.unmount();
      recoverable.store.dispose();
    }

    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'unavailable',
      reason: 'No linked-devices service is configured.',
    });
    const disabled = view();
    try {
      await screen.findByText('Unavailable', { selector: '.linked-devices-state strong' });
      expect(screen.queryByRole('button', { name: 'Resume' })).toBeNull();
      expect(screen.queryByRole('button', { name: /Reset linked devices/ })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Link a device' })).toBeNull();
    } finally {
      disabled.store.dispose();
    }
  });

  it('offers Remove all only when it does more than removing one device', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'sharing',
      enabled: true,
      devices: [{ id: 'device-1', name: 'Phone', connection: 'offline' }],
    });
    const { store } = view();
    try {
      await screen.findByText('Phone');
      expect(screen.getByRole('button', { name: 'Pause all devices' })).toBeVisible();
      expect(screen.queryByRole('button', { name: 'Remove all devices…' })).toBeNull();
    } finally {
      store.dispose();
    }
  });

  it('removes with confirmation, pauses and resumes directly, and confirms remove all', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'sharing',
      enabled: true,
      devices: [
        { id: 'device-1', name: 'Phone', connection: 'direct' },
        { id: 'device-2', name: 'Laptop', connection: 'offline' },
      ],
    });
    const { store } = view();
    try {
      await screen.findByText('Phone');
      expect(screen.getAllByText('Connected now')).toHaveLength(1);
      fireEvent.click(screen.getAllByRole('button', { name: 'Remove' })[0]);
      expect(linkedDevicesAPI.revokeLinkedDevicesDevice).not.toHaveBeenCalled();
      fireEvent.click(
        within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Remove' }),
      );
      await waitFor(() =>
        expect(linkedDevicesAPI.revokeLinkedDevicesDevice).toHaveBeenCalledWith(
          store.api,
          'device-1',
        ),
      );

      fireEvent.click(screen.getByRole('button', { name: 'Pause all devices' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.stopLinkedDevices).toHaveBeenCalledWith(store.api),
      );

      fireEvent.click(screen.getByRole('button', { name: 'Remove all devices…' }));
      expect(
        screen.getByText('Every linked device, invitation, and pending request will be removed.'),
      ).toBeVisible();
      const confirmation = screen.getByRole('alertdialog');
      expect(within(confirmation).getByRole('button', { name: 'Cancel' })).toHaveFocus();
      expect(screen.queryByRole('button', { name: 'Link a device' })).toBeNull();
      fireEvent.click(within(confirmation).getByRole('button', { name: 'Remove all' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.resetLinkedDevices).toHaveBeenCalledWith(store.api),
      );
    } finally {
      store.dispose();
    }

    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'stopped',
      devices: [{ id: 'device-1', name: 'Phone', connection: 'offline' }],
    });
    const resumed = view();
    try {
      await screen.findByText('Paused', { selector: '.linked-devices-state strong' });
      fireEvent.click(screen.getByRole('button', { name: 'Resume' }));
      await waitFor(() =>
        expect(linkedDevicesAPI.resumeLinkedDevices).toHaveBeenCalledWith(resumed.store.api),
      );
    } finally {
      resumed.store.dispose();
    }
  });

  it('cancels a confirmation with Escape without closing Settings', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'sharing',
      enabled: true,
      devices: [{ id: 'device-1', name: 'Phone', connection: 'offline' }],
    });
    const { store } = view();
    try {
      await screen.findByText('Phone');
      const remove = screen.getByRole('button', { name: 'Remove' });
      remove.focus();
      fireEvent.click(remove);
      const confirmation = screen.getByRole('alertdialog');
      expect(confirmation).toHaveAccessibleDescription('It will no longer be able to connect.');
      expect(document.querySelector('.linked-devices-body')).toHaveAttribute('inert');
      const outside = vi.fn();
      document.addEventListener('keydown', outside);
      fireEvent.keyDown(within(confirmation).getByRole('button', { name: 'Cancel' }), {
        key: 'Escape',
      });
      document.removeEventListener('keydown', outside);
      expect(outside).not.toHaveBeenCalled();
      expect(screen.queryByRole('alertdialog')).toBeNull();
      expect(screen.getByRole('button', { name: 'Remove' })).toHaveFocus();
    } finally {
      store.dispose();
    }
  });

  it('restores focus once a confirmed removal finishes', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'sharing',
      enabled: true,
      devices: [{ id: 'device-1', name: 'Phone', connection: 'offline' }],
    });
    let finish: () => void = () => {};
    vi.mocked(linkedDevicesAPI.revokeLinkedDevicesDevice).mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve;
        }),
    );
    const { store } = view();
    try {
      await screen.findByText('Phone');
      fireEvent.click(screen.getByRole('button', { name: 'Remove' }));
      vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
        ...offStatus,
        state: 'sharing',
        enabled: true,
      });
      fireEvent.click(
        within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Remove' }),
      );
      expect(await screen.findByRole('button', { name: 'Removing…' })).toBeDisabled();
      await act(async () => finish());
      expect(await screen.findByText('No devices yet')).toBeVisible();
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Link a device' })).toHaveFocus(),
      );
    } finally {
      store.dispose();
    }
  });

  it('offers one reset when unavailable with devices', async () => {
    vi.mocked(linkedDevicesAPI.getLinkedDevices).mockResolvedValue({
      ...offStatus,
      state: 'unavailable',
      reset_required: true,
      devices: [
        { id: 'device-1', name: 'Phone', connection: 'offline' },
        { id: 'device-2', name: 'Laptop', connection: 'offline' },
      ],
    });
    const { store } = view();
    try {
      await screen.findByText('Phone');
      expect(
        screen.getAllByRole('button', { name: /Remove all devices|Reset linked devices/ }),
      ).toHaveLength(1);
    } finally {
      store.dispose();
    }
  });
});
