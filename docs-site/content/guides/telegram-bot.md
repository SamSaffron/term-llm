---
title: "Telegram Bot"
weight: 8
description: "Run term-llm as a Telegram bot: create a bot, configure access control, and chat with your agent from any device."
kicker: "Messaging"
featured: true
next:
  label: Search
  url: /guides/search/
---

## What this gives you

A Telegram bot that runs your agent. You send a message from your phone or desktop; the bot replies with a streamed response. Sessions persist per chat: history carries over between messages until you `/reset`. Every conversation is stored in the same sessions DB as web and CLI interactions.

## Step 1: Create a bot with BotFather

1. Open Telegram and start a chat with [@BotFather](https://t.me/BotFather)
2. Send `/newbot` and follow the prompts (name + username)
3. Copy the token BotFather gives you — it looks like `123456789:ABCdef...`

## Step 2: Configure the token

The fastest path is the setup wizard:

```bash
term-llm serve telegram --setup
```

This prompts for your token and saves it to `config.yaml` under `serve.telegram`. Re-run with `--setup` any time to update credentials.

To configure manually, add to `~/.config/term-llm/config.yaml`:

```yaml
serve:
  telegram:
    token: "123456789:ABCdef..."
```

## Step 3: Restrict access

Without an allowlist, anyone who knows your bot's username can chat with it. You almost certainly want to restrict this.

You can allowlist by **numeric user ID** (more stable) or by **username**:

```yaml
serve:
  telegram:
    token: "123456789:ABCdef..."
    allowed_user_ids:
      - 987654321
    allowed_usernames:
      - yourusername
```

To find your numeric user ID, forward a message from yourself to [@userinfobot](https://t.me/userinfobot).

Messages from non-allowlisted users are silently ignored and logged.

## Step 4: Start the bot

```bash
term-llm serve telegram
```

With an agent and yolo mode (auto-approve tool calls):

```bash
term-llm serve telegram --agent jarvis --yolo
```

Combined with the web UI on the same process:

```bash
term-llm serve telegram web
```

## Bot commands

| Command | What it does |
|---------|-------------|
| `/start` | Show welcome message and command list |
| `/help`  | Same as `/start` |
| `/reset` | Clear conversation history and start a fresh session |
| `/status` | Show message count and last activity time for the current session |

## Configuration reference

All fields live under `serve.telegram` in `config.yaml`:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `token` | string | — | Bot token from BotFather. Required. |
| `allowed_user_ids` | list of int | — | Numeric Telegram user IDs allowed to use the bot. |
| `allowed_usernames` | list of string | — | Telegram usernames allowed to use the bot (without `@`). |
| `idle_timeout` | int (minutes) | — | Close idle sessions after this many minutes. |
| `interrupt_timeout` | int (seconds) | 3 | How long to wait for an in-flight response to stop before interrupting. |

## CLI flags

```bash
term-llm serve telegram \
  --agent jarvis \
  --provider anthropic \
  --yolo \
  --telegram-carryover-chars 4000
```

`--telegram-carryover-chars` (default `4000`): when a session is reset via `/reset`, this many characters of the previous conversation are carried into the new session as context. Set to `0` to start completely fresh on each reset.

## Set default platforms in config

If you always run the same combination, set it in `config.yaml` so you can just run `term-llm serve`:

```yaml
serve:
  platforms:
    - telegram
    - web
```

`term-llm serve` with no arguments reads from `serve.platforms`.

## Related pages

- [Notifications](/guides/notifications/) — send one-off Telegram messages via `term-llm notify`
- [Web UI and API](/guides/web-ui-and-api/) — run the browser UI alongside the bot
- [Agents](/guides/agents/) — configure which agent handles bot conversations
