# Web UI reliability operations

## Safe diagnostics

The browser store counts discarded stale status results, rejected stale stream callbacks,
supervisor retries/recoveries, stream inactivity timeouts, review-queue validation failures,
interaction reconciliations, and browser-storage failures. WebRTC diagnostics expose only
aggregate active/reserved/rejected admission counts through `/v1/capabilities`; signaling
credentials, attachment contents, prompts, and approval details are never included.

When diagnosing locally, correlate by truncated session/response/operation IDs. Do not export
raw prompts, tokens, file contents, signaling tokens, or attachment data URLs.

## Operational thresholds

Deployment-specific monitoring should alert when any of these conditions persists:

- a stream supervisor retries beyond five attempts or for longer than two minutes;
- signaling timeout or WebRTC admission-rejection rates exceed 10% for five minutes;
- an unresolved approval or ask-user request is older than 30 minutes while its run remains active;
- browser storage migration/persistence failures recur in the same session;
- stale status or stream callback discards rise continuously rather than appearing as isolated races.

## Idempotency replay boundary

Response creation replays `Idempotency-Key` only for stateful streaming runs, using the durable
session plus `client_message_id` ownership contract. Completed run events remain replayable for
five minutes by default. Interjection identities are retained for the live response/runtime and
persisted pending-interjection reload window; cancellation remains permanently scoped to its exact
response ID and repeated late cancellation returns that run's current terminal state. Clients must
reconcile after these windows rather than assuming an unbounded global replay cache.

## Storage migration and rollback

Drafts, queued review comments, pending intents, and attention markers are independently keyed
records. New readers retain compatibility reads for the previous aggregate format, but all new
writes use record keys. Rollback must **not** delete or rewrite the new record keys. An older build
may continue reading its aggregate data while a subsequent upgrade resumes additive migration.
Deletion tombstones must also be retained during that compatibility window so deleted legacy
review comments and drafts are not resurrected.

## Accessibility smoke checklist

Run this checklist on a representative desktop and mobile build after interaction changes:

- **NVDA/Firefox or Chrome:** dialog names are announced; Tab and Shift+Tab remain trapped;
  Escape follows the documented neutral-dismiss policy; focus returns to the opener.
- **VoiceOver/Safari:** mobile sidebar, diff drawer, plan sheet, and run center announce their
  labels and expanded state; background chat controls cannot be activated while open.
- **TalkBack/Chrome:** drawer actions remain above the visual keyboard and safe-area inset;
  explicit close controls remain reachable; menus announce menu items and support sequential
  navigation.
- **Media:** closing a lightbox pauses video, clears playback, restores focus, and revokes only
  object URLs owned by the lightbox.
- **Nested surfaces:** opening an approval or media surface above another overlay makes the lower
  surface inert until the top surface closes.
