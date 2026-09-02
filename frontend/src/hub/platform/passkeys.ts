import type {
  SerializedPublicKeyCredential,
  WebAuthnCreationWireOptions,
  WebAuthnRequestWireOptions,
} from '../domain/types';

export function base64urlToBytes(value: string): Uint8Array<ArrayBuffer> {
  const normalized = String(value).replaceAll('-', '+').replaceAll('_', '/');
  const padded = normalized + '==='.slice((normalized.length + 3) % 4);
  const raw = atob(padded);
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}

export function bytesToBase64url(value: ArrayBuffer | ArrayBufferView): string {
  const bytes =
    value instanceof ArrayBuffer
      ? new Uint8Array(value)
      : new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  let raw = '';
  for (const byte of bytes) raw += String.fromCharCode(byte);
  return btoa(raw).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/g, '');
}

export function decodeCreationOptions(
  data: WebAuthnCreationWireOptions,
): CredentialCreationOptions {
  const publicKey = data.publicKey;
  return {
    publicKey: {
      ...publicKey,
      challenge: base64urlToBytes(publicKey.challenge),
      user: { ...publicKey.user, id: base64urlToBytes(publicKey.user.id) },
      excludeCredentials: publicKey.excludeCredentials?.map((credential) => ({
        ...credential,
        id: base64urlToBytes(credential.id),
      })),
    },
  };
}

export function decodeRequestOptions(data: WebAuthnRequestWireOptions): CredentialRequestOptions {
  const publicKey = data.publicKey;
  return {
    publicKey: {
      ...publicKey,
      challenge: base64urlToBytes(publicKey.challenge),
      allowCredentials: publicKey.allowCredentials?.map((credential) => ({
        ...credential,
        id: base64urlToBytes(credential.id),
      })),
    },
  };
}

function isAttestationResponse(
  response: AuthenticatorResponse,
): response is AuthenticatorAttestationResponse {
  return 'attestationObject' in response;
}

export function serializeCredential(
  credential: PublicKeyCredential,
): SerializedPublicKeyCredential {
  const response = credential.response;
  const serializedResponse: Record<string, unknown> = {
    clientDataJSON: bytesToBase64url(response.clientDataJSON),
  };
  if (isAttestationResponse(response)) {
    serializedResponse.attestationObject = bytesToBase64url(response.attestationObject);
    serializedResponse.transports = response.getTransports?.() ?? [];
  } else {
    const assertion = response as AuthenticatorAssertionResponse;
    serializedResponse.authenticatorData = bytesToBase64url(assertion.authenticatorData);
    serializedResponse.signature = bytesToBase64url(assertion.signature);
    serializedResponse.userHandle = assertion.userHandle
      ? bytesToBase64url(assertion.userHandle)
      : null;
  }
  return {
    id: credential.id,
    rawId: bytesToBase64url(credential.rawId),
    type: credential.type,
    response: serializedResponse,
    clientExtensionResults: credential.getClientExtensionResults?.() ?? {},
    authenticatorAttachment: credential.authenticatorAttachment ?? null,
  };
}

export function passkeysAvailable(target: Window = window): boolean {
  return Boolean('PublicKeyCredential' in target && target.navigator.credentials);
}

export function passkeyErrorMessage(error: unknown): string {
  if (error instanceof DOMException && error.name === 'NotAllowedError') {
    return 'The passkey prompt was cancelled or timed out.';
  }
  if (error && typeof error === 'object' && 'name' in error && error.name === 'NotAllowedError') {
    return 'The passkey prompt was cancelled or timed out.';
  }
  return error instanceof Error && error.message ? error.message : 'Authentication failed.';
}

export interface PasskeyPlatform {
  available(): boolean;
  create(options: WebAuthnCreationWireOptions): Promise<SerializedPublicKeyCredential>;
  get(options: WebAuthnRequestWireOptions): Promise<SerializedPublicKeyCredential>;
}

export function browserPasskeyPlatform(target: Window = window): PasskeyPlatform {
  return {
    available: () => passkeysAvailable(target),
    async create(options) {
      const credential = await target.navigator.credentials.create(decodeCreationOptions(options));
      if (!credential || !('rawId' in credential)) throw new Error('No passkey was created.');
      return serializeCredential(credential as PublicKeyCredential);
    },
    async get(options) {
      const credential = await target.navigator.credentials.get(decodeRequestOptions(options));
      if (!credential || !('rawId' in credential)) throw new Error('No passkey was selected.');
      return serializeCredential(credential as PublicKeyCredential);
    },
  };
}
