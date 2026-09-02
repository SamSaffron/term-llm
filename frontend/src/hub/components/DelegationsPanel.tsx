import type { HubConfig } from '../config';
import { firstDelegationArtifact } from '../domain/links';
import type { HubStore } from '../stores/hub-store';

export function DelegationsPanel({ config, store }: { config: HubConfig; store: HubStore }) {
  const delegations = store.delegations.value;
  return (
    <section class="delegations-panel" aria-label="Delegations">
      <div class="delegations-head">
        <div>
          <h2>Delegations</h2>
          <p>Cross-node work routed through the Hub.</p>
        </div>
        <span class="delegations-count">
          {delegations.length
            ? `${store.activeDelegationCount.value} active · ${delegations.length} total`
            : ''}
        </span>
      </div>
      <div class="delegations-list">
        {delegations.slice(0, 8).map((delegation) => {
          const artifact = delegation.response
            ? firstDelegationArtifact(
                delegation.response,
                delegation,
                store.nodes.value,
                config.basePath,
                window.location.href,
              )
            : null;
          return (
            <article class="delegation-row" key={delegation.id}>
              <div class="delegation-route">
                <strong>{delegation.origin_node || 'unknown'}</strong>
                <span class="route-arrow">→</span>
                <strong>{delegation.target_node || 'unknown'}</strong>
                <span class={`delegation-status status-${delegation.status || 'unknown'}`}>
                  {delegation.status || 'unknown'}
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
        })}
      </div>
      {!store.initialLoading.value && store.delegationError.value && (
        <div class="delegations-empty" role="status">
          {delegations.length
            ? `Could not refresh delegations: ${store.delegationError.value}. Showing the last successful result.`
            : `Could not load delegations: ${store.delegationError.value}`}
        </div>
      )}
      {!delegations.length && !store.initialLoading.value && !store.delegationError.value && (
        <div class="delegations-empty">No delegated work yet.</div>
      )}
    </section>
  );
}
