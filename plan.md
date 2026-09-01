# Native `/commit` Command Plan

## Status

Proposed implementation plan for a first-party `/commit` workflow in both the terminal chat UI and the Web UI.

The first release should be a useful commit workflow, not a complete Git client. It will own the safety-sensitive Git transaction while delegating optional natural-language scope planning and commit-message drafting to the configured commit-message agent.

## Goals

- Make `/commit` available as a built-in slash command in both TUI and Web chat.
- Respect the active session checkout, including a session-bound managed worktree.
- When anything is already staged, immediately ask whether to commit everything or only the staged changes; never infer that choice.
- When nothing is staged and no scope request was supplied, stage everything automatically as the obvious default.
- Let natural-language command intent propose a narrower file set, such as `/commit only the checkbox changes and leave the widget changes out`, with a validated, reviewable selection before the index is changed.
- Use the existing `commit-message` agent by default for optional scope planning and message drafting, while allowing end users to select or override it with their own agent.
- Let the user freely edit, regenerate, or replace the generated message before committing.
- Keep staging and the final commit deterministic and host-controlled rather than model-controlled.
- Detect when checkout identity, Git operation state, `HEAD`, or the index changes during review and refuse to start a stale commit.
- Run normal Git hooks and signing behavior, preserve the message on failure, and report useful errors.
- Share Git status/staging/fingerprint/verified-commit semantics and API types between TUI and Web; transport, reconnect, and idempotency mechanics may differ by client.
- Remain useful when message generation is unavailable by allowing a manual message.

## Non-goals for the first release

- Hunk-level or line-level staging.
- A general-purpose plugin/workflow system.
- Replacing the existing skills or widgets systems.
- Pushing, creating branches, opening pull requests, or publishing commits.
- `--amend`, fixup/squash commits, empty commits, or bypassing hooks with `--no-verify`.
- Merge, cherry-pick, revert, or rebase continuation commits. The first release handles ordinary commits only and blocks every active Git sequencer/operation state with guidance.
- Automatically splitting unrelated changes into multiple commits.
- Automatically rewriting a user's existing staged selection without an explicit **Follow request** confirmation.
- Supporting `/commit` before a Web draft has an active persisted session.
- Providing an interactive terminal to hooks, credential helpers, or signing programs. Normal non-interactive hooks and configured signing are honored; failures remain recoverable.

These may be added later through explicit revisions to the repository-state and fingerprint contracts; the first release must not leave operation state implicit.

## User-facing command

```text
/commit [commit intent]
```

Examples:

```text
/commit
/commit mention issue #482
/commit emphasize the migration compatibility behavior
/commit only the changes to the checkboxes and leave the widget changes out
```

Text after `/commit` is optional commit intent. It may contain message guidance, a natural-language inclusion/exclusion request, or both. It is never interpreted as shell input. When intent is present, a read-only scope-planning pass decides whether it narrows the change set; every proposed path is validated against the current repository status and any subset is shown for confirmation before staging. Keep the first release free of flags.

The built-in command reserves `commit`. A user skill with the same name remains invocable through `/skills run commit ...`, following existing built-in collision behavior.

`/commit` is not streaming-safe. Starting it while the main response, any isolated child run, a direct shell command, a worktree operation, or another commit operation may mutate or observe the same checkout should produce a clear busy message. TUI gating should explicitly inspect `m.streaming`, active `skillRuns`, `directShellRun`, `worktreeOperationBusy()`, and commit state. Server gating must check active response/skill/commit operations for the session and the same canonical checkout. A side question is read-only and does not block the workflow. External Git processes are detected through Git locks and fingerprint changes rather than runtime state.

Adding `/commit` makes the previously unique TUI abbreviation `/com` ambiguous with `/compact`. Preserve the existing prefix-resolution rules, document the ambiguity in tests, and require `/comm…` for commit or `/comp…` for compact; do not introduce a short alias that creates another collision.

## End-to-end user flow

### 1. Open and inspect

Invoking `/commit` clears the submitted slash command but preserves any unrelated composer draft if command startup fails. The host resolves the active session directory and discovers the checkout containing it. It must use the active checkout/worktree, not `worktree.MainRepoRoot`.

The initial inspection gathers:

- checkout root and Git directory identity;
- branch name or detached-HEAD state;
- `HEAD` object ID, including an explicit unborn-branch state;
- staged changes;
- unstaged changes;
- untracked files, excluding ignored files;
- unresolved conflicts;
- repository operation state such as merge, cherry-pick, revert, or rebase, all of which block the first release;
- staged diff summary;
- an index tree fingerprint captured only after conflicts and operation state have been checked.

Repository discovery must scrub inherited repository-selection environment such as `GIT_DIR`, `GIT_COMMON_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`, and `GIT_NAMESPACE` before invoking `git -C`. Otherwise a valid session path can read or mutate a different repository/index. Do not use `tools.DetectGitRepo`, which intentionally canonicalizes linked worktrees to their common/main repository for other features.

If inspection fails, keep the user's composer content and show an actionable error such as “The active session directory is not in a Git checkout.”

### 2. Decide the commit scope

When staged changes exist, the first screen is intentionally simple and appears immediately after repository inspection, before any LLM run.

#### Existing staged changes

If anything is already staged, always present an explicit choice:

1. **Commit everything** — stage every tracked, deleted, renamed, and untracked non-ignored change with `git add -A`, then draft a message for the resulting index.
2. **Commit staged** — leave the index exactly as it is and draft a message only for the currently staged content.
3. **Follow request** — shown when `/commit` included intent; use the intent to propose a narrower whole-file selection, then show that proposal for confirmation.

Do not preselect a choice in a way that makes an accidental Enter ambiguous. Show staged, unstaged, and untracked counts next to the actions. The user's choice is authoritative: choosing **Commit staged** or **Commit everything** bypasses scope selection even if the command contained intent, while the original text still remains available as message guidance.

A file with both staged and unstaged changes must be marked **partially staged**. **Commit staged** preserves only its staged portion. **Commit everything** stages its remaining working-tree content. A **Follow request** subset is whole-file based: including a partially staged path stages its complete current content; excluding it removes the path from the proposed index. The confirmation must disclose either change before applying it.

#### Empty index

If nothing is staged and `/commit` has no intent, there is no staging chooser: automatically run `git add -A`, refresh status, and continue to message drafting. Show a short “Staging all changes…” state so the mutation is visible, including total tracked/untracked counts. Ignored files remain excluded.

If nothing is staged and intent was supplied, run the read-only scope planner first:

- intent that only affects wording, such as `mention issue #482`, resolves to **all changes** and proceeds with `git add -A` without another staging prompt;
- intent that narrows or excludes changes produces a proposed whole-file subset and opens the selection confirmation;
- an empty, invalid, failed, or `needs_manual` proposal never silently stages everything. Preserve the intent, explain any same-file/granularity conflict, and offer **Commit everything**, manual whole-file selection, retry planning, or cancel.

#### Natural-language scope planning

Scope planning is a separate child-run phase from commit-message drafting. It uses the configured message agent's reasoning/style context but overlays a host-owned read-only scope policy and typed finishing tool, for example:

```json
{
  "mode": "selected",
  "include_paths": ["frontend/src/components/Checkboxes.tsx"],
  "summary": "Include checkbox behavior changes; leave widget changes out"
}
```

`mode: "all"` carries an empty path list and means the intent does not narrow the change set. `mode: "needs_manual"` also carries no paths and is required when the request cannot be represented safely at whole-file granularity—for example, checkbox and widget changes are interleaved in the same file.

The planner may inspect staged and unstaged diffs, status, recent log context, and bounded untracked file content, but it receives no mutation tools. Its prompt requires `needs_manual` rather than a misleading subset when one file contains both included and excluded concerns. The host validates `include_paths` as exact paths from the latest complete status snapshot; rejects absolute paths, traversal, pathspec magic/globs, directories, ignored files, duplicates, and stale/unknown paths; and never treats the model's output as authorization to run arbitrary Git arguments. Renames/copies are represented as one logical selectable change and the host expands them to the required old/new path operations so a scoped commit cannot accidentally leave both names in the tree.

A selected proposal is always shown before mutation with:

- **Included in this commit** paths, checked;
- **Left out** paths, unchecked;
- badges for already staged, unstaged, untracked, and partially staged state;
- the planner's short rationale;
- actions to adjust whole-file checkboxes, confirm, retry planning, choose everything, or cancel.

The user—not the planner—makes the final selection. Hunk-level selection remains out of scope.

To apply a confirmed subset safely, construct a temporary index from `HEAD` (or the empty tree on an unborn branch), stage the selected paths into it with literal pathspec handling, compute its tree, revalidate the checkout/status preconditions, and then update the real index to that exact proposed tree using Git's index-locking plumbing. This makes excluded previously staged paths unstaged while leaving their working-tree content intact. Do not copy a raw index file over another process's index. If any precondition changed, discard the temporary proposal and return to review.

For aggregate **Commit everything**, use `git add -A`. For **Commit staged**, perform no staging mutation. After every mutation, refresh status and require a non-empty index before message generation.

#### Staging persistence

The resulting real-index state persists if the user later cancels `/commit`: files staged by all/subset remain staged, and previously staged files excluded by a confirmed subset remain unstaged. The UI must state this. If the workflow changed paths and the user closes it, the confirmation names/counts those paths instead of showing only a generic dirty-index warning. Do not restore a raw index backup on cancel: doing so could erase external index changes made while the workflow was open.

### 3. Draft the message

Once the index is final, capture a review fingerprint containing at least:

- opaque canonical checkout identity, based on the canonical worktree root rather than the shared Git common directory;
- `HEAD` object ID or an explicit unborn marker;
- index tree object ID from `git write-tree`;
- operation state (`none` in the first release) plus operation-head identity fields so starting or ending a merge/sequencer cannot pass revalidation with the same `HEAD` and tree.

Conflict detection must precede `git write-tree`, which fails on an unmerged index. Because `git write-tree` may update the index cache-tree and take `index.lock`, capture the fingerprint under the short host mutation lock; do not hold that lock while the model runs or the user edits.

Resolve the effective message agent through the normal agent registry. The default configured name is `commit-message`, so existing registry precedence (`project-local > user > additional search paths > builtin`) already lets an end user replace the built-in by defining their own agent with that name. Users may also set `commit.message_agent` to a differently named agent. Resolve the name independently in TUI and server wiring through the same registry/config rules; expose the resolved name/source in generation diagnostics. A missing or invalid configured agent must not silently fall back to the builtin: report the configuration error and keep manual message entry available.

Start the resolved message agent in a child runtime rooted at the active checkout. The custom agent supplies its system prompt, style, provider/model preferences, and other non-safety drafting behavior, but the native commit workflow owns the final-output and safety contract. The index is guaranteed non-empty. Pass the original commit intent plus any accepted scope summary as message guidance, while explicitly stating that **only `git diff --cached` describes the final commit** and unstaged/untracked changes are out of scope. The distinct commit-draft child kind applies a narrowed runtime copy of whichever agent resolved: shell-only, no `read_file`, only the staged-diff/status/recent-log Git commands needed for drafting, a host-installed string output tool for the final commit message, unconditional suppression of the agent's configured `on_complete`, and a bounded turn budget. It must not expose a custom agent's write tools, mutation permissions, unstaged-diff scripts, configured output side effects, or broad file reads. Copy and overlay the agent for this run rather than mutating its registry entry.

The child run must capture the host-installed `set_commit_message` string output and skip the resolved agent's `on_complete` hook unconditionally, whether structured capture succeeds, streamed prose exists, or the run fails. Any custom `output_tool` is ignored for this specialized run so the native workflow always receives one editable message string. The built-in hook currently writes `.git/COMMIT_EDITMSG` immediately and assumes `.git` is a directory, which is incorrect for linked worktrees and occurs before user review; custom hooks are suppressed for the same host-owned safety reason.

Add a backward-compatible child-run option, for example:

```go
type ChildRunRequest struct {
    // Existing fields...
    SkipOnComplete  bool
    MaxTurnsOverride int // zero preserves existing behavior
}
```

The default remains `false`/zero so existing `spawn_agent` and isolated-skill behavior is unchanged. Both commit-specific child kinds set `SkipOnComplete` and a small explicit turn cap suitable for one structured scope proposal or message draft.

Message-draft requirements:

- expose progress and cancellation;
- reject an empty/non-captured final message as a generation failure;
- retain provider/model and child-session metadata for diagnostics;
- allow regeneration using a fresh child run;
- do not execute staging or commit commands through the agent;
- if generation fails, open or retain the editor so the user can write the message manually.

### 4. Review and edit

The review UI contains:

- editable multiline message;
- branch or detached-HEAD warning;
- staged file count and additions/deletions summary;
- an indication that unstaged changes remain, when applicable;
- blocked-operation guidance when Git reports merge/cherry-pick/revert/rebase state;
- generation state and agent/model information;
- **Commit**, **Regenerate**, **Review files**, and **Cancel** actions.

The message is always editable; generated text is only a starting point. Validate only that the final message has a non-whitespace subject. Long subjects or unusual formatting may produce a non-blocking hint, but the host must not force Conventional Commits or enforce the agent's stylistic preferences.

Regeneration replaces the editor only after a successful result. If the user has edited the current message, ask before overwriting it or retain the prior draft so cancellation/failure does not destroy work.

“Review files” returns to the scope/status summary without discarding the current message. If the included selection changes in either direction, require regeneration or an explicit acknowledgement that the message may no longer describe the diff. Prefer automatic regeneration after the user confirms the changed selection.

### 5. Revalidate and commit

Immediately before `git commit`, inspect the checkout again and compare the current checkout identity, operation state, `HEAD`, and index tree with the review fingerprint.

If any component changed, do not commit. Refresh the modal and explain that the reviewed content is stale. Offer to review and regenerate while preserving the edited message as a recoverable draft.

Write the final message to a securely created `0600` temporary file and invoke Git through an argv-based process, conceptually:

```text
git -C <active-checkout> commit --cleanup=verbatim --file=<temporary-message-file>
```

Do not invoke a shell and do not interpolate the message or paths. `--cleanup=verbatim` ensures a subject beginning with `#` or other user-authored formatting is not silently stripped after review. Remove the temporary file on every exit path. Place it outside tracked workspace content; use `git rev-parse --git-path` for any Git administrative paths rather than assuming `.git/` is a directory.

The commit operation:

- honors normal pre-commit, prepare-commit-msg, and commit-msg hooks;
- honors configured signing;
- sets a documented non-interactive environment such as `GIT_TERMINAL_PROMPT=0` and does not provide a terminal to credential/signing prompts;
- captures and continuously drains bounded stdout/stderr for display without allowing a full buffer to deadlock Git;
- is detached from an HTTP disconnect, TUI `Esc`, or ordinary request cancellation after the Git process starts;
- uses a soft UI timeout that changes the display to “still running” but does not automatically `SIGKILL` `git commit` and leave an unknown transaction;
- is drained during graceful server shutdown; TUI quit/reload warns and waits while a commit is in flight;
- never retries automatically.

After Git returns—or when recovering from an uncertain transport/process condition—inspect the resulting commit rather than trusting process output. For an ordinary commit, verify that the new commit's first parent is the expected pre-commit `HEAD` (or that it has no parent on an unborn branch), read the actual `HEAD^{tree}`, and read the final subject/body from `git log -1`. Hooks may legitimately rewrite the message or stage files. If the commit succeeded but its tree differs from the reviewed index tree, report success with a prominent warning that a hook or concurrent Git process changed committed content; never reset or rewrite the resulting commit automatically.

On verified success, show the actual short object ID and subject read from the resulting commit, close the workflow, refresh Git-backed Changes views, and publish the normal cross-tab/session refresh signal in Web.

On failure, keep the message and modal open, refresh repository status, and show actionable output. Treat missing `user.name`/`user.email`, hook rejection, signing failure, an index lock, and an unavailable non-interactive credential/signing prompt as recognizable errors. A hook may modify files or the index even when it fails, so recompute the fingerprint before enabling retry. If outcome is genuinely uncertain, do not offer one-click retry until status/operation recovery has classified whether `HEAD` moved.

## Repository states and edge cases

### Supported

- Normal branch with staged changes.
- Empty index with tracked, deleted, renamed, or untracked changes.
- Partially staged files, provided the user leaves the staged portion intact or explicitly stages the remainder.
- Initial/unborn branch with staged content.
- Detached `HEAD`, with a prominent warning but no hard block.
- Submodule entries as whole staged gitlinks. The status UI and agent must describe only the parent repository's gitlink change, not imply that uncommitted submodule working-tree content is included.
- Binary changes, represented through file metadata/diff stats even when textual content is unavailable.

### Blocked with guidance

- Unresolved conflicts/unmerged index entries.
- Any merge, cherry-pick, revert, rebase, or sequencer state in the first release; direct the user to complete or abort it with Git. These operations have parent/message/empty-tree semantics that should be implemented deliberately in a later mode.
- Empty index after staging.
- Bare repositories.
- Active checkout operations that can race with status/generation/commit.
- A changed `HEAD` or index fingerprint at final confirmation.
- A stale or inaccessible session-bound worktree.

### Limits

Bound status output, file counts, diff summaries, hook output, agent intent, and untracked-content inspection. If a repository exceeds exact-selection limits, preserve safe aggregate **Commit everything**/**Commit staged** behavior, identify truncation, and avoid claiming the visible list is complete. Natural-language/manual subset selection requires a complete content-aware status token and is disabled when that guarantee cannot be met. Plain `/commit` with an empty index may still perform its repository-wide `git add -A` default, but the visible staging state must report accurate total counts and truncation before message review.

## Shared Git package

Create a focused package, tentatively `internal/gitcommit`, with no TUI, HTTP, or frontend concerns.

Suggested types:

```go
type RepositoryState struct {
    CheckoutRoot string
    GitDirID     string
    Branch       string
    Detached     bool
    Unborn       bool
    HeadOID      string
    Operation    OperationState
    Staged       []Change
    Unstaged     []Change
    Untracked    []Change
    Conflicted   []Change
    Summary            DiffSummary
    Fingerprint        Fingerprint
    StatusToken        string
    SelectionAvailable bool
    Truncated          bool
}

type Fingerprint struct {
    CheckoutID string
    HeadState  HeadState // born or unborn; never inferred from an omitted JSON field
    HeadOID    string
    IndexTree  string
    Operation  OperationFingerprint
}

type OperationFingerprint struct {
    Kind     OperationKind // none in v1
    HeadOIDs []string      // operation heads when later modes support them
    Digest   string        // sequencer/admin-state digest
}

type StageRequest struct {
    Mode        StageMode // all or exact_selection
    Paths       []string  // required for exact_selection
    StatusToken string
}

type CommitResult struct {
    BeforeHead      string
    HeadOID         string
    TreeOID         string
    ShortOID        string
    Subject         string
    Message         string
    TreeChanged     bool
    OutcomeUncertain bool
    Stdout          string
    Stderr          string
}
```

Suggested operations:

```go
Open(ctx, dir) (*Repository, error)
(*Repository).Inspect(ctx) (RepositoryState, error)
(*Repository).Stage(ctx, request, expected Fingerprint) (RepositoryState, error)
(*Repository).Commit(ctx, message string, expected Fingerprint) (CommitResult, error)
```

Implementation requirements:

- discover and canonicalize the checkout with `git -C <dir> rev-parse --show-toplevel` plus checkout-specific Git-dir queries; never substitute the common/main worktree root;
- distinguish checkout root from shared/main worktree administrative directories and derive an opaque checkout ID from the canonical worktree identity;
- scrub inherited Git repository/index-selection environment before every command;
- parse `git status --porcelain=v2 -z --untracked-files=all` (or an equivalently robust NUL protocol) so spaces, renames, intent-to-add entries, unmerged stages, and unusual filenames are safe;
- derive a complete content-aware status token for exact selection from canonical status records, index entries, and changed working-tree/untracked content identities; disable exact selection rather than issue an incomplete token when limits are exceeded;
- detect conflicts and unsupported operation state before taking a tree fingerprint;
- use bounded command output and context cancellation for inspection/staging/generation, but a dedicated non-killing process lifecycle for `git commit`;
- use `git write-tree` for the exact staged tree fingerprint under the short mutation lock;
- use Git commands via `exec.Command`/`exec.CommandContext` with argv, never shell strings;
- return typed errors for missing Git, unsafe repository ownership, not-a-repository, bare repository, missing identity, conflict, intent-to-add ambiguity, empty index, stale fingerprint, unsupported operation state, index lock, hook/commit/signing failure, uncertain outcome, and output limits;
- avoid global process-directory changes;
- accept exact-selection paths only as a literal subset of the latest complete status snapshot;
- build exact selections in a temporary index initialized from `HEAD`/empty tree, stage selected paths there, and apply the resulting tree to the real index only after revalidation under Git's index lock;
- never copy a raw temporary index over the live index or let an excluded staged path disappear from the working tree;
- serialize host-owned mutations per canonical checkout while still relying on Git's index lock, preconditions, and post-commit verification for external-process races;
- integrate main-checkout mutations with `processRootCheckoutLeases` through an injected coordinator from `cmd`, while linked worktrees use their own checkout-keyed lock and do not accidentally take a common-dir/main-index lock;
- state explicitly that these locks are process-local: separate TUI and `serve` processes coordinate only through Git locks and fingerprint/postcondition checks.

Do not reuse `tools.DetectGitRepo`, which resolves to a common/main root, or the cached `serveServer.sessionGitRepo` lookup for mutations. `internal/gitdiff` may supply low-level display/diff helpers, but its current rename/1000-file behavior is not the commit status contract; do not build `RepositoryState` by calling `gitdiff.List` unchanged.

## Shared scope and message-agent orchestration

Keep agent reasoning separate from Git mutation. A coordinator should expose two operations:

```go
type ScopeProposal struct {
    Mode         ScopeMode // all, selected, or needs_manual
    IncludePaths []string
    Summary      string
}

PlanScope(ctx, request) (ScopeProposal, ChildRunMetadata, error)
DraftMessage(ctx, request) (string, ChildRunMetadata, error)
```

Shared request inputs include:

- parent session ID;
- active checkout directory;
- resolved message-agent name (defaulting from `commit.message_agent` to `commit-message`);
- original commit intent;
- expected status token/fingerprint;
- a `run.ChildRunner`;
- progress callback and cancellation context.

`PlanScope` uses a host-owned typed output tool and a broader but read-only uncommitted-diff policy. `DraftMessage` uses the host-owned string output tool and staged-only policy. Both TUI and server wiring call this coordinator rather than independently composing prompts or interpreting `ChildRunResult`.

Add distinct `run.ChildRunKind` values for commit-scope planning and commit-message drafting so session naming and approval provenance are not mislabeled as `spawn_agent` or `user_skill_activation`. Each kind selects its host-owned agent runtime overlay and small turn cap; copy agent configuration rather than mutating the registry's shared agent. Neither is reported as an ordinary skill activation.

`cmd/spawn_runner.go` must honor `SkipOnComplete` unconditionally for both commit child kinds after the child engine finishes, including capture failure and cancellation paths. Existing child-run tests must prove that default completion hooks still run, while every commit scope/draft terminal path skips the resolved agent's configured hook and output side effects. Ensure the field is preserved through every `ChildRunRequest` construction/adaptation path that can execute the coordinator.

## Server and HTTP API

The Web browser must not receive or choose an arbitrary filesystem path. Every endpoint resolves the checkout from the persisted session using existing project/worktree binding logic. Use the same session-ID consistency headers and authentication policy as `cmd/serve_skills.go` and `cmd/serve_file_changes.go`, but do not reuse the latter's 15-second repository cache for mutations. Route and implement this feature in focused `cmd/serve_commit*.go` files through `cmd/serve_handlers.go`; `internal/serve` is not the session HTTP handler layer.

A practical API split is:

```text
GET    /v1/sessions/{session-id}/commit/status
POST   /v1/sessions/{session-id}/commit/stage
POST   /v1/sessions/{session-id}/commit-runs
GET    /v1/sessions/{session-id}/commit-runs/{run-id}
GET    /v1/sessions/{session-id}/commit-runs/{run-id}/events
DELETE /v1/sessions/{session-id}/commit-runs/{run-id}
POST   /v1/sessions/{session-id}/commit-operations
GET    /v1/sessions/{session-id}/commit-operations/{operation-id}
```

### Status

Returns the serializable repository state, review fingerprint, and an opaque status token covering the complete selectable change set, including index identities and working-tree content identities needed to detect edits while scope planning is in flight. Administrative absolute paths must not be exposed; Web needs relative display paths, accurate total counts, truncation flags, and opaque checkout/fingerprint fields. If a complete content-aware token cannot be produced within limits, aggregate all/staged flows remain available but natural-language/manual exact selection is disabled with an explanation.

Use explicit JSON values for born/unborn and operation state rather than relying on omitted or empty `head_oid` fields.

### Stage

Request:

```json
{
  "mode": "exact_selection",
  "paths": ["internal/foo.go", "frontend/src/bar.ts"],
  "expected_status_token": "opaque-status-token",
  "expected_fingerprint": {
    "checkout_id": "opaque-id",
    "head_state": "born",
    "head_oid": "...",
    "index_tree": "...",
    "operation": {"kind": "none", "head_oids": [], "digest": "..."}
  }
}
```

The response is refreshed status. The endpoint is a fingerprint/status-token-guarded mutation. It does not need retained idempotency results: successful staging changes the index fingerprint, replay with the old precondition becomes stale, and a true no-op is safe. `all` ignores `paths`; `exact_selection` requires a non-empty validated relative path set from the referenced complete status snapshot and uses the temporary-index/tree application described above. **Commit staged** does not call this endpoint. Execute one staging request at a time per checkout and always return refreshed status after partial/error outcomes.

### Scope and message child runs

Creation uses one shared endpoint with an explicit kind. Scope planning is based on the complete working status; message drafting is based on the finalized staged fingerprint.

Scope request:

```json
{
  "kind": "scope",
  "intent": "only checkbox changes; leave widget changes out",
  "expected_status_token": "opaque-status-token",
  "expected_fingerprint": {
    "checkout_id": "opaque-id",
    "head_state": "born",
    "head_oid": "...",
    "index_tree": "...",
    "operation": {"kind": "none", "head_oids": [], "digest": "..."}
  }
}
```

Message request:

```json
{
  "kind": "message",
  "intent": "only checkbox changes; leave widget changes out",
  "scope_summary": "Checkbox files selected; widget files excluded",
  "expected_fingerprint": {
    "checkout_id": "opaque-id",
    "head_state": "born",
    "head_oid": "...",
    "index_tree": "...",
    "operation": {"kind": "none", "head_oids": [], "digest": "..."}
  }
}
```

Return `202 Accepted` with run ID, kind, status, resolved agent name/source, child-session ID when available, and event URL. Scope completion returns a typed `all | selected | needs_manual` proposal; message completion returns one string. The server—not the browser request—selects the configured message agent and host overlay. Extract/reuse the generic child-run registry/event-buffer/SSE/snapshot/cancellation/shutdown machinery currently embedded in `cmd/serve_skills.go` rather than implementing a parallel lifecycle. Commit runs remain distinct public run kinds and endpoints, but should be thin wrappers over that shared infrastructure.

Allow only one active commit child run per session. Retrying scope planning or regenerating a message cancels/supersedes the prior run before starting another, and completion handlers apply output only when run ID, kind, session ID, opaque checkout ID, and status/fingerprint still match the modal's active phase. If a session changes checkout, the old run remains queryable for diagnostics but is orphaned and never auto-applied.

A scope run is bound to one status token and fingerprint; a message run is bound to the finalized staged fingerprint. Stale output can be shown for diagnostics but never applied as a current selection/message without explicit revalidation.

### Commit operations

Commit is an asynchronous, non-cancellable-after-start operation because browser disconnects and soft timeouts must not kill Git or cause a blind retry. Creation request:

```json
{
  "message": "Short imperative subject\n\nOptional body.",
  "expected_fingerprint": {
    "checkout_id": "opaque-id",
    "head_state": "born",
    "head_oid": "...",
    "index_tree": "...",
    "operation": {"kind": "none", "head_oids": [], "digest": "..."}
  }
}
```

Require the standard `Idempotency-Key` header and compute a canonical request-body fingerprint. Reusing a key with a different body returns `409`; replaying the same key joins/returns the original operation. Do not create a separate body-level `client_operation_id` convention.

Persist a small operation record through the session store before starting Git, with `queued | running | succeeded | failed | uncertain`, request hash, expected fingerprint, timestamps, and bounded result/error metadata. A terminal failed operation is replayed as failed; retry after the user refreshes state uses a new key. A record left `running` by process crash becomes `uncertain` on startup and is reconciled against Git state but is never automatically re-executed. This persistence is specifically for the non-repeatable final commit, not generation or staging.

`POST` returns `202 Accepted` and an operation URL. `GET` allows the browser to reconnect/poll through disconnects. The server coalesces in-flight requests for the same key and serializes commit/stage mutations for the same checkout, including different sessions bound to it. External Git processes remain guarded by Git locks, preconditions, and postcondition verification.

The Web client treats `succeeded`, `failed`, and `uncertain` as distinct. An uncertain result requires status refresh and user acknowledgement; it must never turn into an automatic second `git commit`.

## TUI implementation

### Command registration

- Add `commit` to `AllCommands()` in `internal/tui/chat/commands.go`.
- Dispatch it from `ExecuteCommand` using raw command arguments as commit intent.
- Do not add it to `isStreamingLocalSlashCommand`.
- Add help/completion tests and built-in/skill collision coverage.

### State and rendering

Prefer a dedicated `CommitState` and `internal/tui/chat/commit.go` over adding many commit-specific fields to the already broad `DialogModel`.

Suggested phases:

```text
closed
loading
choosing_scope
planning_scope
reviewing_scope
staging
drafting_message
editing
committing
error
success
```

The commit overlay owns:

- repository status, status token, and fingerprint;
- original commit intent;
- immediate all-vs-staged choice;
- active scope-run state and cancel function;
- typed scope proposal, rationale, included/excluded paths, and user-adjusted whole-file selection;
- staging mode and paths whose index state changed;
- message-generation cancel function and progress;
- generated/edited message textarea;
- dirty-message flag and prior generated draft;
- commit output/error;
- focus, scrolling, and responsive geometry.

Integrate the overlay into the normal input priority and rendering layers. An open commit overlay is modal. `Esc` cancels an active scope/message child run first and otherwise asks before discarding an edited selection or message; after `git commit` starts, `Esc` does not kill it. Quit/reload displays that the Git transaction must reach a known result. The command cannot coexist with a parent stream. Provide discoverable keyboard help for all-vs-staged choice, checkbox adjustment, retry planning, regenerate message, commit, and cancel. Mouse support should follow existing modal behavior where practical.

Use `m.effectiveWorkingDir()` as the checkout starting point and `m.childRunner` for generation. The TUI calls the shared Git package directly; it should not call its own HTTP server. Inject the `cmd`-owned main-checkout lease/mutation coordinator when constructing the model rather than importing `cmd` from `internal/tui/chat`; compose it with the package's checkout-keyed single-flight lock.

On successful commit, close the overlay, show a success footer/scrollback event with short SHA and subject, and clear transient commit state. On error, keep the overlay and textarea intact.

### TUI behavior tests

Cover:

- command registration, prefix resolution, and collision reservation;
- no repository and no active session;
- busy main response/worktree/skill/shell/commit operations;
- immediate **Commit everything** vs **Commit staged** choice whenever the index is non-empty;
- optional **Follow request** choice when staged changes and commit intent coexist;
- empty-index/no-intent automatic `git add -A` without a chooser;
- intent that is message-only resolving to all changes, intent that proposes a validated subset, and same-file intent that returns `needs_manual`;
- scope-plan failure never silently staging everything;
- included/excluded checkbox adjustment and exact-selection request shape;
- temporary-index application, including exclusion of previously staged files and partial-staged disclosure;
- scope planning progress, cancellation, retry, stale result rejection, and fallback actions;
- message generation progress, cancellation, failure, regeneration, and manual fallback;
- default, same-name-shadowed, and explicitly configured custom message-agent resolution;
- unresolved/invalid custom-agent errors preserving manual entry;
- resolved agent name/source display and proof that custom writes/output tools/hooks are overlaid by the native draft policy;
- edited-message overwrite protection;
- stale fingerprint at commit;
- hook failure preserving message;
- success closing the overlay and reporting SHA;
- worktree directory routing;
- resize, narrow terminal, keyboard focus, and `Esc` behavior.

Use fake Git/message services for state-machine tests and temp repositories for a smaller number of integration tests.

## Web implementation

### Store and API client

Add a focused `CommitStore` rather than growing `AppStore` with the workflow internals. It owns modal state, status/status-token loading, immediate scope choice, commit intent, scope-run following/reconnection, proposed/user-adjusted selection, staging, message-run following/reconnection, message draft, cancellation, commit-operation submission/polling, and uncertain-outcome recovery.

Wire endpoint methods in `frontend/src/api/endpoints.ts` and expose narrow facade methods from `AppStore` only where components need them.

Add `'commit'` to the `Modal` union in `frontend/src/stores/store-types.ts`. Reset or reconcile commit state when the active session/worktree changes. A commit run remains server-owned, but its output must never be applied to a different active session.

### Composer and completion

- Add `/commit` to `SLASH_COMMANDS` in `frontend/src/domain/completions.ts`.
- Handle `/commit` and optional raw commit intent in `Composer.sendOrCommand` before skill dispatch.
- Do not mark it `streamingSafe`.
- Require a persisted active session; on a new draft, keep the command in the composer and explain that the user must start/select a session first.
- Clear the composer only after the modal successfully starts.

### Modal

Add a native, accessible commit modal using the existing `Overlay` primitive. It should provide equivalent phases and actions to the TUI while adapting layout for desktop and mobile.

Requirements:

- immediate, equally deliberate **Commit everything** and **Commit staged** actions when staged content exists;
- **Follow request** and a checked/unchecked proposal view when natural-language intent narrows scope;
- semantic labels for file status, partially staged files, planner rationale, warnings, progress, and buttons;
- keyboard-accessible checkbox selection and message editing;
- focus trapping and sensible initial focus;
- confirmation before overwriting an edited message or closing it;
- responsive bounded file list and message editor;
- no exposure of absolute server filesystem paths;
- disable duplicate stage/generate/commit submissions;
- reconnect scope/message child-run SSE using the extracted shared child-run snapshot/sequence semantics;
- poll/reconcile a final commit operation through browser disconnects without offering cancellation after it starts;
- preserve the draft across transient network failures and uncertain operation outcomes;
- if active session/worktree changes, abandon the modal binding and never apply the old generation/operation result to the new checkout;
- on verified success, refresh staged/unstaged Changes scopes and publish cross-tab refresh.

### Web tests

Add frontend unit/component coverage for:

- completion and slash dispatch with guidance;
- draft/no-session behavior;
- staged immediate all-vs-staged screen and optional follow-request action;
- empty-index automatic-all flow with visible staging state;
- natural-language scope `all` and `selected` responses;
- proposal checkbox adjustment and exact-selection staging request shape;
- scope/message child-run progress, resolved custom-agent display, reconnect, cancel, failure, and retry/regeneration;
- configured-agent resolution failure preserving manual entry;
- dirty message confirmation;
- stale status/fingerprint conflict refresh;
- duplicate-submit prevention, idempotency key/body mismatch, in-flight coalescing, terminal replay, and restart-to-uncertain behavior;
- hook/signing failure and uncertain outcome display;
- successful commit using the actual resulting subject and Changes refresh;
- accessibility roles/focus and mobile rendering.

Add a Playwright flow with mocked commit endpoints. Keep real Git execution in Go integration tests rather than browser tests.

## Configuration and agent override

The first release supports an optional message-agent override:

```yaml
commit:
  message_agent: my-commit-writer
```

The default is `commit-message`. Resolution uses the standard agent registry, so users have two supported customization paths:

1. Define a project-local or user-global agent named `commit-message`; normal registry precedence shadows the builtin without extra configuration.
2. Set `commit.message_agent` to another registered agent name.

The selected agent controls drafting instructions, tone, provider/model preferences, and other non-safety reasoning behavior. The host always overlays a phase-specific contract: scope planning may read bounded staged/unstaged/untracked content and must return validated paths through a typed tool; message drafting is staged-only and must return one string; both have turn caps, no writes, and no `on_complete`. Document this clearly so custom-agent authors do not depend on tools or hooks that `/commit` intentionally removes.

Validate the configured name using normal safe agent lookup. Both TUI and Web/server must resolve the same effective setting and show the selected agent in generation progress. If it cannot be resolved or validated, show the error and allow manual message entry; do not silently switch agents.

Do not add configurable automatic-staging policy until there is a demonstrated need. The first-release behavior is fixed and understandable: staged content triggers a deliberate all-vs-staged choice; an empty index stages all unless explicit intent produces a reviewed subset.

## Security and safety requirements

- Resolve workspace only from server/session state; never trust a browser-supplied directory.
- Scrub inherited Git repository/index-selection environment before discovery or mutation.
- Use argv-based Git execution, `GIT_LITERAL_PATHSPECS=1`, exact status-snapshot path allowlists, and no shell interpolation.
- Bound all process output and request fields.
- Treat filenames and commit messages as untrusted display text in both clients.
- Resolve the message-agent override from trusted host configuration/registry state; do not accept an arbitrary agent name from the Web request.
- Overlay every resolved custom agent with host-owned phase-specific read tools, typed/string output, turn cap, and hook suppression; never grant its configured write capabilities in a commit scope/draft run.
- Validate every model-proposed path against the latest complete status snapshot and require user confirmation before exact-selection index changes.
- Skip the resolved agent's completion hook unconditionally for scope planning and message drafting.
- Treat plain `/commit` as authorization for the documented empty-index `git add -A` default; when an index already exists or a subset is proposed, require the corresponding explicit all/staged/follow-request decision, and always require final commit confirmation.
- Fingerprint checkout identity, born/unborn `HEAD`, index tree, and Git operation state before generation and commit.
- Serialize host mutations per checkout and handle Git's `index.lock` errors explicitly, while acknowledging process-local lock limits.
- Use durable idempotency only for the non-repeatable Web commit operation; staging relies on stale preconditions and safe no-op replay.
- Redact administrative filesystem paths from Web payloads and logs where they are not required.
- Preserve hooks and signing; do not quietly fall back to `--no-verify` or `--no-gpg-sign`.
- Document that Web `/commit` executes repository hooks and signing programs as the `serve` OS user under Git's normal trust model.
- Set non-interactive Git prompt policy and surface failures rather than hanging on terminal input that Web/TUI does not own.
- Never report success based only on process transport. Verify resulting parent(s), tree, SHA, and message from Git.

## Observability and lifecycle

- Emit structured debug logs for inspection, immediate scope choice, scope-planning lifecycle, staging mode, message-generation lifecycle, stale rejection, commit start, and verified result without logging full intent, commit messages, or diffs by default.
- Include run/session/operation IDs and opaque checkout identity for correlation.
- Cancel and drain active commit scope/message child runs during TUI quit/reload and server shutdown.
- Generation and final commit have different cancellation semantics: generation is cancellable; a started Git commit is not killed for client disconnect or soft timeout.
- Persist Web commit-operation intent/result so reconnects replay one operation. Graceful shutdown waits for running commits; crash recovery marks incomplete records uncertain and never automatically reruns them.
- Do not hold repository locks while an LLM generation is running or while the user edits. Reacquire briefly for status/fingerprint/mutation and rely on preconditions/postconditions across external processes.

## Documentation

Update:

- TUI `/help` metadata and any static command help in `cmd/chat.go`.
- Web slash-command documentation/completion tests.
- `docs-site/content/guides/usage.md` with immediate all-vs-staged behavior, empty-index automatic-all behavior, natural-language scope proposals, staging persistence, regeneration/manual fallback, and worktree behavior.
- `docs-site/content/guides/agents.md` to explain how custom commit-message agents participate in both read-only scope planning and staged-only message drafting, and clarify that `/commit` overlays tools/output and never runs the resolved agent's `on_complete` hook.
- Configuration reference for `commit.message_agent`, including default resolution and failure/manual-fallback behavior.
- Any API reference covering the new session endpoints.

Document the important principle prominently:

> If changes are already staged, `/commit` immediately asks **Commit everything** or **Commit staged**. If nothing is staged, plain `/commit` stages everything; explicit natural-language scope intent may instead propose a reviewed subset.

## Delivery sequence

### Phase 1: Git domain and child-run contract

1. Add `commit.message_agent` to `internal/config` with default `commit-message`, safe lookup validation, merge/default tests, and no silent fallback on resolution failure.
2. Add `internal/gitcommit` status, status-token, stage-all, temporary-index exact-selection, operation-state, fingerprint, and verified commit operations with temp-repository tests.
3. Add Git-environment scrubbing, typed limits/errors, checkout-keyed serialization, and an injected main-checkout lease coordinator.
4. Add `ChildRunRequest.SkipOnComplete` plus distinct commit-scope/commit-draft kinds and tests proving default hooks remain unchanged while every commit child terminal path skips hooks.
5. Add shared scope/message orchestration, typed scope output, normal registry resolution, custom-agent phase overlays, and fake-runner tests.

Exit criterion: a non-UI integration test can resolve the default or a user-defined message agent, distinguish message-only intent from a narrowed scope, validate and apply an exact temporary-index selection, stage all for plain empty-index flow, draft a staged-only message without custom tools/hooks, reject checkout/operation/`HEAD`/index/status staleness, and verify a successful ordinary commit from its resulting parent/tree/message.

### Phase 2: Server API

1. Add session checkout resolution for commit operations using existing persisted project/worktree rules in focused `cmd/serve_commit*.go` handlers.
2. Implement status and fingerprint/status-token-guarded staging endpoints.
3. Extract the generic child-run registry/SSE lifecycle from `cmd/serve_skills.go` and add thin commit-scope/message run endpoints over it.
4. Add persisted commit-operation records and asynchronous idempotent start/status endpoints with parent/tree/message verification and uncertain crash recovery.
5. Add auth, session ownership, request-hash mismatch, in-flight coalescing, terminal replay, stale-race, multi-session/same-checkout, worktree, shutdown, and error tests.

Exit criterion: API tests cover the complete workflow without a browser and prove retries cannot duplicate a commit.

### Phase 3: TUI

1. Register and dispatch `/commit`.
2. Implement the immediate staged all-vs-staged chooser, empty-index automatic-all path, scope-planning/review states, and temporary-index exact selection.
3. Wire scope/message progress, cancel/retry, regenerate, and manual editing.
4. Wire fingerprinted commit and recoverable errors.
5. Add focused TUI tests, then real-repository integration tests.

Exit criterion: a user can complete immediate **Commit everything**/**Commit staged**, plain empty-index automatic-all, and natural-language scoped-subset flows entirely in the TUI, including planner/generation failure fallbacks.

### Phase 4: Web

1. Add API client methods and `CommitStore`.
2. Add slash completion/dispatch.
3. Implement responsive accessible modal, scope/message child-run reconnection, and commit-operation polling/recovery.
4. Refresh Changes/cross-tab state after staging and verified success.
5. Add frontend unit/component tests and mocked Playwright coverage.

Exit criterion: Web behavior matches the shared Git semantics and survives refresh/reconnect during message generation and final commit execution. Transport details may differ from TUI: Web uses SSE/polling/idempotency, while TUI owns in-process state; parity means identical staging, fingerprint, and verified-commit rules rather than identical lifecycle mechanics.

### Phase 5: Documentation and hardening

1. Update help and guides.
2. Test large/truncated repositories; message-only vs narrowing intent; same-file included/excluded concerns requiring `needs_manual`; invalid/empty/hallucinated scope output; exact-selection exclusion of previously staged paths; partial-file disclosure; temporary-index stale races; intent-to-add; ignored and stale selected paths; unusual filenames across supported platforms; inherited `GIT_INDEX_FILE`; unborn branches; linked worktrees and submodule checkouts with `.git` files; detached `HEAD`; every blocked merge/cherry-pick/revert/rebase state; conflicts; missing Git identity; `#`-prefixed messages; hook rejection/message rewrite/tree rewrite; signing and non-interactive prompt failures; external index races; in-flight idempotency; restart-to-uncertain recovery; and proof that started commits are not automatically SIGKILLed.
3. Run root and frontend quality checks.
4. Review final behavior for consistency between TUI and Web.

## Verification matrix

During implementation, run narrow package/tests after each phase. Before completion:

```sh
gofmt -w <changed-go-files>
make build
GOMODCACHE="$(go env GOMODCACHE)"
TEST_HOME="$(mktemp -d)"; trap 'rm -rf "$TEST_HOME"' EXIT
HOME="$TEST_HOME" XDG_CONFIG_HOME="$TEST_HOME/config" \
  XDG_DATA_HOME="$TEST_HOME/data" XDG_CACHE_HOME="$TEST_HOME/cache" \
  GOMODCACHE="$GOMODCACHE" go test ./...
HOME="$TEST_HOME" XDG_CONFIG_HOME="$TEST_HOME/config" \
  XDG_DATA_HOME="$TEST_HOME/data" XDG_CACHE_HOME="$TEST_HOME/cache" \
  GOMODCACHE="$GOMODCACHE" go vet ./...
npm --prefix frontend run format
npm --prefix frontend run format:check
npm --prefix frontend run lint
npm --prefix frontend run typecheck
npm --prefix frontend test
```

Also run the focused Web E2E spec and any CI-required embed/build checks if frontend assets change. Follow `AGENTS.md` nested-module guidance only if implementation unexpectedly touches an owned nested module.

## Acceptance criteria

The feature is complete when all of the following are true:

1. `/commit` is discoverable and native in TUI and Web.
2. Both clients resolve and act on the same active session checkout/worktree.
3. Any non-empty index immediately presents deliberate **Commit everything** and **Commit staged** choices before an LLM run.
4. Plain `/commit` with an empty index visibly stages everything with `git add -A` without an unnecessary chooser.
5. Commit intent such as `only the checkbox changes and leave widget changes out` can produce an **all**, exact whole-file subset, or honest **needs manual separation** proposal before staging.
6. Every model-proposed path is validated against the current complete status and every subset is shown with adjustable included/excluded checkboxes before mutation.
7. A confirmed subset is built through a temporary index and applied exactly; excluded previously staged paths remain in the working tree rather than entering the commit.
8. Partial staging is never silently preserved, expanded, or removed: each all/staged/scoped behavior is disclosed before action.
9. Checkout identity, born/unborn `HEAD`, exact staged tree, and Git operation state are fingerprinted before message drafting and rechecked before commit.
10. Every merge/cherry-pick/revert/rebase/sequencer state is blocked in v1 rather than silently treated as an ordinary commit.
11. The default `commit-message` agent can be shadowed through normal registry precedence or replaced with `commit.message_agent`, with the same effective resolution in TUI and server.
12. The resolved builtin or custom agent participates through host-owned scope and message phases without configured writes, arbitrary output side effects, or `on_complete` on any terminal path.
13. Missing/invalid configured agents or failed scope planning produce visible recovery choices and preserve manual selection/message entry rather than silently falling back or staging everything.
14. The user can edit the proposed whole-file selection and the final message, retry planning, regenerate, cancel, skip message generation, or write manually.
15. Agent/network failure does not prevent explicit selection and a manual commit.
16. Hooks and signing are honored under documented non-interactive rules, and failures preserve the message and updated status.
17. A stale checkout, operation state, `HEAD`, index, or selected-path status token is rejected rather than committed/staged silently.
18. Web retries and reconnects join/replay persisted child/commit operations and cannot automatically create a second commit; crash-left operations become uncertain.
19. Linked worktrees, submodule checkouts, inherited Git environment, ignored/stale paths, natural-language scope failures, and unusual filenames are covered by integration tests.
20. Success is verified from the resulting commit's parent/tree/message and reported with its actual short SHA and subject; hook-changed trees produce a warning.
21. A started Git commit is not automatically killed by `Esc`, HTTP disconnect, or soft timeout.
22. Documentation explains immediate all-vs-staged behavior, empty-index automatic-all behavior, natural-language scope review, custom message-agent resolution, host safety overlays, and index changes that persist after cancellation.

## Follow-up opportunities

After the native workflow is stable, consider:

- skill-backed message generators beyond the agent registry;
- a generic editable-output review primitive for isolated skills;
- direct index editing beyond the all/staged/follow-request flows;
- hunk-preserving scoped selection for partially staged files;
- amend/fixup modes;
- explicit merge/cherry-pick/revert/rebase continuation modes with operation-aware parents and messages;
- hunk staging through a dedicated Git UI;
- commit templates and project-specific lint hints;
- optional multi-commit planning;
- widget slash-command registration only if several unrelated custom visual workflows justify a broader plugin API.
