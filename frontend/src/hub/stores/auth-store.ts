import { signal } from '@preact/signals';
import { HubAPIError, type HubClient } from '../../api/hub-client';
import type { HubPasskeyMode } from '../config';
import type { PasskeyPlatform } from '../platform/passkeys';
import { passkeyErrorMessage } from '../platform/passkeys';

export const grantVerifiedStorageKey = 'term_llm_hub_grant_verified';

export class AuthStore {
  readonly busy = signal(false);
  readonly error = signal('');

  constructor(
    readonly client: HubClient,
    readonly passkeys: PasskeyPlatform,
    private readonly storage: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> = sessionStorage,
    private readonly navigate: (url: string) => void = (url) => window.location.assign(url),
  ) {}

  private grantVerified(): boolean {
    return this.storage.getItem(grantVerifiedStorageKey) === '1';
  }

  private setGrantVerified(value: boolean): void {
    if (value) this.storage.setItem(grantVerifiedStorageKey, '1');
    else this.storage.removeItem(grantVerifiedStorageKey);
  }

  private async registerWithGrant(
    mode: 'setup' | 'recover',
    code: string,
    displayName: string,
  ): Promise<string> {
    const prefix = mode === 'setup' ? '/api/auth/bootstrap' : '/api/auth/recovery';
    const register = async () => {
      const options = await this.client.beginGrantRegistration(prefix, displayName);
      this.setGrantVerified(true);
      const credential = await this.passkeys.create(options);
      return this.client.finishGrantRegistration(prefix, credential);
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

  async submit(
    mode: HubPasskeyMode,
    fields: { code: string; displayName: string; returnPath: string },
  ): Promise<void> {
    if (this.busy.value) return;
    this.busy.value = true;
    this.error.value = '';
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
      } else {
        redirect = await this.registerWithGrant(mode, fields.code, fields.displayName);
        this.setGrantVerified(false);
      }
      this.navigate(redirect);
    } catch (error) {
      this.error.value = passkeyErrorMessage(error);
      this.busy.value = false;
    }
  }
}
