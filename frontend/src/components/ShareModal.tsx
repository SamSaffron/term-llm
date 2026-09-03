import { useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { copyText } from '../platform/browser';
import { Overlay } from './Overlay';
import '../styles/features/share.css';

type ShareScope = 'response' | 'conversation';
type SharePhase = 'form' | 'submitting' | 'success';

export function ShareModal() {
  const store = useStore();
  const target = store.shareTarget.value;
  const [scope, setScope] = useState<ShareScope>('response');
  const [isPublic, setIsPublic] = useState(false);
  const [phase, setPhase] = useState<SharePhase>('form');
  const [error, setError] = useState('');
  const [copied, setCopied] = useState(false);
  const [result, setResult] = useState<{
    previewURL: string;
    gistURL: string;
    public: boolean;
  } | null>(null);
  const success = useRef<HTMLDivElement>(null);

  const close = () => {
    if (phase === 'submitting') return;
    store.shareTarget.value = null;
    store.modal.value = '';
  };

  useEffect(() => {
    if (phase === 'submitting' || !target || store.activeSession.value?.id === target.sessionId)
      return;
    store.shareTarget.value = null;
    store.modal.value = '';
  }, [phase, store, store.activeSession.value?.id, target]);

  useEffect(() => {
    if (phase !== 'success') return;
    const frame = requestAnimationFrame(() => success.current?.focus());
    return () => cancelAnimationFrame(frame);
  }, [phase]);

  if (!target) return null;
  const submitting = phase === 'submitting';
  const githubCLIProblem = /\bgh\b|GitHub CLI/i.test(error);
  const create = async () => {
    if (submitting) return;
    setError('');
    setCopied(false);
    setPhase('submitting');
    try {
      const response = await store.endpoints.createSessionShare(target.sessionId, {
        anchor_message_id: target.anchorMessageId,
        scope,
        public: isPublic,
      });
      setResult({
        previewURL: response.preview_url,
        gistURL: response.gist_url,
        public: response.public,
      });
      setPhase('success');
    } catch (cause) {
      setError(errorMessage(cause));
      setPhase('form');
    }
  };

  return (
    <Overlay
      title="Share transcript"
      className="share-modal"
      close={!submitting}
      dismissDisabled={submitting}
      onClose={close}
    >
      {phase === 'success' && result ? (
        <div class="share-success" ref={success} tabIndex={-1} role="status" aria-live="polite">
          <div class="share-success-mark" aria-hidden="true">
            ✓
          </div>
          <h3>Share created</h3>
          <p>{result.public ? 'Public' : 'Secret (unlisted)'}</p>
          <label class="share-url-label" for="sharePreviewURL">
            Preview link
          </label>
          <input
            id="sharePreviewURL"
            class="share-url"
            value={result.previewURL}
            readOnly
            onFocus={(event) => event.currentTarget.select()}
          />
          <a class="share-gist-link" href={result.gistURL} target="_blank" rel="noreferrer">
            View Gist ↗
          </a>
          <div class="modal-actions share-success-actions">
            <a class="btn primary" href={result.previewURL} target="_blank" rel="noreferrer">
              Open preview
            </a>
            <button
              class="btn"
              type="button"
              onClick={() => {
                void copyText(result.previewURL)
                  .then(() => setCopied(true))
                  .catch((cause) => store.toast(cause, 'error'));
              }}
            >
              {copied ? 'Copied' : 'Copy preview link'}
            </button>
            <button class="btn" type="button" onClick={close}>
              Done
            </button>
          </div>
        </div>
      ) : (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            void create();
          }}
        >
          <p>Create a GitHub Gist with a polished, standalone transcript.</p>

          <fieldset class="share-fieldset" disabled={submitting}>
            <legend>What do you want to share?</legend>
            <label class={`share-choice ${scope === 'response' ? 'is-selected' : ''}`}>
              <input
                type="radio"
                name="share-scope"
                value="response"
                checked={scope === 'response'}
                onChange={() => setScope('response')}
              />
              <span>
                <strong>This response</strong>
                <small>
                  Just the assistant’s complete reply text. No prompts or tool activity.
                </small>
              </span>
            </label>
            <label class={`share-choice ${scope === 'conversation' ? 'is-selected' : ''}`}>
              <input
                type="radio"
                name="share-scope"
                value="conversation"
                checked={scope === 'conversation'}
                onChange={() => setScope('conversation')}
              />
              <span>
                <strong>Conversation up to here</strong>
                <small>
                  The visible transcript, including your messages and tool activity, up to and
                  including this response.
                </small>
              </span>
            </label>
          </fieldset>

          <fieldset class="share-fieldset share-visibility" disabled={submitting}>
            <legend>Visibility</legend>
            <label class={`share-choice ${!isPublic ? 'is-selected' : ''}`}>
              <input
                type="radio"
                name="share-visibility"
                checked={!isPublic}
                onChange={() => setIsPublic(false)}
              />
              <span>
                <strong>Secret (unlisted)</strong>
                <small>Anyone with the link can view it. Secret Gists are not private.</small>
              </span>
            </label>
            <label class={`share-choice ${isPublic ? 'is-selected' : ''}`}>
              <input
                type="radio"
                name="share-visibility"
                checked={isPublic}
                onChange={() => setIsPublic(true)}
              />
              <span>
                <strong>Public</strong>
                <small>Visible on your GitHub Gist profile and discoverable by anyone.</small>
              </span>
            </label>
          </fieldset>

          <div class="share-included">
            <strong>What’s included</strong>
            <p>
              {scope === 'response'
                ? 'The assistant reply text only. Prompts, tool activity, and raw reasoning are excluded.'
                : 'The conversation through this response. Prompts, code, tool output, filenames, and images may be included; raw reasoning is excluded.'}
            </p>
          </div>
          <p class="share-privacy">
            Review sensitive content before sharing—the Gist is created on your GitHub account.
          </p>
          {error && (
            <div class="modal-error share-error" role="alert">
              <strong>Couldn’t create the Gist.</strong> {error}
              {githubCLIProblem && (
                <span class="share-error-help">
                  Sharing currently uses the GitHub CLI on the machine running term-llm. Install{' '}
                  <a href="https://cli.github.com/" target="_blank" rel="noreferrer">
                    <code>gh</code>
                  </a>{' '}
                  and run <code>gh auth login</code>, then try again.
                </span>
              )}
            </div>
          )}
          <div class="modal-actions">
            {!submitting && (
              <button class="btn" type="button" onClick={close}>
                Cancel
              </button>
            )}
            <button
              class={`btn primary ${submitting ? 'is-loading' : ''}`}
              type="submit"
              disabled={submitting}
            >
              <span aria-live="polite">
                {submitting ? 'Creating Gist…' : error ? 'Try again' : 'Create share'}
              </span>
            </button>
          </div>
        </form>
      )}
    </Overlay>
  );
}
