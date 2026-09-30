import type { ComponentType } from 'preact';
import { memo } from './memo';
import { lazyComponent } from './lazyComponent';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import type { ApprovalPrompt, AskUserPrompt } from '../domain/types';
import { Icon } from './Icon';
import { Overlay } from './Overlay';
import { Markdown } from './Markdown';

// Modals opened from explicit user actions load on demand. Agent interactions
// (approval, ask-user) stay eager: they must appear the moment the server asks,
// without a chunk round trip. The side question stays here because other
// surfaces import it directly, so splitting it would not shrink the entry.
//
// A failed import resolves to a dismissible error dialog titled like the real
// one; `onClose` must repeat any cleanup the real dialog does on dismissal.
function lazyModal(
  title: string,
  load: () => Promise<ComponentType>,
  onClose?: (store: ReturnType<typeof useStore>) => void,
) {
  return lazyComponent(() =>
    load().catch((error: unknown) => {
      console.error(`Failed to load the ${title} dialog`, error);
      const ModalLoadError = () => {
        const store = useStore();
        return (
          <Overlay title={title} onClose={onClose && (() => onClose(store))}>
            <p role="alert">Could not load this dialog. Reload the page to retry.</p>
          </Overlay>
        );
      };
      return ModalLoadError;
    }),
  );
}

const LazySettings = lazyModal('Settings', () =>
  import('./SettingsModal').then(({ Settings }) => Settings),
);
// Rename's own close clears renameTarget; a stale target would keep the status
// reconciler from applying server titles to that session.
const LazyRename = lazyModal(
  'Rename session',
  () => import('./RenameModal').then(({ Rename }) => Rename),
  (store) => {
    store.renameTarget.value = null;
    store.modal.value = '';
  },
);
const LazyGoalModal = lazyModal('Session goal', () =>
  import('./GoalModal').then(({ GoalModal }) => GoalModal),
);
const LazyWidgets = lazyModal('Widgets', () =>
  import('./WidgetsModal').then(({ Widgets }) => Widgets),
);
const LazyBranchContext = lazyModal('Start a conversation path', () =>
  import('./BranchModals').then(({ BranchContext }) => BranchContext),
);
const LazyBranchTree = lazyModal('Conversation paths', () =>
  import('./BranchModals').then(({ BranchTree }) => BranchTree),
);
const LazySkills = lazyModal('Skills', () => import('./SkillsModal').then(({ Skills }) => Skills));
const LazyProjectPicker = lazyModal('Add project', () =>
  import('./ProjectPicker').then(({ ProjectPicker }) => ProjectPicker),
);
const LazyProjectAssignment = lazyModal('Assign project', () =>
  import('./ProjectAssignment').then(({ ProjectAssignment }) => ProjectAssignment),
);
const LazyWorktrees = lazyModal('Worktrees', () =>
  import('./Worktrees').then(({ Worktrees }) => Worktrees),
);

/** Resolves once every on-demand modal above can render synchronously. */
export function preloadDeferredModals(): Promise<unknown> {
  return Promise.all(
    [
      LazySettings,
      LazyRename,
      LazyGoalModal,
      LazyWidgets,
      LazyBranchContext,
      LazyBranchTree,
      LazySkills,
      LazyProjectPicker,
      LazyProjectAssignment,
      LazyWorktrees,
    ].map((component) => component.preload()),
  );
}

const LazyCommitModal = lazyComponent(() =>
  import('./CommitModal').then(({ CommitModal }) => CommitModal).catch(() => CommitModalLoadError),
);

const LazyStatsModal = lazyComponent(() =>
  import('./StatsModal').then(({ StatsModal }) => StatsModal),
);

const LazyMCP = lazyComponent(() => import('./mcp/MCPModal').then(({ MCP }) => MCP));
const LazyShareModal = lazyComponent(() =>
  import('./ShareModal').then(({ ShareModal }) => ShareModal),
);
const LazyApprovalsModal = lazyComponent(() =>
  import('./ApprovalsModal').then(({ ApprovalsModal }) => ApprovalsModal),
);

function CommitModalLoadError() {
  const store = useStore();
  return (
    <Overlay title="Git commit" className="commit-modal" onClose={() => store.commitStore.close()}>
      <p role="alert">Could not load commit controls. Reload the page to retry.</p>
    </Overlay>
  );
}

function AskUser({ interactionPrompt }: { interactionPrompt?: AskUserPrompt }) {
  const store = useStore();
  const prompt = interactionPrompt || store.askUser.value;
  const [answers, setAnswers] = useState<Record<number, string[]>>({});
  const [custom, setCustom] = useState<Record<number, string>>({});
  const [tab, setTab] = useState(0);
  const [error, setError] = useState('');
  const [sending, setSending] = useState(false);
  if (!prompt) return null;
  const question = prompt.questions[tab];
  const validate = (item: (typeof prompt.questions)[number], index: number) => {
    const selected = answers[index] || [];
    const own = custom[index]?.trim();
    if (item.multi_select && !selected.length)
      throw new Error(`${item.header || `Question ${index + 1}`}: choose at least one option.`);
    if (!item.multi_select && !selected.length && !own)
      throw new Error(`${item.header || `Question ${index + 1}`}: choose or enter an answer.`);
    return {
      question_index: index,
      header: item.header,
      selected: own || selected.join(', '),
      selected_list: item.multi_select ? selected : undefined,
      is_custom: Boolean(own),
      is_multi_select: Boolean(item.multi_select),
    };
  };
  const submit = async (cancelled = false) => {
    setError('');
    setSending(true);
    try {
      await store.answerAskUser(cancelled ? [] : prompt.questions.map(validate), cancelled, prompt);
    } catch (value) {
      setError(errorMessage(value));
    } finally {
      setSending(false);
    }
  };
  const dismiss = () => {
    if (tab > 0) setTab(tab - 1);
    else store.dismissInteraction('ask-user', prompt);
  };
  const next = () => {
    try {
      validate(question, tab);
      setError('');
      setTab(tab + 1);
    } catch (value) {
      setError(errorMessage(value));
    }
  };
  return (
    <Overlay
      title={
        prompt.questions.length > 1
          ? `Question ${tab + 1} of ${prompt.questions.length}`
          : 'Answer question'
      }
      close={false}
      onEscape={() => {
        if (!sending) dismiss();
      }}
    >
      <p>The agent needs your input to continue.</p>
      {prompt.questions.length > 1 && (
        <div class="ask-user-steps">
          {prompt.questions.map((_item, index) => (
            <button
              class={`ask-user-step ${index === tab ? 'active' : index < tab ? 'completed' : ''}`}
              disabled={sending}
              onClick={() => setTab(index)}
            >
              {index + 1}
            </button>
          ))}
        </div>
      )}
      <fieldset class="ask-user-question" disabled={sending} aria-busy={sending}>
        <legend class="ask-user-question-text">
          {question.header && <strong>{question.header}: </strong>}
          {question.question}
        </legend>
        {question.options?.map((option) => (
          <label class="modal-choice ask-user-option" key={option.label}>
            <input
              type={question.multi_select ? 'checkbox' : 'radio'}
              name={`question-${tab}`}
              value={option.label}
              checked={(answers[tab] || []).includes(option.label)}
              onChange={(event) => {
                const current = answers[tab] || [];
                setAnswers({
                  ...answers,
                  [tab]: question.multi_select
                    ? event.currentTarget.checked
                      ? [...current, option.label]
                      : current.filter((value) => value !== option.label)
                    : [option.label],
                });
                setCustom({ ...custom, [tab]: '' });
              }}
            />
            <span>
              <strong>{option.label}</strong>
              {option.description && <small>{option.description}</small>}
            </span>
          </label>
        ))}
        {!question.multi_select && (
          <label class="ask-user-custom">
            <span>Other</span>
            <textarea
              placeholder="Type your answer…"
              value={custom[tab] || ''}
              onInput={(event) => {
                setCustom({ ...custom, [tab]: event.currentTarget.value });
                setAnswers({ ...answers, [tab]: [] });
              }}
            />
          </label>
        )}
      </fieldset>
      {error && <div class="modal-error">{error}</div>}
      <div class="modal-actions">
        <button class="btn" disabled={sending} onClick={dismiss}>
          {tab > 0 ? 'Back' : 'Dismiss'}
        </button>
        {tab === 0 && (
          <button class="btn danger" disabled={sending} onClick={() => void submit(true)}>
            {sending ? 'Cancelling…' : 'Cancel agent request'}
          </button>
        )}
        {tab < prompt.questions.length - 1 ? (
          <button class="btn primary" disabled={sending} onClick={next}>
            Next
          </button>
        ) : (
          <button class="btn primary" disabled={sending} onClick={() => void submit()}>
            {sending ? 'Sending…' : 'Continue'}
          </button>
        )}
      </div>
    </Overlay>
  );
}

function Approval({ interactionPrompt }: { interactionPrompt?: ApprovalPrompt }) {
  const store = useStore();
  const prompt = interactionPrompt || store.approval.value;
  const options = prompt?.options || [];
  const deny = options.find((option) => option.choice === 'deny')?.index;
  const allowed = options.filter((option) => option.choice !== 'deny');
  const pending = useRef(false);
  const [choice, setChoice] = useState(
    options.find((option) => option.choice !== 'deny')?.index ?? 0,
  );
  const [resume, setResume] = useState(false);
  const [error, setError] = useState('');
  const [sending, setSending] = useState(false);
  if (!prompt) return null;
  const decide = async (selected: number) => {
    if (pending.current) return;
    pending.current = true;
    setSending(true);
    setError('');
    try {
      await store.decideApproval(selected, resume, prompt, false);
    } catch (value) {
      setError(errorMessage(value));
    } finally {
      pending.current = false;
      setSending(false);
    }
  };
  return (
    <Overlay
      title={prompt.title || 'Access request'}
      className="approval-modal"
      dismissDisabled={sending}
      close={false}
    >
      <form
        class="approval-form"
        aria-busy={sending}
        onSubmit={(event) => {
          event.preventDefault();
          if (allowed.some((option) => option.index === choice)) void decide(choice);
        }}
        onKeyDown={(event) => {
          if (event.isComposing || event.repeat || event.altKey || event.metaKey || event.ctrlKey)
            return;
          const target = event.target as HTMLElement;
          if (target instanceof HTMLInputElement && target.type === 'checkbox') return;
          const position = allowed.findIndex((option) => option.index === choice);
          let next = /^[1-9]$/.test(event.key) ? Number(event.key) - 1 : -1;
          if (target instanceof HTMLInputElement && target.type === 'radio') {
            if (event.key === 'ArrowDown' || event.key === 'ArrowRight')
              next = (position + 1) % allowed.length;
            if (event.key === 'ArrowUp' || event.key === 'ArrowLeft')
              next = (position - 1 + allowed.length) % allowed.length;
            if (event.key === 'Enter') {
              event.preventDefault();
              if (!sending) void decide(choice);
              return;
            }
          }
          if (next < 0 || next >= allowed.length) return;
          event.preventDefault();
          if (sending) return;
          setChoice(allowed[next].index);
          event.currentTarget
            .querySelectorAll<HTMLInputElement>('input[type="radio"]')
            [next]?.focus();
        }}
      >
        <div class="approval-content">
          {prompt.intro && <div class="approval-intro">{prompt.intro}</div>}
          {prompt.scope === 'shared_shell' && (
            <div class="approval-intro">Shared interactive shell — target may be remote</div>
          )}
          {(prompt.path || prompt.body) && (
            <section class="approval-request" aria-label="Request details" tabIndex={0}>
              {prompt.path && <code class="approval-path">{prompt.path}</code>}
              {prompt.body && <pre class="approval-body">{prompt.body}</pre>}
            </section>
          )}
          <fieldset class="approval-options" disabled={sending}>
            <legend>Approval scope</legend>
            {allowed.map((option, index) => (
              <label class="modal-choice approval-option" key={option.index}>
                <input
                  type="radio"
                  name="approval"
                  autoFocus={choice === option.index}
                  checked={choice === option.index}
                  onChange={() => setChoice(option.index)}
                />
                <span>
                  <strong>{option.label || option.title || option.choice}</strong>
                  {option.description && <small>{option.description}</small>}
                  {prompt.scope === 'shared_shell' && option.choice !== 'once' && (
                    <small>
                      Remember only for this session’s shared shell; do not save as a project
                      approval.
                    </small>
                  )}
                </span>
                {index < 9 && <kbd aria-hidden="true">{index + 1}</kbd>}
              </label>
            ))}
          </fieldset>
          {prompt.resumeAutoAvailable && (
            <label>
              <input
                type="checkbox"
                checked={resume}
                disabled={sending}
                onChange={(event) => setResume(event.currentTarget.checked)}
              />{' '}
              Resume Guardian auto-approval
            </label>
          )}
          {prompt.note && <div class="approval-note">{prompt.note}</div>}
          {error && (
            <div class="modal-error" role="alert">
              {error}
            </div>
          )}
        </div>
        <div class="approval-footer">
          {allowed.length > 0 && (
            <div class="approval-shortcuts">
              {allowed.length === 1 ? '1' : `1–${Math.min(allowed.length, 9)}`} select · ↑ ↓
              navigate · Enter confirm
            </div>
          )}
          <div class="modal-actions">
            <button
              type="button"
              class="btn"
              disabled={sending || deny === undefined}
              onClick={() => deny !== undefined && void decide(deny)}
            >
              {sending ? 'Submitting…' : 'Deny'}
            </button>
            <button
              type="submit"
              class="btn primary"
              disabled={sending || !allowed.some((option) => option.index === choice)}
            >
              {sending ? 'Submitting…' : 'Approve'}
            </button>
          </div>
        </div>
      </form>
    </Overlay>
  );
}

const SideQuestionHistory = memo(function SideQuestionHistory({
  history,
}: {
  history: { question: string; response: string }[];
}) {
  return (
    <>
      {history.map((entry, index) => (
        <section class="side-question-exchange" key={`${index}-${entry.question}`}>
          <article class="message user">
            <div class="message-body">{entry.question}</div>
          </article>
          <article class="message assistant">
            <div class="message-body">
              <Markdown value={entry.response} className="markdown-body" />
            </div>
          </article>
        </section>
      ))}
    </>
  );
});

export function SideQuestion() {
  const store = useStore();
  const state = store.sideQuestion.value;
  const session = store.activeSession.value;
  const transcript = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const stickToBottom = useRef(true);
  const hasCurrent = Boolean(state.question);
  const hasConversation = state.history.length > 0 || hasCurrent;

  useLayoutEffect(() => {
    const element = transcript.current;
    if (element && stickToBottom.current) element.scrollTop = element.scrollHeight;
  }, [state.history.length, state.response, state.running]);
  useEffect(() => {
    if (!state.loading && !state.running) input.current?.focus({ preventScroll: true });
  }, [state.loading, state.running]);

  if (!session || state.sessionId !== session.id) return null;

  const submit = () => {
    const question = state.draft.trim();
    if (!question || state.running) return;
    void store.askSideQuestion(question);
  };
  const escape = () => {
    if (state.running) store.cancelSideQuestion();
    else if (state.draft) store.setSideQuestionDraft('');
    else store.closeSideQuestion();
  };
  const starters = [
    'Summarise what we decided',
    "What's still unresolved?",
    'Explain the last change',
  ];

  return (
    <Overlay
      title="Side question"
      wide
      className="side-question-modal"
      onClose={() => store.closeSideQuestion()}
      onEscape={escape}
    >
      <div
        class={`side-question-transcript ${hasConversation ? '' : 'empty'}`}
        ref={transcript}
        onScroll={(event) => {
          const element = event.currentTarget;
          stickToBottom.current =
            element.scrollHeight - element.scrollTop - element.clientHeight < 96;
        }}
      >
        {state.loading && !hasConversation && (
          <div class="side-question-loading" role="status">
            <span class="side-question-loading-dot" />
            Loading side questions…
          </div>
        )}
        {!state.loading && !hasConversation && (
          <div class="side-question-empty">
            <div class="side-question-empty-mark" aria-hidden="true">
              ↗
            </div>
            <h3>Ask about this conversation</h3>
            <p>Answers use the transcript as context but are never added to it.</p>
            <div class="side-question-starters" aria-label="Suggested side questions">
              {starters.map((starter) => (
                <button
                  type="button"
                  key={starter}
                  onClick={() => {
                    store.setSideQuestionDraft(starter);
                    requestAnimationFrame(() => input.current?.focus());
                  }}
                >
                  {starter}
                </button>
              ))}
            </div>
          </div>
        )}
        <SideQuestionHistory history={state.history} />
        {hasCurrent && (
          <section class="side-question-exchange side-question-current">
            <article class="message user">
              <div class="message-body">{state.question}</div>
            </article>
            {(state.response || state.running) && (
              <article class="message assistant" aria-busy={state.running}>
                <div class="message-body">
                  {state.response ? (
                    <Markdown
                      value={state.response}
                      streaming={state.running}
                      className="markdown-body"
                    />
                  ) : (
                    <span class="side-question-thinking">
                      Thinking<span aria-hidden="true">…</span>
                    </span>
                  )}
                </div>
              </article>
            )}
          </section>
        )}
      </div>

      {state.error && (
        <div class="side-question-error" role="alert">
          <span>{state.error}</span>
          {state.question && !state.running && (
            <button type="button" onClick={() => void store.askSideQuestion(state.question)}>
              Try again
            </button>
          )}
        </div>
      )}

      <div class="side-question-status" role="status" aria-live="polite" aria-atomic="true">
        {state.loading
          ? 'Loading side questions…'
          : state.running
            ? 'Answering side question…'
            : ''}
      </div>

      {state.running ? (
        <button class="side-question-stop" type="button" onClick={() => store.cancelSideQuestion()}>
          <span aria-hidden="true" />
          Stop answering
        </button>
      ) : (
        <form
          class="side-question-composer"
          onSubmit={(event) => {
            event.preventDefault();
            submit();
          }}
        >
          <label class="visually-hidden" for="sideQuestionInput">
            Ask a side question
          </label>
          <input
            ref={input}
            id="sideQuestionInput"
            autoFocus
            autoComplete="off"
            value={state.draft}
            placeholder="Ask about this conversation…"
            disabled={state.loading}
            onInput={(event) => store.setSideQuestionDraft(event.currentTarget.value)}
          />
          <button
            class="side-question-send"
            type="submit"
            aria-label="Send side question"
            disabled={state.loading || !state.draft.trim()}
          >
            <Icon name="send" />
          </button>
        </form>
      )}
    </Overlay>
  );
}

export function Modals() {
  const store = useStore();
  const activeSessionId = store.activeSessionId.value;
  const interaction = store.interactionOrder.value
    .map((key) => store.interactions.value[key])
    .find(
      (entry) =>
        entry?.sessionId === activeSessionId &&
        ['waiting', 'submitting', 'failed'].includes(entry.state),
    );
  const activeApproval =
    store.approval.value?.sessionId === activeSessionId ? store.approval.value : null;
  const activeAskUser =
    store.askUser.value?.sessionId === activeSessionId ? store.askUser.value : null;
  const modal = interaction
    ? interaction.kind === 'approval'
      ? 'approval'
      : 'ask-user'
    : activeApproval
      ? 'approval'
      : activeAskUser
        ? 'ask-user'
        : store.modal.value;
  switch (modal) {
    case 'stats':
      return <LazyStatsModal />;
    case 'settings':
      return <LazySettings />;
    case 'rename':
      return <LazyRename />;
    case 'project':
      return store.projectTarget.value ? <LazyProjectAssignment /> : <LazyProjectPicker />;
    case 'ask-user':
      return (
        <AskUser
          interactionPrompt={
            interaction?.kind === 'ask-user'
              ? (interaction.prompt as AskUserPrompt)
              : activeAskUser || undefined
          }
        />
      );
    case 'approval':
      return (
        <Approval
          key={`${activeApproval?.sessionId}:${activeApproval?.id}:${interaction?.requestId}`}
          interactionPrompt={
            interaction?.kind === 'approval'
              ? (interaction.prompt as ApprovalPrompt)
              : activeApproval || undefined
          }
        />
      );
    case 'approvals':
      return store.config.approvals === false ? null : <LazyApprovalsModal />;
    case 'mcp':
      return <LazyMCP />;
    case 'goal':
      return <LazyGoalModal />;
    case 'widgets':
      return <LazyWidgets />;
    case 'skills':
      return <LazySkills />;
    case 'commit':
      return <LazyCommitModal />;
    case 'share':
      return <LazyShareModal />;
    case 'side':
      return <SideQuestion />;
    case 'branch':
      return <LazyBranchTree />;
    case 'branch-context':
      return <LazyBranchContext />;
    case 'worktrees':
      return <LazyWorktrees />;
    default:
      return null;
  }
}
