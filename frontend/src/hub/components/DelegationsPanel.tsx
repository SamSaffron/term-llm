import type { HubConfig } from '../config';
import { firstDelegationArtifact } from '../domain/links';
import type { HubDelegation, HubNode } from '../domain/types';
import type { HubStore } from '../stores/hub-store';
import { HubRenderBoundary } from './HubRenderBoundary';

/** One delegated run, including the artifact its response may point at. */
function DelegationRow({
  config,
  delegation,
  nodes,
  stale,
}: {
  config: HubConfig;
  delegation: HubDelegation;
  nodes: Pick<HubNode, 'id' | 'base_path'>[];
  stale: boolean;
}) {
  const artifact = delegation.response
    ? firstDelegationArtifact(
        delegation.response,
        delegation,
        nodes,
        config.basePath,
        window.location.href,
      )
    : null;
  return (
    <article class="delegation-row">
      <div class="delegation-route">
        <strong>{delegation.origin_node || 'unknown'}</strong>
        <span class="route-arrow">→</span>
        <strong>{delegation.target_node || 'unknown'}</strong>
        <span
          class={`delegation-status status-${stale ? 'last-known' : delegation.status || 'unknown'}`}
        >
          {stale ? `last known: ${delegation.status || 'unknown'}` : delegation.status || 'unknown'}
        </span>
      </div>
      <div class="delegation-meta">
        {delegation.agent_name || 'agent'} · depth {delegation.depth || 1}
        {delegation.job_id ? ` · ${delegation.job_id}` : ''}
      </div>
      {delegation.prompt && <div class="delegation-prompt">{delegation.prompt}</div>}
      {delegation.response && (
        <div class="delegation-response">
          {artifact?.type === 'image' && (
            <>
              <img
                class="delegation-artifact-img"
                src={artifact.url}
                alt={artifact.label || 'Delegated artifact'}
              />
              <a
                class="delegation-artifact-link"
                href={artifact.url}
                target="_blank"
                rel="noopener noreferrer"
              >
                {artifact.label}
              </a>
            </>
          )}
          {artifact?.type === 'link' && (
            <a
              class="delegation-artifact-link"
              href={artifact.url}
              target="_blank"
              rel="noopener noreferrer"
            >
              {artifact.label}
            </a>
          )}
          <pre class="delegation-response-text">{delegation.response}</pre>
        </div>
      )}
      {delegation.error && <div class="node-error">{delegation.error}</div>}
    </article>
  );
}

export function DelegationsPanel({ config, store }: { config: HubConfig; store: HubStore }) {
  const stale = !store.delegationsVerified.value;
  const delegations = store.delegations.value;
  return (
    <section class="delegations-panel" aria-label="Delegations">
      <div class="delegations-head">
        <div>
          <h2>
            Delegations
            {stale && delegations.length > 0 && (
              <span class="hub-cache-marker"> · last known · updating</span>
            )}
          </h2>
          <p>Cross-node work routed through the Hub.</p>
        </div>
        <span class="delegations-count">
          {delegations.length
            ? stale
              ? `${delegations.length} last known`
              : `${store.activeDelegationCount.value} active · ${delegations.length} total`
            : ''}
        </span>
      </div>
      <div class="delegations-list">
        {delegations.slice(0, 8).map((delegation) => (
          <HubRenderBoundary key={delegation.id} resetKey={delegation} label="Delegation">
            <DelegationRow
              config={config}
              delegation={delegation}
              nodes={store.nodes.value}
              stale={stale}
            />
          </HubRenderBoundary>
        ))}
      </div>
      {(store.delegationsVerified.value || !store.initialLoading.value) &&
        store.delegationError.value && (
          <div class="delegations-empty" role="status">
            {delegations.length
              ? `Could not refresh delegations: ${store.delegationError.value}. Showing the last successful result.`
              : `Could not load delegations: ${store.delegationError.value}`}
          </div>
        )}
      {!delegations.length &&
        (store.delegationsVerified.value || !store.initialLoading.value) &&
        !store.delegationError.value && <div class="delegations-empty">No delegated work yet.</div>}
    </section>
  );
}
