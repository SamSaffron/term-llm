---
title: "term-llm"
description: "Review code, edit files, research, and automate work with your choice of models. A powerful terminal tool and a complete browser workspace, in one self-hosted AI runtime."
headline: "AI in your terminal."
headline_second: "A workspace in your browser."
intro: "Review code, edit files, research, and automate repeat work with the models you choose: hosted APIs, subscriptions, or local."
web:
  eyebrow: "The browser workspace"
  title: "More than a chat window."
  description: "Projects, saved conversations, Git worktrees, live diffs, and a terminal, served from your own machine. Extend it with your own widget apps."
  command: "term-llm serve web"
  caption: "Real interface. Illustrative conversations and activity."
  slides:
    - id: review
      label: Review
      caption: "Review code in an isolated worktree, with the diff beside the conversation."
      alt: "Code review in the fix-retry-backoff worktree with an expanded retry.go diff."
      url: "/guides/web-ui-and-api/#live-diff-sidebar"
      link: "Explore the workspace"
    - id: hub
      label: Hub
      caption: "Pick up finished agent work across your machines, all in one place."
      alt: "Hub dashboard with two completed sessions ready for review above MacBook Pro, Build server and Research box nodes."
      url: "/guides/hub/"
      link: "Explore Hub"
    - id: worktrees
      label: Worktrees
      caption: "Keep parallel work separate. Pick a checkout or create a new worktree."
      alt: "Worktree picker showing the root checkout and three named isolated worktrees."
      url: "/guides/web-ui-and-api/#project-aware-responses-and-worktrees"
      link: "Explore worktrees"
    - id: agents
      label: Agents
      caption: "Give specialists focused tasks and follow their contributions in one conversation."
      alt: "Developer conversation showing completed codebase and reviewer agent tasks."
      url: "/guides/agents/"
      link: "Meet the agents"
    - id: shell
      label: Shell
      caption: "Run commands in the same worktree, with a real terminal docked beside chat."
      alt: "Browser terminal beside a conversation, showing illustrative passing regression tests."
      url: "/guides/web-ui-and-api/#interactive-shell"
      link: "Explore the shell"
    - id: terminal
      label: Terminal
      image_theme: dark
      caption: "Work with the developer agent in your terminal, with inline diffs and Playwright MCP enabled."
      alt: "term-llm chat @developer --mcp playwright showing a retry.go edit, an inline diff, and the MCP status in the terminal footer."
      url: "/guides/agents/"
      link: "Explore terminal agents"
why:
  title: "Use any model, for any job."
  items:
    - title: "Any provider, or local"
      text: "Hosted APIs, supported subscriptions, or local models through Ollama and LM Studio. Each agent can use a different model."
      url: "/getting-started/providers-and-setup/"
      link: "Connection options"
    - title: "Agents for repeat work"
      text: "Start from the built-in reviewer, developer, and researcher, then change their instructions, tools, permissions, and models."
      url: "/guides/agents/"
      link: "Built-in agents"
    - title: "More than chat"
      text: "Scheduled jobs, MCP servers, an HTTP API, Hub across machines, and image, audio, and video generation."
      url: "/guides/job-runner/"
      link: "Automation"
terminal:
  eyebrow: "At home in your terminal"
  title: "Describe it, then pick a command."
  description: "term-llm exec suggests commands with explanations and runs the one you pick. Not quite right? Refine the request."
  command: 'term-llm exec "list files"'
  options:
    - command: "ls"
      explanation: "List files and directories in the current directory."
    - command: "ls -la"
      explanation: "Include hidden files, permissions, ownership, sizes, and timestamps."
    - command: "ls -lh"
      explanation: "Show detailed metadata with human-readable file sizes."
    - command: "ls -lt"
      explanation: "Sort by modification time, newest first."
    - command: "find . -maxdepth 1 -type f"
      explanation: "List only regular files in the current directory, including hidden files."
  note: "↑/↓ to choose, i for info, Enter to run. Illustrative suggestions."
workflows:
  title: "Things to try first."
  items:
    - title: "Understand an unfamiliar codebase"
      text: "Trace behavior across files and ask follow-up questions without losing the thread."
      command: 'term-llm chat @codebase'
      url: "/guides/agents/"
    - title: "Review before you commit"
      text: "Ask a git-aware, read-only reviewer to look for bugs in your staged changes."
      command: 'term-llm ask @reviewer "review the staged changes"'
      url: "/guides/agents/#built-in-agents"
    - title: "Implement a fix"
      text: "A developer agent reads code, makes changes, and runs checks, within the permissions you set."
      command: 'term-llm chat @developer'
      url: "/guides/file-editing/"
    - title: "Put your diff to work"
      text: "Pipe in the context you already have."
      command: 'git diff --staged | term-llm ask @commit-message'
      url: "/guides/usage/"
control:
  title: "You decide what agents can do."
  items:
    - title: "Approval modes"
      text: "Approve actions yourself, or let a Guardian model review them in auto mode."
      url: "/reference/built-in-tools/#approval-modes"
    - title: "Know where context goes"
      text: "Hosted models see what you send. To stay fully local, use local models and check what your tools and search call."
      url: "/getting-started/providers-and-setup/#use-a-local-model"
    - title: "Usage tracking"
      text: "Tokens and estimated cost, by provider and model."
      url: "/reference/usage-tracking/"
install:
  title: "Install and ask a first question."
  description: "Every release includes the browser workspace. There is nothing else to install."
  mac_download: "https://term-llm.com/mac/download"
  homebrew: "brew install term-llm/tap/term-llm"
  shell: "curl -fsSL https://raw.githubusercontent.com/term-llm/term-llm/main/install.sh | sh"
  powershell: "irm https://raw.githubusercontent.com/term-llm/term-llm/main/install.ps1 | iex"
  trial: 'term-llm ask "Explain git rebase in three sentences"'
  caveat: "Set OPENROUTER_API_KEY to use OpenRouter’s free models, or pick another provider during setup. Using the Mac app? Ask in its window instead."
community:
  eyebrow: "Community"
  title: "Blog and forum"
  items:
    - title: "Blog"
      icon: "blog"
      text: "Releases and write-ups on how term-llm works."
      url: "https://blog.term-llm.com/"
    - title: "Discuss"
      icon: "discuss"
      text: "Questions, setups, and ideas."
      url: "https://discuss.term-llm.com/"
---
