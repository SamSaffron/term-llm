import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
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
import { VoiceRecorder } from '../platform/voice';
import { Icon } from './Icon';

export function Composer() {
  const store = useStore();
  const file = useRef<HTMLInputElement>(null);
  const textarea = useRef<HTMLTextAreaElement>(null);
  const [menu, setMenu] = useState(false);
  const [voiceStatus, setVoiceStatus] = useState('');
  const [recording, setRecording] = useState(false);
  const [completionIndex, setCompletionIndex] = useState(0);
  const [cursor, setCursor] = useState(store.prompt.value.length);
  const [dismissed, setDismissed] = useState('');
  const [projectMentions, setProjectMentions] = useState<MentionSearchResponse | null>(null);
  const voice = useMemo(() => new VoiceRecorder(), []);
  useEffect(() => () => voice.cancel(), [voice]);

  const session = store.draftActive.value ? null : store.activeSession.value;
  const projectId =
    session?.projectId || (store.draftActive.value ? store.activeProjectId.value : '') || '';
  const worktreeDir =
    session?.worktreeDir ||
    (store.draftActive.value ? store.selectedDraftWorktree.value : '') ||
    '';
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
    store.streaming.value,
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
    const value = store.prompt.value.trim();
    const command = value.toLowerCase();
    if (/^\/side(?:\s|$)/i.test(value)) {
      const question = value.replace(/^\/side\b/i, '').trim();
      store.sideQuestion.value = { ...store.sideQuestion.value, visible: true };
      store.prompt.value = '';
      if (question) void store.askSideQuestion(question);
      else void store.recoverSideQuestion();
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
    if (skill && (!store.streaming.value || skill.skill.execution === 'isolated')) {
      store.prompt.value = '';
      void store.invokeSkill(skill.name, skill.args);
      return;
    }
    if (store.streaming.value)
      void store.interject(value).then(() => {
        if (store.prompt.peek().trim() === value) store.prompt.value = '';
      });
    else void store.send();
  };
  const toggleVoice = async () => {
    try {
      if (!recording) {
        await voice.start();
        setRecording(true);
        setVoiceStatus('Recording… tap again to transcribe');
      } else {
        setRecording(false);
        setVoiceStatus('Transcribing…');
        const text = await voice.stop(store.endpoints);
        store.prompt.value = `${store.prompt.value}${store.prompt.value ? ' ' : ''}${text}`;
        setCursor(store.prompt.value.length);
        setVoiceStatus('');
      }
    } catch (error) {
      setRecording(false);
      setVoiceStatus(error instanceof Error ? error.message : String(error));
    }
  };
  const pending = store.interjections.value.filter(
    (entry) => entry.sessionId === store.activeSession.value?.id,
  );
  const hasDraft = Boolean(store.prompt.value.trim()) || store.attachments.value.length > 0;
  const interjecting = store.streaming.value && hasDraft;
  const sendLabel = bindingBlocked
    ? 'Project unavailable'
    : interjecting
      ? 'Interject'
      : 'Send message';
  return (
    <footer
      class="composer"
      onDragOver={(event) => event.preventDefault()}
      onDrop={(event) => {
        event.preventDefault();
        if (event.dataTransfer?.files.length) store.addAttachments(event.dataTransfer.files);
      }}
    >
      <div class="composer-inner">
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
        {voiceStatus && (
          <div id="voiceStatus" class="voice-status" aria-live="polite">
            {voiceStatus}
          </div>
        )}
        {store.attachments.value.length > 0 && (
          <div id="attachmentsStrip" class="attachments">
            {store.attachments.value.map((attachment) => (
              <div class="attachment-chip" key={attachment.id}>
                {attachment.previewURL && attachment.type.startsWith('image/') && (
                  <img src={attachment.previewURL} alt="" />
                )}
                <span class="att-name">{attachment.name}</span>
                <button
                  class="att-remove"
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
            class="composer-icon-btn attach-btn"
            id="attachBtn"
            type="button"
            aria-label="Add"
            aria-expanded={menu}
            onClick={() => setMenu(!menu)}
          >
            <Icon name="add" />
          </button>
          {menu && (
            <div class="composer-add-menu" id="addMenu" role="menu">
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
              {store.worktreesEnabled.value && (
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
                    choose(entry);
                  }}
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
            placeholder={store.streaming.value ? 'Type to interject…' : 'Message…'}
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
              event.currentTarget.style.height = 'auto';
              event.currentTarget.style.height = `${Math.min(event.currentTarget.scrollHeight, 200)}px`;
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
            {store.streaming.value && (
              <button
                class="stop-btn visible"
                id="stopBtn"
                type="button"
                onClick={() => void store.cancel()}
              >
                Stop
              </button>
            )}
            {'MediaRecorder' in window && (
              <button
                class={`composer-icon-btn voice-btn ${recording ? 'recording' : ''}`}
                id="voiceBtn"
                type="button"
                aria-label={recording ? 'Stop recording' : 'Record voice message'}
                onClick={() => void toggleVoice()}
              >
                <Icon name="microphone" />
              </button>
            )}
            <button
              class={`send-btn ${interjecting ? 'interject' : ''}`}
              id="sendBtn"
              type="button"
              title={sendLabel}
              aria-label={sendLabel}
              disabled={bindingBlocked || !hasDraft}
              onClick={sendOrCommand}
            >
              <Icon class="arrow" name={interjecting ? 'interject' : 'send'} />
            </button>
          </div>
        </div>
      </div>
    </footer>
  );
}
