import { describe, expect, it } from 'vitest';
import {
  base64urlToBytes,
  bytesToBase64url,
  decodeCreationOptions,
  decodeRequestOptions,
  passkeyErrorMessage,
  serializeCredential,
} from './passkeys';

describe('Hub passkey platform', () => {
  it('round-trips base64url and decodes wire options', () => {
    const bytes = Uint8Array.from([0, 1, 2, 253, 254, 255]);
    expect(bytesToBase64url(bytes)).toBe('AAEC_f7_');
    expect([...base64urlToBytes('AAEC_f7_')]).toEqual([...bytes]);
    const creation = decodeCreationOptions({
      publicKey: {
        challenge: 'AAEC',
        rp: { name: 'Hub' },
        user: { id: '_w', name: 'admin', displayName: 'Admin' },
        pubKeyCredParams: [],
        excludeCredentials: [{ type: 'public-key', id: 'AQ' }],
      },
    });
    expect([...new Uint8Array(creation.publicKey!.challenge as ArrayBuffer)]).toEqual([0, 1, 2]);
    expect([...new Uint8Array(creation.publicKey!.user.id as ArrayBuffer)]).toEqual([255]);
    expect([
      ...new Uint8Array(creation.publicKey!.excludeCredentials![0].id as ArrayBuffer),
    ]).toEqual([1]);
    const request = decodeRequestOptions({
      publicKey: { challenge: 'AQ', allowCredentials: [{ type: 'public-key', id: 'Ag' }] },
    });
    expect([...new Uint8Array(request.publicKey!.allowCredentials![0].id as ArrayBuffer)]).toEqual([
      2,
    ]);
  });

  it('serializes assertion credentials without exposing binary values', () => {
    const credential = {
      id: 'cred',
      rawId: Uint8Array.from([0, 1]).buffer,
      type: 'public-key',
      response: {
        clientDataJSON: Uint8Array.from([1]).buffer,
        authenticatorData: Uint8Array.from([2]).buffer,
        signature: Uint8Array.from([3]).buffer,
        userHandle: null,
      },
      authenticatorAttachment: 'platform',
      getClientExtensionResults: () => ({}),
    } as unknown as PublicKeyCredential;
    expect(serializeCredential(credential)).toMatchObject({
      id: 'cred',
      rawId: 'AAE',
      response: {
        clientDataJSON: 'AQ',
        authenticatorData: 'Ag',
        signature: 'Aw',
        userHandle: null,
      },
    });
  });

  it('maps passkey cancellation without hiding ordinary errors', () => {
    expect(passkeyErrorMessage({ name: 'NotAllowedError' })).toMatch(/cancelled or timed out/);
    expect(passkeyErrorMessage(new Error('unsupported browser'))).toBe('unsupported browser');
  });
});
