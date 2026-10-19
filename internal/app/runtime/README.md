# Application run runtime

This package owns the parts of a run that are shared by CLI runners and serve:
model selection/admission (`SelectRunModel`, `ModelPolicyForSession`), retriable
model output (`RunnerOutput`), and detached-child lifecycle (`AgentHostOwner`).

`AgentHostRuntime` is deliberately limited to outstanding-child state, drain,
shutdown, and close. The serve runtime implements it without making the runtime
package depend on HTTP routes, session preparation, or `cmd`. An owner keeps the
runtime and its store alive until detached children finish, including during
shutdown. `RunnerOutput` commits text and reasoning by provider turn and
retracts discarded attempts without treating usage events as commit boundaries.

`RepairChildResumeHistory` persists synthetic error results for interrupted
child tool calls before continuation, so the next provider turn sees a valid
transcript. `bootstrap/` owns config loading/setup, provider metadata
registration, theme, search preference, and debug logger creation.
`approval/` owns approval policy
resolution, persistence conversion, and diagnostics. `progressive/` owns the
progress tracker, work/finalization budget, and progressive engine loop; it
uses the core jobs exit-reason constants rather than depending on serve jobs.
Cobra's changed-flag interpretation and guardian callback installation remain
in `cmd` for now; command implementations not yet extracted use temporary
compatibility adapters.
