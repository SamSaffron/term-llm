---
title: "Architecture overview"
weight: 1
featured: true
description: "The core pieces of term-llm and how they relate: runtimes, providers, sessions, tools, MCP, agents, skills, and jobs."
kicker: "Mental model"
---
## The runtime shape

term-llm is a terminal-first AI runtime with a few distinct surfaces:

- **CLI commands** like `exec`, `ask`, `chat`, `edit`, and `image`
- **Serve modes** for web and jobs
- **Persistent local state** for sessions, config, logs, agents, skills, and MCP servers

The important thing is that these are not separate products bolted together. They share configuration, provider routing, tool execution, and session state.

## Providers are the model layer

Providers are the LLM backends: Anthropic, OpenAI, ChatGPT, xAI, OpenRouter, Gemini, local OpenAI-compatible servers, and others.

They answer slightly different questions:

- what model should be used?
- how is authentication done?
- does the provider support native search, image generation, or tool calling?

The CLI can override provider or model per command, while config sets the defaults.

## Sessions are local persistence

`chat` sessions are stored locally in SQLite. That gives you resumable conversation history, inspection, searching, exporting, and retention controls.

That statefulness is local and explicit rather than magical. The runtime can be told not to use sessions, or to use a different session database path.

## Memory is long-term context

Memory sits beside sessions rather than replacing them.

- **Sessions** preserve the transcript of a conversation.
- **Memory fragments** preserve durable facts mined from many conversations.
- **Insights** preserve behavioral rules that should influence future runs.

That gives term-llm a way to accumulate reusable context without pretending every prior token is still in the prompt. The runtime can search memory when needed, while keeping the live session focused on the current task.

## Tools and MCP extend the model

Built-in tools handle common filesystem and shell tasks. MCP servers extend that with external capabilities such as browser automation, GitHub, or custom APIs.

In practice that means the model is not just generating text — it can interrogate files, run commands, call external tools, and then keep going.

## Jobs turn it into a runtime, not just a CLI

The jobs platform lets term-llm run work on a schedule, after a delay, or on manual trigger.

That matters because it moves the system beyond interactive use:

- recurring agent runs
- background program execution
- inspectable run history and events
- controllable retention and cancellation

Once jobs are in the picture, term-llm starts looking less like a terminal helper and more like an automation substrate.

## Agents and skills are different things

- **Agents** are named configuration bundles. They can choose provider, model, tools, MCP, search, and instructions.
- **Skills** are instruction bundles. They add portable expertise without changing the whole runtime configuration.

That distinction matters because it stops everything from becoming one giant system prompt with no boundaries.

## Jobs and loops are how it becomes a workflow runtime

Jobs make scheduled or delayed execution possible. Loops let an agent keep iterating until some completion condition is reached. Combined with tools and persistent filesystem state, term-llm starts acting less like a chat wrapper and more like an automation runtime.
