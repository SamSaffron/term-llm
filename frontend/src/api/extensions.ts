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

export async function loadExtensions(store: AppStore): Promise<void> {
  if (extensionRuntime.safeMode.peek()) return;
  try {
    const status = await store.endpoints.extensionsStatus();
    if (status.disabled) return;
    extensionRuntime.generation.value = status.generation;
    extensionRuntime.errors.value = [...status.errors];
    if (!status.enabled.length) return;
    // Extensions use module/style URLs, not bearer-header fetches. WebRTC's
    // JSON tunnel cannot currently serve browser module subresources.
    if (store.config.webRTC)
      throw new Error('Extensions require a direct or Hub HTTP connection (not WebRTC).');
    for (const id of status.enabled) {
      const entry = status.entries.find((entry) => entry.id === id);
      if (!entry || entry.error) continue;
      const base = store.api.url(`/extensions/${status.generation}/${encodeURIComponent(id)}/`);
      let style: HTMLLinkElement | null = null;
      try {
        if (entry.css) {
          style = document.createElement('link');
          style.rel = 'stylesheet';
          style.dataset.extension = id;
          style.href = base + entry.css;
          const link = style;
          await new Promise<void>((resolve, reject) => {
            const timer = setTimeout(
              () => reject(new Error('Stylesheet loading timed out')),
              15_000,
            );
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
  } catch (error) {
    extensionRuntime.errors.value = [String(error)];
    // Optional feature: a missing endpoint on older servers must not stop chat.
  }
}

export type { ExtensionStatus };

// Only explicit activation requests trigger a document reset. Polling the
// authenticated endpoint also works through Hub HTTP without a second stream.
// Never interrupt a response or discard an unsent draft to apply a theme.
export function watchExtensionActivation(
  store: AppStore,
  reload: () => void = () => location.reload(),
): () => void {
  if (extensionRuntime.safeMode.peek() || store.config.webRTC) return () => {};
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
