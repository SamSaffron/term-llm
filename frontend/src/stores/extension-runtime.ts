import { computed, effect, signal } from '@preact/signals';
import type { AppStore } from './app-store';
import type { AppConfig } from '../app/config';
import type { ContextUsage } from '../domain/types';

export interface WebExtension {
  id: string;
  title: string;
  description: string;
  css?: string;
  js?: string;
  error?: string;
}
export interface ExtensionStatus {
  directory: string;
  enabled: string[];
  entries: WebExtension[];
  errors: string[];
  generation: string;
  activation_generation?: string;
  source: string;
  disabled: boolean;
  config_revision: string;
  config_path: string;
}

// Only the recovery override is browser-local. Enabled extensions belong to the server.
export const extensionRuntime = {
  safeMode: signal(false),
  loaded: signal<string[]>([]),
  generation: signal(''),
  errors: signal<string[]>([]),
};
let recoveryKey = '';
export function initializeExtensionRecovery(config: AppConfig): boolean {
  recoveryKey = `term-llm:safe-mode:${config.hub?.nodeId || ''}:${config.prefix}`;
  let safe = new URL(location.href).searchParams.get('safe-mode') === '1';
  try {
    safe ||= sessionStorage.getItem(recoveryKey) === '1';
    if (safe) sessionStorage.setItem(recoveryKey, '1');
  } catch {
    /* The URL remains the fallback when storage is unavailable. */
  }
  extensionRuntime.safeMode.value = safe;
  return safe;
}
export function recoveryURL(): string {
  const url = new URL(location.href);
  url.searchParams.set('safe-mode', '1');
  return url.href;
}
export function exitExtensionRecovery(): void {
  try {
    sessionStorage.removeItem(recoveryKey);
  } catch {
    /* URL fallback. */
  }
  const url = new URL(location.href);
  url.searchParams.delete('safe-mode');
  location.assign(url.href);
}

export interface ExtensionHost {
  root: HTMLElement;
  mount: HTMLElement;
  getContext(): { sessionId: string; version: string; prefix: string };
  onSessionChanged(callback: (sessionId: string) => void): () => void;
  getContextUsage(): Readonly<ContextUsage> | null;
  onContextUsageChanged(callback: (usage: Readonly<ContextUsage> | null) => void): () => void;
  insertComposerText(text: string): void;
}

export function extensionHost(store: AppStore, id: string): ExtensionHost {
  const root = document.getElementById('root')!;
  const mount = document.createElement('div');
  const contextUsage = computed(() => store.activeSession.value?.contextUsage ?? null);
  const publicContextUsage = (
    usage: ContextUsage | null = contextUsage.peek(),
  ): Readonly<ContextUsage> | null => (usage ? Object.freeze({ ...usage }) : null);
  mount.dataset.extension = id;
  document.getElementById('extension-mount')!.append(mount);
  return Object.freeze({
    root,
    mount,
    getContext: () => ({
      sessionId: store.activeSessionId.peek(),
      version: store.config.version,
      prefix: store.config.prefix,
    }),
    onSessionChanged: (callback: (id: string) => void) =>
      effect(() => callback(store.activeSessionId.value)),
    getContextUsage: publicContextUsage,
    onContextUsageChanged: (callback: (usage: Readonly<ContextUsage> | null) => void) =>
      effect(() => callback(publicContextUsage(contextUsage.value))),
    insertComposerText: (text: string) => {
      store.composer.prompt.value += String(text);
    },
  });
}
