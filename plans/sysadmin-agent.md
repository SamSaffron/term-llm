# Built-in `sysadmin` agent

Status: implemented in PR #1168. This document describes the shipped design.

## Goal

`term-llm chat @sysadmin` or `term-llm ask @sysadmin "why is disk full?"` works
from any directory and gives an agent that:

1. knows which host it is on (OS, distro, kernel, init system, package
   managers, container/virtualization, user) without being told;
2. runs read-only diagnostics immediately, without a workspace confirmation;
3. asks before anything else, through the **existing** approval model.

Non-goals: remote hosts, a bespoke tool set (`shell` covers the job), a
sandbox, or a privilege broker. The agent never requests or handles sudo
passwords.

## Principle: no new permission model

The existing model is unchanged and is the whole story:

- allowlisted commands run;
- anything else prompts (Guardian reviews it in auto mode);
- yolo approves everything, for this agent as for every other.

Safety for a host-scoped agent comes from a **narrow allowlist**, not from a
deny tier. A command that could mutate the host or execute a helper must not be
reachable from any allow pattern.

## Components

### `workspace: none` (agent field)

Every other agent binds its launch directory as the proposed primary workspace;
the first file-tool access inside it prompts for workspace confirmation. That is
wrong for a host-scoped agent: started in `~`, it would prompt on
`~/.bashrc` but not on `/etc/fstab`.

`Agent.Workspace` accepts `""`, `auto`, or `none` (validated). With `none`:

- `SessionSettings.SetupToolManager` keeps `BaseDir`/`ShellWorkingDir` but does
  not set `PrimaryWorkspace` (`cmd/session.go`).
- `ToolConfig.UpdateBaseDir` and `LocalToolRegistry.SetBaseDirWithContext` do
  not propose a workspace on rebind (`internal/tools/config.go`, `registry.go`).
- `ApprovalManager.WorkspacePolicy == "none"` skips primary-workspace
  confirmation, including a proposal inherited from a parent manager, without
  mutating the parent (`internal/tools/workspace_capability.go`).
- `FilterToolSpecsForApprovalMode` omits `manage_workspace` (same mechanism as
  yolo; the executor stays registered).
- The policy survives ask resume (`cmd/ask_resume.go`) and runner restored
  settings (`cmd/runner.go`). `agents show` prints it.

`none` only removes the workspace gate. It grants nothing: file paths still go
through read/write grants, prompts, Guardian, or yolo exactly as before.

### Root read grant fix

`ToolPermissions.isPathInDirs` built `resolvedDir + "/"`, so a `/` grant became
the prefix `//` and matched only `/` itself. It now uses the existing
`filepath.Rel` containment helper (`pathWithinWorkspace`), which handles root
and still rejects siblings and `..` escapes.

### `{{host_facts}}` and `{{host_notes}}`

`internal/hostfacts` supplies two template variables, computed only when a
prompt references them (`internal/agents/template.go`).

`{{host_facts}}` renders **stable identity only**:

```text
host: <hostname>  (<os> <kernel>, <arch>)  distro: <name> <version>
init: <init>  pkg: <managers>  container: <c>  virtualization: <v>
user: <user> (uid <n>)  root: <yes|no>
```

No uptime, load, memory, disk, `sudo -n` state, timestamps, or version.
Rationale: session-input refresh (`d344a701`) re-resolves the system prompt on
the first resume of a session in a new process and, if the text differs,
rewrites the stored system row and drops provider continuation. Volatile facts
would do that on every resume, and would present stale measurements as
timeless context. Identity changes only when the machine does, which is when a
refresh is wanted. Live state is measured with allowlisted commands.

Collection:

- Linux: `/etc/os-release`, `/proc/sys/kernel/osrelease`, `/proc/1/comm` plus
  `/run/systemd/system` for init, container markers, `systemd-detect-virt`.
- Darwin: `launchd`, `sw_vers -productVersion`, `sysctl -n kern.osrelease`.
- Other platforms: portable identity only; unknown fields render as `n/a`.
- Package managers: `exec.LookPath` over a fixed list.
- Subprocesses share a 2 s budget. Result is cached for the process lifetime;
  a collection with warnings (timeouts, errors) is not cached, so a later
  render can complete it.

`{{host_notes}}` reads user-maintained notes from
`$XDG_CONFIG_HOME/term-llm/hosts/<short-hostname>.md` (64 KiB cap, inserted as
text, not template-expanded). When absent it tells the model where notes can be
added.

### Agent bundle `internal/agents/builtin/sysadmin/`

- Tools: `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `shell`,
  `view_image`, `ask_user`. No spawning.
- `read.dirs: ["/"]`, `workspace: none`, `agents_md: false`,
  `time_grounding: true`, `max_turns: 300`.
- `search: true`: looking up error messages and upstream docs is core
  sysadmin work. The prompt forbids putting host data into URLs or queries;
  see Known limitations for the residual risk.
- `shell.auto_run: true` with a read-only allowlist:
  identity (`whoami`, `id`, `hostname`, `uname *`, `uptime`, `date`,
  `command -v *`), files (`cat *`, `head *`, `tail -n *`, `wc *`, `stat *`,
  `ls *`, `grep *`), storage (`du *`, `df *`, `findmnt *`, `lsblk *`), processes
  and memory (`ps *`, `pgrep *`, `free *`, `vmstat *`, `nproc`, `lscpu`),
  services (`systemctl status|show|is-active|is-enabled *`, `sv status *`,
  `rc-service * status`, `launchctl list|print *`), packages (`apt list|show *`,
  `apt-cache show|policy|search *`, `dpkg -l*|-L* *`, `pacman -Q* *`,
  `apk info *`, `brew list *`), containers (`docker ps|logs|inspect *`), and
  exact `sudo -n true` / `sudo -n -l`.
- The system prompt covers host scope and absolute paths, how approval works in
  each mode, credential hygiene, method (observe first, back up before editing,
  reload before restart, never lock the user out, no blocking commands), and
  privilege (no sudo passwords; hand the command back instead).

#### Allowlist rule and exclusions

Pattern semantics: words match individually (doublestar); only a final
standalone `*` means "any remaining arguments", and those arguments are never
inspected. So `systemctl status*` does not match `systemctl status nginx`, and a
prefix such as `journalctl *` admits every later option.

| Excluded | Reason |
|---|---|
| `journalctl *`, `sudo -n journalctl *` | `--vacuum-*`, `--rotate`, `--flush`, `--setup-keys` mutate |
| `sudo -n <cmd> *` | privileged reads should prompt |
| `printenv *` | exposes API keys and other secrets from the environment |
| `rg *` | `--pre` executes a helper |
| `file *` | `-C -m` writes a compiled magic file |
| `apt-cache *` | `gencaches` writes |
| `rpm -q* *` | `--pipe` executes a command |
| `pacman -Si* *` | `-Siy` refreshes databases |
| `dnf list *`, `dnf info *` | `--setopt` loads plugins / redirects logs |
| `brew info *` | `--github` launches a browser |
| `podman ps *`, `podman logs *` | profiling flags write files |

Adding a pattern requires checking every option the prefix can reach.

## Tests

- `internal/hostfacts`: Linux fixtures, probe budget, deterministic render with
  no volatile fields, process-lifetime cache with degraded-collection retry,
  host key sanitisation, notes path/literal content.
- `internal/agents`: lazy host variables, expansion, builtin sysadmin config
  (workspace none, search on, no spawning, prompt variables), builtin tables.
- `internal/tools`: root read grant, workspace none on rebind, inherited parent
  proposal not prompted or mutated, `manage_workspace` omitted, and
  `TestBuiltinSysadminShellAllowlist` covering intended matches and every
  excluded escape above.

## Rejected alternatives

- **`shell.always_confirm` deny tier** (human prompt that overrides allowlists,
  caches, Guardian, and yolo). Built, then rejected: it made yolo not mean yolo
  and required security-sensitive wrapper/flag parsing to compensate for an
  over-broad allowlist.
- **Change journal** (`journal: true`, JSONL of executed commands). Not policy,
  but new framework without a demonstrated need.
- **`search: false` by default.** Closes the fetch path but removes web lookup,
  which the agent needs; rejected in favour of search on plus documentation.
- **Volatile host snapshot** in the system prompt or a start-of-conversation
  developer message. The former churns prompts on resume; the latter needs a
  new agent flag threaded through every conversation-start path to save one
  `df`.

## Known limitations

- Host-wide unprompted reads mean the model and stored transcripts can see
  sensitive files, process arguments, and container environments
  (`docker inspect`). That is the agent's job.
- Search is on and `read_url` has no approval gate, so unprompted host reads
  plus an unprompted fetch form a possible injection-to-exfiltration path. The
  mitigations are the prompt rule, keeping `printenv` off the allowlist, and
  `--no-web-fetch` (`chat`/`ask`/`loop`) for users who want `web_search`
  without `read_url`. Closing
  it structurally would need an approval gate on `read_url`, a separate change
  for all agents.
- Anything outside the allowlist prompts, including compound commands with
  redirections (`sudo -n true >/dev/null && …`). Expected behaviour.
- The Go complexity ratchet flags small increases in the touched functions;
  resolved separately.
