import type { AppStore } from '../stores/app-store';
import { extensionHost, extensionRuntime, type ExtensionStatus } from '../stores/extension-runtime';

async function activationTimeout<T>(promise: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error('Extension activation timed out; reload in safe mode if needed')),
          15_000,
        );
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

// The server links enabled stylesheets in the shell's <head> so they apply
// before first paint (internal/app/serve/serve_extension_styles.go).
function renderedExtensionStyles(): HTMLLinkElement[] {
  return [
    ...document.head.querySelectorAll<HTMLLinkElement>('link[rel="stylesheet"][data-extension]'),
  ];
}

export function removeRenderedExtensionStyles(): void {
  for (const link of renderedExtensionStyles()) link.remove();
}

// Reuse a server-rendered stylesheet only when it is for this exact generation
// and loaded. Head stylesheets settle before deferred module scripts run, so a
// missing sheet means it failed. Links that are not adopted stay applied until
// loading finishes, so a generation change never shows unstyled content.
function adoptRenderedStyle(
  rendered: HTMLLinkElement[],
  id: string,
  href: string,
): HTMLLinkElement | null {
  const url = new URL(href, document.baseURI).href;
  const index = rendered.findIndex(
    (link) => link.dataset.extension === id && link.href === url && link.sheet,
  );
  return index < 0 ? null : rendered.splice(index, 1)[0];
}

export async function loadExtensions(store: AppStore): Promise<void> {
  if (extensionRuntime.safeMode.peek()) {
    removeRenderedExtensionStyles();
    return;
  }
  // A failed status request keeps the server-rendered styles: they came from
  // the same snapshot this request would have returned.
  const rendered = renderedExtensionStyles();
  try {
    const status = await store.endpoints.extensionsStatus();
    try {
      await loadEnabledExtensions(store, status, rendered);
    } finally {
      // Anything not adopted is disabled, broken or from another generation.
      for (const link of rendered) link.remove();
    }
  } catch (error) {
    extensionRuntime.errors.value = [String(error)];
    // Optional feature: a missing endpoint on older servers must not stop chat.
  }
}

async function loadEnabledExtensions(
  store: AppStore,
  status: ExtensionStatus,
  rendered: HTMLLinkElement[],
): Promise<void> {
  if (status.disabled) return;
  extensionRuntime.generation.value = status.generation;
  extensionRuntime.errors.value = [...status.errors];
  if (!status.enabled.length) return;
  for (const id of status.enabled) {
    const entry = status.entries.find((entry) => entry.id === id);
    if (!entry || entry.error) continue;
    const base = store.api.url(`/extensions/${status.generation}/${encodeURIComponent(id)}/`);
    let style: HTMLLinkElement | null = entry.css
      ? adoptRenderedStyle(rendered, id, base + entry.css)
      : null;
    try {
      if (entry.css && !style) {
        style = document.createElement('link');
        style.rel = 'stylesheet';
        style.dataset.extension = id;
        style.href = base + entry.css;
        const link = style;
        await new Promise<void>((resolve, reject) => {
          const timer = setTimeout(() => reject(new Error('Stylesheet loading timed out')), 15_000);
          link.onload = () => {
            clearTimeout(timer);
            resolve();
          };
          link.onerror = () => {
            clearTimeout(timer);
            reject(new Error('Could not load stylesheet'));
          };
          document.head.append(link);
        });
      }
      if (entry.js) {
        const module = await activationTimeout(import(/* @vite-ignore */ base + entry.js));
        if (typeof module.activate === 'function')
          await activationTimeout(Promise.resolve(module.activate(extensionHost(store, id))));
      }
      extensionRuntime.loaded.value = [...extensionRuntime.loaded.peek(), id];
    } catch (error) {
      style?.remove();
      extensionRuntime.errors.value = [
        ...extensionRuntime.errors.peek(),
        `${id}: ${String(error)}`,
      ];
    }
  }
  if (extensionRuntime.errors.peek().length)
    store.toast(
      'An extension could not load. Open Settings → Extensions, or use ?safe-mode=1.',
      'error',
    );
}

export type { ExtensionStatus };

// Only explicit activation requests trigger a document reset. Polling the
// authenticated endpoint also works through Hub HTTP without a second stream.
// Never interrupt a response or discard an unsent draft to apply a theme.
export function watchExtensionActivation(
  store: AppStore,
  reload: () => void = () => location.reload(),
): () => void {
  if (extensionRuntime.safeMode.peek()) return () => {};
  let stopped = false;
  let timer: ReturnType<typeof setTimeout>;
  const poll = async () => {
    try {
      if (extensionRuntime.safeMode.peek() || store.authRequired.peek()) return;
      const status = await store.endpoints.extensionsStatus();
      if (
        !stopped &&
        !extensionRuntime.safeMode.peek() &&
        !status.disabled &&
        status.activation_generation &&
        status.activation_generation === status.generation &&
        status.generation !== extensionRuntime.generation.peek() &&
        !store.runActive.peek() &&
        !store.composer.sendPending.peek() &&
        !Object.values(store.runs.peek()).some(({ run }) =>
          ['connecting', 'checking', 'streaming', 'cancelling'].includes(run.status),
        ) &&
        !store.composer.prompt.peek() &&
        !store.composer.attachments.peek().length &&
        !store.modal.peek()
      ) {
        stopped = true;
        reload();
      }
    } catch {
      // Older servers/offline connections must never interfere with chat.
    } finally {
      if (!stopped) timer = setTimeout(() => void poll(), 3000);
    }
  };
  timer = setTimeout(() => void poll(), 3000);
  return () => {
    stopped = true;
    clearTimeout(timer);
  };
}
