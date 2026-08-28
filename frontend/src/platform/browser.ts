import type { AppConfig } from '../app/config';

export function installVisualViewportSizing(): () => void {
  const viewport = window.visualViewport;
  const update = () => {
    const height = viewport?.height || window.innerHeight;
    const offset = viewport?.offsetTop || 0;
    document.documentElement.style.setProperty('--app-height', `${Math.round(height)}px`);
    document.documentElement.style.setProperty('--app-offset-top', `${Math.round(offset)}px`);
  };
  update();
  viewport?.addEventListener('resize', update);
  viewport?.addEventListener('scroll', update);
  window.addEventListener('resize', update);
  return () => {
    viewport?.removeEventListener('resize', update);
    viewport?.removeEventListener('scroll', update);
    window.removeEventListener('resize', update);
    document.documentElement.style.removeProperty('--app-height');
    document.documentElement.style.removeProperty('--app-offset-top');
  };
}

export function positionPopover(
  trigger: HTMLElement,
  panel: HTMLDialogElement,
  stretchOnMobile = false,
): void {
  if (!panel.open) panel.showModal();
  const viewport = window.visualViewport;
  const viewportLeft = viewport?.offsetLeft || 0;
  const viewportTop = viewport?.offsetTop || 0;
  const viewportWidth = viewport?.width || innerWidth;
  const viewportHeight = viewport?.height || innerHeight;
  const margin = 8;
  const panelRect = panel.getBoundingClientRect();

  panel.style.bottom = 'auto';
  if (viewportWidth <= 540) {
    panel.style.left = `${viewportLeft + margin}px`;
    panel.style.right =
      stretchOnMobile || panel.classList.contains('chip-popover-runtime')
        ? `${Math.max(margin, innerWidth - viewportLeft - viewportWidth + margin)}px`
        : 'auto';
    panel.style.top = `${Math.max(viewportTop + margin, viewportTop + viewportHeight - panelRect.height - margin)}px`;
    return;
  }

  const triggerRect = trigger.getBoundingClientRect();
  panel.style.right = 'auto';
  panel.style.left = `${Math.max(viewportLeft + margin, Math.min(triggerRect.left, viewportLeft + viewportWidth - panelRect.width - margin))}px`;
  const below = triggerRect.bottom + 4;
  panel.style.top = `${
    below + panelRect.height <= viewportTop + viewportHeight - margin
      ? below
      : Math.max(viewportTop + margin, triggerRect.top - panelRect.height - 4)
  }px`;
}

export function observePopoverPosition(
  trigger: HTMLElement,
  panel: HTMLDialogElement,
  stretchOnMobile = false,
): () => void {
  const update = () => positionPopover(trigger, panel, stretchOnMobile);
  const frame = requestAnimationFrame(update);
  const viewport = window.visualViewport;
  viewport?.addEventListener('resize', update);
  viewport?.addEventListener('scroll', update);
  window.addEventListener('resize', update);
  const resizeObserver = typeof ResizeObserver === 'function' ? new ResizeObserver(update) : null;
  resizeObserver?.observe(panel);
  return () => {
    cancelAnimationFrame(frame);
    viewport?.removeEventListener('resize', update);
    viewport?.removeEventListener('scroll', update);
    window.removeEventListener('resize', update);
    resizeObserver?.disconnect();
  };
}

export function syncTokenCookie(
  prefix: string,
  token: string,
  documentTarget: Document = document,
): void {
  const path = prefix.replace(/\/$/, '') || '/';
  const write = (value: string, cookiePath: string, maxAge: number) => {
    documentTarget.cookie = `term_llm_token=${value}; path=${cookiePath}; SameSite=Strict; max-age=${maxAge}`;
  };
  write('', `${path}/images`, 0);
  write(token ? encodeURIComponent(token) : '', path, token ? 31_536_000 : 0);
}

export async function registerServiceWorker(
  config: AppConfig,
): Promise<ServiceWorkerRegistration | null> {
  if (!('serviceWorker' in navigator)) return null;
  try {
    return await navigator.serviceWorker.register(
      `${config.prefix}/sw.js?v=${encodeURIComponent(config.version)}`,
      { scope: `${config.prefix}/` },
    );
  } catch {
    return null;
  }
}

export async function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) return navigator.clipboard.writeText(text);
  const textarea = document.createElement('textarea');
  textarea.value = text;
  textarea.readOnly = true;
  textarea.setAttribute('aria-hidden', 'true');
  textarea.style.cssText = 'position:fixed;left:-9999px;top:-9999px;width:1px;height:1px;opacity:0';
  const previous = document.activeElement as HTMLElement | null;
  document.body.append(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange(0, textarea.value.length);
  try {
    if (!document.execCommand('copy')) throw new Error('Clipboard unavailable');
  } finally {
    textarea.remove();
    previous?.focus({ preventScroll: true });
  }
}

export async function hardRefreshAssets(
  config: AppConfig,
  registration?: ServiceWorkerRegistration | null,
): Promise<void> {
  try {
    if ('caches' in window) {
      const keys = await caches.keys();
      await Promise.all(
        keys.filter((key) => key.startsWith('term-llm-shell-')).map((key) => caches.delete(key)),
      );
    }
    const activeRegistration =
      registration ||
      ('serviceWorker' in navigator
        ? await navigator.serviceWorker.getRegistration(`${config.prefix}/`)
        : null);
    await activeRegistration?.update().catch(() => undefined);
  } finally {
    location.reload();
  }
}
