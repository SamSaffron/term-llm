import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as mobileAPI from '../api/mobile-sharing';
import { StoreContext } from '../app/context';
import * as clipboard from '../platform/clipboard';
import { AppStore } from '../stores/app-store';
import { testConfig } from '../stores/store-test-fixtures';
import { MOBILE_SHARING_POLL_MS, MobileSharingSettings } from './MobileSharingSettings';

vi.mock('../api/mobile-sharing', async (loadOriginal) => {
  const original = await loadOriginal<typeof import('../api/mobile-sharing')>();
  return {
    ...original,
    getMobileSharing: vi.fn(),
    createMobileSharingInvitation: vi.fn(),
    cancelMobileSharingInvitation: vi.fn(),
    approveMobileSharingRequest: vi.fn(),
    denyMobileSharingRequest: vi.fn(),
    revokeMobileSharingDevice: vi.fn(),
    stopMobileSharing: vi.fn(),
    resumeMobileSharing: vi.fn(),
    resetMobileSharing: vi.fn(),
  };
});

const offStatus: mobileAPI.MobileSharingStatus = {
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
      <MobileSharingSettings active={active} />
    </StoreContext.Provider>,
  );
  return { store, ...result };
}

beforeEach(() => {
  vi.mocked(mobileAPI.getMobileSharing).mockResolvedValue(offStatus);
  vi.mocked(mobileAPI.createMobileSharingInvitation).mockResolvedValue(offStatus);
  for (const operation of [
    mobileAPI.cancelMobileSharingInvitation,
    mobileAPI.approveMobileSharingRequest,
    mobileAPI.denyMobileSharingRequest,
    mobileAPI.revokeMobileSharingDevice,
    mobileAPI.stopMobileSharing,
    mobileAPI.resumeMobileSharing,
    mobileAPI.resetMobileSharing,
  ]) {
    vi.mocked(operation).mockResolvedValue(undefined);
  }
});

afterEach(() => {
  vi.clearAllMocks();
  vi.useRealTimers();
});

describe('MobileSharingSettings', () => {
  it('polls only while its settings tab is active', async () => {
    vi.useFakeTimers();
    const { store, rerender } = view(false);
    try {
      await act(async () => {});
      expect(mobileAPI.getMobileSharing).not.toHaveBeenCalled();

      rerender(
        <StoreContext.Provider value={store}>
          <MobileSharingSettings active />
        </StoreContext.Provider>,
      );
      await act(async () => {});
      expect(mobileAPI.getMobileSharing).toHaveBeenCalledOnce();

      rerender(
        <StoreContext.Provider value={store}>
          <MobileSharingSettings active={false} />
        </StoreContext.Provider>,
      );
      await act(async () => {
        await vi.advanceTimersByTimeAsync(MOBILE_SHARING_POLL_MS * 2);
      });
      expect(mobileAPI.getMobileSharing).toHaveBeenCalledOnce();
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
    ['stopped', 'Stopped'],
  ] as const)('renders the honest %s state from the server', async (state, label) => {
    vi.mocked(mobileAPI.getMobileSharing).mockResolvedValue({
      ...offStatus,
      state,
      reason: state === 'unavailable' ? 'Not supported by this server.' : undefined,
    });
    const { store } = view();
    try {
      expect(
        await screen.findByText(label, { selector: '.mobile-sharing-state strong' }),
      ).toBeVisible();
      expect(screen.getByText(/same UID/i)).toBeVisible();
      if (state === 'unavailable')
        expect(screen.getByText('Not supported by this server.')).toBeVisible();
    } finally {
      store.dispose();
    }
  });

  it('uses the backend QR, warns before copying, and submits the entered SAS code', async () => {
    const status: mobileAPI.MobileSharingStatus = {
      ...offStatus,
      state: 'pending',
      enabled: true,
      invitation: {
        url: 'https://example.test/invite/secret',
        qr_data_url: 'data:image/png;base64,cXItYnl0ZXM=',
        expires_at: '2099-01-01T00:00:00Z',
      },
      pending_requests: [{ id: 'request-1', name: 'Sam’s phone' }],
      devices: [],
    };
    vi.mocked(mobileAPI.getMobileSharing).mockResolvedValue(status);
    const copy = vi.spyOn(clipboard, 'copyText').mockResolvedValue(undefined);
    const { store } = view();
    try {
      expect(await screen.findByRole('img', { name: /QR code/ })).toHaveAttribute(
        'src',
        status.invitation!.qr_data_url,
      );
      expect(screen.getByRole('note')).toHaveTextContent('Treat the copied link like a password');
      fireEvent.click(screen.getByRole('button', { name: 'Copy invitation link' }));
      await waitFor(() => expect(copy).toHaveBeenCalledWith(status.invitation!.url));

      const approve = screen.getByRole('button', { name: 'Approve' });
      expect(approve).toBeDisabled();
      fireEvent.input(screen.getByLabelText('One-time SAS code'), {
        target: { value: 'ab12-cd34' },
      });
      expect(approve).toBeEnabled();
      fireEvent.click(approve);
      await waitFor(() =>
        expect(mobileAPI.approveMobileSharingRequest).toHaveBeenCalledWith(
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

  it('revokes devices and requires explicit stop, resume, and reset confirmations', async () => {
    vi.mocked(mobileAPI.getMobileSharing).mockResolvedValue({
      ...offStatus,
      state: 'sharing',
      enabled: true,
      devices: [{ id: 'device-1', name: 'Phone', connection: 'direct' }],
    });
    const { store } = view();
    try {
      await screen.findByText('Phone');
      fireEvent.click(screen.getByRole('button', { name: 'Revoke' }));
      expect(mobileAPI.revokeMobileSharingDevice).not.toHaveBeenCalled();
      fireEvent.click(screen.getByRole('button', { name: 'Confirm revoke' }));
      await waitFor(() =>
        expect(mobileAPI.revokeMobileSharingDevice).toHaveBeenCalledWith(store.api, 'device-1'),
      );

      fireEvent.click(screen.getByRole('button', { name: 'Stop mobile sharing…' }));
      expect(mobileAPI.stopMobileSharing).not.toHaveBeenCalled();
      fireEvent.click(screen.getByRole('button', { name: 'Confirm stop' }));
      await waitFor(() => expect(mobileAPI.stopMobileSharing).toHaveBeenCalledWith(store.api));

      fireEvent.click(screen.getByRole('button', { name: 'Forget all phones…' }));
      expect(
        screen.getByText(
          'All invitations, pending requests, and approved devices will be removed.',
        ),
      ).toBeVisible();
      fireEvent.click(screen.getByRole('button', { name: 'Confirm reset' }));
      await waitFor(() => expect(mobileAPI.resetMobileSharing).toHaveBeenCalledWith(store.api));
    } finally {
      store.dispose();
    }

    vi.mocked(mobileAPI.getMobileSharing).mockResolvedValue({
      ...offStatus,
      state: 'stopped',
    });
    const resumed = view();
    try {
      await screen.findByText('Stopped', { selector: '.mobile-sharing-state strong' });
      fireEvent.click(screen.getByRole('button', { name: 'Resume sharing…' }));
      expect(mobileAPI.resumeMobileSharing).not.toHaveBeenCalled();
      fireEvent.click(screen.getByRole('button', { name: 'Confirm resume' }));
      await waitFor(() =>
        expect(mobileAPI.resumeMobileSharing).toHaveBeenCalledWith(resumed.store.api),
      );
    } finally {
      resumed.store.dispose();
    }
  });
});
