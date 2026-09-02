import { describe, expect, it } from 'vitest';
import { hubPath, normalizeHubBasePath, parseHubConfig, readHubConfig } from './config';

describe('Hub config', () => {
  it('normalizes root and mounted paths', () => {
    expect(normalizeHubBasePath('/')).toBe('');
    expect(normalizeHubBasePath('/hub/')).toBe('/hub');
    expect(hubPath('/hub/', 'api/nodes')).toBe('/hub/api/nodes');
  });

  it('rejects unsafe or incomplete records', () => {
    expect(() => normalizeHubBasePath('hub')).toThrow('invalid');
    expect(() => normalizeHubBasePath('/../hub')).toThrow('invalid');
    expect(() => parseHubConfig({ page: 'dashboard', authMode: 'magic' })).toThrow(
      'authentication mode',
    );
    expect(() =>
      parseHubConfig({ page: 'passkey-auth', authMode: 'passkey', basePath: '' }),
    ).toThrow('passkey page configuration');
  });

  it('reads escaped server configuration from the mount data attribute', () => {
    const root = document.createElement('div');
    root.dataset.hubConfig = JSON.stringify({
      page: 'dashboard',
      authMode: 'none',
      basePath: '/hub/',
      canAddNodes: true,
      passkeyAuth: false,
    });
    expect(readHubConfig(root)).toMatchObject({
      page: 'dashboard',
      authMode: 'none',
      basePath: '/hub',
      canAddNodes: true,
    });
  });

  it('requires a root-absolute bearer form action', () => {
    for (const formAction of ['relative', '//evil.example/login', '/\\evil.example/login']) {
      expect(() =>
        parseHubConfig({
          page: 'bearer-login',
          authMode: 'bearer',
          basePath: '',
          formAction,
        }),
      ).toThrow('form action');
    }
  });
});
