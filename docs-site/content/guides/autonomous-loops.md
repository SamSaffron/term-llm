---
title: "Autonomous loops"
weight: 6
description: "Run an agent in a loop until a done condition is met."
kicker: "Automation"
source_readme_heading: "Autonomous Loops"
next:
  label: Job runner
  url: /guides/job-runner/
---
Run an agent in a loop until a completion condition is met. State persists in the filesystem—the agent reads/writes files to track progress, and each iteration starts with fresh context.

```bash
# Run until tests pass
term-llm loop --done "go test ./..." --tools all "fix the failing tests"

# Run until file contains marker
term-llm loop --done-file TODO.md:COMPLETE \
  "Implement features in {{TODO.md}}. Mark COMPLETE when done."
```

### Loop Flags

| Flag | Description |
|------|-------------|
| `--done "cmd"` | Exit when command returns 0 |
| `--done-file FILE:TEXT` | Exit when file contains TEXT |
| `--max N` | Maximum iterations (0 = unlimited) |
| `--history N` | Inject last N iteration summaries to avoid repeating mistakes |
| `--yolo` | Auto-approve all tool operations (for unattended runs) |

All standard flags work: `--tools`, `--mcp`, `--agent`, `--provider`, `--search`, etc.

### File Expansion

Use `{{file}}` in your prompt to inline file contents. Files are re-read each iteration, so agents can update them for inter-iteration state:

```bash
term-llm loop --done "npm test" \
  "Implement the spec in {{SPEC.md}}. Track progress in {{TODO.md}}."
```

### History

Use `--history N` to inject summaries of previous iterations. This helps the agent avoid repeating failed approaches:

```bash
term-llm loop --done "go test" --history 3 --tools all \
  "Fix the tests. Don't repeat failed approaches."
```

Each iteration summary includes tools used and a truncated output.

### Examples

```bash
# Migration: run until no class components remain
term-llm loop --done "! grep -r 'React.Component' src/" --tools all \
  "Convert class components to hooks. One file at a time. Run tests after each."

# Research: run until conclusion written
term-llm loop --done-file RESEARCH.md:"## Conclusion" --tools read,write --search --max 20 \
  "Research WebGPU compute shaders. Current progress: {{RESEARCH.md}}. Write a Conclusion section when done."

# With an agent and iteration cap
term-llm loop @developer --done "make build" --max 20 --yolo \
  "Implement the feature described in {{SPEC.md}}"
```
