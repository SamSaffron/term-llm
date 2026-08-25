import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import type { Message, ToolCall } from '../domain/types';
import { indexTranscriptTurns, windowTranscript } from '../domain/transcript';
import { useStore } from '../app/context';
import { Markdown } from './Markdown';
import { copyText } from '../platform/browser';
import { rebaseHubAssetURL } from '../app/config';

function relativeTime(value: number): string { const seconds = Math.max(0, Math.round((Date.now() - value) / 1000)); if (seconds < 60) return 'now'; if (seconds < 3600) return `${Math.floor(seconds / 60)}m`; if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`; return `${Math.floor(seconds / 86400)}d`; }
function toolSummary(tool: ToolCall): string {
  let args: Record<string, unknown> = {}; try { args = JSON.parse(tool.arguments || '{}') as Record<string, unknown>; } catch { return tool.arguments || ''; }
  const preferred: Record<string, string[]> = { spawn_agent: ['agent_name', 'prompt', 'model', 'timeout'], image_generate: ['prompt', 'size', 'quality'] }; const keys = preferred[tool.name] || Object.keys(args);
  return keys.filter((key) => args[key] !== '' && args[key] != null).slice(0, 10).map((key) => `${key}: ${typeof args[key] === 'object' ? JSON.stringify(args[key]) : String(args[key])}`).join('\n');
}

function Tool({ tool }: { tool: ToolCall }) {
  const store = useStore(); const [expanded, setExpanded] = useState(tool.status === 'error'); const summary = toolSummary(tool);
  let formattedArguments = tool.arguments || ''; try { formattedArguments = JSON.stringify(JSON.parse(tool.arguments || '{}'), null, 2); } catch { /* Partial streaming arguments stay readable. */ }
  if (tool.name === 'update_plan' && tool.status === 'done' && tool.resultStatus !== 'error') return null;
  return <div class={`tool-call tool-${tool.status}`} data-tool-id={tool.id}>
    <button class="tool-header" type="button" aria-expanded={expanded} onClick={() => setExpanded(!expanded)}><span class="tool-icon">{tool.status === 'running' ? '◌' : tool.status === 'error' ? '!' : '✓'}</span><span class="tool-name">{tool.name}</span><span class="tool-summary">{summary.split('\n')[0]}</span><span class="tool-status">{tool.status}</span></button>
    {tool.guardianReviews?.map((review, index) => <div class={`guardian-review guardian-${review.outcome || 'notice'}`} key={`${review.outcome}-${index}`}><strong>{review.outcome || 'Guardian review'}</strong>{review.message && <span>{review.message}</span>}{(review.command || review.path) && <code>{review.command || review.path}</code>}</div>)}
    {expanded && <div class="tool-body">{formattedArguments && <pre><code>{formattedArguments}</code></pre>}{tool.result && <pre class="tool-result"><code>{tool.result}</code></pre>}{tool.subagent && <div class="subagent-result"><strong>{String(tool.subagent.agentName || 'Agent')}</strong>{tool.subagent.output && <Markdown value={String(tool.subagent.output)} />}{tool.subagent.childSessionId && <button onClick={() => { const session = store.sessions.value.find((entry) => entry.id === tool.subagent?.childSessionId); if (session) void store.selectSession(session); }}>Open child conversation</button>}</div>}{tool.images?.map((raw) => { const src = rebaseHubAssetURL(store.config, raw); return <button class="message-image-button" key={src} onClick={() => { store.lightbox.value = { src, type: 'image' }; }}><img src={src} alt={`${tool.name} output`} loading="lazy" /></button>; })}{summary && <button class="tool-copy" onClick={() => void copyText(summary)}>Copy details</button>}</div>}
  </div>;
}

function Attachments({ message }: { message: Message }) {
  const store = useStore(); return <div class="message-attachments">{message.attachments?.map((attachment, index) => {
    const src = rebaseHubAssetURL(store.config, attachment.previewURL || attachment.url || attachment.dataURL || '');
    if (attachment.mention && !src) return <span class="message-file" key={`${attachment.name}-${index}`}>📎 {attachment.name}</span>;
    if (attachment.type.startsWith('image/')) return <button class="message-image-button" type="button" key={`${attachment.name}-${index}`} onClick={() => { store.lightbox.value = { src, type: 'image' }; }}><img src={src} alt={attachment.name} width={attachment.width} height={attachment.height} loading="lazy" /></button>;
    if (attachment.type.startsWith('video/')) return <button class="message-image-button" type="button" key={`${attachment.name}-${index}`} onClick={() => { store.lightbox.value = { src, type: 'video' }; }}><video src={src} playsInline /></button>;
    if (attachment.type.startsWith('audio/')) return <audio key={`${attachment.name}-${index}`} src={src} controls />;
    return <a class="message-file" key={`${attachment.name}-${index}`} href={src} target="_blank" rel="noopener">📎 {attachment.name}</a>;
  })}</div>;
}

function MessageRow({ message, streaming, turnText }: { message: Message; streaming: boolean; turnText: string }) {
  const store = useStore(); const [expanded, setExpanded] = useState(false); const rebase = (value: string) => rebaseHubAssetURL(store.config, value); const media = (src: string, type: 'image' | 'video') => { if (!streaming) store.lightbox.value = { src, type }; };
  if (message.role === 'tool-group') return <div class="message tool-group" data-message-id={message.id}>{message.tools?.map((tool) => <Tool key={tool.id} tool={tool} />)}</div>;
  if (message.role === 'compaction' || message.role === 'compaction-boundary') return <div class={`message compaction-boundary ${message.activeBoundary ? 'active' : ''}`} data-message-id={message.id}><button type="button" class="compaction-toggle" onClick={() => setExpanded(!expanded)} aria-expanded={expanded}>◇ {message.content || 'Context compacted'}{message.lineCount ? ` · ${message.lineCount} lines` : ''}</button>{expanded && message.rawContent && <Markdown value={message.rawContent} className="compaction-content markdown" />}</div>;
  if (message.role === 'model-swap' || message.role === 'phase') return <div class={`message ${message.role}`} data-message-id={message.id}>↔ {message.content}</div>;
  if (message.role === 'path-note') return <article class="message path-note" data-message-id={message.id}><details><summary>Notes from an earlier path · not authoritative</summary><Markdown value={message.content} rebase={rebase} onMedia={media} /></details></article>;
  if (message.role === 'skill-run') return <div class={`message skill-run skill-${message.status || 'running'}`} data-message-id={message.id}><strong>{String(message.skill || 'Skill')}</strong><Markdown value={message.content} rebase={rebase} onMedia={media} />{!['complete', 'completed', 'failed', 'cancelled'].includes(String(message.status)) && message.runId && <button onClick={() => void store.cancelSkill(String(message.runId))}>Cancel</button>}{message.childSessionId && <button onClick={() => { const child = store.sessions.value.find((entry) => entry.id === message.childSessionId); if (child) void store.selectSession(child); }}>Open child conversation</button>}</div>;
  return <article class={`message ${message.role} ${message.askUser ? 'ask-user-answer' : ''}`} data-message-id={message.id} data-created={message.created} data-durable-id={message.durableRowId} dir="auto">
    <div class="message-header"><span class="message-role">{message.role === 'user' ? 'You' : message.role === 'assistant' ? 'Assistant' : 'Notice'}</span><time title={new Date(message.created).toLocaleString()}>{relativeTime(message.created)}</time></div>
    {message.attachments?.length ? <Attachments message={message} /> : null}{message.diffComments?.map((comment, index) => <div class="message-diff-comment" key={`${comment.path}-${comment.line}-${index}`}><strong>{comment.path}:{comment.line}</strong> {comment.body}</div>)}
    {message.content && (message.role === 'assistant' ? <Markdown value={message.content} streaming={streaming} rebase={rebase} onMedia={media} /> : <div class="message-text">{message.content}</div>)}
    {message.usage && <div class="message-usage">{Number(message.usage.total_tokens || 0).toLocaleString()} tokens</div>}
    {message.role === 'assistant' && message.content && <div class="message-actions"><button class="message-copy" type="button" aria-label="Copy response" onClick={() => void copyText(turnText || message.content)}>Copy turn</button>{message.durableRowId && <button type="button" onClick={() => void store.branchFrom(String(message.durableRowId), 'clean')}>Branch from here</button>}</div>}
  </article>;
}

export function Transcript() {
  const store = useStore(); const scroll = useRef<HTMLElement>(null); const [nearTail, setNearTail] = useState(true); const [turnLimit, setTurnLimit] = useState(80); const [clock, setClock] = useState(0); const anchorHeight = useRef(0);
  const messages = store.visibleMessages.value; const runs = useMemo(() => windowTranscript(messages, turnLimit, nearTail), [messages, turnLimit, nearTail]);
  const rowContexts = useMemo(() => indexTranscriptTurns(messages, (message) => message.content || message.tools?.map((tool) => `${tool.name}\n${toolSummary(tool)}\n${tool.result || ''}`).join('\n') || ''), [messages]);
  useEffect(() => { setTurnLimit(80); }, [store.activeSession.value?.id]);
  useEffect(() => { const timer = window.setInterval(() => setClock((value) => value + 1), 60_000); return () => clearInterval(timer); }, []); void clock;
  useEffect(() => { const element = scroll.current; if (!element || !nearTail) return; requestAnimationFrame(() => element.scrollTo({ top: element.scrollHeight, behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })); }, [messages.length, messages.at(-1)?.content, nearTail]);
  useLayoutEffect(() => { const element = scroll.current; if (element && anchorHeight.current) { element.scrollTop += element.scrollHeight - anchorHeight.current; anchorHeight.current = 0; } }, [turnLimit]);
  const activeRun = store.activeProjection.value;
  return <section class="chat-scroll" id="chatScroll" ref={scroll} onScroll={(event) => { const element = event.currentTarget; setNearTail(element.scrollHeight - element.scrollTop - element.clientHeight < 96); }}>
    <div class="messages" id="messages" data-session-id={store.activeSession.value?.id || ''}>{!messages.length && <div class="empty-chat"><h2>{store.config.title || 'How can I help?'}</h2><p>Start a conversation with your agent.</p></div>}
      {runs.map((run) => run.type === 'gap' ? <button key={run.key} class="transcript-gap" style={{ height: `${run.height}px` }} onClick={() => { anchorHeight.current = scroll.current?.scrollHeight || 0; setTurnLimit((value) => value + 80); }}>Load {run.count} earlier messages</button> : run.messages?.map((message) => { const context = rowContexts.get(message); const streaming = Boolean(activeRun && message.role === 'assistant' && message.responseId === activeRun.run.responseId && ['connecting', 'streaming'].includes(activeRun.run.status)); return <MessageRow key={message.id} message={message} streaming={streaming} turnText={context?.turnText || message.content} />; }))}
      {activeRun?.phase && <div class="message phase transient" role="status">{activeRun.phase}</div>}{activeRun?.modelSwap && <div class="message model-swap transient" role="status">{activeRun.modelSwap.content}</div>}{activeRun?.retry && <div class="provider-retry" role="status">Retrying provider{activeRun.retry.attempt ? ` · attempt ${activeRun.retry.attempt}` : ''}…</div>}{store.streaming.value && <div class="streaming-indicator" aria-label="Assistant is responding"><span /><span /><span /></div>}
    </div>
  </section>;
}
