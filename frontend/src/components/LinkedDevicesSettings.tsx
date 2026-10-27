import '../styles/features/linked-devices.css';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import {
  approveLinkedDevicesRequest,
  cancelLinkedDevicesInvitation,
  createLinkedDevicesInvitation,
  denyLinkedDevicesRequest,
  getLinkedDevices,
  resetLinkedDevices,
  resumeLinkedDevices,
  revokeLinkedDevicesDevice,
  stopLinkedDevices,
  type LinkedDevicesStatus,
} from '../api/linked-devices';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { copyText } from '../platform/clipboard';
import { PairingCodeInput } from './PairingCodeInput';

export const LINKED_DEVICES_POLL_MS = 1_500;

type ConfirmAction = 'reset';

const stateLabels: Record<LinkedDevicesStatus['state'], string> = {
  unavailable: 'Unavailable',
  off: 'Off',
  inviting: 'Invitation open',
  pending: 'Approval pending',
  sharing: 'Sharing',
  reconnecting: 'Reconnecting',
  stopped: 'Paused',
};

function countdown(expiresAt: string, now: number): string {
  const remaining = Math.max(0, Date.parse(expiresAt) - now);
  if (!Number.isFinite(remaining) || remaining === 0) return 'Expired';
  const totalSeconds = Math.ceil(remaining / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')} remaining`;
}

function deviceTime(value?: string): string {
  if (!value) return 'Not seen yet';
  const parsed = Date.parse(value);
  return Number.isFinite(parsed)
    ? `Last seen ${new Date(parsed).toLocaleString()}`
    : 'Last seen recently';
}

export function LinkedDevicesSettings({ active }: { active: boolean }) {
  const store = useStore();
  const [status, setStatus] = useState<LinkedDevicesStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [codes, setCodes] = useState<Record<string, string>>({});
  const [confirming, setConfirming] = useState<ConfirmAction | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<{ id: string; name: string } | null>(null);
  // The invitation URL last copied, so a slow clipboard write can never mark
  // a newer invitation as copied.
  const [copiedURL, setCopiedURL] = useState('');
  const [now, setNow] = useState(Date.now());
  const activeRef = useRef(active);
  activeRef.current = active;
  // The panel stays mounted while another Settings tab is shown, so actions
  // finish (and keep a new invitation's one-time secret) even when inactive.
  const mounted = useRef(true);
  useEffect(
    () => () => {
      mounted.current = false;
    },
    [],
  );
  // Confirmations replace the footer actions, so move focus into the
  // confirmation when it opens. When it closes, return focus to the control
  // that opened it once the resulting request has settled, falling back to
  // the footer and then the panel so focus is never dropped.
  const footerRef = useRef<HTMLDivElement>(null);
  const bodyRef = useRef<HTMLDivElement>(null);
  const confirmOpen = Boolean(confirming || revokeTarget);
  const opener = useRef<HTMLElement | null>(null);
  const restoreFocus = useRef(false);
  useEffect(() => {
    if (confirmOpen) {
      restoreFocus.current = true;
      footerRef.current?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
      return;
    }
    if (!restoreFocus.current || busy) return;
    restoreFocus.current = false;
    const target = opener.current;
    opener.current = null;
    if (target?.isConnected && !(target as HTMLButtonElement).disabled) target.focus();
    else
      (
        footerRef.current?.querySelector<HTMLButtonElement>('button:not(:disabled)') ??
        bodyRef.current
      )?.focus();
  }, [confirmOpen, busy]);
  // Safari does not focus buttons on click, so remember the clicked control
  // rather than document.activeElement.
  const openConfirm = (event: MouseEvent, open: () => void) => {
    opener.current = event.currentTarget as HTMLElement;
    open();
  };
  const closeConfirm = () => {
    setConfirming(null);
    setRevokeTarget(null);
  };
  // Only a create response carries the invitation secret. A status poll that
  // started before it must not overwrite it, so each response is tagged.
  const statusEpoch = useRef(0);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      const ticket = statusEpoch.current;
      try {
        const next = await getLinkedDevices(store.api, signal);
        if (!activeRef.current || signal?.aborted || ticket !== statusEpoch.current) return;
        setStatus((current) =>
          next.state === 'inviting' && !next.invitation && current?.invitation
            ? { ...next, invitation: current.invitation }
            : next,
        );
        setError('');
      } catch (cause) {
        if (signal?.aborted || !activeRef.current || ticket !== statusEpoch.current) return;
        // Keep the last status: it may hold an invitation secret that no later
        // poll can return. Before any status, the failure screen shows instead.
        setError(errorMessage(cause));
      } finally {
        if (activeRef.current && !signal?.aborted) setLoading(false);
      }
    },
    [store],
  );

  useEffect(() => {
    if (!active) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      await load(controller.signal);
      if (!controller.signal.aborted) timer = setTimeout(() => void poll(), LINKED_DEVICES_POLL_MS);
    };
    void poll();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [active, load]);

  const invitationExpiresAt = status?.invitation?.expires_at;
  const pendingExpiresAt = status?.pending_requests?.[0]?.expires_at;
  useEffect(() => {
    if (!active || (!invitationExpiresAt && !pendingExpiresAt)) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [active, invitationExpiresAt, pendingExpiresAt]);

  const mutate = async (key: string, action: () => Promise<void>) => {
    if (busy) return;
    setBusy(key);
    setError('');
    try {
      await action();
      if (mounted.current) await load();
    } catch (cause) {
      if (mounted.current) setError(errorMessage(cause));
    } finally {
      if (mounted.current) setBusy('');
    }
  };

  const createInvitation = async () => {
    if (busy) return;
    setBusy('invite');
    setError('');
    try {
      const next = await createLinkedDevicesInvitation(store.api);
      statusEpoch.current += 1;
      if (mounted.current) setStatus(next);
    } catch (cause) {
      if (mounted.current) setError(errorMessage(cause));
    } finally {
      if (mounted.current) setBusy('');
    }
  };

  const confirmedAction = () => {
    setConfirming(null);
    void mutate('reset', () => resetLinkedDevices(store.api));
  };

  if (loading && !status && !error)
    return (
      <section class="linked-devices" aria-label="Linked devices">
        <div class="settings-panel-body linked-devices-body">
          <p role="status">Checking linked devices…</p>
        </div>
      </section>
    );

  if (!status)
    return (
      <section class="linked-devices" aria-label="Linked devices">
        <div class="settings-panel-body linked-devices-body">
          <div class="linked-devices-state is-unavailable">
            <strong>Linked devices unavailable</strong>
            <p>{error || 'The server did not provide linked devices status.'}</p>
            <button class="btn" type="button" disabled={loading} onClick={() => void load()}>
              {loading ? 'Checking…' : 'Try again'}
            </button>
          </div>
        </div>
      </section>
    );

  const requests = status.pending_requests ?? [];
  const devices = status.devices ?? [];
  const request = requests[0];
  const invitation = status.invitation;
  const isUnavailable = status.state === 'unavailable';
  // Linking is a short task, not a standing state: while an invitation or a
  // pairing request exists the panel shows that task instead of the list.
  const linking = Boolean(request || invitation) || status.state === 'inviting';
  // The status banner only appears when something needs attention.
  const showState = ['unavailable', 'reconnecting', 'stopped'].includes(status.state);
  const canLink = status.state === 'sharing' || status.state === 'off';
  const canPause = devices.length > 0 && ['sharing', 'reconnecting'].includes(status.state);
  const canResume = ['reconnecting', 'stopped'].includes(status.state) || status.can_resume;
  // Remove all is offered only when it does more than removing the one
  // device, or when the server says a reset is required to continue.
  const canReset = devices.length > 1 || status.reset_required || status.can_reset;
  const disabled = Boolean(busy);
  const cancelInvitation = () =>
    mutate('cancel-invite', () => cancelLinkedDevicesInvitation(store.api));
  const startOver = () =>
    mutate('invite', async () => {
      await cancelLinkedDevicesInvitation(store.api);
      const next = await createLinkedDevicesInvitation(store.api);
      statusEpoch.current += 1;
      if (mounted.current) setStatus(next);
    });

  const notices = (
    <>
      {showState && (
        <div class={`linked-devices-state is-${status.state}`} role="status">
          <span>Linked devices</span>
          <strong>{stateLabels[status.state]}</strong>
        </div>
      )}
      {status.reason && <p class="linked-devices-reason">{status.reason}</p>}
      {(status.warnings ?? []).map((warning) => (
        <p class="linked-devices-warning" role="status" key={warning}>
          {warning}
        </p>
      ))}
      {error && (
        <p class="linked-devices-error" role="alert">
          {error}
        </p>
      )}
    </>
  );

  const steps = (step: 1 | 2) => (
    <ol class="linked-devices-steps" aria-label="Link a device">
      <li aria-current={step === 1 ? 'step' : undefined}>1 Scan</li>
      <li aria-current={step === 2 ? 'step' : undefined}>2 Confirm</li>
    </ol>
  );

  let body: preact.ComponentChildren;
  let footer: preact.ComponentChildren = null;
  if (request) {
    const name = request.name || 'Unnamed device';
    const code = codes[request.id] || '';
    body = (
      <>
        {steps(2)}
        <div class="linked-devices-confirm-code">
          {requests.length > 1 && (
            <small class="linked-devices-queue">1 of {requests.length} devices waiting</small>
          )}
          <strong class="linked-devices-confirm-title">{name} wants to connect</strong>
          <p>Enter the code shown on {name}</p>
          <PairingCodeInput
            key={request.id}
            label={`Code shown on ${name}`}
            value={code}
            disabled={disabled}
            autoFocus
            onChange={(value) => setCodes({ ...codes, [request.id]: value })}
          />
          {request.expires_at && (
            <small class="linked-devices-countdown">{countdown(request.expires_at, now)}</small>
          )}
          <details>
            <summary>It will see your conversations and can use this agent’s tools</summary>
            <p>{status.authority.description}</p>
            <p>{status.authority.same_uid_note}</p>
          </details>
        </div>
      </>
    );
    footer = (
      <>
        <div class="settings-footer-secondary">
          <button
            class="btn"
            type="button"
            disabled={disabled}
            onClick={() =>
              void mutate(`deny-${request.id}`, () =>
                denyLinkedDevicesRequest(store.api, request.id),
              )
            }
          >
            {busy === `deny-${request.id}` ? 'Denying…' : 'Deny'}
          </button>
        </div>
        <button
          class="btn primary"
          type="button"
          disabled={disabled || code.length < 6}
          onClick={() =>
            void mutate(`approve-${request.id}`, () =>
              approveLinkedDevicesRequest(store.api, request.id, code),
            )
          }
        >
          {busy === `approve-${request.id}` ? 'Approving…' : `Approve ${name}`}
        </button>
      </>
    );
  } else if (linking) {
    body = (
      <>
        {steps(1)}
        {invitation ? (
          <div class="linked-devices-scan">
            <img src={invitation.qr_data_url} alt="QR code for the linked devices invitation" />
            <div>
              <strong>Scan with your phone</strong>
              <p>Open the camera on the device you want to link and point it at this code.</p>
              <p class="linked-devices-countdown" aria-live="polite">
                {countdown(invitation.expires_at, now)}
              </p>
              <button
                class="btn linked-devices-copy"
                type="button"
                disabled={disabled}
                onClick={() => {
                  void copyText(invitation.url)
                    .then(() => setCopiedURL(invitation.url))
                    .catch((cause) => setError(errorMessage(cause)));
                }}
              >
                {copiedURL === invitation.url ? 'Link copied' : 'Copy link instead'}
              </button>
              <p class="linked-devices-note" role="note">
                Keep the link private until it expires.
              </p>
            </div>
          </div>
        ) : (
          <p class="linked-devices-note">
            This invitation’s code was already shown and can’t be shown again. Start over to get a
            new one.
          </p>
        )}
      </>
    );
    footer = (
      <>
        <div class="settings-footer-secondary">
          <button
            class="btn"
            type="button"
            disabled={disabled}
            onClick={() => void cancelInvitation()}
          >
            {busy === 'cancel-invite' ? 'Cancelling…' : 'Cancel'}
          </button>
        </div>
        {invitation ? (
          <span class="linked-devices-waiting" role="status">
            Waiting for your device…
          </span>
        ) : (
          <button
            class="btn primary"
            type="button"
            disabled={disabled}
            onClick={() => void startOver()}
          >
            {busy === 'invite' ? 'Starting over…' : 'Start over'}
          </button>
        )}
      </>
    );
  } else {
    body = devices.length ? (
      <>
        <ul class="linked-devices-list">
          {devices.map((device) => {
            const online = device.connection !== 'offline';
            return (
              <li class="linked-devices-device" key={device.id}>
                <div>
                  <strong>{device.name || 'Unnamed device'}</strong>
                  <small>
                    <span class={`linked-devices-dot ${online ? 'is-online' : ''}`} />
                    {online ? 'Connected now' : deviceTime(device.last_seen_at)}
                  </small>
                </div>
                <button
                  class="btn"
                  type="button"
                  disabled={disabled}
                  onClick={(event) =>
                    openConfirm(event, () => setRevokeTarget({ id: device.id, name: device.name }))
                  }
                >
                  {busy === `revoke-${device.id}` ? 'Removing…' : 'Remove'}
                </button>
              </li>
            );
          })}
        </ul>
        {(canPause || canReset) && (
          <div class="linked-devices-quiet-actions">
            {canPause && (
              <button
                class="linked-devices-text-button"
                type="button"
                disabled={disabled}
                onClick={() => void mutate('stop', () => stopLinkedDevices(store.api))}
              >
                {busy === 'stop' ? 'Pausing…' : 'Pause all devices'}
              </button>
            )}
            {canReset && (
              <button
                class="linked-devices-text-button is-danger"
                type="button"
                disabled={disabled}
                onClick={(event) => openConfirm(event, () => setConfirming('reset'))}
              >
                Remove all devices…
              </button>
            )}
          </div>
        )}
      </>
    ) : (
      !isUnavailable && (
        <div class="linked-devices-empty">
          <strong>No devices yet</strong>
          <p>Link your phone or another computer to use this server from it.</p>
          {canReset && (
            <button
              class="linked-devices-text-button is-danger"
              type="button"
              disabled={disabled}
              onClick={(event) => openConfirm(event, () => setConfirming('reset'))}
            >
              Reset linked devices…
            </button>
          )}
        </div>
      )
    );
    const resetFromFooter = canReset && isUnavailable && devices.length === 0;
    if (canResume || canLink || resetFromFooter)
      footer = (
        <>
          {resetFromFooter && (
            <div class="settings-footer-secondary">
              <button
                class="btn danger"
                type="button"
                disabled={disabled}
                onClick={(event) => openConfirm(event, () => setConfirming('reset'))}
              >
                Reset linked devices…
              </button>
            </div>
          )}
          {canResume ? (
            <button
              class="btn primary"
              type="button"
              disabled={disabled}
              onClick={() => void mutate('resume', () => resumeLinkedDevices(store.api))}
            >
              {busy === 'resume' ? 'Resuming…' : 'Resume'}
            </button>
          ) : (
            canLink && (
              <button
                class="btn primary"
                type="button"
                disabled={disabled}
                onClick={() => void createInvitation()}
              >
                {busy === 'invite' ? 'Creating invitation…' : 'Link a device'}
              </button>
            )
          )}
        </>
      );
  }

  // The confirmation is modal within the panel: Escape cancels it (instead of
  // closing Settings) and Tab stays on its buttons.
  const confirmKeyDown = (event: KeyboardEvent) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      closeConfirm();
    } else if (event.key === 'Tab') {
      const buttons = [
        ...(event.currentTarget as HTMLElement).querySelectorAll<HTMLButtonElement>('button'),
      ];
      const index = buttons.indexOf(document.activeElement as HTMLButtonElement);
      const next = (index + (event.shiftKey ? -1 : 1) + buttons.length) % buttons.length;
      event.preventDefault();
      event.stopPropagation();
      buttons[next]?.focus();
    }
  };

  return (
    <section class="linked-devices" aria-label="Linked devices">
      <div
        ref={bodyRef}
        class="settings-panel-body linked-devices-body"
        tabIndex={-1}
        inert={confirmOpen}
      >
        {notices}
        {body}
      </div>

      {revokeTarget ? (
        <div
          ref={footerRef}
          class="modal-actions settings-footer linked-devices-confirm"
          role="alertdialog"
          aria-labelledby="linked-devices-revoke-title"
          aria-describedby="linked-devices-revoke-description"
          onKeyDown={confirmKeyDown}
        >
          <div class="settings-footer-message">
            <strong id="linked-devices-revoke-title">
              Remove {revokeTarget.name || 'this device'}?
            </strong>
            <p id="linked-devices-revoke-description">It will no longer be able to connect.</p>
          </div>
          <button class="btn" type="button" onClick={closeConfirm}>
            Cancel
          </button>
          <button
            class="btn danger"
            type="button"
            onClick={() => {
              const target = revokeTarget;
              setRevokeTarget(null);
              void mutate(`revoke-${target.id}`, () =>
                revokeLinkedDevicesDevice(store.api, target.id),
              );
            }}
          >
            Remove
          </button>
        </div>
      ) : confirming ? (
        <div
          ref={footerRef}
          class="modal-actions settings-footer linked-devices-confirm"
          role="alertdialog"
          aria-labelledby="linked-devices-confirm-title"
          aria-describedby="linked-devices-confirm-description"
          onKeyDown={confirmKeyDown}
        >
          <div class="settings-footer-message">
            <strong id="linked-devices-confirm-title">Remove all devices?</strong>
            <p id="linked-devices-confirm-description">
              Every linked device, invitation, and pending request will be removed.
            </p>
          </div>
          <button class="btn" type="button" onClick={closeConfirm}>
            Cancel
          </button>
          <button class="btn danger" type="button" onClick={confirmedAction}>
            Remove all
          </button>
        </div>
      ) : (
        footer && (
          <div ref={footerRef} class="modal-actions settings-footer">
            {footer}
          </div>
        )
      )}
    </section>
  );
}
