import type { Message } from '../domain/types';
import { useStore } from '../app/context';
import { Icon } from './Icon';
import { Markdown } from './Markdown';

export function ThoughtRow({ message, live = false }: { message: Message; live?: boolean }) {
  const thinking = useStore().thinking;
  if (!thinking.visible(message) || !message.content) return null;
  const expanded = thinking.expanded(message);
  const raw = message.reasoningKind === 'raw';
  const rawLabel = raw && thinking.display.value === 'raw';
  const label = rawLabel ? 'Raw thinking' : message.reasoningTitle;
  const bodyID = `thought-body-${message.id}`;
  return (
    <article
      class={`message thought${live ? ' live' : ''}${raw ? ' thought-raw' : ''}`}
      data-message-id={message.id}
      data-durable-id={message.durableRowId}
    >
      <button
        class="thought-toggle"
        type="button"
        aria-expanded={expanded}
        aria-controls={expanded ? bodyID : undefined}
        onClick={() => thinking.toggleThought(message.id, expanded)}
      >
        <span class="thought-arrow">
          <Icon name="chevron-right" />
        </span>
        <span class="thought-label">
          {label ? (
            rawLabel ? (
              label
            ) : (
              <>
                <strong class="thought-prefix">Thought:</strong> {label}
              </>
            )
          ) : (
            thinking.policy.value.hidden_label
          )}
        </span>
      </button>
      {expanded && (
        <div class="thought-body" id={bodyID}>
          <Markdown value={message.content} />
        </div>
      )}
    </article>
  );
}
