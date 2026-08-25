import type { AppConfig } from '../app/config';
import type { Endpoints } from '../api/endpoints';

function base64URLToBytes(value: string): Uint8Array<ArrayBuffer> {
  const padding = '='.repeat((4 - value.length % 4) % 4);
  const raw = atob((value + padding).replaceAll('-', '+').replaceAll('_', '/'));
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}

export function syncTokenCookie(prefix: string, token: string, documentTarget: Document = document): void {
  const path = prefix.replace(/\/$/, '') || '/';
  const write = (value: string, cookiePath: string, maxAge: number) => {
    documentTarget.cookie = `term_llm_token=${value}; path=${cookiePath}; SameSite=Strict; max-age=${maxAge}`;
  };
  write('', `${path}/images`, 0);
  write(token ? encodeURIComponent(token) : '', path, token ? 31_536_000 : 0);
}

export async function registerServiceWorker(config: AppConfig): Promise<ServiceWorkerRegistration | null> {
  if (!('serviceWorker' in navigator)) return null;
  try { return await navigator.serviceWorker.register(`${config.prefix}/sw.js?v=${encodeURIComponent(config.version)}`, { scope: `${config.prefix}/` }); }
  catch { return null; }
}

export async function enableNotifications(config: AppConfig, endpoints: Endpoints): Promise<boolean> {
  if (!('Notification' in window) || !config.vapidKey) return false;
  const permission = await Notification.requestPermission(); if (permission !== 'granted') return false;
  const registration = await registerServiceWorker(config); if (!registration) return false;
  let subscription = await registration.pushManager.getSubscription();
  subscription ||= await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: base64URLToBytes(config.vapidKey) });
  await endpoints.pushSubscribe(subscription.toJSON()); return true;
}

export async function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) return navigator.clipboard.writeText(text);
  const textarea = document.createElement('textarea');
  textarea.value = text; textarea.readOnly = true; textarea.setAttribute('aria-hidden', 'true');
  textarea.style.cssText = 'position:fixed;left:-9999px;top:-9999px;width:1px;height:1px;opacity:0';
  const previous = document.activeElement as HTMLElement | null; document.body.append(textarea); textarea.focus(); textarea.select(); textarea.setSelectionRange(0, textarea.value.length);
  try { if (!document.execCommand('copy')) throw new Error('Clipboard unavailable'); }
  finally { textarea.remove(); previous?.focus({ preventScroll: true }); }
}

export async function hardRefreshAssets(config: AppConfig, registration?: ServiceWorkerRegistration | null): Promise<void> {
  try {
    if ('caches' in window) {
      const keys = await caches.keys();
      await Promise.all(keys.filter((key) => key.startsWith('term-llm-shell-')).map((key) => caches.delete(key)));
    }
    const activeRegistration = registration || (('serviceWorker' in navigator) ? await navigator.serviceWorker.getRegistration(`${config.prefix}/`) : null);
    await activeRegistration?.update().catch(() => undefined);
  } finally { location.reload(); }
}
