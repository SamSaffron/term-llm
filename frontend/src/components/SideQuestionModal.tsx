import { memo } from './memo';
import { useEffect, useLayoutEffect, useRef } from 'preact/hooks';
import { useStore } from '../app/context';
import { Icon } from './Icon';
import { Overlay } from './Overlay';
import { Markdown } from './Markdown';

// Loaded on demand: the side question opens only from an explicit /side.
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
              <Icon class="glyph-icon" name="arrow-up-right" />
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
