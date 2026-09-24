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
    const storage = memoryStorage(new Map([[`${grantVerifiedStorageKey}:/hub`, '1']]));
    const client = {
      config: { basePath: '/hub' },
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
    await store.submit('setup', {
      code: 'new-code',
      displayName: 'Primary',
      returnPath: '/hub/auth/native/challenge',
    });
    expect(client.beginGrantRegistration).toHaveBeenCalledTimes(2);
    expect(client.verifyGrant).toHaveBeenCalledWith('/api/auth/bootstrap', 'new-code');
    expect(client.finishGrantRegistration).toHaveBeenCalledOnce();
    expect(client.finishGrantRegistration).toHaveBeenCalledWith(
      '/api/auth/bootstrap',
      { id: 'credential' },
      '/hub/auth/native/challenge',
    );
    expect(storage.getItem(`${grantVerifiedStorageKey}:/hub`)).toBeNull();
    expect(navigate).toHaveBeenCalledWith('/hub/');
  });

  it('follows the server-validated setup redirect rather than the requested path', async () => {
    const client = {
      config: { basePath: '/hub' },
      beginGrantRegistration: vi.fn(async () => ({ publicKey: {} })),
      verifyGrant: vi.fn(async () => ({ ok: true })),
      finishGrantRegistration: vi.fn(async () => ({ redirect: '/hub/' })),
    } as unknown as HubClient;
    const platform = {
      available: () => true,
      create: vi.fn(async () => ({ id: 'credential' })),
    } as unknown as PasskeyPlatform;
    const navigate = vi.fn();
    const store = new AuthStore(client, platform, memoryStorage(), navigate);
    await store.submit('setup', {
      code: 'new-code',
      displayName: 'Primary',
      returnPath: `/hub/auth/native/${'A'.repeat(43)}`,
    });
    expect(client.finishGrantRegistration).toHaveBeenCalledWith(
      '/api/auth/bootstrap',
      { id: 'credential' },
      `/hub/auth/native/${'A'.repeat(43)}`,
    );
    expect(navigate).toHaveBeenLastCalledWith('/hub/');
  });

  it('maps passkey cancellation and allows another submission', async () => {
    const client = {
      config: { basePath: '/hub' },
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

  it('approves a native sign-in directly when the session is recently authenticated', async () => {
    const client = {
      config: { basePath: '/hub' },
      authorizeNative: vi.fn(async () => ({ redirect: 'termllm-auth://callback?code=c&state=s' })),
      beginReauthentication: vi.fn(),
    } as unknown as HubClient;
    const platform = { available: () => true, get: vi.fn() } as unknown as PasskeyPlatform;
    const navigate = vi.fn();
    const store = new AuthStore(client, platform, memoryStorage(), navigate);
    await store.submit('native', { code: '', displayName: '', returnPath: '/', challenge: 'chal' });
    expect(client.authorizeNative).toHaveBeenCalledWith('chal');
    expect(client.beginReauthentication).not.toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith('termllm-auth://callback?code=c&state=s');
    expect(store.handedOff.value).toBe(true);
    expect(store.busy.value).toBe(false);
  });

  it('performs a passkey reauthentication before retrying native approval', async () => {
    const client = {
      config: { basePath: '/hub' },
      authorizeNative: vi
        .fn()
        .mockRejectedValueOnce(new HubAPIError(403, 'recent', 'recent_auth_required'))
        .mockResolvedValueOnce({ redirect: 'termllm-auth://callback?code=c&state=s' }),
      beginReauthentication: vi.fn(async () => ({ publicKey: {} })),
      finishReauthentication: vi.fn(async () => ({ ok: true })),
    } as unknown as HubClient;
    const platform = {
      available: () => true,
      get: vi.fn(async () => ({ id: 'credential' })),
    } as unknown as PasskeyPlatform;
    const navigate = vi.fn();
    const store = new AuthStore(client, platform, memoryStorage(), navigate);
    await store.submit('native', { code: '', displayName: '', returnPath: '/', challenge: 'chal' });
    expect(client.finishReauthentication).toHaveBeenCalledOnce();
    expect(client.authorizeNative).toHaveBeenCalledTimes(2);
    expect(navigate).toHaveBeenCalledWith('termllm-auth://callback?code=c&state=s');
  });

  it('does not reauthenticate for other native approval failures', async () => {
    const client = {
      config: { basePath: '/hub' },
      authorizeNative: vi.fn(async () => {
        throw new HubAPIError(403, 'request origin is not allowed', 'invalid_origin');
      }),
      beginReauthentication: vi.fn(),
    } as unknown as HubClient;
    const platform = { available: () => true } as unknown as PasskeyPlatform;
    const navigate = vi.fn();
    const store = new AuthStore(client, platform, memoryStorage(), navigate);
    await store.submit('native', { code: '', displayName: '', returnPath: '/', challenge: 'chal' });
    expect(client.beginReauthentication).not.toHaveBeenCalled();
    expect(navigate).not.toHaveBeenCalled();
    expect(store.busy.value).toBe(false);
    expect(store.handedOff.value).toBe(false);
  });

  it('restarts native approval through sign-in when the browser session ends', async () => {
    const challenge = 'A'.repeat(43);
    for (const failing of ['authorize', 'reauth'] as const) {
      const client = {
        config: { basePath: '/hub' },
        authorizeNative: vi.fn(async () => {
          throw failing === 'authorize'
            ? new HubAPIError(401, 'expired', 'invalid_session')
            : new HubAPIError(403, 'recent', 'recent_auth_required');
        }),
        beginReauthentication: vi.fn(async () => {
          throw new HubAPIError(401, 'expired', 'invalid_session');
        }),
      } as unknown as HubClient;
      const platform = { available: () => true } as unknown as PasskeyPlatform;
      const navigate = vi.fn();
      const store = new AuthStore(client, platform, memoryStorage(), navigate);
      await store.submit('native', { code: '', displayName: '', returnPath: '/', challenge });
      expect(navigate).toHaveBeenCalledOnce();
      expect(navigate).toHaveBeenCalledWith(`/hub/auth/native/${challenge}`);
      expect(store.handedOff.value).toBe(false);
      if (failing === 'reauth') expect(client.beginReauthentication).toHaveBeenCalledWith(false);
    }
  });

  it('keeps ordinary logins busy while the browser navigates away', async () => {
    const client = {
      config: { basePath: '/hub' },
      beginLogin: vi.fn(async () => ({ publicKey: {} })),
      finishLogin: vi.fn(async () => ({ redirect: '/hub/' })),
    } as unknown as HubClient;
    const platform = {
      available: () => true,
      get: vi.fn(async () => ({ id: 'credential' })),
    } as unknown as PasskeyPlatform;
    const store = new AuthStore(client, platform, memoryStorage(), vi.fn());
    await store.submit('login', { code: '', displayName: '', returnPath: '/hub/' });
    expect(store.handedOff.value).toBe(false);
    expect(store.busy.value).toBe(true);
  });
});
