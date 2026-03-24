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

The `notify` command sends to all configured notification platforms.

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

## Web push

Web push notifications require configured VAPID keys under `serve.web_push`.

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
