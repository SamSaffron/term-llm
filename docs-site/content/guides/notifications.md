---
title: "Notifications"
weight: 11
description: "Send notifications through Telegram or web push from the command line."
kicker: "Notify"
---
## Broadcast notifications

```bash
term-llm notify "build finished"
```

The `notify` command broadcasts to usable configured destinations. Telegram requires both a configured bot token and `--chat-id`; web push requires VAPID keys and saved browser subscriptions. A configured key alone does not mean there is a recipient.

Examples:

```bash
term-llm notify --chat-id 12345 "deploy complete"
term-llm notify telegram --chat-id 12345 "test"
term-llm notify web "test"
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

## Web push

Web push notifications require VAPID keys under `serve.web_push` and browser push subscriptions saved by the web runtime. Run the notification command with the same configuration/data directories as that runtime. Users must grant notification permission and subscribe in the browser; keys alone do not create a subscription.

```bash
term-llm notify web "job failed"
```

## When to use it

Notifications are useful for:

- build and deploy completion
- long-running jobs finishing or failing
- lightweight alerts from scripts and cron jobs

## Related pages

- [Jobs](/guides/job-runner/)
- [Web UI and API](/guides/web-ui-and-api/)
