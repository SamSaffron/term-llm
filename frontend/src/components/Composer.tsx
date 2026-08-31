import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import { displayName } from '../app/config';
import { useStore } from '../app/context';
import {
  activeMentionAtCursor,
  applyCompletion,
  completedCursor,
  composerCompletions,
  mentionCompletions,
  type Completion,
  type MentionSearchResponse,
} from '../domain/completions';
import { validateAttachmentFile } from '../domain/attachments';
import { VoiceOperation, type VoiceSnapshot } from '../platform/voice';
import { Icon } from './Icon';
import { useMenuKeyboard } from './Menu';
import { requestTranscriptScrollToTail } from './transcript-scroll';

function resizePrompt(element: HTMLTextAreaElement | null): void {
  if (!element) return;
  element.style.height = 'auto';
  if (element.value) element.style.height = `${Math.min(element.scrollHeight, 200)}px`;
}

export function insertTranscriptAtCaret(value: string, transcript: string, caret: number) {
  const clean = transcript.trim();
  if (!clean) return { value, caret: Math.max(0, Math.min(value.length, caret)) };
  const position = Math.max(0, Math.min(value.length, caret));
  const before = value.slice(0, position);
  const after = value.slice(position);
  const leading = before && !/\s$/.test(before) && !/^\s/.test(clean) ? ' ' : '';
  const trailing = after && !/\s$/.test(clean) && !/^\s/.test(after) ? ' ' : '';
  const insertion = `${leading}${clean}${trailing}`;
  return {
    value: `${before}${insertion}${after}`,
    caret: before.length + leading.length + clean.length,
  };
}

function voiceTime(milliseconds = 0): string {
  const seconds = Math.max(0, Math.floor(milliseconds / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}

export function Composer() {
  const store = useStore();
  const runActive = store.runActive.value;
  const canStop = store.canStop.value;
  const canInterject = store.canInterject.value;
  const file = useRef<HTMLInputElement>(null);
  const camera = useRef<HTMLInputElement>(null);
  const attach = useRef<HTMLButtonElement>(null);
  const textarea = useRef<HTMLTextAreaElement>(null);
  const [menu, setMenu] = useState(false);
  const voice = useMemo(
    () => new VoiceOperation((form, controls) => store.endpoints.transcribe(form, controls)),
    [store],
  );
  const [voiceState, setVoiceState] = useState<VoiceSnapshot>(voice.snapshot);
  const appliedVoiceGeneration = useRef(0);
  const [completionIndex, setCompletionIndex] = useState(0);
  const [cursor, setCursor] = useState(store.prompt.value.length);
  const [dismissed, setDismissed] = useState('');
  const [dragging, setDragging] = useState(false);
  const [dragError, setDragError] = useState('');
  const [projectMentions, setProjectMentions] = useState<MentionSearchResponse | null>(null);
  const addMenu = useMenuKeyboard(menu, () => setMenu(false), attach);
  useEffect(() => voice.subscribe(setVoiceState), [voice]);
  useEffect(() => () => voice.dispose(), [voice]);
  useLayoutEffect(() => resizePrompt(textarea.current), [store.prompt.value]);

  const session = store.draftActive.value ? null : store.activeSession.value;
  const agentName =
    session?.agent ||
    (store.draftActive.value ? store.selectedAgent.value : store.config.agentName);
  const messagePlaceholder = agentName ? `Message ${displayName(agentName)}…` : 'Message…';
  const projectId =
    session?.projectId || (store.draftActive.value ? store.activeProjectId.value : '') || '';
  const worktreeDir = store.currentWorktreeDir.value;
  const composerOwner = store.composerOwnerKey();
  useEffect(() => {
    if (
      voice.snapshot.owner &&
      voice.snapshot.owner !== composerOwner &&
      !['idle', 'cancelled'].includes(voice.snapshot.phase)
    )
      voice.cancel();
  }, [voice, composerOwner]);
  useEffect(() => {
    if (voiceState.phase === 'complete' && voiceState.transcript) {
      if (
        voiceState.owner !== store.composerOwnerKey() ||
        appliedVoiceGeneration.current === voiceState.generation
      )
        return;
      appliedVoiceGeneration.current = voiceState.generation;
      const inserted = insertTranscriptAtCaret(store.prompt.peek(), voiceState.transcript, cursor);
      store.prompt.value = inserted.value;
      setCursor(inserted.caret);
      requestAnimationFrame(() => {
        textarea.current?.focus();
        textarea.current?.setSelectionRange(inserted.caret, inserted.caret);
      });
      const timer = window.setTimeout(() => voice.settle(), 1_200);
      return () => clearTimeout(timer);
    }
    if (voiceState.phase === 'cancelled') {
      const timer = window.setTimeout(() => voice.settle(), 800);
      return () => clearTimeout(timer);
    }
  }, [voice, voiceState, store, cursor]);
  const mention = activeMentionAtCursor(store.prompt.value, cursor);

  const mentionActive = Boolean(mention);
  useEffect(() => {
    setProjectMentions(null);
    if (!mentionActive) return;
    const controller = new AbortController();
    const source = store.prompt.value;
    const sourceCursor = cursor;
    const timer = window.setTimeout(() => {
      void store.endpoints
        .mentionSearch(
          {
            text: source,
            cursor_utf16: sourceCursor,
            limit: 10,
            project_id: projectId,
            no_project: Boolean(store.projectsEnabled.peek() && !projectId),
            worktree_dir: worktreeDir,
          },
          session?.id || '',
          controller.signal,
        )
        .then((payload) => {
          if (!controller.signal.aborted && store.prompt.peek() === source)
            setProjectMentions(payload);
        })
        .catch((error: unknown) => {
          if ((error as { name?: string })?.name !== 'AbortError') setProjectMentions(null);
        });
    }, 50);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [store, store.prompt.value, cursor, projectId, worktreeDir, session?.id, mentionActive]);

  const local = composerCompletions(
    store.prompt.value.slice(0, cursor),
    store.config.agentNames,
    store.skills.value,
    runActive,
  );
  const combined = mention ? [...local, ...mentionCompletions(projectMentions)] : local;
  const completions =
    dismissed === `${store.prompt.value}\u0000${cursor}`
      ? []
      : [...new Map(combined.map((entry) => [`${entry.kind}:${entry.value}`, entry])).values()];
  const project = store.projects.value.find((entry) => entry.id === store.activeProjectId.value);
  const bindingBlocked = store.draftActive.value && Boolean(project && project.available === false);
  useEffect(() => {
    if (completionIndex >= completions.length) setCompletionIndex(0);
  }, [completionIndex, completions.length]);

  const choose = (completion: Completion) => {
    const nextCursor = completedCursor(store.prompt.value, completion);
    store.prompt.value = applyCompletion(store.prompt.value, completion);
    setCompletionIndex(0);
    setDismissed('');
    setCursor(nextCursor);
    requestAnimationFrame(() => {
      textarea.current?.focus();
      textarea.current?.setSelectionRange(nextCursor, nextCursor);
    });
  };
  const liveSkill = (value: string) => {
    const match = value.match(/^\/([a-z0-9](?:[a-z0-9-]*[a-z0-9])?)(?:\s+([\s\S]*))?$/);
    if (!match) return null;
    const skill = store.skills.value.find(
      (entry) => String(entry.name || '') === match[1] && !entry.collides_with_builtin,
    );
    return skill ? { skill, name: match[1], args: match[2] || '' } : null;
  };
  const sendOrCommand = () => {
    if (voiceBusy) return;
    const value = store.prompt.value.trim();
    const command = value.toLowerCase();
    if (/^\/side(?:\s|$)/i.test(value)) {
      const question = value.replace(/^\/side\b/i, '').trim();
      if (store.openSideQuestion(question)) store.prompt.value = '';
      return;
    }
    if (/^\/(?:fork|thread)(?:\s|$)/i.test(value)) {
      const match = value.match(/^\/(fork|thread)(?:\s+([\s\S]*))?$/i)!;
      void store.branchCommand(match[1].toLowerCase() as 'fork' | 'thread', match[2] || '');
      return;
    }
    if (command === '/new') return store.newChat();
    if (command === '/goal') {
      store.modal.value = 'goal';
      return;
    }
    if (command === '/mcp') {
      store.modal.value = 'mcp';
      void store.loadMCP();
      return;
    }
    if (command === '/skills') {
      store.modal.value = 'skills';
      void store.loadSkills();
      return;
    }
    if (command === '/paths' || command === '/tree') {
      store.prompt.value = '';
      void store.loadBranchTree();
      return;
    }
    if (command === '/worktree') {
      store.modal.value = 'worktrees';
      void store.loadWorktrees();
      return;
    }
    if (command === '/model' || command === '/effort') {
      store.modal.value = 'settings';
      return;
    }
    if (command === '/undo' || command === '/redo') {
      store.prompt.value = '';
      void store.mutateTranscript(command.slice(1) as 'undo' | 'redo');
      return;
    }
    if (command === '/compact') {
      const active = store.activeSession.value;
      store.prompt.value = '';
      if (active) void store.endpoints.compact(active.id).then(() => store.loadSession(active.id));
      else store.toast('Start the conversation before compacting.', 'error');
      return;
    }
    const skill = liveSkill(value);
    if (skill) {
      if (runActive && skill.skill.execution !== 'isolated') {
        store.toast('This main-conversation skill cannot run while a response is active.', 'error');
        return;
      }
      store.prompt.value = '';
      void store.invokeSkill(skill.name, skill.args);
      return;
    }
    requestTranscriptScrollToTail();
    if (canInterject) void store.interject(value);
    else void store.send();
  };
  const startVoice = () => {
    void voice.start(composerOwner, cursor);
  };
  const pending = store.interjections.value.filter(
    (entry) => entry.sessionId === store.activeSession.value?.id,
  );
  const waitingInteraction = store.interactionOrder.value
    .map((key) => store.interactions.value[key])
    .find(
      (entry) =>
        entry?.sessionId === store.activeSession.value?.id &&
        ['waiting', 'dismissed', 'failed'].includes(entry.state),
    );
  const voiceBusy = ['requesting-permission', 'recording', 'preparing', 'transcribing'].includes(
    voiceState.phase,
  );
  const hasDraft = Boolean(store.prompt.value.trim()) || store.attachments.value.length > 0;
  const sendPending = store.sendPending.value;
  const sendBlocked = store.sendBlocked.value;
  const attachmentBlocked = store.attachments.value.some(
    (attachment) => attachment.status === 'preparing' || attachment.status === 'error',
  );
  const interjecting = canInterject && hasDraft;
  const loading = runActive && !hasDraft;
  const sendLabel = bindingBlocked
    ? 'Project unavailable'
    : sendPending
      ? 'Sending message'
      : loading
        ? 'Response is running'
        : sendBlocked
          ? 'Checking whether sent'
          : interjecting
            ? 'Interject'
            : 'Send message';
  const inspectDraggedFiles = (files: FileList | null): string => {
    let count = store.attachments.peek().length;
    for (const candidate of Array.from(files || [])) {
      const validation = validateAttachmentFile(candidate, count, store.attachmentPolicy.peek());
      if (validation) {
        setDragError(validation.message);
        return validation.message;
      }
      count += 1;
    }
    setDragError('');
    return '';
  };
  const openAttachmentPreview = (attachmentId: string, previewURL: string) => {
    const images = store.attachments
      .peek()
      .filter(
        (attachment) =>
          attachment.id &&
          attachment.previewURL &&
          attachment.type.startsWith('image/') &&
          attachment.status !== 'preparing' &&
          attachment.status !== 'error',
      )
      .map((attachment) => ({
        key: attachment.id!,
        src: attachment.previewURL!,
        type: 'image' as const,
        name: attachment.name,
      }));
    const index = images.findIndex((item) => item.key === attachmentId || item.src === previewURL);
    if (index < 0) return;
    store.lightbox.value = {
      ...images[index],
      items: images,
      index,
      onRemove: (item) => store.removeAttachment(item.key),
      fallbackFocus: () => textarea.current,
    };
  };
  return (
    <footer
      class="composer"
      onDragEnter={(event) => {
        event.preventDefault();
        setDragging(true);
        inspectDraggedFiles(event.dataTransfer?.files || null);
      }}
      onDragOver={(event) => {
        event.preventDefault();
        inspectDraggedFiles(event.dataTransfer?.files || null);
        if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy';
      }}
      onDragLeave={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget as Node | null)) {
          setDragging(false);
          setDragError('');
        }
      }}
      onDrop={(event) => {
        event.preventDefault();
        setDragging(false);
        inspectDraggedFiles(event.dataTransfer?.files || null);
        if (event.dataTransfer?.files.length) store.addAttachments(event.dataTransfer.files);
        setDragError('');
      }}
    >
      {dragging && (
        <div
          class={`drop-overlay ${dragError ? 'rejected' : 'accepted'}`}
          role="status"
          aria-live="polite"
        >
          {dragError || 'Drop supported files to attach'}
        </div>
      )}
      <div class="composer-inner">
        {waitingInteraction && (
          <button
            type="button"
            class="interaction-attention-banner"
            onClick={() => store.openInteraction(waitingInteraction.key)}
          >
            Decision waiting — Open
          </button>
        )}
        {pending.length > 0 && (
          <div
            class="pending-interjection pending-interjection-banner"
            role="list"
            aria-label="Pending messages"
          >
            {pending.map((entry) => (
              <div class={`pending-interjection-row ${entry.state}`} role="listitem" key={entry.id}>
                <span class="pending-interjection-icon" aria-hidden="true">
                  …
                </span>
                <span class="pending-interjection-text">{entry.content}</span>
                <span class="pending-interjection-label">
                  ({entry.state === 'sending' ? 'deciding…' : entry.state})
                </span>
                {entry.state !== 'committed' && (
                  <button
                    class="pending-interjection-cancel"
                    onClick={() => void store.cancelInterjection(entry.id)}
                  >
                    Cancel
                  </button>
                )}
              </div>
            ))}
          </div>
        )}
        {voiceState.phase !== 'idle' && (
          <div
            id="voiceStatus"
            class={`voice-status voice-status-${voiceState.phase}`}
            aria-live="polite"
            role={voiceState.phase === 'failed' ? 'alert' : 'status'}
          >
            <span
              class={voiceState.phase === 'recording' ? 'voice-status-dot' : 'voice-status-spinner'}
              aria-hidden="true"
            />
            <span class="voice-status-copy">
              {voiceState.phase === 'requesting-permission' && 'Requesting microphone access…'}
              {voiceState.phase === 'recording' && `Recording ${voiceTime(voiceState.durationMs)}`}
              {voiceState.phase === 'preparing' && 'Preparing recording…'}
              {voiceState.phase === 'transcribing' &&
                (voiceState.stage === 'uploading'
                  ? `Uploading${voiceState.total ? ` ${Math.min(100, Math.round(((voiceState.loaded || 0) / voiceState.total) * 100))}%` : '…'}`
                  : voiceState.stage === 'stalled'
                    ? 'Upload stalled'
                    : `Transcribing… ${voiceTime(voiceState.elapsedMs)}`)}
              {voiceState.phase === 'complete' && 'Transcription inserted.'}
              {voiceState.phase === 'cancelled' && 'Voice recording cancelled.'}
              {voiceState.phase === 'failed' && (voiceState.error || 'Voice transcription failed.')}
            </span>
            {voiceState.phase === 'recording' && (
              <button type="button" class="btn voice-status-action" onClick={() => voice.stop()}>
                Stop
              </button>
            )}
            {voiceBusy && (
              <button type="button" class="btn voice-status-cancel" onClick={() => voice.cancel()}>
                Cancel
              </button>
            )}
            {voiceState.phase === 'failed' && voiceState.retryable && (
              <button type="button" class="btn voice-status-action" onClick={() => voice.retry()}>
                Retry
              </button>
            )}
            {voiceState.phase === 'failed' && (
              <button type="button" class="btn voice-status-cancel" onClick={() => voice.discard()}>
                Discard
              </button>
            )}
          </div>
        )}
        {!voiceState.capability.supported && (
          <span class="voice-unsupported" id="voiceUnsupported">
            {voiceState.capability.reason}
          </span>
        )}
        {store.attachments.value.length > 0 && (
          <div id="attachmentsStrip" class="attachments">
            {store.attachments.value.map((attachment) => (
              <div class="attachment-chip" key={attachment.id}>
                {attachment.id &&
                  attachment.previewURL &&
                  attachment.type.startsWith('image/') &&
                  attachment.status !== 'preparing' &&
                  attachment.status !== 'error' && (
                    <button
                      class="att-preview"
                      type="button"
                      aria-label={`Preview ${attachment.name}`}
                      onClick={() => openAttachmentPreview(attachment.id!, attachment.previewURL!)}
                    >
                      <img src={attachment.previewURL} alt="" />
                    </button>
                  )}
                <span class="att-name">{attachment.name}</span>
                {attachment.status === 'preparing' && (
                  <span class="att-status" role="status">
                    Preparing {Math.round((attachment.progress || 0) * 100)}%
                  </span>
                )}
                {attachment.status === 'error' && (
                  <span class="att-error" role="alert">
                    {attachment.error || 'Preparation failed'}
                    {attachment.file && (
                      <button
                        type="button"
                        aria-label={`Retry preparing ${attachment.name}`}
                        onClick={() => store.retryAttachment(attachment.id)}
                      >
                        Retry
                      </button>
                    )}
                  </span>
                )}
                <button
                  class="att-remove close-button"
                  type="button"
                  aria-label={`Remove ${attachment.name}`}
                  onClick={() => store.removeAttachment(attachment.id)}
                >
                  <Icon name="close" />
                </button>
              </div>
            ))}
          </div>
        )}
        {store.goal.value && (
          <button
            type="button"
            id="goalChip"
            class={`goal-chip goal-${store.goal.value.status || 'active'}`}
            onClick={() => {
              store.modal.value = 'goal';
            }}
          >
            <span class="goal-chip-label">Goal</span> · {store.goal.value.status || 'active'}
            {store.goal.value.token_budget
              ? ` · ${store.goal.value.tokens_used || 0}/${store.goal.value.token_budget} tok`
              : ''}{' '}
            · {store.goal.value.objective}
          </button>
        )}
        {bindingBlocked && (
          <div class="composer-binding-error" role="alert">
            This project is unavailable. Restore it or choose No project before sending.
          </div>
        )}
        <div class="composer-box">
          <button
            ref={attach}
            class="composer-icon-btn attach-btn"
            id="attachBtn"
            type="button"
            aria-label="Add"
            aria-haspopup="menu"
            aria-controls="addMenu"
            aria-expanded={menu}
            onClick={() => setMenu(!menu)}
          >
            <Icon name="add" />
          </button>
          {menu && (
            <div ref={addMenu} class="composer-add-menu" id="addMenu" role="menu">
              <button
                type="button"
                role="menuitem"
                onClick={() => {
                  file.current?.click();
                  setMenu(false);
                }}
              >
                <span class="composer-add-menu-icon">
                  <Icon name="add" />
                </span>
                <span>Upload file</span>
              </button>
              <button
                type="button"
                role="menuitem"
                onClick={() => {
                  camera.current?.click();
                  setMenu(false);
                }}
              >
                <span class="composer-add-menu-icon">◉</span>
                <span>Take photo</span>
              </button>
              {store.config.locationSharing && (
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    void store.shareLocation();
                    setMenu(false);
                  }}
                >
                  <span class="composer-add-menu-icon">◎</span>
                  <span>Share current location</span>
                </button>
              )}
              <button
                type="button"
                role="menuitem"
                onClick={() => {
                  store.modal.value = 'mcp';
                  void store.loadMCP();
                  setMenu(false);
                }}
              >
                <span class="composer-add-menu-icon">
                  <Icon name="widgets" />
                </span>
                <span>MCP servers</span>
              </button>
              <button
                type="button"
                role="menuitem"
                onClick={() => {
                  store.modal.value = 'goal';
                  setMenu(false);
                }}
              >
                <span class="composer-add-menu-icon">○</span>
                <span>Set goal…</span>
              </button>
              {store.worktreesAvailable() && (
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    store.modal.value = 'worktrees';
                    void store.loadWorktrees();
                    setMenu(false);
                  }}
                >
                  <span class="composer-add-menu-icon">
                    <Icon name="branch" />
                  </span>
                  <span>Worktrees</span>
                </button>
              )}
            </div>
          )}
          <input
            ref={file}
            type="file"
            id="fileInput"
            multiple
            accept={store.attachmentAccept.value}
            hidden
            onChange={(event) => {
              if (event.currentTarget.files) store.addAttachments(event.currentTarget.files);
              event.currentTarget.value = '';
            }}
          />
          <input
            ref={camera}
            type="file"
            accept="image/*"
            capture="environment"
            aria-label="Take a photo"
            hidden
            onChange={(event) => {
              if (event.currentTarget.files) store.addAttachments(event.currentTarget.files);
              event.currentTarget.value = '';
            }}
          />
          {completions.length > 0 && (
            <div
              class="slash-command-menu"
              id="slashCommandMenu"
              role="listbox"
              aria-label="Composer completions"
            >
              {completions.map((entry, index) => (
                <button
                  id={`composer-completion-${index}`}
                  type="button"
                  role="option"
                  aria-selected={completionIndex === index}
                  class={`slash-command-option ${completionIndex === index ? 'selected' : ''}`}
                  key={`${entry.kind}:${entry.value}`}
                  onMouseDown={(event) => {
                    event.preventDefault();
                  }}
                  onClick={() => choose(entry)}
                >
                  <strong class="slash-command-name">
                    {entry.segments?.length
                      ? entry.segments.map((segment, part) => (
                          <span
                            class={segment.matched ? 'mention-completion-match' : ''}
                            key={part}
                          >
                            {segment.text}
                          </span>
                        ))
                      : entry.label}
                  </strong>
                  <span class="slash-command-description">{entry.description}</span>
                </button>
              ))}
            </div>
          )}
          <textarea
            ref={textarea}
            id="promptInput"
            class="prompt"
            rows={1}
            placeholder={runActive ? 'Type to interject…' : messagePlaceholder}
            aria-label="Message"
            autoComplete="off"
            spellcheck={false}
            aria-autocomplete="list"
            aria-controls="slashCommandMenu"
            aria-activedescendant={
              completions.length ? `composer-completion-${completionIndex}` : undefined
            }
            aria-expanded={completions.length > 0}
            value={store.prompt.value}
            onPaste={(event) => {
              const files = Array.from(event.clipboardData?.items || [])
                .filter((item) => item.kind === 'file')
                .map((item) => item.getAsFile())
                .filter((item): item is File => Boolean(item));
              if (files.length) store.addAttachments(files);
            }}
            onClick={(event) => {
              setCursor(event.currentTarget.selectionStart ?? event.currentTarget.value.length);
              setDismissed('');
            }}
            onInput={(event) => {
              store.prompt.value = event.currentTarget.value;
              const nextCursor =
                event.currentTarget.selectionStart ?? event.currentTarget.value.length;
              setCursor(nextCursor);
              setDismissed('');
              resizePrompt(event.currentTarget);
              setCompletionIndex(0);
            }}
            onKeyUp={(event) => {
              if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) {
                setCursor(event.currentTarget.selectionStart ?? event.currentTarget.value.length);
                setDismissed('');
              }
            }}
            onKeyDown={(event) => {
              if (completions.length && ['ArrowDown', 'ArrowUp'].includes(event.key)) {
                event.preventDefault();
                setCompletionIndex(
                  (index) =>
                    (index + (event.key === 'ArrowDown' ? 1 : -1) + completions.length) %
                    completions.length,
                );
                return;
              }
              const selected = completions[completionIndex];
              const exactSlash =
                selected?.kind === 'slash' &&
                store.prompt.value.trim().toLowerCase() === selected.value.toLowerCase();
              if (
                selected &&
                (event.key === 'Tab' ||
                  (event.key === 'Enter' &&
                    !event.shiftKey &&
                    !exactSlash &&
                    !(selected.kind !== 'slash' && mention?.query === '')))
              ) {
                event.preventDefault();
                choose(selected);
                return;
              }
              if (event.key === 'Escape' && completions.length) {
                event.preventDefault();
                setDismissed(`${store.prompt.value}\u0000${cursor}`);
                return;
              }
              if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
                event.preventDefault();
                sendOrCommand();
              }
            }}
          />
          <div class="composer-actions">
            {canStop && (
              <button
                class="stop-btn visible"
                id="stopBtn"
                type="button"
                onClick={() => void store.cancel()}
              >
                Stop
              </button>
            )}
            <button
              class={`composer-icon-btn voice-btn ${voiceState.phase === 'recording' ? 'recording' : ''} ${voiceBusy ? 'busy' : ''}`}
              id="voiceBtn"
              type="button"
              aria-label="Record voice message"
              aria-describedby={!voiceState.capability.supported ? 'voiceUnsupported' : undefined}
              disabled={
                !voiceState.capability.supported || voiceBusy || voiceState.phase === 'failed'
              }
              onClick={startVoice}
            >
              <Icon name="microphone" />
            </button>
            <button
              class={`send-btn ${loading ? 'loading' : ''} ${interjecting ? 'interject' : ''}`}
              id="sendBtn"
              type="button"
              title={sendLabel}
              aria-label={sendLabel}
              disabled={
                (sendBlocked && !loading) ||
                voiceBusy ||
                bindingBlocked ||
                attachmentBlocked ||
                (!hasDraft && !loading)
              }
              onClick={sendOrCommand}
            >
              <Icon class="arrow" name={interjecting ? 'interject' : 'send'} />
              <span class="spinner" aria-hidden="true" />
            </button>
          </div>
        </div>
      </div>
    </footer>
  );
}
