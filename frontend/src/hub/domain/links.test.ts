import { describe, expect, it } from 'vitest';
import { firstDelegationArtifact, safeArtifactURL } from './links';

const delegation = { target_node: 'alpha' };
const nodes = [{ id: 'alpha', base_path: '/chat' }];

describe('Hub delegation links', () => {
  it('preserves mounted links and mounts bare node links', () => {
    expect(
      safeArtifactURL(
        '/hub/node/alpha/image.png',
        delegation,
        nodes,
        '/hub',
        'https://hub.test/hub/',
      ),
    ).toBe('/hub/node/alpha/image.png');
    expect(
      safeArtifactURL('/node/alpha/image.png', delegation, nodes, '/hub', 'https://hub.test/hub/'),
    ).toBe('/hub/node/alpha/image.png');
    expect(
      safeArtifactURL(
        '/hub/node/beta/image.png',
        delegation,
        nodes,
        '/hub',
        'https://hub.test/hub/',
      ),
    ).toBe('');
    expect(
      safeArtifactURL('/node/beta/image.png', delegation, nodes, '/hub', 'https://hub.test/hub/'),
    ).toBe('');
    expect(
      safeArtifactURL(
        '/hub/node/%62eta/image.png',
        delegation,
        nodes,
        '/hub',
        'https://hub.test/hub/',
      ),
    ).toBe('');
  });

  it('strips the target base path before rebasing root-absolute artifacts', () => {
    expect(
      safeArtifactURL('/chat/files/a.png', delegation, nodes, '/hub', 'https://hub.test/hub/'),
    ).toBe('/hub/node/alpha/files/a.png');
    expect(safeArtifactURL('/chat', delegation, nodes, '/hub', 'https://hub.test/hub/')).toBe(
      '/hub/node/alpha/',
    );
  });

  it('allows HTTP(S) and rejects malformed or active schemes', () => {
    expect(
      safeArtifactURL('https://cdn.test/a.png', delegation, nodes, '/hub', 'https://hub.test/hub/'),
    ).toBe('https://cdn.test/a.png');
    expect(
      safeArtifactURL('javascript:alert(1)', delegation, nodes, '/hub', 'https://hub.test/hub/'),
    ).toBe('');
    expect(
      safeArtifactURL('//evil.example/a.png', delegation, nodes, '/hub', 'https://hub.test/hub/'),
    ).toBe('');
    expect(
      safeArtifactURL('/\\evil.example/a.png', delegation, nodes, '/hub', 'https://hub.test/hub/'),
    ).toBe('');
    expect(safeArtifactURL('http://[', delegation, nodes, '/hub', 'https://hub.test/hub/')).toBe(
      '',
    );
  });

  it('extracts at most the first safe image or link', () => {
    expect(
      firstDelegationArtifact(
        '![chart](/chat/a.png) [later](https://other.test)',
        delegation,
        nodes,
        '/hub',
        'https://hub.test/hub/',
      ),
    ).toEqual({ type: 'image', url: '/hub/node/alpha/a.png', label: 'chart' });
    expect(
      firstDelegationArtifact(
        '[report](javascript:bad) [safe](https://safe.test)',
        delegation,
        nodes,
        '/hub',
        'https://hub.test/hub/',
      ),
    ).toBeNull();
  });
});
