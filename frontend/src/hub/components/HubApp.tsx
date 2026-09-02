import { useEffect } from 'preact/hooks';
import type { HubConfig } from '../config';
import type { ClipboardAdapter } from '../platform/clipboard';
import type { HubStore } from '../stores/hub-store';
import { AddNodeDialog } from './AddNodeDialog';
import { AttentionPanels } from './AttentionPanels';
import { DelegationsPanel } from './DelegationsPanel';
import { HubHeader } from './HubHeader';
import { NodeGrid } from './NodeCard';
import { SecurityPanel } from './SecurityPanel';

export function HubApp({
  config,
  store,
  clipboard,
}: {
  config: HubConfig;
  store: HubStore;
  clipboard: ClipboardAdapter;
}) {
  useEffect(() => {
    store.start();
    return () => store.dispose();
  }, [store]);
  const error =
    store.nodeError.value ||
    (store.resolverWarning.value
      ? `Some sources failed to resolve: ${store.resolverWarning.value}`
      : '');
  return (
    <div class="hub-dashboard">
      <HubHeader config={config} store={store} />
      <main class="hub-main">
        {error && (
          <div class="hub-error" role="alert">
            {error}
          </div>
        )}
        <AttentionPanels store={store} />
        <NodeGrid store={store} />
        <DelegationsPanel config={config} store={store} />
        {config.passkeyAuth && <SecurityPanel store={store} />}
        {!store.initialLoading.value && !store.nodes.value.length && (
          <div class="hub-empty">
            <p>No nodes yet.</p>
            <p class="hub-empty-hint">
              Point the hub at nodes with <code>--config</code>, create contain workspaces, or add
              one with <em>Add node</em>.
            </p>
          </div>
        )}
        {store.initialLoading.value && (
          <div class="hub-empty" role="status">
            Loading Hub…
          </div>
        )}
      </main>
      {config.canAddNodes && store.addDialogOpen.value && (
        <AddNodeDialog config={config} store={store} clipboard={clipboard} />
      )}
    </div>
  );
}
