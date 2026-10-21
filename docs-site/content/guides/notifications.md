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

## When to use it

Notifications are useful for:

- build and deploy completion
- long-running jobs finishing or failing
- lightweight alerts from scripts and cron jobs

## Related pages

- [Jobs](/guides/job-runner/)
- [Web UI and API](/guides/web-ui-and-api/)
