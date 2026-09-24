import { signal } from '@preact/signals';
import { HubAPIError, type HubClient } from '../../api/hub-client';
import { hubPath, type HubPasskeyMode } from '../config';
import type { PasskeyPlatform } from '../platform/passkeys';
import { passkeyErrorMessage } from '../platform/passkeys';

export const grantVerifiedStorageKey = 'term_llm_hub_grant_verified';

export class AuthStore {
  readonly busy = signal(false);
  readonly error = signal('');
  // Set after a native-app approval hands off to the app's callback URL. The
  // page stays loaded in desktop browsers, so it must stop showing "waiting".
  readonly handedOff = signal(false);

  constructor(
    readonly client: HubClient,
    readonly passkeys: PasskeyPlatform,
    private readonly storage: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> = sessionStorage,
    private readonly navigate: (url: string) => void = (url) => window.location.assign(url),
  ) {}

  private grantVerified(): boolean {
    return (
      this.storage.getItem(`${grantVerifiedStorageKey}:${this.client.config.basePath}`) === '1'
    );
  }

  private setGrantVerified(value: boolean): void {
    if (value)
      this.storage.setItem(`${grantVerifiedStorageKey}:${this.client.config.basePath}`, '1');
    else this.storage.removeItem(`${grantVerifiedStorageKey}:${this.client.config.basePath}`);
  }

  private async registerWithGrant(
    mode: 'setup' | 'recover',
    code: string,
    displayName: string,
    returnPath: string,
  ): Promise<string> {
    const prefix = mode === 'setup' ? '/api/auth/bootstrap' : '/api/auth/recovery';
    const register = async () => {
      const options = await this.client.beginGrantRegistration(prefix, displayName);
      this.setGrantVerified(true);
      const credential = await this.passkeys.create(options);
      return this.client.finishGrantRegistration(prefix, credential, returnPath);
    };
    if (this.grantVerified()) {
      try {
        return (await register()).redirect;
      } catch (error) {
        if (!(error instanceof HubAPIError) || error.status !== 401) throw error;
        this.setGrantVerified(false);
      }
    }
    await this.client.verifyGrant(prefix, code);
    this.setGrantVerified(true);
    return (await register()).redirect;
  }

  private async authorizeNative(challenge: string): Promise<string> {
    try {
      return (await this.client.authorizeNative(challenge)).redirect;
    } catch (error) {
      if (!(error instanceof HubAPIError) || error.type !== 'recent_auth_required') throw error;
    }
    const options = await this.client.beginReauthentication(false);
    const credential = await this.passkeys.get(options);
    await this.client.finishReauthentication(credential, false);
    return (await this.client.authorizeNative(challenge)).redirect;
  }

  async submit(
    mode: HubPasskeyMode,
    fields: { code: string; displayName: string; returnPath: string; challenge?: string },
  ): Promise<void> {
    if (this.busy.value) return;
    this.busy.value = true;
    this.error.value = '';
    this.handedOff.value = false;
    try {
      if (!this.passkeys.available()) {
        throw new Error(
          'Passkeys require a supported browser, HTTPS, and the configured hostname.',
        );
      }
      let redirect: string;
      if (mode === 'login') {
        const options = await this.client.beginLogin(fields.returnPath);
        const credential = await this.passkeys.get(options);
        redirect = (await this.client.finishLogin(credential)).redirect;
      } else if (mode === 'native') {
        redirect = await this.authorizeNative(fields.challenge ?? '');
      } else {
        redirect = await this.registerWithGrant(
          mode,
          fields.code,
          fields.displayName,
          fields.returnPath,
        );
        this.setGrantVerified(false);
      }
      this.navigate(redirect);
      if (mode === 'native') {
        this.handedOff.value = true;
        this.busy.value = false;
      }
    } catch (error) {
      if (mode === 'native' && error instanceof HubAPIError && error.status === 401) {
        // The browser session ended while the page was open. Reload the
        // approval page; the server sends it through sign-in and back here.
        this.navigate(
          hubPath(this.client.config.basePath, `/auth/native/${fields.challenge ?? ''}`),
        );
        return;
      }
      this.error.value = passkeyErrorMessage(error);
      this.busy.value = false;
    }
  }
}
