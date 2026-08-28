---
title: "Herdr"
weight: 10
description: "Run term-llm chat in Herdr and see its live lifecycle state."
kicker: "Integrations"
next:
  label: Shell integration
  url: /guides/shell-integration/
---

[Herdr](https://herdr.dev) keeps terminal panes and coding agents running in
the background. When `term-llm chat` runs inside a Herdr pane, term-llm
automatically reports its lifecycle state to that pane:

- **Idle** when the chat is ready for a prompt.
- **Working** while it is generating a response, running a direct shell
  command, or changing a worktree.
- **Blocked** when it needs an approval or an answer through `ask_user`.

Start Herdr in the project, then open `term-llm chat` in one of its panes:

```bash
herdr
term-llm chat @developer
```

Herdr injects the connection details into every managed pane. No term-llm or
Herdr configuration is required. Verify the state from another pane with:

```bash
herdr agent list
herdr agent get <pane-id>
```

Term-llm also reports its persisted chat-session ID, so it is visible through
Herdr's pane and agent APIs. Herdr does not yet have a built-in term-llm
restorer, so reopening a restored pane still uses term-llm's normal resume
command:

```bash
term-llm chat --resume=<session-id>
```

Outside a Herdr-managed pane the integration is inert: term-llm does not look
up a socket, alter your environment, or require Herdr to be installed.
