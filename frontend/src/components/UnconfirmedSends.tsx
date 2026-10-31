import { useEffect, useId, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { attachmentIconName } from '../domain/attachments';
import type { Attachment } from '../domain/types';
import type { PendingIntent } from '../platform/storage';
import { Icon } from './Icon';

/** Attachment chips shown before the rest collapse into a count. */
const PREVIEW_ATTACHMENTS = 2;

// Focusing the message box on a touch-only device would raise the on-screen
// keyboard, so dismissal only moves focus there for keyboard and pointer users.
// Same check as ChipPicker and the composer's return key.
function touchOnly(): boolean {
  return Boolean(globalThis.matchMedia?.('(hover: none) and (pointer: coarse)').matches);
}

function attachmentIcon(attachment: Attachment) {
  return (attachment.type || '').startsWith('image/')
    ? 'image'
    : attachmentIconName(attachment.type, attachment.name);
}

/**
 * New-chat runtime IDs have no server/sidebar owner after a reload, so their
 * unconfirmed sends stay listed above whichever conversation is open.
 */
export function LocalUnconfirmedSends({ standalone = false }: { standalone?: boolean }) {
  const store = useStore();
  const active = store.activeSessionId.value;
  const owners = Object.entries(store.pendingIntents.value).flatMap(([ownerId, intents]) => {
    if (!ownerId.startsWith('draft_') || ownerId === active) return [];
    const unconfirmed = intents.filter((intent) => intent.state === 'unconfirmed');
    return unconfirmed.length ? [{ ownerId, intents: unconfirmed }] : [];
  });
  if (!owners.length) return null;
  const section = (
    <section class="unconfirmed-sends" aria-label="Unconfirmed new conversations">
      {owners.map(({ ownerId, intents }) => (
        <UnconfirmedSendNotice key={ownerId} ownerId={ownerId} intents={intents} />
      ))}
    </section>
  );
  // A delegated child view has no composer, but the warning keeps its place.
  return standalone ? (
    <div class="composer">
      <div class="composer-inner">{section}</div>
    </div>
  ) : (
    section
  );
}

function UnconfirmedPreview({ intent }: { intent: PendingIntent }) {
  const attachments = intent.attachments || [];
  const hidden = attachments.length - PREVIEW_ATTACHMENTS;
  return (
    <>
      {intent.content && <p class="unconfirmed-send-preview">{intent.content}</p>}
      {attachments.length > 0 && (
        <ul class="unconfirmed-send-attachments" aria-label="Unconfirmed attachments">
          {attachments.slice(0, PREVIEW_ATTACHMENTS).map((attachment, index) => (
            <li
              key={attachment.id || index}
              class="unconfirmed-send-attachment"
              title={attachment.name}
            >
              <Icon name={attachmentIcon(attachment)} />
              <span>{attachment.name}</span>
            </li>
          ))}
          {hidden > 0 && <li class="unconfirmed-send-attachment is-count">+{hidden} more</li>}
        </ul>
      )}
    </>
  );
}

/**
 * A send whose outcome is unknown may already be running. Checking status is
 * read-only, and dismissal needs an explicit duplicate-work acknowledgement;
 * neither resends nor cancels anything.
 *
 * The acknowledgement replaces the actions inside the same card, so it never
 * pushes the composer away. Its safe choice takes the opener's place under the
 * finger, so a repeated tap keeps the warning instead of dismissing it.
 */
export function UnconfirmedSendNotice({
  ownerId,
  intents,
}: {
  ownerId: string;
  /** Previews for a send that has no conversation to show it in. */
  intents?: PendingIntent[];
}) {
  const store = useStore();
  // Keyed by owner: switching conversations reuses this notice, and another
  // send's warning must never inherit an acknowledgement it did not get.
  const [confirmingOwner, setConfirmingOwner] = useState<string | null>(null);
  const confirming = confirmingOwner === ownerId;
  const setConfirming = (open: boolean) => setConfirmingOwner(open ? ownerId : null);
  const [checking, setChecking] = useState(false);
  const mounted = useRef(true);
  const opener = useRef<HTMLButtonElement>(null);
  const keep = useRef<HTMLButtonElement>(null);
  const restoreFocus = useRef<string | null>(null);
  const copyId = useId();
  useEffect(
    () => () => {
      mounted.current = false;
    },
    [],
  );
  // Returning to the earlier conversation must not reopen a stale acknowledgement.
  useEffect(() => setConfirmingOwner(null), [ownerId]);
  // Move focus into the acknowledgement when it opens and back to its opener
  // when it closes, so replacing the buttons never drops focus. A conversation
  // switch closes it without pulling focus back here.
  useEffect(() => {
    if (confirming) {
      restoreFocus.current = ownerId;
      keep.current?.focus();
    } else if (restoreFocus.current) {
      if (restoreFocus.current === ownerId) opener.current?.focus();
      restoreFocus.current = null;
    }
  }, [confirming, ownerId]);
  const checkStatus = () => {
    if (checking) return;
    setChecking(true);
    void store.checkSendStatus().finally(() => {
      if (mounted.current) setChecking(false);
    });
  };
  const dismiss = () => {
    store.runEngine.dismissUnconfirmed(ownerId);
    if (!touchOnly()) document.getElementById('promptInput')?.focus();
  };
  return (
    <div
      class={`unconfirmed-send${intents ? ' has-preview' : ''}${confirming ? ' is-confirming' : ''}`}
      data-pending-owner={ownerId}
    >
      <div class="unconfirmed-send-summary">
        <Icon name="alert-circle" class="unconfirmed-send-mark" />
        <div class="unconfirmed-send-content">
          {intents && <strong class="unconfirmed-send-title">Unconfirmed new conversation</strong>}
          {intents?.map((intent) => (
            <UnconfirmedPreview key={intent.clientMessageId} intent={intent} />
          ))}
          {confirming ? (
            <p id={copyId} class="unconfirmed-send-copy">
              This message may still be running; dismissing does not cancel it. Sending it again
              could duplicate the work.
            </p>
          ) : (
            <p class="unconfirmed-send-copy" role="status">
              Could not confirm whether this was sent. It may still be running.
            </p>
          )}
        </div>
      </div>
      {confirming ? (
        <div
          class="unconfirmed-send-actions"
          role="group"
          aria-label="Confirm warning dismissal"
          aria-describedby={copyId}
          onKeyDown={(event) => {
            if (event.key !== 'Escape') return;
            // Escape keeps the warning without also closing a surrounding panel.
            event.preventDefault();
            event.stopPropagation();
            setConfirming(false);
          }}
        >
          <button type="button" class="btn unconfirmed-send-danger" onClick={dismiss}>
            Dismiss anyway
          </button>
          <button ref={keep} type="button" class="btn" onClick={() => setConfirming(false)}>
            Keep warning
          </button>
        </div>
      ) : (
        <div class="unconfirmed-send-actions">
          <button
            type="button"
            class="btn unconfirmed-send-check"
            aria-busy={checking || undefined}
            onClick={checkStatus}
          >
            Check status
          </button>
          <button ref={opener} type="button" class="btn" onClick={() => setConfirming(true)}>
            Dismiss warning
          </button>
        </div>
      )}
    </div>
  );
}
