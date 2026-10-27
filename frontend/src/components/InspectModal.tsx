import '../styles/features/inspect.css';
import { useEffect, useId, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import {
  activeInspectModel,
  filterInspectBlocks,
  formatInspectModel,
  sameInspectModel,
  INSPECT_CATEGORIES,
  inspectBodyKey,
  inspectTailStarts,
  inspectTurnStarts,
  inspectWindow,
  type InspectBlock,
  type InspectBody,
  type InspectCategory,
  type SessionInspect,
} from '../domain/inspect';
import { errorMessage } from '../domain/text';
import { copyText } from '../platform/clipboard';
import { Overlay } from './Overlay';
import { Icon } from './Icon';

type BodyState = { body: InspectBody } | { error: string };

function runtimeBlocks(data: SessionInspect): InspectBlock[] {
  const runtime = data.header.runtime;
  if (!runtime.loaded || runtime.unavailable_reason) return [];
  return [
    ...(runtime.system_prompt_block_id
      ? [
          {
            id: runtime.system_prompt_block_id,
            kind: 'system' as const,
            title: 'Current system prompt',
            has_body: true,
          },
        ]
      : []),
    ...(runtime.tools ?? []).map((tool) => ({
      id: tool.block_id,
      kind: 'tool_call' as const,
      title: tool.name,
      preview: tool.description,
      total_bytes: tool.schema_bytes,
      has_body: true,
    })),
  ];
}

// One debounce collects rapid expansions. Bodies live only for this mounted
// session/snapshot, never in persistent browser storage or the shared transcript.
function useInspectBodies(data: SessionInspect | undefined, expanded: ReadonlySet<string>) {
  const store = useStore();
  const cache = useRef(new Map<string, BodyState>());
  const [, redraw] = useState(0);
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    cache.current.clear();
    redraw((value) => value + 1);
  }, [data]);
  useEffect(() => {
    if (!data) return;
    const controller = new AbortController();
    const candidates = [...data.blocks, ...runtimeBlocks(data)].filter(
      (block) =>
        block.has_body && expanded.has(block.id) && !cache.current.has(inspectBodyKey(block)),
    );
    if (!candidates.length) return;
    const timer = setTimeout(() => {
      void (async () => {
        const remaining = candidates.map((block) => block.id);
        const byId = new Map(candidates.map((block) => [block.id, block]));
        const deferrals = new Map<string, number>();
        let batchSize = 64;
        const save = (id: string, state: BodyState) => {
          const block = byId.get(id);
          if (block) cache.current.set(inspectBodyKey(block), state);
        };
        try {
          while (remaining.length && !controller.signal.aborted) {
            // 64 ids also stays well below the 8 KiB query limit for contract IDs.
            const ids = remaining.splice(0, batchSize);
            const result = await store.endpoints.sessionInspectBodies(
              data.session_id,
              ids,
              controller.signal,
            );
            if (controller.signal.aborted) return;
            const bodies = new Map(result.bodies.map((body) => [body.id, body]));
            for (const id of ids) {
              const body = bodies.get(id);
              if (body) save(id, { body });
              else if (result.deferred?.includes(id)) {
                // Four capped bodies fit comfortably inside the 2 MiB budget.
                batchSize = 4;
                const count = (deferrals.get(id) ?? 0) + 1;
                deferrals.set(id, count);
                if (count <= 3) remaining.push(id);
                else save(id, { error: 'Body deferred by the server. Try again.' });
              } else
                save(id, {
                  error: 'Body unavailable. It may have changed; refresh the inspector.',
                });
            }
            redraw((value) => value + 1);
          }
        } catch (reason: unknown) {
          if (controller.signal.aborted) return;
          for (const block of candidates)
            if (!cache.current.has(inspectBodyKey(block)))
              cache.current.set(inspectBodyKey(block), { error: errorMessage(reason) });
          redraw((value) => value + 1);
        }
      })();
    }, 60);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [data, expanded, retry, store]);
  return {
    get: (block: InspectBlock) => cache.current.get(inspectBodyKey(block)),
    retry: (block: InspectBlock) => {
      cache.current.delete(inspectBodyKey(block));
      setRetry((value) => value + 1);
    },
  };
}

function InspectModelPanel({ data }: { data: SessionInspect }) {
  const initial = data.header.initial_model ?? {};
  const current = data.header.current_model ?? {};
  return (
    <section class="inspect-model-panel" aria-label="Model">
      <h3>Model</h3>
      <p>Started with: {formatInspectModel(initial)}</p>
      {!sameInspectModel(initial, current) && <p>Now: {formatInspectModel(current)}</p>}
    </section>
  );
}

function InspectModelChange({ block }: { block: InspectBlock }) {
  const change = block.model_change;
  const failed = change?.status === 'failed';
  return (
    <div
      class={`inspect-model-change ${failed ? 'inspect-model-warning' : ''}`}
      role="separator"
      aria-label={block.title || 'Model change'}
    >
      <strong>Model changed{change?.strategy ? ` (${change.strategy})` : ''}: </strong>
      {change ? (
        <span>
          {formatInspectModel(change.from)} → {formatInspectModel(change.to)}
          {failed
            ? ` — failed; restored ${formatInspectModel(change.from)}`
            : change.status && ` — ${change.status}`}
        </span>
      ) : (
        <span>{block.title || 'unknown'}</span>
      )}
    </div>
  );
}

function InspectItem({
  block,
  expanded,
  toggle,
  state,
  retry,
  onChild,
}: {
  block: InspectBlock;
  expanded: boolean;
  toggle: () => void;
  state?: BodyState;
  retry: () => void;
  onChild: (id: string) => void;
}) {
  const store = useStore();
  const regionId = useId();
  const encrypted = block.reasoning_kind === 'encrypted';
  const expandable = !!block.has_body && !encrypted;
  const kindLabel = block.kind.replaceAll('_', ' ');
  const title = block.title || kindLabel;
  // Generic blocks carry their kind as title; avoid "user user".
  const distinctTitle = block.title && block.title !== block.kind ? block.title : '';
  const meta = [
    block.sequence !== undefined && `seq ${block.sequence}`,
    block.id === 'runtime.system_prompt'
      ? 'Size available on expansion'
      : block.has_body &&
        `${block.total_bytes ?? 0} ${block.id.startsWith('runtime.tool.') ? 'schema bytes' : 'bytes'}`,
    block.has_body &&
      block.total_lines !== undefined &&
      `${block.total_lines} ${block.total_lines === 1 ? 'line' : 'lines'}`,
    block.reasoning_kind,
    block.tool?.is_error && 'failed',
  ].filter(Boolean);
  const heading = (
    <>
      <span class="inspect-kind">{kindLabel}</span>
      {distinctTitle && <strong>{distinctTitle}</strong>}
      <span class="inspect-meta">{meta.join(' · ')}</span>
    </>
  );
  return (
    <article
      class={`inspect-block ${block.tool?.is_error ? 'inspect-failed' : ''} ${block.compaction_tail ? 'inspect-tail' : ''}`}
    >
      {expandable ? (
        <button
          class="inspect-block-toggle"
          type="button"
          aria-expanded={expanded}
          aria-controls={regionId}
          onClick={toggle}
        >
          <Icon name={expanded ? 'chevron-down' : 'chevron-right'} />
          {heading}
        </button>
      ) : (
        <div class="inspect-block-heading">{heading}</div>
      )}
      {encrypted ? (
        <p class="inspect-note">
          Encrypted reasoning ({block.encrypted_bytes ?? 0} bytes); plaintext unavailable.
        </p>
      ) : (
        !expanded && block.preview && <p class="inspect-preview">{block.preview}</p>
      )}
      {block.kind === 'handover_document' && (
        <p class="inspect-note">Written by the outgoing model</p>
      )}
      {block.synthetic && <p class="inspect-notice">Synthetic — not model output</p>}
      {block.compaction && (
        <p class="inspect-note">
          Compaction {block.compaction.ordinal}
          {block.compaction.active_boundary && ' · active boundary'} ·{' '}
          {block.compaction.tail_count ?? 0} tail rows
        </p>
      )}
      {expanded && expandable && (
        <div id={regionId} role="region" aria-label={`${title} body`}>
          {!state ? (
            <p role="status">Loading body…</p>
          ) : 'error' in state ? (
            <div role="alert">
              <p>{state.error}</p>
              <button type="button" onClick={retry}>
                Retry body
              </button>
            </div>
          ) : (
            <>
              <button
                type="button"
                class="inspect-copy"
                onClick={() => {
                  void copyText(state.body.text)
                    .then(() => store.toast('Copied', 'success'))
                    .catch((reason: unknown) => store.toast(errorMessage(reason), 'error'));
                }}
              >
                Copy {title}
              </button>
              {state.body.truncated && (
                <p class="inspect-notice" role="note">
                  Truncated: showing a bounded excerpt of {state.body.total_bytes} bytes. Copy
                  copies only this excerpt.
                </p>
              )}
              <pre>{state.body.text}</pre>
            </>
          )}
        </div>
      )}
      {!!block.child_session_ids?.length && (
        <div class="inspect-children">
          {block.child_session_ids.map((id) => (
            <button
              type="button"
              key={id}
              onClick={(event) => {
                // This opener disappears on navigation. Keep keyboard focus in
                // the existing dialog rather than falling back to the page.
                event.currentTarget.closest<HTMLElement>('[role="dialog"]')?.focus();
                onChild(id);
              }}
            >
              Inspect child session {id}
            </button>
          ))}
        </div>
      )}
    </article>
  );
}

function InspectContent({
  sessionId,
  onChild,
}: {
  sessionId: string;
  onChild: (id: string) => void;
}) {
  const store = useStore();
  const [refresh, setRefresh] = useState(0);
  const [data, setData] = useState<SessionInspect>();
  const [error, setError] = useState('');
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const [hidden, setHidden] = useState<ReadonlySet<InspectCategory>>(new Set());
  const [limit, setLimit] = useState(200);
  const bottom = useRef<HTMLDivElement>(null);
  const opened = useRef(false);
  useEffect(() => {
    const controller = new AbortController();
    setData(undefined);
    setError('');
    store.endpoints
      .sessionInspect(sessionId, controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) setData(result);
      })
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(errorMessage(reason));
      });
    return () => controller.abort();
  }, [sessionId, refresh, store]);
  useEffect(() => {
    if (!data || opened.current) return;
    const frame = requestAnimationFrame(() => {
      bottom.current?.scrollIntoView?.({ block: 'end' });
      opened.current = true;
    });
    return () => cancelAnimationFrame(frame);
  }, [data]);
  const bodies = useInspectBodies(data, expanded);
  const toggle = (id: string) =>
    setExpanded((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const item = (block: InspectBlock) =>
    block.kind === 'model_change' ? (
      <InspectModelChange key={block.id} block={block} />
    ) : (
      <InspectItem
        key={block.id}
        block={block}
        expanded={expanded.has(block.id)}
        toggle={() => toggle(block.id)}
        state={bodies.get(block)}
        retry={() => bodies.retry(block)}
        onChild={onChild}
      />
    );
  const filtered = filterInspectBlocks(data?.blocks ?? [], hidden);
  const window = inspectWindow(filtered, limit);
  const turnStarts = inspectTurnStarts(data?.blocks ?? []);
  const tailStarts = inspectTailStarts(window.blocks);
  const runtime = data?.header.runtime;
  const liveBlocks = data ? runtimeBlocks(data) : [];
  return (
    <>
      {data && <InspectModelPanel data={data} />}
      <div class="inspect-toolbar" role="group" aria-label="Inspect controls">
        <button type="button" onClick={() => setRefresh((value) => value + 1)}>
          Refresh
        </button>
        {INSPECT_CATEGORIES.map((category) => (
          <button
            key={category.id}
            type="button"
            aria-pressed={!hidden.has(category.id)}
            onClick={() =>
              setHidden((current) => {
                const next = new Set(current);
                if (next.has(category.id)) next.delete(category.id);
                else next.add(category.id);
                return next;
              })
            }
          >
            {category.label}
          </button>
        ))}
        <span role="status">
          {filtered.length} of {data?.blocks.length ?? 0} blocks
        </span>
      </div>
      <div class="inspect-scroll" role="region" aria-label="Session inspection" tabIndex={0}>
        <p class="inspect-note">
          Session: {sessionId} · Read-only snapshot; use Refresh for updates.
        </p>
        {error ? (
          <p class="inspect-notice" role="alert">
            {error}
          </p>
        ) : !data ? (
          <p role="status">Loading inspection…</p>
        ) : (
          <>
            <section aria-label="Inspection provenance" class="inspect-header">
              <p>
                {data.scope === 'persisted_history'
                  ? 'Persisted history'
                  : 'Runtime history (not persisted)'}
              </p>
              <p>
                Compactions: {data.header.compaction_count ?? 0} · Boundary sequence:{' '}
                {data.header.compaction_seq ?? 'none'} · Transcript revision:{' '}
                {data.transcript_rev < 0 ? 'unavailable' : data.transcript_rev}
              </p>
            </section>
            <section aria-label="Current runtime" class="inspect-runtime">
              <h3>Current runtime</h3>
              {runtime?.loaded && !runtime.unavailable_reason ? (
                <>
                  <p class="inspect-note">
                    Current runtime definitions; historical turns may have differed.
                  </p>
                  {runtime.system_prompt_block_id && (
                    <p class="inspect-note">
                      Current system prompt
                      {runtime.system_prompt_matches_persisted === false
                        ? ' differs from persisted.'
                        : runtime.system_prompt_matches_persisted === true
                          ? ' matches persisted.'
                          : ': persisted comparison unavailable.'}
                    </p>
                  )}
                  {liveBlocks.map(item)}
                  {!runtime.tools?.length && (
                    <p class="inspect-note">No current tool definitions.</p>
                  )}
                  {runtime.tool_discovery && (
                    <section aria-label="Tool discovery diagnostics">
                      <h4>Tool discovery diagnostics</h4>
                      <dl class="inspect-diagnostics">
                        {Object.entries(runtime.tool_discovery).map(([key, value]) => (
                          <div key={key}>
                            <dt>{key}</dt>
                            <dd>
                              <pre>
                                {typeof value === 'string' ? value : JSON.stringify(value, null, 2)}
                              </pre>
                            </dd>
                          </div>
                        ))}
                      </dl>
                    </section>
                  )}
                </>
              ) : (
                <p class="inspect-note">
                  {runtime?.unavailable_reason === 'runtime_busy'
                    ? 'Tool definitions unavailable: runtime busy.'
                    : 'Tool definitions unavailable: runtime not loaded.'}
                </p>
              )}
            </section>
            <section aria-label="Transcript blocks">
              <h3>Transcript blocks</h3>
              {window.earlier > 0 && (
                <button type="button" onClick={() => setLimit((value) => value + 200)}>
                  Show {Math.min(window.earlier, 200)} earlier ({window.earlier} hidden)
                </button>
              )}
              {window.earlier > 0 && (
                <p class="inspect-active-model">
                  Active model here:{' '}
                  {formatInspectModel(
                    activeInspectModel(
                      data.header.initial_model ?? {},
                      data.blocks,
                      window.blocks[0]?.id,
                    ),
                  )}
                </p>
              )}
              {!filtered.length && <p class="inspect-note">No blocks match these filters.</p>}
              {window.blocks.map((block) => (
                <div key={block.id}>
                  {turnStarts.has(block.id) && (
                    <div class="inspect-turn" role="separator" aria-label="User turn">
                      User turn
                    </div>
                  )}
                  {tailStarts.has(block.id) && (
                    <p class="inspect-note">
                      Compaction tail
                      {block.compaction_summary_id && ` · ${block.compaction_summary_id}`}
                    </p>
                  )}
                  {item(block)}
                </div>
              ))}
            </section>
          </>
        )}
        <div ref={bottom} />
      </div>
    </>
  );
}

function InspectNavigation({ sessionId }: { sessionId: string }) {
  const [stack, setStack] = useState([sessionId]);
  const current = stack[stack.length - 1];
  return (
    <Overlay
      title="Inspect"
      wide
      className="inspect-modal"
      focusContent={false}
      titleLeading={
        stack.length > 1 && (
          <button
            type="button"
            class="inspect-back"
            onClick={(event) => {
              event.currentTarget.closest<HTMLElement>('[role="dialog"]')?.focus();
              setStack((current) => current.slice(0, -1));
            }}
          >
            Back to {stack[stack.length - 2]}
          </button>
        )
      }
    >
      <InspectContent
        key={current}
        sessionId={current}
        onChild={(id) => setStack((current) => [...current, id])}
      />
    </Overlay>
  );
}

export function InspectModal() {
  const store = useStore();
  const sessionId = store.activeSession.value?.id;
  return sessionId ? (
    <InspectNavigation key={sessionId} sessionId={sessionId} />
  ) : (
    <Overlay title="Inspect" focusContent={false}>
      <p>Select a conversation to inspect.</p>
    </Overlay>
  );
}
