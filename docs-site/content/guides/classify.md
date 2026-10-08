---
title: "Classification"
weight: 4
description: "Route intents, run parallel safety checks, and score text, structured state, or images with TypeSafe, OpenAI Decisions, or Cloudflare Clef."
kicker: "Typed decisions"
---

`term-llm classify` asks typed questions about a piece of state and returns decisions, not generated prose. Mix **choice** (pick an option), **score** (a probability-weighted level on an ordered rubric), and **noul** (the probability that a statement is true) in a single request.

Classification has its own providers, separate from chat LLM providers, and one provider-neutral question format based on [TypeSafe System One](https://docs.typesafe.ai/). The same question files work with every provider. Only `classify` and `classify models` call a classification provider, unless you explicitly enable the [classify-backed Guardian](#guardian-backend) or the live voice router's classify control plane.

## Providers

| Provider (`-p`) | Service | Models | API key fallback | Images |
| --- | --- | --- | --- | --- |
| `typesafe` (default) | [TypeSafe System One](https://docs.typesafe.ai/) | `jev-latest` | `TYPESAFE_API_KEY` | No |
| `openai` | [OpenAI Decisions API](https://developers.openai.com/api/docs/guides/decisions) | `gpt-6-luna` | `OPENAI_API_KEY` | Yes |
| `cloudflare` | [Clef on Cloudflare Workers AI](https://developers.cloudflare.com/workers-ai/models/clef/) | `clef`, `clef-flash` | `CLOUDFLARE_API_TOKEN`, then `CLOUDFLARE_AUTH_TOKEN` | Yes |

All three are built in and work without a config file: set the API key variable (and `CLOUDFLARE_ACCOUNT_ID` for Cloudflare). Select one with `--provider/-p`, or set `classify.default_provider`. To configure a provider, set fields under `classify.providers.<name>`; keys can use the [secret-management conventions](/guides/secret-management/). Only the selected provider's credentials are resolved, when a classify command runs or eagerly when a classify-backed Guardian starts.

You can also define named aliases. An alias must declare `type: typesafe`, `type: openai`, or `type: cloudflare`, and inherits that type's model, base URL, and timeout defaults:

```yaml
classify:
  default_provider: work
  providers:
    work:
      type: typesafe
      api_key: ${WORK_TYPESAFE_API_KEY}
```

```bash
term-llm classify models -p work
term-llm classify "Production is down" -p work --type noul --question "Is this urgent?"
```

**Environment keys stay on the default endpoint.** The fallback variables in the table are sent only to the provider type's default endpoint. A provider with its own `base_url`, or a run with `--base-url` pointing elsewhere, sends only its configured `api_key`, so redirecting a provider can never forward your primary credential to another host.

Use `--model`, `--base-url`, and `--timeout 5s` to override configuration for one run. The timeout covers the whole HTTP operation, including retries. Transient HTTP errors (408, 425, 429, and 5xx including 529) are retried up to twice; server retry delays are respected, and if a delay exceeds the two-second backoff cap or the remaining timeout, the last error is returned instead of retrying early. Redirects are not followed.

### TypeSafe

```yaml
classify:
  default_provider: typesafe
  providers:
    typesafe:
      type: typesafe # Optional for the built-in key
      api_key: ${TYPESAFE_API_KEY}
      model: jev-latest
      base_url: https://api.typesafe.ai
      timeout_seconds: 10
```

### OpenAI Decisions API

```yaml
classify:
  providers:
    openai:
      type: openai # Optional for the built-in key
      api_key: ${OPENAI_API_KEY}
      model: gpt-6-luna
      base_url: https://api.openai.com/v1
      timeout_seconds: 10
```

The provider calls `POST /v1/decisions` and translates questions and answers, so output looks the same as TypeSafe's:

- **noul** is sent as a `predicate`. Its `true`/`false` criteria are appended to the instructions, because predicates have no separate criteria.
- **choice** criteria become `choices`, in the order written.
- **score** criteria become `levels`. A level is a plain label or an object with `label` and optional `description`; an empty label is rejected before sending.
- Structured state, instructions, and descriptions are sent as compact JSON text, because the API accepts only text. Instructions must not be null.
- A question the model refuses to answer is reported as an error.

The API requires an OpenAI API key; ChatGPT sign-in credentials cannot call it.

### Cloudflare Workers AI (Clef)

```yaml
classify:
  providers:
    cloudflare:
      type: cloudflare # Optional for the built-in key
      api_key: ${CLOUDFLARE_API_TOKEN}
      model: clef      # or clef-flash
      base_url: https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/run
      timeout_seconds: 10
```

[Clef](https://developers.cloudflare.com/workers-ai/models/clef/) is Cloudflare's open-weight decision model: `clef` (27B) for precision, `clef-flash` (9B) for latency. It speaks the System One format natively, so questions and answers pass through unchanged.

- **Account:** `{account_id}` is replaced with `CLOUDFLARE_ACCOUNT_ID` (shown by `wrangler whoami`), or write your account ID into `base_url`.
- **Token:** it needs Workers AI access. Wrangler's login token works: `api_key: $(wrangler auth token)`.
- **AI Gateway:** set `base_url` to `https://gateway.ai.cloudflare.com/v1/<account_id>/<gateway>/workers-ai` and an `api_key` the gateway accepts.
- **Limits**, checked before sending: 1 to 64 questions, 2 to 255 options per choice question, 2 to 10 score levels, and up to 4 PNG, JPEG, or WebP images (4 MiB each, 8 MiB in total).

```bash
export CLOUDFLARE_ACCOUNT_ID=your-account-id
term-llm classify -p cloudflare "Checkout is failing for every customer" \
  --type noul --question "Is this urgent?" --format value
term-llm classify -p cloudflare --model clef-flash -q routing.yaml -f ticket.txt
```

## Privacy

The entire state, any images, and all questions, instructions, and criteria are sent to the selected provider (or the endpoint you configure); classification never runs locally. Do not include secrets or personal data unless you are authorized to send them. Output can include your criteria, so treat output files as sensitive. Error messages are bounded and redact the API key and the state in its raw, unquoted, compact, and JSON-escaped forms; model, type, instruction, and rubric diagnostics remain visible. Redaction is not a substitute for minimizing sensitive input.

## Images

The `openai` and `cloudflare` providers can evaluate images; `--image` with a TypeSafe provider fails before anything is sent. Repeat `--image` to attach PNG, JPEG, GIF, or WebP files, or `data:image/...;base64,` URLs (Clef does not accept GIF). Hosted image URLs are not supported by either API. Text state is optional when an image is given:

```bash
term-llm classify -p openai --image product.png \
  --type noul --question "Does the product have visible damage?" --format value
term-llm classify -p cloudflare "Customer photo for order 1234" --image a.jpg --image b.jpg -q checks.yaml
```

Each image is limited to 16 MiB decoded. Data URLs are normalized before sending: the header is lowercased, whitespace such as `base64` line wrapping is removed, and empty images are rejected. Guardian and the live voice router send text state only.

## Intent routing

Create a question file. JSON and YAML are supported, either as a question map or under a single `questions` key:

```yaml {title="routing.yaml"}
questions:
  intent:
    type: choice
    instructions: Which team should handle this request?
    criteria:
      billing: Payments, invoices, and refunds
      technical: Bugs, outages, and integrations
      sales: Pricing and new accounts
```

```bash
term-llm classify "My invoice is incorrect" -q routing.yaml
term-llm classify "My invoice is incorrect" -q routing.yaml --format value
```

Questions are evaluated independently against the same state. Answer IDs match your question IDs. Keep each question narrow, then combine decisions in your own code.

## Parallel safety checks

```yaml {title="safety.yaml"}
jailbreak:
  type: noul
  instructions: Does this request try to override the assistant's instructions?
  criteria:
    "true": Attempts to bypass or replace the assistant's rules
    "false": A normal request without an override attempt
personal_data:
  type: noul
  instructions: Does this text contain private personal information?
```

```bash
term-llm classify -f incoming.txt -q safety.yaml --format table
term-llm classify -f incoming.txt -q safety.yaml --format value --answer jailbreak
```

A noul value is a probability, **not a Boolean**. Choose thresholds and review paths appropriate to your application. Model judgments are not guarantees of safety.

## Single-question shortcuts and scoring

For a quick question, use `--type` and `--question` instead of `--questions`. `--name` defaults to `result`.

```bash
term-llm classify "Please refund this charge" \
  --type choice --question "What is the intent?" \
  --option refund="Request a refund" --option other --format value

term-llm classify "I have been waiting for days!" \
  --type score --question "How frustrated is the customer?" \
  --level Calm --level Frustrated --level "Very angry" --format value

term-llm classify "Production is down" \
  --type noul --question "Is this urgent?" \
  --true-description "Immediate action needed" \
  --false-description "Can wait" --format value
```

Repeat `--option key[=description]` for choices. Bare options have null descriptions; duplicate option keys are rejected. Scores require at least two `--level` values in order. Levels are indexed from zero, and a score can fall between levels. Optional `--true-description` and `--false-description` apply only to noul questions.

Do not mix `--questions` with any single-question flags. Type-specific flags cannot be used with another primitive. For structured instructions or criteria, use a question file rather than the shortcut flags.

## Structured state and input sources

Choose exactly one state source: positional words (joined with spaces), `--file/-f`, or stdin. Non-whitespace piped stdin and positional/file state together are rejected rather than silently ignoring data. Empty or whitespace-only inherited stdin is ignored, so positional and file inputs also work in non-interactive runners. `-f -` explicitly selects stdin.

```bash
cat incoming.txt | term-llm classify -q routing.yaml
term-llm classify --state-json '{"message":"Refund please","account":{"tier":"pro"}}' -q routing.yaml
term-llm classify --state-json -f event.json -q safety.yaml
```

Without `--state-json`, even JSON-looking input is sent as a string. With it, input must be valid JSON and is sent as structured state without rounding large numbers. Prefer the documented System One state forms: string, object, or array. The `openai` provider sends structured state as compact JSON text.

Question files may also come from stdin, but state must then be supplied separately:

```bash
cat routing.yaml | term-llm classify "My invoice is incorrect" --questions -
```

Both inputs cannot consume stdin. Input files/streams are limited to 16 MiB each. Question files must contain one document without YAML aliases or duplicate keys, with `type`, `instructions`, and optional `criteria` for each question. Instructions and criteria descriptions support structured JSON objects and arrays as described in the [TypeSafe API](https://docs.typesafe.ai/api).

## Output and model discovery

Requested answers must include their matching type and primary value. Usage, confidence, probability distributions, score legends, and model descriptions/release dates may be omitted; confidence and probabilities are validated when present. Additional answer IDs and fields are preserved in raw JSON output.

- `--format json` is the classify default: preserves the **full raw API JSON response**, including model, answers, probability distributions, nullable confidence, score legend, token usage, and any additional API fields. Every provider prints this same shape, so scripts work with any of them: `openai` prints its answers converted from the Decisions response, and `cloudflare` prints Clef's `result` object without Cloudflare's response envelope.
- `--format table` displays answers sorted by ID, with type, value, and confidence. Blank confidence means none was reported (including null).
- `--format value` prints only a choice, score, or noul value. It requires one question or `--answer ID`. `--answer` is only valid with this format.
- `--pretty-print` indents JSON output for reading. It requires `--format json` and is rejected with an error for any other format, rather than being silently ignored. Without it, output is the API response bytes verbatim followed by a newline, so piped output stays stable for diffing and hashing.
- `--output/-o result.json` writes output to a file instead of stdout. New files are created with owner-only permissions (`0600`) because output may contain sensitive state or criteria. Existing files are overwritten without changing their permissions.

```bash
term-llm classify "My invoice is incorrect" -q routing.yaml -o result.json
term-llm classify models
term-llm classify models --format json --pretty-print
term-llm classify models --timeout 5s --format table -o models.txt
```

```bash
term-llm classify "Sam is eating vanilla ice cream" \
  --question "Am I happy?" --type choice --option yes --option no --pretty-print
```

```json
{
  "model": "jev-1.13.0",
  "answers": {
    "result": {
      "type": "choice",
      "choice": "yes",
      "confidence": 0.56,
      "probabilities": {
        "yes": 0.78,
        "no": 0.22
      }
    }
  },
  "usage": {
    "input_tokens": 291,
    "output_tokens": 31
  }
}
```

`classify models` defaults to table output and also accepts `--provider/-p`, `--format json`, `--base-url`, `--timeout`, and `--output`. Because table is its default format, `classify models --pretty-print` must pass `--format json` as well. It uses the selected classification provider’s credential and configuration. TypeSafe lists models from its API; the `openai` (`gpt-6-luna`) and `cloudflare` (`clef`, `clef-flash`) providers list their models locally without a request, but still require a credential. Because `models` names a subcommand, use `-f` or stdin to classify the literal text `models`.

`--type`, `--format`, `--provider`, `--model`, `--base-url`, and `--answer` have shell completion. `--model` and `--base-url` offer the selected provider's configured value plus its type's built-in models and endpoint, without calling the API. `--answer` offers the question IDs the current flags request, from the `--questions` file or `--name`.

## Guardian backend

Guardian continues to use its LLM backend by default. To review actions with any classification provider instead:

```yaml
guardian:
  backend: classify          # default: llm
  classify:
    provider: typesafe       # optional; inherits classify.default_provider
    min_confidence: 0.15     # inclusive range 0–1; see the note below
  fallback:                  # optional: standard LLM Guardian for classifier denials/failures
    provider: chatgpt        # optional; same resolution rules as guardian.provider
    model: gpt-5.6-luna-low  # optional; same resolution rules as guardian.model
    # log_path: off          # optional; default <XDG_DATA_HOME>/term-llm/guardian/escalations.jsonl
```

The selected `classify.providers` entry supplies the model, endpoint, API key and transport timeout. Guardian resolves the provider and credentials during setup, before any action is reviewed; setup itself makes no classification call. `guardian.provider` and `guardian.model` apply only to the LLM backend. Both backends honor `guardian.policy_path`, `guardian.timeout_seconds` (default 90 seconds), approval callbacks, yolo bypass and the existing interactive/headless failure behavior. The classify transport timeout also applies, so the earlier deadline wins.

With `guardian.fallback` configured, the classifier still runs first and its allow is final — no LLM request, no log record. A classifier **deny** (including a low-confidence gate) or **failure** (transport error, malformed answers, over-budget request) escalates the action to a standard LLM Guardian reviewer built from the resolved `guardian.fallback.provider`/`model`, and that LLM verdict becomes the decision; at least one of those two settings must be present, because with neither the fallback is disabled and classify runs alone. The fallback reviewer is resolved and constructed eagerly at setup, exactly like the LLM backend, so unresolved providers or credentials fail installation (one warning plus prompt mode interactively, startup failure headless) instead of surfacing later. The approval manager's checks still run against the LLM verdict, so a fallback allow with high/critical risk or unclear authorization is denied as contradictory and counts toward the breaker. A classifier denial counts toward the breaker only when the fallback could not review the action: the classifier's denial then stands with `fallback unavailable: ...` in the rationale. Only a classifier failure together with a fallback failure is a review failure, which fails closed without counting. Approved actions are marked `via fallback` in the status line. Each reviewer applies `guardian.timeout_seconds` to its own call, so an escalated review can take roughly twice that; reviewer-pool queueing adds further latency, so this is not a hard bound.

Every escalation appends one JSON line to `$XDG_DATA_HOME/term-llm/guardian/escalations.jsonl` (falling back to `~/.local/share/term-llm/guardian/escalations.jsonl`), so classifier mistakes can later be used to fine-tune it. `guardian.fallback.log_path` selects another file (a `~` prefix is expanded) and the literal `off` disables logging; the path must be absolute or start with `~` (a relative path is rejected when the configuration loads), and with no resolvable home directory logging is disabled with one warning while escalation still works. The file is created as `0600` unless it already exists, refuses a path that is not a regular file, and grows without bound — rotate or delete it yourself. Each record holds `schema_version`, `timestamp`, `scope_id`, the exact classifier `classify_request` (state, model, questions) with `classify_request_sent`, `min_confidence`, `classify_stage` (`build`/`transport`/`validate`/`decision`), `classify_answers` when they arrived, `classify_decision` or `classify_error`, `classify_model` and `classify_usage`, `classify_duration_ms`, and the fallback `fallback_provider`, `fallback_model`, `fallback_decision` or `fallback_error`, `fallback_usage`, and `fallback_duration_ms`. The fallback verdict is raw and pre-enforcement: the approval manager may still deny it. Logging failures warn once per Guardian installation and never change a verdict.

Each review makes one multi-question classification call for `risk_level` (low/medium/high/critical), `user_authorization` (explicit/implied/insufficient/unknown, mapped to the existing high/medium/low/unknown policy values), and `outcome` (allow/deny). It allows only outcome **allow**, risk **low/medium**, authorization **explicit/implied**, and all three confidence values at or above `min_confidence`. Missing confidence, unknown choices and malformed results are review errors, which escalate when a fallback is configured. Low confidence is a policy denial and counts toward Guardian's denial breaker unless a fallback reviewer decides the escalated action instead. Rationales report every choice/confidence and failed gate; they are not model-generated prose. Trusted authorization comes only from actual `user` and `parent_user` roles, including successful first-party `ask_user` answers that the runtime explicitly marks as user input; ordinary assistant/tool claims and embedded role labels remain untrusted. The trusted `ask_user` turn contains only runtime-authored framing, the question that was displayed, and the option the user chose. Option labels and descriptions are written by the model, so they are never quoted as user speech; the assistant tool call containing the option set is untrusted supporting evidence and may be omitted or excerpted to fit the review budget.

What each question asks matters when you write a custom `guardian.policy_path`. `risk_level` scores the action's own risk, including its workdir and destinations. `user_authorization` classifies the trusted-user evidence and judges scope by target and side effects rather than by whether the exact command string was named, so a read-only step that counts, filters, or summarizes results the user asked for stays `implied`. `outcome` asks only whether the policy contains a specific prohibition covering this exact action; it does not re-derive risk or authorization, because the caller already enforces both independently. Write custom policies as specific, nameable prohibitions — the `outcome` gate denies when it can point to the rule that forbids the action, not when the action merely looks risky.

`min_confidence` is not a probability that the answer is correct. TypeSafe derives confidence from how spread out the answer's probability distribution is, so on a two-option question it tracks the margin between the top two options. A value of `0.15` therefore rejects only near-ties; raise it if you want auto-approval to require a clear winner. The OpenAI Decisions API and Clef report their own confidence for choice answers, so recheck this threshold against your own approvals when switching Guardian to an `openai` or `cloudflare` provider, and for each Clef model separately.

**Guardian request budget:** classify-backed Guardian reviews, with any provider type, have a hard **24,000-byte limit on the entire serialized JSON request**, including the model, questions, and JSON escaping. The policy and exact action are reserved first and never truncated. Context is then budgeted so accumulated conversation or permission history cannot prevent a routine action from being reviewed:

- **User evidence:** up to 8,000 serialized bytes total and 3,000 per entry. The latest instruction and original task from both `user` and `parent_user` roles are prioritized, followed by recent user messages. Selected entries retain their actual roles and chronological indexes; long entries use marked head/tail excerpts, not generated summaries. Older messages can be omitted, including restrictions, so retained evidence is not a complete authorization history or blanket consent.
- **Approval context:** up to 2,000 serialized bytes. Duplicate local shell command/workdir fields are removed because the exact values are already in the action. When context needs compaction, historical exact shell-command approvals are dropped first; remaining permission lines are kept whole or omitted, never clipped into misleading grants.
- **Supporting evidence:** up to 1,500 serialized bytes total, considering only the six most recent eligible assistant/tool entries, at most 500 bytes each. System/developer transcript entries are excluded.

These budgets shrink further when the exact action leaves less room. The packet reports omitted entries, omitted user entries, excerpted messages, and whether approval context is incomplete. Guardian must not treat omissions as permission or as revocation of retained restrictions. If the exact action, policy, model and questions alone cannot fit, no request is sent and the review fails closed with a manual-approval error (escalating to the fallback reviewer instead, when one is configured). Switch to prompt mode to approve that action deliberately; this error does not automatically open a prompt or count as a model policy denial. These limits do **not** apply to LLM-backed Guardian or standalone `term-llm classify` requests.

**Guardian privacy:** the selected classification provider (or your configured endpoint) receives the policy, role-labelled compact transcript including tool evidence, omission/truncation metadata, deterministic approval context, and exact shell, file/directory/selector or workspace action. Transcript/context compaction is not secret redaction; the exact action and policy are not truncated. Do not enable this backend unless sending that evidence to the endpoint is authorized. With `guardian.fallback` configured, an escalated action also sends that same evidence to the resolved fallback provider, and writes the classifier request and both verdicts to the escalation log on disk. `guardian.review` JSON events expose `duration_ms` for both backends and `state_bytes` for classify (serialized state bytes, not the whole HTTP request). Events do not include the state itself. Classification model pricing (TypeSafe, OpenAI Decisions, or Clef) remains unpriced; token usage does not imply an invented dollar cost.
