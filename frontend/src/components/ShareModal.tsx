import { useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type {
  SessionShareResponse,
  SharingCapabilitiesResponse,
  ShareVisibility,
} from '../api/endpoints';
import { errorMessage } from '../domain/text';
import { copyText } from '../platform/browser';
import { Overlay } from './Overlay';
import '../styles/features/share.css';

type ShareScope = 'response' | 'conversation';
type SharePhase = 'form' | 'submitting' | 'success';

const visibilityLabel = (visibility: ShareVisibility): string =>
  visibility === 'public' ? 'Public' : visibility === 'private' ? 'Private' : 'Unlisted';

const visibilityDescription = (visibility: ShareVisibility): string => {
  if (visibility === 'public') return 'Discoverable by anyone through the sharing provider.';
  if (visibility === 'private') return 'Access is restricted by the sharing provider.';
  return 'Anyone with the link may be able to view it, but it is not publicly listed.';
};

export function ShareModal() {
  const store = useStore();
  const target = store.shareTarget.value;
  const [scope, setScope] = useState<ShareScope>('response');
  const [visibility, setVisibility] = useState<ShareVisibility | ''>('');
  const [capabilities, setCapabilities] = useState<SharingCapabilitiesResponse | null>(null);
  const [capabilityLoading, setCapabilityLoading] = useState(true);
  const [capabilityError, setCapabilityError] = useState('');
  const [phase, setPhase] = useState<SharePhase>('form');
  const [error, setError] = useState('');
  const [copied, setCopied] = useState(false);
  const [result, setResult] = useState<SessionShareResponse | null>(null);
  const success = useRef<HTMLDivElement>(null);

  const close = () => {
    if (phase === 'submitting') return;
    store.shareTarget.value = null;
    store.modal.value = '';
  };

  useEffect(() => {
    let active = true;
    setCapabilityLoading(true);
    setCapabilityError('');
    setCapabilities(null);
    setVisibility('');
    void store.endpoints
      .sharingCapabilities()
      .then((loaded) => {
        if (!active) return;
        if (!loaded.enabled || !loaded.visibilities.length) {
          throw new Error('Sharing is not enabled on this server.');
        }
        setCapabilities(loaded);
        setVisibility(loaded.default_visibility);
      })
      .catch((cause) => {
        if (active) setCapabilityError(errorMessage(cause));
      })
      .finally(() => {
        if (active) setCapabilityLoading(false);
      });
    return () => {
      active = false;
    };
  }, [store, target?.sessionId]);

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
  const create = async () => {
    if (submitting || !capabilities || !visibility) return;
    setError('');
    setCopied(false);
    setPhase('submitting');
    try {
      const response = await store.endpoints.createSessionShare(target.sessionId, {
        anchor_message_id: target.anchorMessageId,
        scope,
        visibility,
      });
      setResult(response);
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
          <p>{visibilityLabel(result.visibility)}</p>
          {!result.ready && (
            <p class="share-ready-note">
              The link was created, but readiness was not confirmed yet.
            </p>
          )}
          <label class="share-url-label" for="shareURL">
            Share link
          </label>
          <input
            id="shareURL"
            class="share-url"
            value={result.url}
            readOnly
            onFocus={(event) => event.currentTarget.select()}
          />
          {result.source_url && (
            <a class="share-source-link" href={result.source_url} target="_blank" rel="noreferrer">
              View source ↗
            </a>
          )}
          <div class="modal-actions share-success-actions">
            <a class="btn primary" href={result.url} target="_blank" rel="noreferrer">
              Open link
            </a>
            <button
              class="btn"
              type="button"
              onClick={() => {
                void copyText(result.url)
                  .then(() => setCopied(true))
                  .catch((cause) => store.toast(cause, 'error'));
              }}
            >
              {copied ? 'Copied' : 'Copy link'}
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
          {capabilityLoading && <p role="status">Loading sharing options…</p>}
          {!capabilityLoading && capabilityError && (
            <div class="modal-error share-error" role="alert">
              <strong>Sharing unavailable.</strong> {capabilityError}
            </div>
          )}
          {!capabilityLoading && capabilities && (
            <>
              <p>
                {capabilities.provider.id === 'github'
                  ? 'Create a GitHub Gist with a polished, standalone transcript.'
                  : `Create a share with ${capabilities.provider.name}.`}
              </p>

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
                {capabilities.visibilities.length === 1 ? (
                  <div class="share-included">
                    <strong>{visibilityLabel(capabilities.visibilities[0])}</strong>
                    <span>{visibilityDescription(capabilities.visibilities[0])}</span>
                  </div>
                ) : (
                  capabilities.visibilities.map((option) => (
                    <label
                      key={option}
                      class={`share-choice ${visibility === option ? 'is-selected' : ''}`}
                    >
                      <input
                        type="radio"
                        name="share-visibility"
                        value={option}
                        checked={visibility === option}
                        onChange={() => setVisibility(option)}
                      />
                      <span>
                        <strong>{visibilityLabel(option)}</strong>
                        <small>{visibilityDescription(option)}</small>
                      </span>
                    </label>
                  ))
                )}
              </fieldset>

              <div class="share-included">
                <strong>What’s included</strong>
                <p>
                  {scope === 'response'
                    ? 'The assistant reply text only. Prompts, tool activity, and raw reasoning are excluded.'
                    : 'The conversation through this response. Prompts, code, tool output, filenames, and images may be included; raw reasoning is excluded.'}
                </p>
              </div>
              {capabilities.notes?.map((note) => (
                <p class="share-privacy" key={note}>
                  {note}
                </p>
              ))}
              {capabilities.help && <p class="share-provider-help">{capabilities.help}</p>}
              {error && (
                <div class="modal-error share-error" role="alert">
                  <strong>Couldn’t create share.</strong> {error}
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
                  disabled={submitting || !visibility}
                >
                  <span aria-live="polite">
                    {submitting ? 'Creating share…' : error ? 'Try again' : 'Create share'}
                  </span>
                </button>
              </div>
            </>
          )}
        </form>
      )}
    </Overlay>
  );
}
