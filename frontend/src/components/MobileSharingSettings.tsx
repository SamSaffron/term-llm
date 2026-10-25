import '../styles/features/mobile-sharing.css';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import {
  approveMobileSharingRequest,
  cancelMobileSharingInvitation,
  createMobileSharingInvitation,
  denyMobileSharingRequest,
  getMobileSharing,
  resetMobileSharing,
  resumeMobileSharing,
  revokeMobileSharingDevice,
  stopMobileSharing,
  type MobileSharingStatus,
} from '../api/mobile-sharing';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { copyText } from '../platform/clipboard';

export const MOBILE_SHARING_POLL_MS = 1_500;

type ConfirmAction = 'stop' | 'resume' | 'reset';

const stateLabels: Record<MobileSharingStatus['state'], string> = {
  unavailable: 'Unavailable',
  off: 'Off',
  inviting: 'Invitation open',
  pending: 'Approval pending',
  sharing: 'Sharing',
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

export function MobileSharingSettings({ active }: { active: boolean }) {
  const store = useStore();
  const [status, setStatus] = useState<MobileSharingStatus | null>(null);
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
        const next = await getMobileSharing(store.api, signal);
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
      if (!controller.signal.aborted) timer = setTimeout(() => void poll(), MOBILE_SHARING_POLL_MS);
    };
    void poll();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [active, load]);

  const invitationExpiresAt = status?.invitation?.expires_at;
  useEffect(() => {
    if (!active || !invitationExpiresAt) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [active, invitationExpiresAt]);

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
      const next = await createMobileSharingInvitation(store.api);
      if (activeRef.current) setStatus(next);
    } catch (cause) {
      if (activeRef.current) setError(errorMessage(cause));
    } finally {
      if (activeRef.current) setBusy('');
    }
  };

  const confirmedAction = (action: ConfirmAction) => {
    setConfirming(null);
    const operation =
      action === 'stop'
        ? () => stopMobileSharing(store.api)
        : action === 'resume'
          ? () => resumeMobileSharing(store.api)
          : () => resetMobileSharing(store.api);
    void mutate(action, operation);
  };

  if (loading && !status && !error)
    return (
      <section class="mobile-sharing" aria-label="Mobile sharing">
        <p role="status">Checking mobile sharing…</p>
      </section>
    );

  if (!status)
    return (
      <section class="mobile-sharing" aria-label="Mobile sharing">
        <div class="mobile-sharing-state is-unavailable">
          <strong>Mobile sharing unavailable</strong>
          <p>{error || 'The server did not provide mobile sharing status.'}</p>
          <button class="btn" type="button" disabled={loading} onClick={() => void load()}>
            {loading ? 'Checking…' : 'Try again'}
          </button>
        </div>
      </section>
    );

  const requests = status.pending_requests ?? [];
  const devices = status.devices ?? [];
  const isUnavailable = status.state === 'unavailable';
  const canStop = ['sharing', 'pending', 'inviting'].includes(status.state);
  const canReset = ['sharing', 'pending', 'inviting', 'stopped'].includes(status.state);

  return (
    <section class="mobile-sharing" aria-label="Mobile sharing">
      <div class={`mobile-sharing-state is-${status.state}`} role="status">
        <span>Mobile sharing</span>
        <strong>{stateLabels[status.state]}</strong>
      </div>
      <div class="mobile-sharing-authority">
        <strong>{status.authority.agent_name || 'This server'}</strong>
        <p>{status.authority.description}</p>
        <p>{status.authority.same_uid_note}</p>
      </div>
      {status.reason && <p class="mobile-sharing-reason">{status.reason}</p>}
      {error && (
        <p class="mobile-sharing-error" role="alert">
          {error}
        </p>
      )}

      {status.state === 'off' && (
        <div class="mobile-sharing-section">
          <h3>Connect a mobile device</h3>
          <p>Create a short-lived invitation, then scan it from the device you want to approve.</p>
          <button
            class="btn primary"
            type="button"
            disabled={Boolean(busy)}
            onClick={() => void createInvitation()}
          >
            {busy === 'invite' ? 'Creating…' : 'Connect a phone'}
          </button>
        </div>
      )}

      {status.invitation && (
        <div class="mobile-sharing-section mobile-sharing-invitation">
          <div>
            <h3>Mobile invitation</h3>
            <p class="mobile-sharing-countdown" aria-live="polite">
              {countdown(status.invitation.expires_at, now)}
            </p>
          </div>
          <img
            src={status.invitation.qr_data_url}
            alt="QR code for the mobile sharing invitation"
          />
          <div class="mobile-sharing-invite-actions">
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
                void mutate('cancel-invite', () => cancelMobileSharingInvitation(store.api))
              }
            >
              {busy === 'cancel-invite' ? 'Cancelling…' : 'Cancel invitation'}
            </button>
          </div>
          <p class="mobile-sharing-warning" role="note">
            Treat the copied link like a password until it expires. A request still requires the
            one-time code shown on the mobile device and your approval here.
          </p>
        </div>
      )}

      {status.state === 'inviting' && !status.invitation && (
        <div class="mobile-sharing-section">
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
              void mutate('cancel-invite', () => cancelMobileSharingInvitation(store.api))
            }
          >
            Cancel active invitation
          </button>
        </div>
      )}

      {requests.length > 0 && (
        <div class="mobile-sharing-section">
          <h3>Pending devices</h3>
          <div class="mobile-sharing-list">
            {requests.map((request) => {
              const code = codes[request.id] || '';
              return (
                <div class="mobile-sharing-card" key={request.id}>
                  <div>
                    <strong>{request.name || 'Unnamed mobile device'}</strong>
                    <small>Confirm the SAS code shown on that device.</small>
                  </div>
                  <label for={`mobile-sharing-code-${request.id}`}>One-time SAS code</label>
                  <input
                    id={`mobile-sharing-code-${request.id}`}
                    value={code}
                    inputMode="text"
                    autoComplete="one-time-code"
                    spellcheck={false}
                    disabled={Boolean(busy)}
                    onInput={(event) =>
                      setCodes({ ...codes, [request.id]: event.currentTarget.value.toUpperCase() })
                    }
                  />
                  <div class="mobile-sharing-card-actions">
                    <button
                      class="btn primary"
                      type="button"
                      disabled={Boolean(busy) || !code.trim()}
                      onClick={() =>
                        void mutate(`approve-${request.id}`, () =>
                          approveMobileSharingRequest(store.api, request.id, code.trim()),
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
                          denyMobileSharingRequest(store.api, request.id),
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
        <div class="mobile-sharing-section">
          <h3>Approved devices</h3>
          <div class="mobile-sharing-list">
            {devices.map((device) => (
              <div class="mobile-sharing-card mobile-sharing-device" key={device.id}>
                <div>
                  <strong>{device.name || 'Unnamed mobile device'}</strong>
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

      {!isUnavailable && (
        <div class="mobile-sharing-controls">
          {status.state === 'sharing' && (
            <button
              class="btn primary"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => void createInvitation()}
            >
              Connect another phone
            </button>
          )}
          {canStop && (
            <button
              class="btn"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => setConfirming('stop')}
            >
              Stop mobile sharing…
            </button>
          )}
          {status.state === 'stopped' && (
            <button
              class="btn primary"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => setConfirming('resume')}
            >
              Resume sharing…
            </button>
          )}
          {canReset && (
            <button
              class="btn danger"
              type="button"
              disabled={Boolean(busy)}
              onClick={() => setConfirming('reset')}
            >
              Forget all phones…
            </button>
          )}
        </div>
      )}

      {revokeTarget && (
        <div
          class="mobile-sharing-confirm"
          role="alertdialog"
          aria-labelledby="mobile-sharing-revoke-title"
        >
          <strong id="mobile-sharing-revoke-title">
            Revoke {revokeTarget.name || 'this device'}?
          </strong>
          <p>
            Revocation stops this device's future mobile-sharing connections only. It cannot undo
            actions already taken or retrieve information already disclosed.
          </p>
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
                  revokeMobileSharingDevice(store.api, target.id),
                );
              }}
            >
              Confirm revoke
            </button>
          </div>
        </div>
      )}

      {confirming && (
        <div
          class="mobile-sharing-confirm"
          role="alertdialog"
          aria-labelledby="mobile-sharing-confirm-title"
        >
          <strong id="mobile-sharing-confirm-title">
            {confirming === 'stop'
              ? 'Stop all mobile access?'
              : confirming === 'resume'
                ? 'Resume approved device access?'
                : 'Reset mobile sharing?'}
          </strong>
          <p>
            {confirming === 'stop'
              ? 'Approved devices will be unable to connect until sharing is resumed.'
              : confirming === 'resume'
                ? 'Previously approved devices will regain the same-UID authority described above.'
                : 'All invitations, pending requests, and approved devices will be removed.'}
          </p>
          <div>
            <button class="btn" type="button" onClick={() => setConfirming(null)}>
              Cancel
            </button>
            <button class="btn danger" type="button" onClick={() => confirmedAction(confirming)}>
              {confirming === 'stop'
                ? 'Confirm stop'
                : confirming === 'resume'
                  ? 'Confirm resume'
                  : 'Confirm reset'}
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
