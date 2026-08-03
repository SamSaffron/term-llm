(() => {
'use strict';

const app = window.TermLLMApp || (window.TermLLMApp = {});
const region = document.getElementById('toastRegion');

const dismissToast = (toast) => {
  if (!toast || toast._dismissed) return;
  toast._dismissed = true;
  if (toast._dismissTimer) window.clearTimeout(toast._dismissTimer);
  toast.classList?.add?.('toast-leaving');
  const remove = () => toast.remove?.();
  if (typeof window.setTimeout === 'function') window.setTimeout(remove, 180);
  else remove();
};

// Shared notifications; specialized dialogs and legacy alerts stay separate.
const showToast = (message, options = {}) => {
  const text = String(message || '').trim();
  if (!text || !region || typeof document.createElement !== 'function') return null;
  const toastID = String(options.id || '').trim();
  if (toastID) {
    const previous = Array.from(region.children || []).find((item) => item?.attributes?.['data-toast-id'] === toastID || item?.getAttribute?.('data-toast-id') === toastID);
    if (previous) {
      if (previous._dismissTimer) window.clearTimeout(previous._dismissTimer);
      previous.remove?.();
    }
  }
  const tone = ['success', 'error', 'warning'].includes(options.tone) ? options.tone : 'info';
  const toast = document.createElement('div');
  toast.className = `toast toast-${tone}`;
  if (toastID) toast.setAttribute('data-toast-id', toastID);
  toast.setAttribute('role', tone === 'error' ? 'alert' : 'status');
  toast.setAttribute('aria-atomic', 'true');

  const copy = document.createElement('span');
  copy.className = 'toast-message';
  copy.textContent = text;
  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'toast-close';
  close.setAttribute('aria-label', 'Dismiss notification');
  close.textContent = '×';
  close.addEventListener('click', () => dismissToast(toast));
  toast.append(copy, close);
  region.appendChild(toast);

  while (region.children.length > 4) {
    const oldest = region.children[0];
    if (oldest?._dismissTimer) window.clearTimeout(oldest._dismissTimer);
    oldest?.remove?.();
  }
  const duration = Number.isFinite(Number(options.duration)) ? Math.max(0, Number(options.duration)) : 4200;
  if (duration > 0 && typeof window.setTimeout === 'function') {
    toast._dismissTimer = window.setTimeout(() => dismissToast(toast), duration);
  }
  return toast;
};

Object.assign(app, { showToast, dismissToast });
})();
