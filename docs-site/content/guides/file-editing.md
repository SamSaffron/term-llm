---
title: "File editing"
weight: 5
description: "Edit files with natural-language instructions, targeted ranges, and multiple diff formats."
kicker: "Editing"
source_readme_heading: "File Editing"
next:
  label: Autonomous loops
  url: /guides/autonomous-loops/
---
Edit files using natural language instructions:

```bash
term-llm edit "add error handling" --file main.go
term-llm edit "refactor to use interfaces" --file "*.go"
term-llm edit "fix the bug" --file utils.go:45-60     # only lines 45-60
term-llm edit "use the API" -f main.go -c api/client.go  # with context files
```

### Edit Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--file` | `-f` | File(s) to edit (required, supports globs) |
| `--context` | `-c` | Read-only reference file(s) (supports globs, 'clipboard') |
| `--dry-run` | | Preview changes without applying |
| `--provider` | | Override provider (e.g., `openai:gpt-5.2-codex`) |
| `--per-edit` | | Prompt for each edit separately |
| `--debug` | `-d` | Show debug information |

### Context Files

Use `--context`/`-c` to include reference files that inform the edit but won't be modified:

```bash
term-llm edit "refactor to use the client" -f handler.go -c api/client.go -c types.go
```

Context files are shown to the AI as read-only references. This is useful when your edit depends on types, interfaces, or patterns defined elsewhere.

You can also pipe stdin as context, which is handy for git diffs:

```bash
git diff | term-llm edit "apply these changes" -f main.go
git show HEAD~1 | term-llm edit "undo this change" -f handler.go
```

### Line Range Syntax

Both `edit` and `ask` support line range syntax to focus on specific parts of a file:

```bash
# Edit specific lines
term-llm edit "fix this" --file main.go:11-22    # lines 11 to 22
term-llm edit "fix this" --file main.go:11-      # line 11 to end
term-llm edit "fix this" --file main.go:-22      # start to line 22

# Ask about specific lines
term-llm ask -f main.go:50-100 "explain this function"
```

### Diff Format

term-llm supports two edit strategies:

| Format | Description | Best For |
|--------|-------------|----------|
| `replace` | Multiple parallel find/replace tool calls | All models (default) |
| `udiff` | Single unified diff with elision support | Explicit opt-in for large refactors |

The `udiff` format uses unified diff syntax with `-...` elision to efficiently replace large code blocks without listing every line:

```diff
--- file.go
+++ file.go
@@ func BigFunction @@
-func BigFunction() error {
-...
-}
+func BigFunction() error {
+    return newImpl()
+}
```

Configure in `~/.config/term-llm/config.yaml`:

```yaml
edit:
  diff_format: auto  # auto, udiff, or replace
```

- `auto` (default): Uses `replace` for all models, including Codex
- `udiff`: Always use unified diff format
- `replace`: Always use multiple find/replace calls

Use `--diff-format udiff` or `--diff-format replace` to override the configuration for one edit.
