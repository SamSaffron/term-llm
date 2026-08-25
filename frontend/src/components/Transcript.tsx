import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import type { Message, ToolCall } from '../domain/types';
import { indexTranscriptTurns, windowTranscript } from '../domain/transcript';
import { useStore } from '../app/context';
import { Markdown } from './Markdown';
import { Icon } from './Icon';
import { copyText } from '../platform/browser';
import { rebaseHubAssetURL } from '../app/config';

function relativeTime(value: number): string { const seconds = Math.max(0, Math.round((Date.now() - value) / 1000)); if (seconds < 60) return 'now'; if (seconds < 3600) return `${Math.floor(seconds / 60)}m`; if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`; return `${Math.floor(seconds / 86400)}d`; }
function toolSummary(tool: ToolCall): string {
  let args: Record<string, unknown> = {}; try { args = JSON.parse(tool.arguments || '{}') as Record<string, unknown>; } catch { return tool.arguments || ''; }
  const preferred: Record<string, string[]> = {
    read_file: ['path'], write_file: ['path'], edit_file: ['path'], glob: ['pattern', 'path'], grep: ['pattern', 'path'], shell: ['description', 'command'],
    web_search: ['query'], read_url: ['url'], spawn_agent: ['agent_name', 'prompt', 'model'], image_generate: ['prompt', 'size', 'quality'],
  }; const keys = preferred[tool.name] || Object.keys(args);
  return keys.filter((key) => args[key] !== '' && args[key] != null).slice(0, 10).map((key) => `${key}: ${typeof args[key] === 'object' ? JSON.stringify(args[key]) : String(args[key])}`).join('\n');
}
function toolIcon(name: string): string { return ({ shell: '💻', bash: '💻', read_file: '📄', write_file: '✏️', edit_file: '✏️', web_search: '🔍', read_url: '🌐', image_generate: '🎨', spawn_agent: '🤖' } as Record<string, string>)[name.toLowerCase()] || '🔧'; }
function formatUsage(message: Message): string {
  const usage = message.usage || {}; const details = usage.input_tokens_details && typeof usage.input_tokens_details === 'object' ? usage.input_tokens_details as Record<string, unknown> : {};
  return `↙ ${Number(usage.input_tokens || 0).toLocaleString()} in · ${Number(usage.output_tokens || 0).toLocaleString()} out · ${Number(details.cached_tokens || usage.cached_input_tokens || 0).toLocaleString()} cached`;
}

function Tool({ tool }: { tool: ToolCall }) {
  const store = useStore(); const [expanded, setExpanded] = useState(tool.status === 'error'); const summary = toolSummary(tool);
  let formattedArguments = tool.arguments || ''; try { formattedArguments = JSON.stringify(JSON.parse(tool.arguments || '{}'), null, 2); } catch { /* Partial streaming arguments stay readable. */ }
  if (tool.name === 'update_plan' && tool.status === 'done' && tool.resultStatus !== 'error') return null;
  return <article class={`message tool tool-${tool.status}`} data-tool-id={tool.id}><div class="tool-card">
    <button class="tool-toggle" type="button" aria-expanded={expanded} onClick={() => setExpanded(!expanded)}><span class="tool-arrow"><Icon name="chevron-right" /></span><span class="tool-name">{tool.name}</span>{summary && <span class="tool-summary">{summary.split('\n')[0]}</span>}<span class={`tool-status ${tool.status === 'done' ? 'done' : ''}`}>{tool.status === 'running' ? 'running…' : tool.status}</span></button>
    {expanded && <div class="tool-details open">{formattedArguments && <><div class="tool-details-label">Arguments</div><pre><code>{formattedArguments}</code></pre></>}{tool.guardianReviews?.map((review, index) => <div class={`guardian-review guardian-${review.outcome || 'notice'}`} key={`${review.outcome}-${index}`}><strong>{review.outcome || 'Guardian review'}</strong>{review.message && <span>{review.message}</span>}{(review.command || review.path) && <code>{review.command || review.path}</code>}</div>)}{tool.result && <pre class="tool-result"><code>{tool.result}</code></pre>}{tool.subagent && <div class="subagent-result"><strong>{String(tool.subagent.agentName || 'Agent')}</strong>{tool.subagent.output && <Markdown value={String(tool.subagent.output)} className="markdown-body" />}{tool.subagent.childSessionId && <button class="text-action" onClick={() => { const session = store.sessions.value.find((entry) => entry.id === tool.subagent?.childSessionId); if (session) void store.selectSession(session); }}>Open child conversation</button>}</div>}{tool.images?.map((raw) => { const src = rebaseHubAssetURL(store.config, raw); return <button class="message-image-button" key={src} onClick={() => { store.lightbox.value = { src, type: 'image' }; }}><img src={src} alt={`${tool.name} output`} loading="lazy" /></button>; })}{summary && <button class="tool-copy text-action" onClick={() => void copyText(summary)}>Copy details</button>}</div>}
  </div></article>;
}

function ToolGroup({ tools }: { tools: ToolCall[] }) {
  const visible = tools.filter((tool) => !(tool.name === 'update_plan' && tool.status === 'done' && tool.resultStatus !== 'error'));
  const active = visible.some((tool) => tool.status === 'running' || tool.status === 'error'); const [expanded, setExpanded] = useState(active); useEffect(() => { if (active) setExpanded(true); }, [active]);
  if (!visible.length) return null; if (visible.length === 1) return <Tool tool={visible[0]} />;
  const names = [...new Set(visible.map((tool) => tool.name))]; const status = visible.some((tool) => tool.status === 'error') ? 'error' : visible.some((tool) => tool.status === 'running') ? 'running…' : 'done';
  return <article class="tool-group-card"><button class="tool-group-toggle" type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}><span class="tool-arrow"><Icon name="chevron-right" /></span><span class="tool-group-summary">{visible.length} tool calls · {names.slice(0, 3).join(', ')}{names.length > 3 ? '…' : ''}</span><span class={`tool-status ${status === 'done' ? 'done' : ''}`}>{status}</span></button><div class={`tool-group-details ${expanded ? 'open' : ''}`}>{visible.map((tool) => <div class="tool-group-entry" key={tool.id}><span aria-hidden="true">{toolIcon(tool.name)}</span><div class="tool-group-entry-body"><Tool tool={tool} /></div></div>)}</div></article>;
}

function Attachments({ message }: { message: Message }) {
  const store = useStore(); return <div class="message-attachments">{message.attachments?.map((attachment, index) => {
    const src = rebaseHubAssetURL(store.config, attachment.previewURL || attachment.url || attachment.dataURL || '');
    if (attachment.mention && !src) return <span class="message-file" key={`${attachment.name}-${index}`}>{attachment.name}</span>;
    if (attachment.type.startsWith('image/')) return <button class="message-image-button" type="button" key={`${attachment.name}-${index}`} onClick={() => { store.lightbox.value = { src, type: 'image' }; }}><img src={src} alt={attachment.name} width={attachment.width} height={attachment.height} loading="lazy" /></button>;
    if (attachment.type.startsWith('video/')) return <button class="message-image-button" type="button" key={`${attachment.name}-${index}`} onClick={() => { store.lightbox.value = { src, type: 'video' }; }}><video src={src} playsInline /></button>;
    if (attachment.type.startsWith('audio/')) return <audio key={`${attachment.name}-${index}`} src={src} controls />;
    return <a class="message-file" key={`${attachment.name}-${index}`} href={src} target="_blank" rel="noopener">{attachment.name}</a>;
  })}</div>;
}

function MessageMeta({ message }: { message: Message }) {
  const interrupt = ({ evaluating: ['pending', '', 'evaluating…'], pending_interject: ['pending', '⏳', 'will incorporate'], interject: ['interject', '✓', 'injected'], cancel: ['cancel', '⏹', 'cancelled + queued'], queue: ['queue', '⏳', 'queued'], error: ['error', '⚠', 'failed'] } as Record<string, [string, string, string]>)[String(message.interruptState || '')];
  return <div class="message-meta">{message.role === 'user' && interrupt && <span class={`interrupt-badge ${interrupt[0]}`}>{interrupt[1] || <span class="interrupt-spinner" />}{interrupt[2]}</span>}<time title={new Date(message.created).toLocaleString()}>{relativeTime(message.created)}</time></div>;
}

function TurnCopyButton({ text }: { text: string }) {
  const [feedback, setFeedback] = useState<'idle' | 'copying' | 'copied' | 'failed'>('idle');
  const resetTimer = useRef<number | undefined>(undefined);
  useEffect(() => () => { if (resetTimer.current !== undefined) window.clearTimeout(resetTimer.current); }, []);
  const copied = feedback === 'copied'; const failed = feedback === 'failed';
  const label = copied ? 'Copied' : failed ? 'Copy failed' : 'Copy response';
  const copy = async () => {
    setFeedback('copying');
    try { await copyText(text); setFeedback('copied'); }
    catch { setFeedback('failed'); }
    if (resetTimer.current !== undefined) window.clearTimeout(resetTimer.current);
    resetTimer.current = window.setTimeout(() => { setFeedback('idle'); resetTimer.current = undefined; }, 1_500);
  };
  return <button class={`turn-action-btn turn-copy-btn ${copied ? 'copied' : ''}`} type="button" title={label} aria-label={label} disabled={feedback === 'copying'} onClick={() => void copy()}><Icon name={copied ? 'check' : 'copy'} /></button>;
}

function MessageRow({ message, streaming, turnText, copyTarget }: { message: Message; streaming: boolean; turnText: string; copyTarget: boolean }) {
  const store = useStore(); const [expanded, setExpanded] = useState(false); const rebase = (value: string) => rebaseHubAssetURL(store.config, value); const media = (src: string, type: 'image' | 'video') => { if (!streaming) store.lightbox.value = { src, type }; };
  if (message.role === 'tool-group') return <div class="tool-group" data-message-id={message.id}><ToolGroup tools={message.tools || []} /></div>;
  if (message.role === 'compaction' || message.role === 'compaction-boundary') return <article class={`message compaction-boundary ${message.activeBoundary ? 'active' : ''}`} data-message-id={message.id}><div class={`message-body ${message.activeBoundary ? 'active-boundary' : ''}`}><button type="button" class="compaction-toggle" onClick={() => setExpanded(!expanded)} aria-expanded={expanded}>◇ {message.content || 'Context compacted'}{message.lineCount ? ` · ${message.lineCount} lines` : ''}</button>{expanded && message.rawContent && <Markdown value={message.rawContent} className="compaction-raw markdown-body" />}</div></article>;
  if (message.role === 'model-swap' || message.role === 'phase') return <article class={`message ${message.role}`} data-message-id={message.id}><div class="message-body">↔ {message.content}</div></article>;
  if (message.role === 'path-note') return <article class="message path-note" data-message-id={message.id}><details><summary>Notes from an earlier path · not authoritative</summary><Markdown value={message.content} className="path-note-body markdown-body" rebase={rebase} onMedia={media} /></details></article>;
  if (message.role === 'skill-run') return <article class={`message skill-run skill-${message.status || 'running'}`} data-message-id={message.id}><div class="message-body"><strong>{String(message.skill || 'Skill')}</strong><Markdown value={message.content} className="markdown-body" rebase={rebase} onMedia={media} />{!['complete', 'completed', 'failed', 'cancelled'].includes(String(message.status)) && message.runId && <button class="text-action" onClick={() => void store.cancelSkill(String(message.runId))}>Cancel</button>}{message.childSessionId && <button class="text-action" onClick={() => { const child = store.sessions.value.find((entry) => entry.id === message.childSessionId); if (child) void store.selectSession(child); }}>Open child conversation</button>}</div></article>;
  return <article class={`message ${message.role} ${message.askUser ? 'ask-user-answer' : ''}`} data-message-id={message.id} data-created={message.created} data-durable-id={message.durableRowId} dir="auto">
    <div class="message-body">{message.attachments?.length ? <Attachments message={message} /> : null}{message.diffComments?.map((comment, index) => <div class="message-diff-comment" key={`${comment.path}-${comment.line}-${index}`}><strong>{comment.path}:{comment.line}</strong> {comment.body}</div>)}
      {message.content && (message.role === 'assistant' ? <Markdown value={message.content} streaming={streaming} className="markdown-body" rebase={rebase} onMedia={media} /> : <div class="message-text">{message.content}</div>)}
    </div>
    {message.usage && <div class="usage-line">{formatUsage(message)}</div>}
    {message.role === 'assistant' && message.content && copyTarget && <div class="turn-action-panel"><TurnCopyButton text={turnText || message.content} />{message.durableRowId && <button class="text-action" type="button" onClick={() => store.openBranchContext(String(message.durableRowId))}>Branch from here</button>}</div>}
    <MessageMeta message={message} />
  </article>;
}

export function Transcript() {
  const store = useStore(); const scroll = useRef<HTMLElement>(null); const [nearTail, setNearTail] = useState(true); const [turnLimit, setTurnLimit] = useState(80); const [clock, setClock] = useState(0); const anchorHeight = useRef(0);
  const messages = store.visibleMessages.value; const runs = useMemo(() => windowTranscript(messages, turnLimit, nearTail), [messages, turnLimit, nearTail]);
  const rowContexts = useMemo(() => indexTranscriptTurns(messages, (message) => message.content || message.tools?.map((tool) => `${tool.name}\n${toolSummary(tool)}\n${tool.result || ''}`).join('\n') || ''), [messages]);
  useEffect(() => { setTurnLimit(80); }, [store.activeSession.value?.id]);
  useEffect(() => { const timer = window.setInterval(() => setClock((value) => value + 1), 60_000); return () => clearInterval(timer); }, []); void clock;
  useLayoutEffect(() => { const element = scroll.current; if (element && nearTail) element.scrollTop = element.scrollHeight; }, [store.activeSession.value?.id, messages.length, messages.at(-1)?.content, nearTail]);
  useLayoutEffect(() => { const element = scroll.current; if (element && anchorHeight.current) { element.scrollTop += element.scrollHeight - anchorHeight.current; anchorHeight.current = 0; } }, [turnLimit]);
  const activeRun = store.activeProjection.value;
  return <section class="chat-scroll" id="chatScroll" ref={scroll} onScroll={(event) => { const element = event.currentTarget; setNearTail(element.scrollHeight - element.scrollTop - element.clientHeight < 96); }}>
    <div class="messages" id="messages" data-session-id={store.activeSession.value?.id || ''}>{!messages.length && <div class="empty-chat"><h2>{store.config.title || 'How can I help?'}</h2><p>Start a conversation with your agent.</p></div>}
      {runs.map((run) => run.type === 'gap' ? <button key={run.key} class="transcript-gap" style={{ height: `${run.height}px` }} onClick={() => { anchorHeight.current = scroll.current?.scrollHeight || 0; setTurnLimit((value) => value + 80); }}>Load {run.count} earlier messages</button> : run.messages?.map((message) => { const context = rowContexts.get(message); const streaming = Boolean(activeRun && message.role === 'assistant' && message.responseId === activeRun.run.responseId && ['connecting', 'streaming'].includes(activeRun.run.status)); return <MessageRow key={message.id} message={message} streaming={streaming} turnText={context?.turnText || message.content} copyTarget={context?.copyTarget === true} />; }))}
      {activeRun?.phase && <div class="message phase transient" role="status">{activeRun.phase}</div>}{activeRun?.modelSwap && <div class="message model-swap transient" role="status">{activeRun.modelSwap.content}</div>}{activeRun?.retry && <div class="provider-retry" role="status">Retrying provider{activeRun.retry.attempt ? ` · attempt ${activeRun.retry.attempt}` : ''}…</div>}{store.streaming.value && <div class="streaming-indicator" aria-label="Assistant is responding"><span /><span /><span /></div>}
    </div>
  </section>;
}
