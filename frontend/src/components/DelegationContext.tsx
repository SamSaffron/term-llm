import type { AppStore } from '../stores/app-store';
import { useStore } from '../app/context';
import { requestTranscriptScrollToDurable } from './transcript-scroll';

export async function returnToParent(store: AppStore): Promise<void> {
  const child = store.activeSession.peek();
  const parentId = child?.parentSessionId || '';
  if (!child || !parentId) return;
  const parent = store.sessions.peek().find((session) => session.id === parentId);
  // Selection starts before provenance is requested. Missing or slow history
  // must never turn the breadcrumb into a loading gate.
  const navigation = parent
    ? store.selectSession(parent)
    : store.resolveAndSelectSession(parentId, false);
  const provenance = store.endpoints
    .sessionChildren(parentId)
    .then(
      (response) =>
        response.children.find((candidate) => candidate.session_id === child.id)
          ?.parent_spawn_item_id,
    )
    .catch(() => undefined);
  const [, durableId] = await Promise.all([navigation, provenance]);
  if (!durableId || store.activeSessionId.peek() !== parentId) return;
  requestAnimationFrame(() => requestTranscriptScrollToDurable(durableId));
}

export function DelegationContext() {
  const store = useStore();
  const parentId = store.activeSession.value?.parentSessionId;
  if (!parentId) return null;
  const parent = store.sessions.value.find((session) => session.id === parentId);
  const title = parent?.title || 'Parent conversation';
  return (
    <nav class="delegated-presence" aria-label="Parent conversation">
      <button type="button" class="text-action" onClick={() => void returnToParent(store)}>
        Return to parent
      </button>
      <span class="delegated-parent-name" title={title}>
        {title}
      </span>
    </nav>
  );
}
