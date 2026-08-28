import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import type { Message, ToolCall } from '../domain/types';
import { indexTranscriptTurns, windowTranscript } from '../domain/transcript';
import { useStore } from '../app/context';
import { Markdown } from './Markdown';
import { ChipPicker } from './ChipPicker';
import { Icon } from './Icon';
import { copyText } from '../platform/browser';
import { rebaseHubAssetURL } from '../app/config';
import { responseActivity } from '../domain/activity';
import type { AppStore } from '../stores/app-store';

function openMediaGallery(store: AppStore, src: string, type: 'image' | 'video'): void {
  const nodes = [...document.querySelectorAll<HTMLElement>('[data-lightbox-src]')];
  const items = nodes
    .map((node, index) => ({
      key: node.dataset.lightboxKey || `${node.dataset.lightboxSrc}-${index}`,
      src: node.dataset.lightboxSrc || '',
      type: node.dataset.lightboxType === 'video' ? ('video' as const) : ('image' as const),
      name: node.dataset.lightboxName || '',
    }))
    .filter((item) => item.src);
  let index = items.findIndex((item) => item.src === src && item.type === type);
  if (index < 0) {
    items.push({ key: `${src}-${items.length}`, src, type, name: '' });
    index = items.length - 1;
  }
  store.lightbox.value = { ...items[index], items, index };
}

function relativeTime(value: number): string {
  const seconds = Math.max(0, Math.round((Date.now() - value) / 1000));
  if (seconds < 60) return 'now';
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}
function parseToolArguments(
  raw: string,
): { entries: [string, unknown][]; fallback: string } | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed))
      return { entries: [], fallback: raw };
    return { entries: Object.entries(parsed as Record<string, unknown>), fallback: '' };
  } catch {
    return { entries: [], fallback: raw };
  }
}

function ToolArgumentValue({ value }: { value: unknown }) {
  if (value == null || typeof value === 'boolean' || typeof value === 'number')
    return <code class="tool-argument-literal">{value == null ? 'null' : String(value)}</code>;
  if (typeof value === 'string')
    return (
      <span class={`tool-argument-text ${value.includes('\n') ? 'multiline' : ''}`}>{value}</span>
    );
  const compact = JSON.stringify(value) || String(value);
  const formatted = compact.length > 100 ? JSON.stringify(value, null, 2) : compact;
  return (
    <code class={`tool-argument-structured ${compact.length > 100 ? 'multiline' : ''}`}>
      {formatted}
    </code>
  );
}

function ToolArguments({ raw }: { raw: string }) {
  const parsed = parseToolArguments(raw);
  if (!parsed || (!parsed.entries.length && !parsed.fallback)) return null;
  return (
    <>
      <div class="tool-details-label">Arguments</div>
      {parsed.entries.length ? (
        <dl class="tool-arguments">
          {parsed.entries.map(([name, value]) => (
            <div class="tool-argument" key={name}>
              <dt>{name}</dt>
              <dd>
                <ToolArgumentValue value={value} />
              </dd>
            </div>
          ))}
        </dl>
      ) : (
        <pre class="tool-arguments-fallback">
          <code>{parsed.fallback}</code>
        </pre>
      )}
    </>
  );
}

function toolSummary(tool: ToolCall): string {
  let args: Record<string, unknown> = {};
  try {
    args = JSON.parse(tool.arguments || '{}') as Record<string, unknown>;
  } catch {
    return tool.arguments || '';
  }
  const preferred: Record<string, string[]> = {
    read_file: ['path'],
    write_file: ['path'],
    edit_file: ['path'],
    glob: ['pattern', 'path'],
    grep: ['pattern', 'path'],
    shell: ['description', 'command'],
    web_search: ['query'],
    read_url: ['url'],
    spawn_agent: ['agent_name', 'prompt', 'model'],
    image_generate: ['prompt', 'size', 'quality'],
  };
  const keys = preferred[tool.name] || Object.keys(args);
  return keys
    .filter((key) => args[key] !== '' && args[key] != null)
    .slice(0, 10)
    .map(
      (key) =>
        `${key}: ${typeof args[key] === 'object' ? JSON.stringify(args[key]) : String(args[key])}`,
    )
    .join('\n');
}
function toolIcon(name: string): string {
  return (
    (
      {
        shell: '💻',
        bash: '💻',
        read_file: '📄',
        write_file: '✏️',
        edit_file: '✏️',
        web_search: '🔍',
        grep: '🔍',
        read_url: '🌐',
        image_generate: '🎨',
        spawn_agent: '🤖',
      } as Record<string, string>
    )[name.toLowerCase()] || '🔧'
  );
}
function guardianReviewReason(message: string, outcome: string): string {
  const text = message.trim().replace(/^guardian:\s*/i, '');
  if (!text) return '';
  return outcome === 'denied' || outcome === 'error'
    ? text.replace(/^(?:denied|error)\b[:;,.\s-]*/i, '').trim()
    : text;
}

function formatUsage(message: Message): string {
  const usage = message.usage || {};
  const details =
    usage.input_tokens_details && typeof usage.input_tokens_details === 'object'
      ? (usage.input_tokens_details as Record<string, unknown>)
      : {};
  return `↙ ${Number(usage.input_tokens || 0).toLocaleString()} in · ${Number(usage.output_tokens || 0).toLocaleString()} out · ${Number(details.cached_tokens || usage.cached_input_tokens || 0).toLocaleString()} cached`;
}

function Tool({
  tool,
  expanded: controlledExpanded,
  onToggle,
}: {
  tool: ToolCall;
  expanded?: boolean;
  onToggle?: () => void;
}) {
  const store = useStore();
  const failed = tool.status === 'error' || tool.resultStatus === 'error';
  const stopped = tool.status === 'cancelled';
  const [localExpanded, setLocalExpanded] = useState(false);
  const expanded = controlledExpanded ?? localExpanded;
  const summary = toolSummary(tool);
  const failureReason = failed ? String(tool.result || tool.subagent?.error || '').trim() : '';
  if (tool.name === 'update_plan' && tool.status === 'done' && tool.resultStatus !== 'error')
    return null;
  return (
    <article class={`message tool tool-${tool.status}`} data-tool-id={tool.id}>
      <div class="tool-card">
        <button
          class="tool-toggle"
          type="button"
          aria-expanded={expanded}
          onClick={onToggle || (() => setLocalExpanded((value) => !value))}
        >
          <span class="tool-name">{tool.name}</span>
          {summary && <span class="tool-summary">{summary.split('\n')[0]}</span>}
          <span
            class={`tool-status ${failed ? 'error' : tool.status === 'done' ? 'done' : ''}`}
            aria-label={
              failed
                ? 'Failed'
                : stopped
                  ? 'Stopped'
                  : tool.status === 'done'
                    ? 'Complete'
                    : undefined
            }
          >
            {tool.status === 'running' ? 'running…' : failed ? '✕' : stopped ? 'stopped' : '✓'}
          </span>
        </button>
        {expanded && (
          <div class="tool-details open">
            <ToolArguments raw={tool.arguments || ''} />
            {tool.guardianReviews?.map((review, index) => {
              const outcome = String(review.outcome || 'notice').toLowerCase();
              const denied = outcome === 'denied' || outcome === 'error';
              const label = outcome === 'approved' ? 'approved' : denied ? 'denied' : outcome;
              const reason =
                outcome === 'approved' ? '' : guardianReviewReason(review.message || '', outcome);
              return (
                <div
                  class={`guardian-review guardian-${denied ? 'denied' : outcome}`}
                  key={`${outcome}-${index}`}
                >
                  <strong>{label}</strong>
                  {reason && <span>{reason}</span>}
                </div>
              );
            })}
            {tool.result && !failed && (
              <pre class="tool-result">
                <code>{tool.result}</code>
              </pre>
            )}
            {tool.subagent && (
              <div class="subagent-result">
                <strong>{String(tool.subagent.agentName || 'Agent')}</strong>
                {tool.subagent.output && (
                  <Markdown value={String(tool.subagent.output)} className="markdown-body" />
                )}
                {tool.subagent.childSessionId && (
                  <button
                    class="text-action"
                    onClick={() => {
                      const session = store.sessions.value.find(
                        (entry) => entry.id === tool.subagent?.childSessionId,
                      );
                      if (session) void store.selectSession(session);
                    }}
                  >
                    Open child conversation
                  </button>
                )}
              </div>
            )}
            {tool.images?.map((raw) => {
              const src = rebaseHubAssetURL(store.config, raw);
              return (
                <button
                  class="message-image-button"
                  key={src}
                  data-lightbox-src={src}
                  data-lightbox-type="image"
                  data-lightbox-name={`${tool.name} output`}
                  onClick={() => {
                    openMediaGallery(store, src, 'image');
                  }}
                >
                  <img src={src} alt={`${tool.name} output`} loading="lazy" />
                </button>
              );
            })}
            {failureReason && (
              <div class="tool-failure-reason" role="alert">
                <strong>Failure</strong>
                <pre>{failureReason}</pre>
              </div>
            )}
          </div>
        )}
      </div>
    </article>
  );
}

function ToolGroup({ tools }: { tools: ToolCall[] }) {
  const visible = tools.filter(
    (tool) =>
      !(tool.name === 'update_plan' && tool.status === 'done' && tool.resultStatus !== 'error'),
  );
  const running = visible.some((tool) => tool.status === 'running');
  const [expanded, setExpanded] = useState(false);
  if (!visible.length) return null;
  if (visible.length === 1)
    return (
      <Tool tool={visible[0]} expanded={expanded} onToggle={() => setExpanded((value) => !value)} />
    );
  const names = [...new Set(visible.map((tool) => tool.name))];
  const stopped = !running && visible.some((tool) => tool.status === 'cancelled');
  return (
    <article class="tool-group-card">
      <button
        class="tool-group-toggle"
        type="button"
        aria-expanded={expanded}
        onClick={() => setExpanded((value) => !value)}
      >
        <span class="tool-arrow">
          <Icon name="chevron-right" />
        </span>
        <span class="tool-group-summary">
          {visible.length} tool calls · {names.slice(0, 3).join(', ')}
          {names.length > 3 ? '…' : ''}
        </span>
        <span
          class={`tool-status ${running || stopped ? '' : 'done'}`}
          aria-label={running ? undefined : stopped ? 'Stopped' : 'Complete'}
        >
          {running ? 'running…' : stopped ? 'stopped' : '✓'}
        </span>
      </button>
      <div class={`tool-group-details ${expanded ? 'open' : ''}`}>
        {visible.map((tool) => (
          <div class="tool-group-entry" key={tool.id}>
            <span class="tool-entry-icon" aria-hidden="true">
              {toolIcon(tool.name)}
            </span>
            <div class="tool-group-entry-body">
              <Tool tool={tool} />
            </div>
          </div>
        ))}
      </div>
    </article>
  );
}

function Attachments({ message }: { message: Message }) {
  const store = useStore();
  return (
    <div class="message-attachments">
      {message.attachments?.map((attachment, index) => {
        const src = rebaseHubAssetURL(
          store.config,
          attachment.previewURL || attachment.url || attachment.dataURL || '',
        );
        if (attachment.mention && !src)
          return (
            <span class="message-file" key={`${attachment.name}-${index}`}>
              {attachment.name}
            </span>
          );
        if (attachment.type.startsWith('image/'))
          return (
            <button
              class="message-image-button"
              type="button"
              key={`${attachment.name}-${index}`}
              data-lightbox-src={src}
              data-lightbox-type="image"
              data-lightbox-name={attachment.name}
              onClick={() => {
                openMediaGallery(store, src, 'image');
              }}
            >
              <img
                src={src}
                alt={attachment.name}
                width={attachment.width}
                height={attachment.height}
                loading="lazy"
              />
            </button>
          );
        if (attachment.type.startsWith('video/'))
          return (
            <button
              class="message-image-button"
              type="button"
              key={`${attachment.name}-${index}`}
              data-lightbox-src={src}
              data-lightbox-type="video"
              data-lightbox-name={attachment.name}
              onClick={() => {
                openMediaGallery(store, src, 'video');
              }}
            >
              <video src={src} playsInline />
            </button>
          );
        if (attachment.type.startsWith('audio/'))
          return <audio key={`${attachment.name}-${index}`} src={src} controls />;
        return (
          <a
            class="message-file"
            key={`${attachment.name}-${index}`}
            href={src}
            target="_blank"
            rel="noopener"
          >
            {attachment.name}
          </a>
        );
      })}
    </div>
  );
}

function MessageMeta({ message }: { message: Message }) {
  const interrupt = (
    {
      evaluating: ['pending', '', 'evaluating…'],
      checking_send: ['pending', '⏳', 'checking whether this was sent'],
      pending_interject: ['pending', '⏳', 'will incorporate'],
      interject: ['interject', '✓', 'injected'],
      cancel: ['cancel', '⏹', 'cancelled + queued'],
      queue: ['queue', '⏳', 'queued'],
      error: ['error', '⚠', 'failed'],
    } as Record<string, [string, string, string]>
  )[String(message.interruptState || '')];
  return (
    <div class="message-meta">
      {message.role === 'user' && interrupt && (
        <span class={`interrupt-badge ${interrupt[0]}`}>
          {interrupt[1] || <span class="interrupt-spinner" />}
          {interrupt[2]}
        </span>
      )}
      <time title={new Date(message.created).toLocaleString()}>
        {relativeTime(message.created)}
      </time>
    </div>
  );
}

function TurnCopyButton({ text }: { text: string }) {
  const [feedback, setFeedback] = useState<'idle' | 'copying' | 'copied' | 'failed'>('idle');
  const resetTimer = useRef<number | undefined>(undefined);
  useEffect(
    () => () => {
      if (resetTimer.current !== undefined) window.clearTimeout(resetTimer.current);
    },
    [],
  );
  const copied = feedback === 'copied';
  const failed = feedback === 'failed';
  const label = copied ? 'Copied' : failed ? 'Copy failed' : 'Copy response';
  const copy = async () => {
    setFeedback('copying');
    try {
      await copyText(text);
      setFeedback('copied');
    } catch {
      setFeedback('failed');
    }
    if (resetTimer.current !== undefined) window.clearTimeout(resetTimer.current);
    resetTimer.current = window.setTimeout(() => {
      setFeedback('idle');
      resetTimer.current = undefined;
    }, 1_500);
  };
  return (
    <button
      class={`turn-action-btn turn-copy-btn ${copied ? 'copied' : ''}`}
      type="button"
      title={label}
      aria-label={label}
      disabled={feedback === 'copying'}
      onClick={() => void copy()}
    >
      <Icon name={copied ? 'check' : 'copy'} />
    </button>
  );
}

function MessageRow({
  message,
  streaming,
  turnText,
  copyTarget,
}: {
  message: Message;
  streaming: boolean;
  turnText: string;
  copyTarget: boolean;
}) {
  const store = useStore();
  const [expanded, setExpanded] = useState(false);
  const rebase = (value: string) => rebaseHubAssetURL(store.config, value);
  const media = (src: string, type: 'image' | 'video') => {
    if (!streaming) openMediaGallery(store, src, type);
  };
  if (message.role === 'tool-group')
    return (
      <div class="tool-group" data-message-id={message.id}>
        <ToolGroup tools={message.tools || []} />
      </div>
    );
  if (message.role === 'compaction' || message.role === 'compaction-boundary')
    return (
      <article
        class={`message compaction-boundary ${message.activeBoundary ? 'active' : ''}`}
        data-message-id={message.id}
      >
        <div class={`message-body ${message.activeBoundary ? 'active-boundary' : ''}`}>
          <button
            type="button"
            class="compaction-toggle"
            onClick={() => setExpanded(!expanded)}
            aria-expanded={expanded}
          >
            <Icon name="compact" class="compaction-icon" />
            <span>
              {message.content || 'Context compacted'}
              {message.lineCount ? ` · ${message.lineCount} lines` : ''}
            </span>
          </button>
          {expanded && message.rawContent && (
            <Markdown value={message.rawContent} className="compaction-raw markdown-body" />
          )}
        </div>
      </article>
    );
  if (message.role === 'model-swap' || message.role === 'phase') {
    const content = message.content.trimStart().startsWith('↔')
      ? message.content
      : `↔ ${message.content}`;
    return (
      <article class={`message ${message.role}`} data-message-id={message.id}>
        <div class="message-body">{content}</div>
      </article>
    );
  }
  if (message.role === 'path-note')
    return (
      <article class="message path-note" data-message-id={message.id}>
        <details>
          <summary>Notes from an earlier path · not authoritative</summary>
          <Markdown
            value={message.content}
            className="path-note-body markdown-body"
            rebase={rebase}
            onMedia={media}
          />
        </details>
      </article>
    );
  if (message.role === 'skill-run')
    return (
      <article
        class={`message skill-run skill-${message.status || 'running'}`}
        data-message-id={message.id}
      >
        <div class="message-body">
          <strong>{String(message.skill || 'Skill')}</strong>
          <Markdown
            value={message.content}
            className="markdown-body"
            rebase={rebase}
            onMedia={media}
          />
          {!['complete', 'completed', 'failed', 'cancelled'].includes(String(message.status)) &&
            message.runId && (
              <button
                class="text-action"
                onClick={() => void store.cancelSkill(String(message.runId))}
              >
                Cancel
              </button>
            )}
          {message.childSessionId && (
            <button
              class="text-action"
              onClick={() => {
                const child = store.sessions.value.find(
                  (entry) => entry.id === message.childSessionId,
                );
                if (child) void store.selectSession(child);
              }}
            >
              Open child conversation
            </button>
          )}
        </div>
      </article>
    );
  return (
    <article
      class={`message ${message.role} ${message.askUser ? 'ask-user-answer' : ''}`}
      data-message-id={message.id}
      data-created={message.created}
      data-durable-id={message.durableRowId}
      dir="auto"
    >
      <div class="message-body">
        {message.attachments?.length ? <Attachments message={message} /> : null}
        {message.diffComments?.map((comment, index) => (
          <div class="message-diff-comment" key={`${comment.path}-${comment.line}-${index}`}>
            <strong>
              {comment.path}:{comment.line}
            </strong>{' '}
            {comment.body}
          </div>
        ))}
        {message.content &&
          (message.role === 'assistant' ? (
            <Markdown
              value={message.content}
              streaming={streaming}
              className="markdown-body"
              rebase={rebase}
              onMedia={media}
            />
          ) : (
            <div class="message-text">{message.content}</div>
          ))}
      </div>
      {message.usage && <div class="usage-line">{formatUsage(message)}</div>}
      {message.role === 'assistant' && message.content && copyTarget && (
        <div class="turn-action-panel">
          <TurnCopyButton text={turnText || message.content} />
          {message.durableRowId && (
            <button
              class="text-action"
              type="button"
              onClick={() => store.openBranchContext(String(message.durableRowId))}
            >
              Branch from here
            </button>
          )}
        </div>
      )}
      <MessageMeta message={message} />
    </article>
  );
}

function NewChatControls() {
  const store = useStore();
  if (!store.draftActive.value) return null;
  const projects = store.projects.value
    .filter((project) => !project.archived && project.available !== false)
    .sort((left, right) => left.name.localeCompare(right.name));
  return (
    <div class="new-chat-pickers">
      {store.config.agentNames.length > 0 && (
        <div class="new-chat-project-picker new-chat-agent-picker">
          <ChipPicker
            ariaLabel="Choose agent for new chat"
            value={store.selectedAgent.value}
            options={[
              { value: '', label: 'Default' },
              ...store.config.agentNames.map((agent) => ({ value: agent, label: agent })),
            ]}
            triggerClass="new-chat-project-trigger new-chat-agent-trigger"
            onChange={(agent) => store.setPreference('agent', agent)}
            renderTrigger={(selected) => (
              <>
                <span class="new-chat-project-label">Agent</span>
                <span class="new-chat-project-value">{selected.label}</span>
                <span class="new-chat-project-chevron">⌄</span>
              </>
            )}
          />
        </div>
      )}
      {store.projectsEnabled.value && (
        <div class="new-chat-project-picker">
          <ChipPicker
            ariaLabel="Choose chat or project"
            value={store.activeProjectId.value}
            options={[
              { value: '', label: 'Chat' },
              ...projects.map((project) => ({ value: project.id, label: project.name })),
            ]}
            triggerClass="new-chat-project-trigger"
            actions={[
              {
                label: 'Add project…',
                icon: <Icon name="add" />,
                onSelect: () => store.openAddProject(),
              },
            ]}
            onChange={(projectId) => store.newChat(true, projectId)}
            renderTrigger={(selected) => (
              <>
                {selected.value && <span class="new-chat-project-label">Project</span>}
                <span class="new-chat-project-value">{selected.label}</span>
                <span class="new-chat-project-chevron">⌄</span>
              </>
            )}
          />
        </div>
      )}
    </div>
  );
}

export function Transcript() {
  const store = useStore();
  const scroll = useRef<HTMLElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const stickToTail = useRef(true);
  const touchY = useRef<number | null>(null);
  const [nearTail, setNearTail] = useState(true);
  const [turnLimit, setTurnLimit] = useState(80);
  const [clock, setClock] = useState(0);
  const anchorHeight = useRef(0);
  const messages = store.visibleMessages.value;
  const runs = useMemo(
    () => windowTranscript(messages, turnLimit, nearTail),
    [messages, turnLimit, nearTail],
  );
  const rowContexts = useMemo(
    () =>
      indexTranscriptTurns(
        messages,
        (message) =>
          message.content ||
          message.tools
            ?.map((tool) => `${tool.name}\n${toolSummary(tool)}\n${tool.result || ''}`)
            .join('\n') ||
          '',
      ),
    [messages],
  );
  useEffect(() => {
    setTurnLimit(80);
  }, [store.activeSession.value?.id]);
  useEffect(() => {
    const timer = window.setInterval(() => setClock((value) => value + 1), 60_000);
    return () => clearInterval(timer);
  }, []);
  void clock;
  useLayoutEffect(() => {
    const element = scroll.current;
    const contents = content.current;
    if (!element || !contents) return;

    stickToTail.current = true;
    setNearTail(true);
    const scrollToTail = () => {
      if (stickToTail.current) element.scrollTop = element.scrollHeight;
    };
    scrollToTail();

    if (typeof ResizeObserver !== 'function') return;
    const observer = new ResizeObserver(scrollToTail);
    observer.observe(contents);
    return () => observer.disconnect();
  }, [store.activeSession.value?.id]);
  useLayoutEffect(() => {
    const element = scroll.current;
    if (element && stickToTail.current) element.scrollTop = element.scrollHeight;
  }, [messages]);
  useLayoutEffect(() => {
    const element = scroll.current;
    if (element && anchorHeight.current) {
      element.scrollTop += element.scrollHeight - anchorHeight.current;
      anchorHeight.current = 0;
    }
  }, [turnLimit]);
  const activeRun = store.activeProjection.value;
  const activity = activeRun
    ? responseActivity(activeRun, store.currentPlan.value, activeRun.run.status)
    : null;
  const activeOutput = activeRun?.messages.at(-1);
  const streamingText = Boolean(
    activeRun?.run.status === 'streaming' &&
    activeOutput?.role === 'assistant' &&
    activeOutput.responseId === activeRun.run.responseId,
  );
  const showActivity = Boolean(
    store.streaming.value && activity && !(streamingText && activity.kind === 'working'),
  );
  return (
    <section
      class="chat-scroll"
      id="chatScroll"
      ref={scroll}
      onWheel={(event) => {
        if (event.deltaY < 0) stickToTail.current = false;
      }}
      onTouchStart={(event) => {
        touchY.current = event.touches[0]?.clientY ?? null;
      }}
      onTouchMove={(event) => {
        const nextY = event.touches[0]?.clientY;
        if (nextY !== undefined && touchY.current !== null && nextY > touchY.current) {
          stickToTail.current = false;
        }
        touchY.current = nextY ?? null;
      }}
      onTouchEnd={() => {
        touchY.current = null;
      }}
      onScroll={(event) => {
        const element = event.currentTarget;
        const distanceFromTail = element.scrollHeight - element.scrollTop - element.clientHeight;

        if (distanceFromTail > 1) stickToTail.current = false;
        else if (distanceFromTail <= 0) stickToTail.current = true;

        setNearTail(distanceFromTail < 96);
      }}
    >
      <div
        class="messages"
        id="messages"
        ref={content}
        data-session-id={store.activeSession.value?.id || ''}
      >
        {!messages.length && (
          <div class="empty-chat">
            <h2>{store.config.title || 'How can I help?'}</h2>
            <p>Start a conversation with your agent.</p>
            <NewChatControls />
          </div>
        )}
        {runs.map((run) =>
          run.type === 'gap' ? (
            <button
              key={run.key}
              class="transcript-gap"
              style={{ height: `${run.height}px` }}
              onClick={() => {
                anchorHeight.current = scroll.current?.scrollHeight || 0;
                setTurnLimit((value) => value + 80);
              }}
            >
              Load {run.count} earlier messages
            </button>
          ) : (
            run.messages?.map((message) => {
              const context = rowContexts.get(message);
              const streaming = Boolean(
                activeRun &&
                message.role === 'assistant' &&
                message.responseId === activeRun.run.responseId &&
                ['connecting', 'streaming'].includes(activeRun.run.status),
              );
              return (
                <MessageRow
                  key={message.id}
                  message={message}
                  streaming={streaming}
                  turnText={context?.turnText || message.content}
                  copyTarget={context?.copyTarget === true}
                />
              );
            })
          ),
        )}
        {activeRun?.phase && (
          <div class="message phase transient" role="status">
            {activeRun.phase}
          </div>
        )}
        {activeRun?.modelSwap && (
          <div class="message model-swap transient" role="status">
            {activeRun.modelSwap.content}
          </div>
        )}
        {showActivity && activity && (
          <div
            class={`streaming-indicator streaming-indicator-${activity.kind}`}
            role="status"
            aria-live="polite"
            aria-atomic="true"
            aria-label={
              activity.kind === 'stopping'
                ? 'Stopping response'
                : `Assistant is responding: ${activity.text}`
            }
          >
            <span class="streaming-indicator-text" key={`${activity.kind}:${activity.text}`}>
              {activity.text}
            </span>
          </div>
        )}
      </div>
    </section>
  );
}
