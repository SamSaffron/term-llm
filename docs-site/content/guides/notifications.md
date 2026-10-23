---
title: "Notifications"
weight: 11
description: "Send notifications through Telegram, native app push, or browser push from the command line."
kicker: "Notify"
---
## Broadcast notifications

```bash
term-llm notify "build finished"
```

The plain `notify MESSAGE` command broadcasts to usable configured destinations: Telegram plus **all app and browser push subscriptions**. Telegram requires both a configured bot token and `--chat-id`; push requires VAPID keys and saved subscriptions. A configured key alone does not mean there is a recipient.

Examples:

```bash
term-llm notify --chat-id 12345 "deploy complete"
term-llm notify telegram --chat-id 12345 "test"
term-llm notify app "test"   # native apps only
term-llm notify web "test"   # browsers only
```

## Telegram

To send Telegram notifications you need:

- `serve.telegram.token` configured
- a target `--chat-id`

```bash
term-llm notify --chat-id 12345 "deploy complete"
term-llm notify telegram --chat-id 12345 --parse-mode Markdown "*done*"
```

`--parse-mode` accepts `Markdown` (the default) or `HTML`, case-insensitively. `MarkdownV2` is not supported by this command.

## App and browser push

Both native app and browser notifications use Web Push with VAPID keys under `serve.web_push`. No separate APNs configuration is needed for app delivery. Run `term-llm serve web` to auto-generate VAPID keys, then grant notification permission and enroll the app or browser. Keys alone do not create a subscription.

```bash
term-llm notify app "job finished"  # subscribed native apps only
term-llm notify web "job failed"    # subscribed browsers only
term-llm notify "job finished"      # all push clients, plus Telegram when configured with --chat-id
```

`notify app` and `notify web` select subscriptions by their saved client kind (`app` or `browser`), not by the push endpoint hostname. They do not send to Telegram. Legacy records without a client kind count as browsers, including older app enrollments. Re-enroll notifications from the updated app to update its existing subscription to `app`; until then, it receives broadcasts and browser-targeted messages, not app-targeted messages.

### Server and session-store scope

Targeting applies to **all matching subscriptions in the configured session store**, not a selected conversation, user, or device. Run `notify` with the same configuration, VAPID keys, and session database as the server where clients enrolled (including the same configuration/data directories or `--session-db` override). Enrolling with a different server or store does not make that subscription available to this command. Stale subscriptions are skipped. If no subscriptions match `notify app` or `notify web`, the command prints `no push subscriptions found` and succeeds. Configuration, session-store, or delivery failures return an error (nonzero exit status), even if some matching subscriptions were sent successfully.

## Automatic completion alerts

Automatic alerts are different from the explicit broadcasts above. An eligible
human turn captures **one enrolled device and the window that sent it**. The
server never broadcasts an automatic completion to other subscriptions. A newer
accepted human turn supersedes an older pending notification, even when the new
request comes from a terminal or has no notification destination. Incorporated
human steering transfers the target; merely queued or withdrawn steering does not.

A response ending is not necessarily the end of the user's task. Client-tool
handoffs and explicitly awaited child generations keep the logical turn open
until the final synthesis is durable. For example, `media_list → media_add → final`
followed by `media_play → final` has two eligible completions, not five. Detached
subagents, scheduled jobs, goal ticks and synthetic host notices do not acquire
OS-notification permission. `notify_when_done` wakes an agent loop; it is not an
instruction to alert every device. Use an explicit `notify` command or an explicitly
configured notification action when an alert is wanted. Stop/cancellation is silent;
a permanent failure may produce one failed-turn alert.

### Around is not read

An owning window with fresh interaction, policy focus and visibility defers its
OS alert—even when it is viewing another conversation. Visibility alone, a restored
focused page, or an unrelated tab is not attendance. Presence leases expire after
absence, suspension or lost heartbeats; an unread, unexpired result can then alert.
This deferral does **not** mark the result read.

Read suppression uses the same exact store/session/attention marker acknowledged
by the browser or explicit Hub mark-read action. Automatic acknowledgement still
requires the selected transcript's final content, recent presence, focus, tail
position, no covering modal and the existing dwell. Reading an older completion
cannot clear newer alerts, and marking an item unread does not generate another
notification. Clicking a browser notification targets its original window; if it
has gone, a new window opens rather than hijacking an arbitrary sibling.

### Disable, cleanup and updates

Browser Disable is a durable site preference, independent of the browser's OS
permission. Reload, sign-out, focus and network repair do not undo it; only Enable
clears the preference. Failed server unsubscribe requests remain queued for
retry after authentication/network recovery. Other windows observe the same
preference. There is necessarily a race with an alert already accepted by an
external push service. Because browser subscriptions use `userVisibleOnly`, every
already-delivered push must show or replace a notification. Read, opted-out,
invalid and over-capacity deliveries are displayed silently and then closed;
local suppression cannot guarantee that the OS never briefly presents a card.

Automatic alerts require the current server, frontend/service worker, and a
window-aware native bridge where applicable. Older clients can still chat and
receive supported manual notifications, but cannot acquire automatic completion
eligibility by supplying only a subscription ID. Reload already-open pages to
activate the new worker; an old offline page cannot be retroactively upgraded.
The native app's local toggle is not equivalent to revoking notification permission
in OS Settings. Offline or unauthenticated server cleanup remains pending.

### Delivery guarantees

Final logical-turn state and its outbox candidate are committed together. Delivery
uses fenced claims, stable event-scoped topics, exact read/supersession checks and
an absolute 24-hour expiry. Browser retries replace the same event-tagged card
without requesting another alert; a durably recorded replay of a dismissed card
is shown silently and then closed, including after worker restart. Browser
replay/read protection retains up to 512 unexpired event IDs and 512 exact read
watermarks per scope over 24 hours. Saturation suppresses retained cards rather
than evicting unexpired protection; invalid envelopes or unavailable receiver
storage use a safe generic show-and-close fallback. These defenses do not make
external push acceptance exactly-once:
an accepted send with a lost acknowledgement is still ambiguous. Native physical
device presentation and notification-service-extension behavior require device
verification, not just server or simulated receiver tests.

## When to use it

Notifications are useful for:

- build and deploy completion
- long-running jobs finishing or failing
- lightweight alerts from scripts and cron jobs

## Related pages

- [Jobs](/guides/job-runner/)
- [Web UI and API](/guides/web-ui-and-api/)
