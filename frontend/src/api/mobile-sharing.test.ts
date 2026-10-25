import { describe, expect, it, vi } from 'vitest';
import type { APIClient } from './client';
import {
  approveMobileSharingRequest,
  cancelMobileSharingInvitation,
  createMobileSharingInvitation,
  denyMobileSharingRequest,
  getMobileSharing,
  resetMobileSharing,
  resumeMobileSharing,
  revokeMobileSharingDevice,
  stopMobileSharing,
} from './mobile-sharing';

function fixture() {
  const get = vi.fn().mockResolvedValue({ state: 'off', pending_requests: [], devices: [] });
  const post = vi.fn().mockResolvedValue(undefined);
  const remove = vi.fn().mockResolvedValue(undefined);
  return {
    api: { get, post, delete: remove } as unknown as APIClient,
    get,
    post,
    remove,
  };
}

describe('mobile sharing API', () => {
  it('uses the authenticated status endpoint and exact mutation routes', async () => {
    const { api, get, post, remove } = fixture();
    const controller = new AbortController();

    await getMobileSharing(api, controller.signal);
    await createMobileSharingInvitation(api);
    await cancelMobileSharingInvitation(api);
    await approveMobileSharingRequest(api, 'request/one', 'ABCD-1234');
    await denyMobileSharingRequest(api, 'request/two');
    await revokeMobileSharingDevice(api, 'device/one');
    await stopMobileSharing(api);
    await resumeMobileSharing(api);
    await resetMobileSharing(api);

    expect(get).toHaveBeenCalledWith('/v1/mobile-sharing', controller.signal, { auth: 'session' });
    expect(post).toHaveBeenNthCalledWith(1, '/v1/mobile-sharing/invitations', {});
    expect(post).toHaveBeenNthCalledWith(2, '/v1/mobile-sharing/requests/request%2Fone/approve', {
      code: 'ABCD-1234',
    });
    expect(post).toHaveBeenNthCalledWith(3, '/v1/mobile-sharing/requests/request%2Ftwo/deny', {});
    expect(post).toHaveBeenNthCalledWith(4, '/v1/mobile-sharing/stop', {});
    expect(post).toHaveBeenNthCalledWith(5, '/v1/mobile-sharing/resume', {});
    expect(post).toHaveBeenNthCalledWith(6, '/v1/mobile-sharing/reset', {});
    expect(remove).toHaveBeenNthCalledWith(1, '/v1/mobile-sharing/invitations/current');
    expect(remove).toHaveBeenNthCalledWith(2, '/v1/mobile-sharing/devices/device%2Fone');
  });
});
