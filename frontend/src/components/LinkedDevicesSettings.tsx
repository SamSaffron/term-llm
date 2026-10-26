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

export const LINKED_DEVICES_POLL_MS = 1_500;

type ConfirmAction = 'reset';

const stateLabels: Record<LinkedDevicesStatus['state'], string> = {
  unavailable: 'Unavailable',
  off: 'Off',
  inviting: 'Invitation open',
  pending: 'Approval pending',
  sharing: 'Sharing',
  reconnecting: 'Reconnecting',
  stopped: 'Stopped',
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
  const [copied, setCopied] = useState(false);
  const [now, setNow] = useState(Date.now());
  const activeRef = useRef(active);
  activeRef.current = active;

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      try {
        const next = await getLinkedDevices(store.api, signal);
        if (!activeRef.current || signal?.aborted) return;
        setStatus((current) =>
          next.state === 'inviting' && !next.invitation && current?.invitation
            ? { ...next, invitation: current.invitation }
            : next,
        );
        setError('');
      } catch (cause) {
        if (signal?.aborted || !activeRef.current) return;
        setStatus(null);
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
      if (activeRef.current) await load();
    } catch (cause) {
      if (activeRef.current) setError(errorMessage(cause));
    } finally {
      if (activeRef.current) setBusy('');
    }
  };

  const createInvitation = async () => {
    if (busy) return;
    setBusy('invite');
    setError('');
    try {
      const next = await createLinkedDevicesInvitation(store.api);
      if (activeRef.current) setStatus(next);
    } catch (cause) {
      if (activeRef.current) setError(errorMessage(cause));
    } finally {
      if (activeRef.current) setBusy('');
    }
  };

  const confirmedAction = () => {
    setConfirming(null);
    void mutate('reset', () => resetLinkedDevices(store.api));
  };

  if (loading && !status && !error)
    return (
      <section class="linked-devices" aria-label="Linked devices">
        <p role="status">Checking linked devices…</p>
      </section>
    );

  if (!status)
    return (
      <section class="linked-devices" aria-label="Linked devices">
        <div class="linked-devices-state is-unavailable">
          <strong>Linked devices unavailable</strong>
          <p>{error || 'The server did not provide linked devices status.'}</p>
          <button class="btn" type="button" disabled={loading} onClick={() => void load()}>
            {loading ? 'Checking…' : 'Try again'}
          </button>
        </div>
      </section>
    );

  const requests = status.pending_requests ?? [];
  const devices = status.devices ?? [];
  const isUnavailable = status.state === 'unavailable';
  const canStop = ['sharing', 'pending', 'inviting', 'reconnecting'].includes(status.state);
  const canReset =
    ['sharing', 'pending', 'inviting', 'reconnecting', 'stopped'].includes(status.state) ||
    status.reset_required;

  return (
    <section class="linked-devices" aria-label="Linked devices">
      <div class={`linked-devices-state is-${status.state}`} role="status">
        <span>Linked devices</span>
        <strong>{stateLabels[status.state]}</strong>
      </div>
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

      {status.state === 'off' && (
        <div class="linked-devices-section">
          <h3>Connect a device</h3>
          <p>Scan the QR code or copy the pairing link into a compatible client.</p>
          <button
            class="btn primary"
            type="button"
            disabled={Boolean(busy)}
            onClick={() => void createInvitation()}
          >
            {busy === 'invite' ? 'Connecting' : 'Connect'}
          </button>
        </div>
      )}

      {status.invitation && (
        <div class="linked-devices-section linked-devices-invitation">
          <div>
            <h3>Pairing invitation</h3>
            <p class="linked-devices-countdown" aria-live="polite">
              {countdown(status.invitation.expires_at, now)}
            </p>
          </div>
          <img
            src={status.invitation.qr_data_url}
            alt="QR code for the linked devices invitation"
          />
          <div class="linked-devices-invite-actions">
            <button
              class="btn"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => {
                void copyText(status.invitation!.url)
                  .then(() => setCopied(true))
                  .catch((cause) => setError(errorMessage(cause)));
              }}
            >
              {copied ? 'Invitation copied' : 'Copy invitation link'}
            </button>
            <button
              class="btn"
              type="button"
              disabled={Boolean(busy)}
              onClick={() =>
                void mutate('cancel-invite', () => cancelLinkedDevicesInvitation(store.api))
              }
            >
              {busy === 'cancel-invite' ? 'Cancelling…' : 'Cancel invitation'}
            </button>
          </div>
          <p class="linked-devices-warning" role="note">
            Keep the invitation link private until it expires.
          </p>
        </div>
      )}

      {status.state === 'inviting' && !status.invitation && (
        <div class="linked-devices-section">
          <h3>Invitation already active</h3>
          <p>
            Pairing secrets are displayed only once. Cancel this invitation and create a new one if
            you closed the original view.
          </p>
          <button
            class="btn"
            type="button"
            disabled={Boolean(busy)}
            onClick={() =>
              void mutate('cancel-invite', () => cancelLinkedDevicesInvitation(store.api))
            }
          >
            Cancel active invitation
          </button>
        </div>
      )}

      {requests.length > 0 && (
        <div class="linked-devices-section">
          <h3>Pending devices</h3>
          <div class="linked-devices-list">
            {requests.map((request) => {
              const code = codes[request.id] || '';
              return (
                <div class="linked-devices-card" key={request.id}>
                  <div>
                    <strong>{request.name || 'Unnamed device'}</strong>
                    <small>
                      Enter the code shown on that device
                      {request.expires_at ? ` · ${countdown(request.expires_at, now)}` : ''}.
                    </small>
                    <p>This device can read conversations and use this agent’s tools.</p>
                    <details>
                      <summary>Access details</summary>
                      <p>{status.authority.description}</p>
                      <p>{status.authority.same_uid_note}</p>
                    </details>
                  </div>
                  <label for={`linked-devices-code-${request.id}`}>Confirmation code</label>
                  <input
                    id={`linked-devices-code-${request.id}`}
                    value={code}
                    inputMode="text"
                    autoComplete="one-time-code"
                    spellcheck={false}
                    disabled={Boolean(busy)}
                    onInput={(event) =>
                      setCodes({ ...codes, [request.id]: event.currentTarget.value.toUpperCase() })
                    }
                  />
                  <div class="linked-devices-card-actions">
                    <button
                      class="btn primary"
                      type="button"
                      disabled={Boolean(busy) || !code.trim()}
                      onClick={() =>
                        void mutate(`approve-${request.id}`, () =>
                          approveLinkedDevicesRequest(store.api, request.id, code.trim()),
                        )
                      }
                    >
                      {busy === `approve-${request.id}` ? 'Approving…' : 'Approve'}
                    </button>
                    <button
                      class="btn"
                      type="button"
                      disabled={Boolean(busy)}
                      onClick={() =>
                        void mutate(`deny-${request.id}`, () =>
                          denyLinkedDevicesRequest(store.api, request.id),
                        )
                      }
                    >
                      {busy === `deny-${request.id}` ? 'Denying…' : 'Deny'}
                    </button>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}

      {devices.length > 0 && (
        <div class="linked-devices-section">
          <h3>Approved devices</h3>
          <div class="linked-devices-list">
            {devices.map((device) => (
              <div class="linked-devices-card linked-devices-device" key={device.id}>
                <div>
                  <strong>{device.name || 'Unnamed device'}</strong>
                  <small>
                    {device.connection} · {deviceTime(device.last_seen_at)}
                  </small>
                </div>
                <button
                  class="btn danger"
                  type="button"
                  disabled={Boolean(busy)}
                  onClick={() => setRevokeTarget({ id: device.id, name: device.name })}
                >
                  {busy === `revoke-${device.id}` ? 'Revoking…' : 'Revoke'}
                </button>
              </div>
            ))}
          </div>
        </div>
      )}

      {(!isUnavailable || status.reset_required) && (
        <div class="linked-devices-controls">
          {status.state === 'sharing' && (
            <button
              class="btn primary"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => void createInvitation()}
            >
              Connect
            </button>
          )}
          {canStop && (
            <button
              class="btn"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => void mutate('stop', () => stopLinkedDevices(store.api))}
            >
              {busy === 'stop' ? 'Stopping' : 'Stop sharing'}
            </button>
          )}
          {status.state === 'reconnecting' && (
            <button
              class="btn primary"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => void mutate('resume', () => resumeLinkedDevices(store.api))}
            >
              {busy === 'resume' ? 'Resuming' : 'Resume'}
            </button>
          )}
          {status.state === 'stopped' && (
            <button
              class="btn primary"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => void mutate('resume', () => resumeLinkedDevices(store.api))}
            >
              {busy === 'resume' ? 'Resuming' : 'Resume'}
            </button>
          )}
          {canReset && (
            <button
              class="btn danger"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => setConfirming('reset')}
            >
              Forget all
            </button>
          )}
        </div>
      )}

      {revokeTarget && (
        <div
          class="linked-devices-confirm"
          role="alertdialog"
          aria-labelledby="linked-devices-revoke-title"
        >
          <strong id="linked-devices-revoke-title">
            Revoke {revokeTarget.name || 'this device'}?
          </strong>
          <p>This device will no longer be able to connect.</p>
          <div>
            <button class="btn" type="button" onClick={() => setRevokeTarget(null)}>
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
              Revoke
            </button>
          </div>
        </div>
      )}

      {confirming && (
        <div
          class="linked-devices-confirm"
          role="alertdialog"
          aria-labelledby="linked-devices-confirm-title"
        >
          <strong id="linked-devices-confirm-title">Forget all devices?</strong>
          <p>All invitations, pending requests, and approved devices will be removed.</p>
          <div>
            <button class="btn" type="button" onClick={() => setConfirming(null)}>
              Cancel
            </button>
            <button class="btn danger" type="button" onClick={confirmedAction}>
              Forget all
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
