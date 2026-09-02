import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import type { HubConfig } from '../config';
import { buildRegistrationCommand, currentHubURL } from '../domain/formatting';
import type { ClipboardAdapter } from '../platform/clipboard';
import type { HubStore } from '../stores/hub-store';

const maskedToken = '••••••••••••••••••••••••••••••••';

export function RegistrationHelp({
  config,
  store,
  clipboard,
}: {
  config: HubConfig;
  store: HubStore;
  clipboard: ClipboardAdapter;
}) {
  const [copied, setCopied] = useState('');
  const timer = useRef<number>();
  const hubURL = currentHubURL(window.location, config.basePath);
  const command = useMemo(() => buildRegistrationCommand(hubURL), [hubURL]);
  const info = store.registrationInfo.value;
  const token = info?.registration_token ?? '';

  useEffect(
    () => () => {
      if (timer.current !== undefined) window.clearTimeout(timer.current);
    },
    [],
  );

  const copy = async (value: string, label: string) => {
    try {
      await clipboard.writeText(value);
      setCopied(label);
      store.registrationStatus.value = `Copied ${label}.`;
      store.registrationError.value = '';
      if (timer.current !== undefined) window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(''), 1_400);
    } catch (error) {
      store.registrationStatus.value = '';
      store.registrationError.value = `Copy failed: ${error instanceof Error ? error.message : String(error)}`;
    }
  };

  return (
    <section class="registration-help" aria-label="Reverse registration help">
      <div class="registration-help-head">
        <div>
          <h3>Register a private / Docker node</h3>
          <p>
            Use this when the Hub cannot directly reach the node — for example, a Docker container,
            laptop, NAT’d machine, or private network service.
          </p>
        </div>
      </div>
      <p>
        The node registers itself with this Hub, opens an outbound reverse connection, and appears
        in the node list automatically. You do not need to enter a URL here.
      </p>
      {store.registrationLoading.value && (
        <div class="registration-loading">Loading registration settings…</div>
      )}
      {!store.registrationLoading.value && info?.enabled && token && (
        <div class="registration-enabled">
          <div class="help-field">
            <span class="help-label">Hub URL</span>
            <div class="copy-row">
              <code>{hubURL}</code>
              <button
                class={`hub-btn ghost small${copied === 'Hub URL' ? ' copied' : ''}`}
                type="button"
                onClick={() => void copy(hubURL, 'Hub URL')}
              >
                {copied === 'Hub URL' ? '✓ Copied' : 'Copy'}
              </button>
            </div>
          </div>
          <div class="help-field">
            <span class="help-label">Registration token</span>
            <div class="copy-row">
              <code>{store.registrationRevealed.value ? token : maskedToken}</code>
              <button
                class={`hub-btn ghost small${copied === 'registration token' ? ' copied' : ''}`}
                type="button"
                onClick={() => void copy(token, 'registration token')}
              >
                {copied === 'registration token' ? '✓ Copied' : 'Copy token'}
              </button>
              <button
                class="hub-btn ghost small"
                type="button"
                onClick={() =>
                  (store.registrationRevealed.value = !store.registrationRevealed.value)
                }
              >
                {store.registrationRevealed.value ? 'Hide' : 'Reveal'}
              </button>
            </div>
            <p class="help-warning">
              Treat this like a deployment secret. It cannot administer the Hub, but it can add or
              update reverse nodes.
            </p>
          </div>
          <div class="help-field">
            <span class="help-label">Start a reverse node</span>
            <pre class="command-box">
              <code>{command}</code>
            </pre>
            <div class="help-actions">
              <button
                class={`hub-btn ghost small${copied === 'reverse node command' ? ' copied' : ''}`}
                type="button"
                onClick={() => void copy(command, 'reverse node command')}
              >
                {copied === 'reverse node command' ? '✓ Copied' : 'Copy command'}
              </button>
            </div>
          </div>
          <p class="help-note">
            For a stable container, use a stable <code>NODE_ID</code> and <code>NODE_TOKEN</code>{' '}
            from Docker secrets or env instead of generating them on every boot. If the same{' '}
            <code>NODE_ID</code> registers again, Hub updates that registered node.
          </p>
          <dl class="token-glossary">
            <dt>Hub token</dt>
            <dd>Unlocks/administers this Hub UI.</dd>
            <dt>Registration token</dt>
            <dd>Allows scripts or containers to register reverse nodes.</dd>
            <dt>Node token</dt>
            <dd>Per-node bearer token used after registration.</dd>
          </dl>
        </div>
      )}
      {!store.registrationLoading.value && info && (!info.enabled || !token) && (
        <div class="registration-disabled">
          <p>
            <strong>Reverse registration is disabled for this Hub.</strong>
          </p>
          <p>Start Hub with a registration token:</p>
          <pre class="command-box">
            <code>{`term-llm serve hub \\
  --registration-token "$HUB_REGISTRATION_TOKEN"`}</code>
          </pre>
          <p>or set the environment variable:</p>
          <pre class="command-box">
            <code>{`TERM_LLM_HUB_REGISTRATION_TOKEN="$HUB_REGISTRATION_TOKEN" \\
term-llm serve hub`}</code>
          </pre>
        </div>
      )}
      <div class="modal-status" role="status" aria-live="polite">
        {store.registrationError.value || store.registrationStatus.value}
      </div>
    </section>
  );
}
