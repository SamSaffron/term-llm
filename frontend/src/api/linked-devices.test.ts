import { describe, expect, it, vi } from 'vitest';
import type { APIClient } from './client';
import {
  approveLinkedDevicesRequest,
  cancelLinkedDevicesInvitation,
  createLinkedDevicesInvitation,
  denyLinkedDevicesRequest,
  getLinkedDevices,
  resetLinkedDevices,
  resumeLinkedDevices,
  revokeLinkedDevicesDevice,
  stopLinkedDevices,
} from './linked-devices';

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

describe('linked devices API', () => {
  it('uses the authenticated status endpoint and exact mutation routes', async () => {
    const { api, get, post, remove } = fixture();
    const controller = new AbortController();

    await getLinkedDevices(api, controller.signal);
    await createLinkedDevicesInvitation(api);
    await cancelLinkedDevicesInvitation(api);
    await approveLinkedDevicesRequest(api, 'request/one', 'ABCD-1234');
    await denyLinkedDevicesRequest(api, 'request/two');
    await revokeLinkedDevicesDevice(api, 'device/one');
    await stopLinkedDevices(api);
    await resumeLinkedDevices(api);
    await resetLinkedDevices(api);

    expect(get).toHaveBeenCalledWith('/v1/linked-devices', controller.signal, { auth: 'session' });
    expect(post).toHaveBeenNthCalledWith(1, '/v1/linked-devices/invitations', {});
    expect(post).toHaveBeenNthCalledWith(2, '/v1/linked-devices/requests/request%2Fone/approve', {
      code: 'ABCD-1234',
    });
    expect(post).toHaveBeenNthCalledWith(3, '/v1/linked-devices/requests/request%2Ftwo/deny', {});
    expect(post).toHaveBeenNthCalledWith(4, '/v1/linked-devices/stop', {});
    expect(post).toHaveBeenNthCalledWith(5, '/v1/linked-devices/resume', {});
    expect(post).toHaveBeenNthCalledWith(6, '/v1/linked-devices/reset', {});
    expect(remove).toHaveBeenNthCalledWith(1, '/v1/linked-devices/invitations/current');
    expect(remove).toHaveBeenNthCalledWith(2, '/v1/linked-devices/devices/device%2Fone');
  });
});
