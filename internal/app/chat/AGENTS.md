# Chat application

`Run(ctx, args, Options, Dependencies)` owns orchestration, runtime construction,
UI wiring, handover/session switches and terminal cleanup. Cmd binds/captures
flags and supplies `ExecReload`; it remains responsible for process re-exec.
No production or test Cobra/pflag/cmd imports. Chat may import the host, never
host -> chat. Keep runtime/session fields private; do not export internals for
fixtures or add replacement-run hooks.

Pointer-receiver callbacks must observe current invocation/model/agent policy.
Retain terminal/reload authority independently from renderer output authority,
TTY cleanup, actual handover approval carry and late session-runtime lookup.
Preserve terminal gate -> restore -> config -> session/settings -> approval;
config reload on later relaunches is intentional. Pass session/memory overrides
to all child runners and prompt/skills refreshes. Nil-command helpers migrated
with explicit zero Changed state and discard writers; do not infer explicitness
from bound flag values or fix the historical `system` lookup here.

Move real runtime tests with code, keeping Cobra contracts in cmd. Source-output
guards scan this directory, including forced-exit paths. Validate focused tests,
race with host/child/main-run lifecycle, and full check-changed --all.
