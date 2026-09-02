import { describe, expect, it, vi } from 'vitest';
import { HubAPIError, type HubClient } from '../../api/hub-client';
import type { PasskeyPlatform } from '../platform/passkeys';
import { AuthStore, grantVerifiedStorageKey } from './auth-store';

function memoryStorage(initial = new Map<string, string>()) {
  return {
    getItem: (key: string) => initial.get(key) ?? null,
    setItem: (key: string, value: string) => initial.set(key, value),
    removeItem: (key: string) => initial.delete(key),
    values: initial,
  };
}

describe('Hub AuthStore', () => {
  it('re-verifies once when a remembered grant has expired', async () => {
    const storage = memoryStorage(new Map([[grantVerifiedStorageKey, '1']]));
    const client = {
      beginGrantRegistration: vi
        .fn()
        .mockRejectedValueOnce(new HubAPIError(401, 'expired grant'))
        .mockResolvedValueOnce({ publicKey: {} }),
      verifyGrant: vi.fn(async () => ({ ok: true })),
      finishGrantRegistration: vi.fn(async () => ({ redirect: '/hub/' })),
    } as unknown as HubClient;
    const platform = {
      available: () => true,
      create: vi.fn(async () => ({ id: 'credential' })),
    } as unknown as PasskeyPlatform;
    const navigate = vi.fn();
    const store = new AuthStore(client, platform, storage, navigate);
    await store.submit('setup', { code: 'new-code', displayName: 'Primary', returnPath: '/' });
    expect(client.beginGrantRegistration).toHaveBeenCalledTimes(2);
    expect(client.verifyGrant).toHaveBeenCalledWith('/api/auth/bootstrap', 'new-code');
    expect(client.finishGrantRegistration).toHaveBeenCalledOnce();
    expect(storage.getItem(grantVerifiedStorageKey)).toBeNull();
    expect(navigate).toHaveBeenCalledWith('/hub/');
  });

  it('maps passkey cancellation and allows another submission', async () => {
    const client = {
      beginLogin: vi.fn(async () => ({ publicKey: {} })),
    } as unknown as HubClient;
    const platform = {
      available: () => true,
      get: vi.fn(async () => {
        throw new DOMException('cancelled', 'NotAllowedError');
      }),
    } as unknown as PasskeyPlatform;
    const store = new AuthStore(client, platform, memoryStorage(), vi.fn());
    await store.submit('login', { code: '', displayName: '', returnPath: '/hub/node/a/' });
    expect(store.error.value).toMatch(/cancelled or timed out/);
    expect(store.busy.value).toBe(false);
  });
});
