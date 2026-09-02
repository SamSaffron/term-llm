import { describe, expect, it, vi } from 'vitest';
import type { SerializedPublicKeyCredential } from '../hub/domain/types';
import { HubAPIError, HubClient } from './hub-client';

function jsonResponse(body: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

describe('HubClient', () => {
  it('owns mounted same-origin JSON transport for reads and mutations', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => jsonResponse({ nodes: [] }));
    const client = new HubClient(
      { basePath: '/hub', authMode: 'bearer' },
      { fetch: fetcher as unknown as typeof fetch },
    );
    await client.listNodes();
    expect(fetcher).toHaveBeenCalledOnce();
    const url = fetcher.mock.calls[0][0];
    const request = fetcher.mock.calls[0][1]!;
    expect(url).toBe('/hub/api/nodes');
    expect(request).toMatchObject({ method: 'GET', credentials: 'same-origin' });
    expect((request.headers as Headers).get('Accept')).toBe('application/json');

    fetcher.mockResolvedValueOnce(jsonResponse({ id: 'alpha' }));
    await client.addNode({ name: 'Alpha', url: 'http://node.test', token: 'secret' });
    const mutationURL = fetcher.mock.calls[1][0];
    const mutation = fetcher.mock.calls[1][1]!;
    expect(mutationURL).toBe('/hub/api/nodes');
    expect(mutation).toMatchObject({
      method: 'POST',
      body: JSON.stringify({ name: 'Alpha', url: 'http://node.test', token: 'secret' }),
    });
    expect((mutation.headers as Headers).get('Content-Type')).toBe('application/json');
  });

  it('forwards cancellation to reads and never retries a failed mutation', async () => {
    const controller = new AbortController();
    const fetcher = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(jsonResponse({ nodes: [] }))
      .mockRejectedValueOnce(new Error('network failed'));
    const client = new HubClient(
      { basePath: '/hub', authMode: 'none' },
      { fetch: fetcher as unknown as typeof fetch },
    );

    await client.listNodes(controller.signal);
    expect(fetcher.mock.calls[0][1]?.signal).toBe(controller.signal);
    await expect(
      client.addNode({ name: 'Alpha', url: 'http://node.test', token: '' }),
    ).rejects.toThrow('network failed');
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it('keeps every task route under the injected mount', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => jsonResponse({}));
    const client = new HubClient(
      { basePath: '/ops', authMode: 'none' },
      { fetch: fetcher as unknown as typeof fetch },
    );
    const credential = { id: 'credential' } as SerializedPublicKeyCredential;
    const calls: Array<() => Promise<unknown>> = [
      () => client.listAttention(),
      () => client.listDelegations(),
      () => client.testNode({ name: '', url: '', token: '' }),
      () => client.removeNode('a/b'),
      () => client.registrationInfo(),
      () => client.listCredentials(),
      () => client.session(),
      () => client.renameCredential('record/id', 'New'),
      () => client.removeCredential('record/id'),
      () => client.revokeOtherSessions(),
      () => client.logout(),
      () => client.verifyGrant('/api/auth/recovery', 'code'),
      () => client.beginGrantRegistration('/api/auth/bootstrap', 'Primary'),
      () => client.finishGrantRegistration('/api/auth/recovery', credential),
      () => client.beginLogin('/hub/node/alpha/'),
      () => client.finishLogin(credential),
      () => client.beginReauthentication(),
      () => client.finishReauthentication(credential),
      () => client.beginAdditionalRegistration('Backup'),
      () => client.finishAdditionalRegistration(credential),
    ];
    for (const call of calls) await call();
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([
      '/ops/api/attention',
      '/ops/api/delegations',
      '/ops/api/nodes/test',
      '/ops/api/nodes/a%2Fb',
      '/ops/api/registration-info',
      '/ops/api/auth/credentials',
      '/ops/api/auth/session',
      '/ops/api/auth/credentials/record%2Fid',
      '/ops/api/auth/credentials/record%2Fid',
      '/ops/api/auth/sessions/revoke-others',
      '/ops/api/auth/logout',
      '/ops/api/auth/recovery/verify',
      '/ops/api/auth/bootstrap/register/begin',
      '/ops/api/auth/recovery/register/finish',
      '/ops/api/auth/login/begin',
      '/ops/api/auth/login/finish',
      '/ops/api/auth/reauth/begin',
      '/ops/api/auth/reauth/finish',
      '/ops/api/auth/credentials/register/begin',
      '/ops/api/auth/credentials/register/finish',
    ]);
  });

  it('decodes structured and plain-text errors and rejects malformed success bodies', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ error: { message: 'specific failure' } }, 409))
      .mockResolvedValueOnce(new Response('plain failure', { status: 400 }))
      .mockResolvedValueOnce(
        new Response('{', { status: 200, headers: { 'Content-Type': 'application/json' } }),
      );
    const client = new HubClient(
      { basePath: '', authMode: 'none' },
      { fetch: fetcher as unknown as typeof fetch },
    );
    await expect(client.listNodes()).rejects.toEqual(
      expect.objectContaining({ name: 'HubAPIError', status: 409, message: 'specific failure' }),
    );
    await expect(client.listNodes()).rejects.toThrow('plain failure');
    await expect(client.listNodes()).rejects.toThrow('empty response');
  });

  it('redirects protected passkey 401s without relying on the login header', async () => {
    const navigate = vi.fn();
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ error: { message: 'expired' } }, 401))
      .mockResolvedValueOnce(
        jsonResponse({ error: { message: 'expired' } }, 401, {
          'X-Term-LLM-Login-URL': '/custom/login',
        }),
      )
      .mockResolvedValueOnce(
        jsonResponse({ error: { message: 'expired' } }, 401, {
          'X-Term-LLM-Login-URL': '//evil.example/login',
        }),
      )
      .mockResolvedValueOnce(
        jsonResponse({ error: { message: 'expired' } }, 401, {
          'X-Term-LLM-Login-URL': '/\\evil.example/login',
        }),
      );
    const client = new HubClient(
      { basePath: '/hub', authMode: 'passkey' },
      { fetch: fetcher as unknown as typeof fetch, navigate },
    );
    await expect(client.listNodes()).rejects.toBeInstanceOf(HubAPIError);
    await expect(client.listCredentials()).rejects.toBeInstanceOf(HubAPIError);
    await expect(client.listNodes()).rejects.toBeInstanceOf(HubAPIError);
    await expect(client.listNodes()).rejects.toBeInstanceOf(HubAPIError);
    expect(navigate.mock.calls).toEqual([
      ['/hub/auth/login'],
      ['/custom/login'],
      ['/hub/auth/login'],
      ['/hub/auth/login'],
    ]);
  });

  it('does not turn an invalid public setup grant into a login redirect', async () => {
    const navigate = vi.fn();
    const fetcher = vi.fn(async () => jsonResponse({ error: { message: 'invalid code' } }, 401));
    const client = new HubClient(
      { basePath: '/hub', authMode: 'passkey' },
      { fetch: fetcher as unknown as typeof fetch, navigate },
    );
    await expect(client.verifyGrant('/api/auth/bootstrap', 'bad')).rejects.toThrow('invalid code');
    expect(navigate).not.toHaveBeenCalled();
  });
});
