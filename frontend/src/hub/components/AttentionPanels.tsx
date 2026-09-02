import { relativeSessionTime } from '../domain/formatting';
import type { HubStore } from '../stores/hub-store';

export function AttentionPanels({ store }: { store: HubStore }) {
  const waiting = store.inputRequired.value;
  const inbox = store.inbox.value;
  return (
    <>
      {waiting.length > 0 && (
        <section class="attention-panel input-required-panel" aria-label="Needs your input">
          <div class="delegations-head">
            <div>
              <h2>Needs your input</h2>
              <p>Conversations blocked on a question or approval.</p>
            </div>
            <span class="delegations-count" aria-live="polite">
              {store.totalInputRequired.value} waiting
              {store.attentionHasMore.value ? ' · showing first' : ''}
            </span>
          </div>
          <ul class="attention-list">
            {waiting.map((item) => {
              const count = Number(item.pending_interaction_count) || 1;
              const kinds = item.pending_interaction_kinds ?? [];
              const label = kinds.some((kind) => kind.startsWith('approval'))
                ? kinds.includes('ask_user')
                  ? 'Question and approval waiting'
                  : 'Approval waiting'
                : 'Question waiting';
              const when = relativeSessionTime(item.required_since);
              return (
                <li key={`${item.node_id}:${item.session_id}`}>
                  <a
                    class={`attention-row input-required-row${item.stale ? ' is-stale' : ''}`}
                    href={item.resume_path || '#'}
                  >
                    <span class="attention-dot" />
                    <span class="attention-body">
                      <strong class="attention-title">
                        {item.title || item.session_id || 'Untitled conversation'}
                      </strong>
                      <span class="attention-meta">
                        {[
                          item.node_name || item.node_id,
                          count > 1 ? `${count} decisions waiting` : label,
                          item.stale ? 'last known state' : when,
                        ]
                          .filter(Boolean)
                          .join(' · ')}
                      </span>
                    </span>
                  </a>
                </li>
              );
            })}
          </ul>
        </section>
      )}
      {inbox.length > 0 && (
        <section class="attention-panel" aria-label="Ready to review">
          <div class="delegations-head">
            <div>
              <h2>Ready to review</h2>
              <p>Finished conversations not yet visited.</p>
            </div>
            <span class="delegations-count" aria-live="polite">
              {store.totalUnseen.value} ready
              {store.attentionHasMore.value ? ' · showing newest' : ''}
            </span>
          </div>
          <ul class="attention-list">
            {inbox.map((item) => (
              <li key={`${item.node_id}:${item.session_id}`}>
                <a class="attention-row" href={item.resume_path || '#'}>
                  <span class="attention-dot" />
                  <span class="attention-body">
                    <strong class="attention-title">
                      {item.title || item.session_id || 'Untitled conversation'}
                    </strong>
                    <span class="attention-meta">
                      {[
                        item.node_name || item.node_id,
                        item.outcome || 'completed',
                        relativeSessionTime(item.terminal_at),
                      ]
                        .filter(Boolean)
                        .join(' · ')}
                    </span>
                  </span>
                </a>
              </li>
            ))}
          </ul>
        </section>
      )}
    </>
  );
}
