# Guaranteed all-subagents-finished wake

Status: implemented

Implemented for web, persisted terminal chat (including background sessions),
and nested children. This revision incorporates the astra/argon design reviews.
The shared predicate, generation admission, retries, migration, and all three
execution paths have regression coverage. See implementation notes below for
resource retention and delivery-budget details.

## Contract

The host, not the model, owns the failsafe. There is no tool argument to turn
it off. For a parent with no outstanding direct children and at least one
terminal result not collected or previously notified, admit a host-authored
lifecycle turn when the parent is idle. The model may ignore the notice.
Collection by spawn_agent, wait_agent, or continue_agent suppresses redundant
delivery. Acknowledgement is per child execution generation, not merely ID.
Queued and approval-blocked children are outstanding. A known-dead owner is
interrupted, not running; an unknown owner must not be declared dead.

Check this state after every child finishes and every parent turn settles,
including failed turns and host-owned turns; reconcile periodically to recover
missed signals and external-owner changes. Do not rely solely on a running-count
transition. Revalidate offered generations and the barrier under durable turn
admission. Acknowledge before releasing the turn lease. Preserve human stop,
archive, and deletion semantics: these are explicit exceptions, not model
opt-outs. Model cancel_agent does not suppress lifecycle delivery.

This is at-least-once delivery under a functioning host/store/provider, not an
exactly-once inference claim. Failed provider execution and persistence remain
pending and retry with bounded backoff, cancellation-aware shutdown, and visible
failure information. No implementation can force a successful inference from an
unavailable provider or prove a remote host dead without evidence.

The existing notify_when_done:true web option remains an additional eager
per-child notification, independent of the unconditional all-done barrier.
No arbitrary consecutive-wake cap is introduced: new child generations justify
new wakes, and acknowledgment prevents a model ignoring a notice from waking
again for the same event. Existing child turn/depth budgets remain enforced.

## Shared lifecycle support

internal/tools owns:

- PendingAgentBarrier(ctx, store, parent): all direct children participate,
  regardless of spawn surface; return unseen terminal generations only when
  no child is outstanding. Include process-local entries and durable records.
- AgentLifecycleMessage(events, allDone): metadata-only marked synthetic input;
  never promote child output into instruction authority. Reuse the existing
  provider-compatible internal-prompt provenance rather than inventing a
  provider-specific trailing developer message.
- ContextWithAgentLifecycleRestricted: block spawning/continuation at execution
  for budget-exhausted nested delivery, not merely hide tool schemas.
- SpawnAgentTool.WaitForChildren(ctx, parent): a non-draining wait on existing
  process-local completion channels, not a lossy signal subscription.

Terminal notification follows durable completion and safe completion-channel
ordering. A failed terminal write must remain recoverable, not silently strand
a durable running row. Store queries count ALL direct children. Surface/origin
is routing provenance, not barrier membership. Transactional admission carries
AgentEvents and AgentBarrier, verifies parent existence and event generations,
and rejects a barrier if new outstanding work appeared.

Historical unnotified non-opt-in completions are acknowledged on schema upgrade
so deploying does not wake every old session. Preserve existing opt-in events,
host_restarted events, and in-flight rows.

## Web

Reuse the existing per-session wake worker, runtime restoration, turn admission,
and synthetic prompt path. Distinguish eager and barrier eligibility. Retry
real failures; no retry/error banner for events concurrently collected, explicit
stop, archive/deletion, or a barrier blocked by live children. Periodic discovery
must still revisit blocked parents. Acknowledgment failure retries acknowledgment
before another inference where possible. A settled host wake may itself have
spawned children and must recheck after acknowledgment.

## Terminal chat

Use MainRunManager and durable leases, not the currently displayed model's
streaming flag as the sole execution authority. Wake ownership survives switching
the displayed session. Retain/reconstruct session execution resources until the
barrier is resolved; the view subscribes to normal background-run events and
attention. Never consume or overwrite the composer's draft with synthetic input.

Pass offered generations into admission. Refresh stale durable transcript state
before retrying, rather than repeatedly submitting an obsolete transcript. Run
acknowledgment in finalization before releasing ownership; StreamEventDone can
arrive before Finalize and is not proof that a turn is settled. User cancellation
suppresses the stopped turn's lifecycle events. Detached approvals continue via
the existing session UI transport.

## Nested children

Keep the child entry running until its own direct children finish and any owed
lifecycle turn settles. Drive a shared single-turn operation above both the bare
runtime path and hosted childRunHandle.Execute path. Retain runtime ownership
across hosted turns; keep conversation history on follow-ups.

Wait without setting agentManager.draining. Additional model calls consume the
remaining ordinary child turn allowance, never reset it each turn. If the child
used its final ordinary call to spawn work, still grant a bounded lifecycle-only
allowance to collect/report results, with further spawn/continue disabled. This
is delivery reserve, not renewed autonomous work permission. Preserve turn_limit
classification when the ordinary budget is exhausted.

Reported text is the final report, not concatenated interim answers. Aggregate
usage/media across turns and run completion hooks once after the complete loop.
Cancellation interrupts waits and propagates to descendants. Keep hosted response
identities, approvals, and fencing intact on every inference.

Host lifecycle turns may continue completed or turn_limit children within their
budgets. Interrupted/cancelled work still requires fresh human authorization;
do not give a forbidden continue_agent hint in that context.

## Verification

Regression cases: fan-out finishes out of order; finishes during a busy turn;
all results already collected; notice ignored; wake spawns another child; failed
user/wake turn; retry and acknowledgment failure; human stop; cross-surface
membership; admission race with new child; generation race; dead/unknown owners;
upgrade behavior. TUI additionally covers draft preservation, finalized-lease
ordering, background session switching, and shutdown. Nested additionally covers
forgotten waits, hosted runtime retention, final output, and final-budget spawn.

Use scripted providers only. Run focused tests, make check-changed,
make check-changed ARGS=--all, make plans-lint, and make complexity. Record any
unavailable checks and do not mark this plan implemented prematurely.

## Implementation notes

- Empty detached TUI sessions release their resources. Sessions adopted with
  active/pending work retain provider/store/approval resources until host
  shutdown, even after their wake chain settles; this avoids racing delivery
  retries and remote completions during teardown.
- Nested delivery has four restricted reserve calls after the ordinary budget.
  A typed turn-limit result from a lifecycle inference counts as delivered:
  the model received the notice and its tool results were persisted. It is
  acknowledged, its turn-limit classification is preserved, and any new child
  is awaited before another notice. Provider outages without inference do not
  consume this reserve. Completion hooks run once after the whole child run.
- Local entries are scoped to their durable registry; unrelated temporary or
  in-memory stores with identical parent strings cannot contaminate a barrier.
- Completed/turn-limit continuations are permitted in host turns; interrupted
  or cancelled work still requires a subsequent human-authorized turn.
- Jobs-v2 progressive execution is not a delegated-child entry path and does
  not use this barrier. No queue_agent/ask/Telegram wake behavior was added.

- TUI human-stop intent belongs to the execution, not its current view. Stops
  target a specific run ID, so navigation retains authority and stale views
  cannot stop replacements. Successful inference retries acknowledgment only;
  failed human suppression retains generation-scoped intent across lease loss.
- Synthetic notices are persisted inside admitted execution, never speculative
  preparation. Hosted follow-up admission errors cannot roll back prior work;
  an initial admission refusal never enters the child lifecycle loop.
- Unanswered TUI retries reuse the trailing trusted lifecycle notice for the
  same sorted child execution generations, preserving its durable anchor without
  extra transcript/UI rows. Matching human-authored text is never reused.
- Stop intent set before run registration cancels the detached execution before
  any inference; execution checks intent again before notice persistence and
  provider invocation even if cancellation is queued behind publication. A
  cancelled barrier still settles suppression. Archive races during preparation
  retire quietly, like deletion.
- Deleted parents terminate delivery quietly, and lifecycle result hints respect
  recovery/reserve continuation restrictions. Host-independent TUI delivery
  persistence lives in `internal/tui/chat/agentbarrier` to keep the owning chat
  package within its size ratchet.

Verification: all 184 non-scratch root Go packages passed tests and vet;
non-scratch buildable packages compiled; lifecycle regressions passed race
coverage. Frontend checks, nested modules, docs generation, plan lint and the
complexity ratchet passed. The stock changed/full checkers also traverse the
pre-existing ignored tmp/inspectdemo/seed program, which references removed
llm types and fails build/vet/test; that scratch source was left untouched.
