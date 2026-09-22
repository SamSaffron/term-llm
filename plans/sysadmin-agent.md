# Built-in `sysadmin` agent: host-scoped debugging and administration

Status: design draft (Jarvis, 2026-09-22) followed by a grounded implementation
plan from an Astra planner pass (`chatgpt:gpt-6-astra-medium`) against
`aace792b`. **Where the grounded plan contradicts the draft, the grounded plan
wins**; the draft is kept for intent and for the full prompt text.

## Goal

`term-llm chat @sysadmin` (or `ask @sysadmin "why is disk full"`) from any
directory gives you an agent that:

1. already knows what box it is on (distro, init system, package manager,
   privilege situation, resource headroom) without being told;
2. runs read-only diagnostics immediately, without a workspace confirmation
   dance or per-command prompts;
3. asks before anything that mutates the host, and refuses to auto-run the
   lockout/wipe class even in yolo;
4. leaves a durable record of what it changed.

Non-goals: remote hosts (SSH fan-out), a bespoke tool zoo (`shell` covers the
job), a full sandbox, a privilege-escalation broker. The agent never holds or
prompts for sudo passwords.

## Why the existing agent model does not fit

Every current agent is implicitly "a thing that works on a project in a
directory". Verified consequences for a host-scoped agent:

- **Workspace confirmation.** `LocalToolRegistry` binds the launch directory as
  the proposed primary workspace (`internal/tools/registry.go:77`). The first
  file-tool access *inside* that directory outside yolo goes through
  `ensurePrimaryWorkspaceAccess` (`internal/tools/workspace_capability.go:374`)
  and prompts a human. Paths outside the proposal skip that gate and fall through
  to `read.dirs`/prompting (`approval.go:1204-1225`). A sysadmin session started
  in `~` would therefore prompt on `cat ~/.bashrc` but not on `/etc/fstab`,
  which is exactly backwards from what the user expects.
- **Shell policy is allowlist + prompt, with no "never auto" tier.** Yolo
  short-circuits every shell check (`approval.go:1453`). There is no way for an
  agent to say "this class of command must always be confirmed by a human".
- **Compound commands are already handled.** `matchAnyShellPattern`
  (`approval.go:2165`) splits `;`/`&&` sequences and pipelines and accepts safe
  pipe targets, so a read-only allowlist covers `journalctl -u nginx | tail -50`.
- **Bundled resources are `.md/.yaml/.env` only** (`embedded.go:168`), so a
  shell-script probe cannot ship inside the builtin bundle. The probe must be Go.
- **System prompt is the right carrier for host facts.** `{{...}}` template
  variables are computed lazily only when referenced
  (`template.go:108-122`) and re-rendered on reload/compaction. For host facts
  a re-render is desirable (fresh headroom numbers), unlike time grounding,
  which needs an immutable developer message and touches every platform start
  path (TUI, web, Telegram, jobs). One template hook is the smallest coherent
  change.

## Design

### 1. Agent bundle `internal/agents/builtin/sysadmin/`

`agent.yaml` (draft):

```yaml
name: sysadmin
description: "Debug and administer the local machine: services, logs, disk, network, packages"
time_grounding: true
skills: "all"
agents_md: "false"        # host work is not project work
workspace: none           # NEW: do not bind cwd as the primary workspace
journal: true             # NEW: record executed commands and file writes

tools:
  enabled: [read_file, write_file, edit_file, glob, grep, shell, view_image, ask_user]

read:
  dirs: ["/"]             # host-wide read for file tools; writes still prompt per path

shell:
  auto_run: true
  allow:
    # identity / environment
    - "pwd"
    - "whoami"
    - "id"
    - "id *"
    - "hostname"
    - "hostname *"
    - "hostnamectl"
    - "uname *"
    - "uptime"
    - "date"
    - "env"
    - "printenv*"
    - "which *"
    - "type *"
    - "command -v *"
    - "sudo -n true"
    - "sudo -n -l"
    - "sudo -ln"
    # files (read-only)
    - "cat *"
    - "head *"
    - "tail -n *"
    - "tail -c *"
    - "wc *"
    - "stat *"
    - "file *"
    - "ls *"
    - "du *"
    - "df *"
    - "findmnt*"
    - "lsblk*"
    - "blkid"
    - "mount"
    - "grep *"
    - "rg *"
    # processes / resources
    - "ps *"
    - "pgrep *"
    - "free *"
    - "vmstat *"
    - "iostat *"
    - "nproc"
    - "lscpu"
    - "lsmem"
    - "lspci*"
    - "lsusb*"
    - "lsof *"
    - "sysctl -a"
    - "sysctl -n *"
    - "ulimit -a"
    - "dmesg*"
    # network (read-only forms only; `ip addr add`/`ip link set` are mutations)
    - "ss *"
    - "ip addr"
    - "ip addr show*"
    - "ip -br addr"
    - "ip link"
    - "ip link show*"
    - "ip -br link"
    - "ip route"
    - "ip route show*"
    - "ip neigh"
    - "ip neigh show*"
    - "getent *"
    - "dig *"
    - "host *"
    - "nslookup *"
    - "ping -c *"
    - "resolvectl status*"
    - "nmcli device*"
    - "nmcli connection show*"
    # systemd (read-only subcommands)
    - "systemctl status*"
    - "systemctl show*"
    - "systemctl cat*"
    - "systemctl list-units*"
    - "systemctl list-unit-files*"
    - "systemctl list-timers*"
    - "systemctl list-dependencies*"
    - "systemctl is-active*"
    - "systemctl is-enabled*"
    - "systemctl is-failed*"
    - "systemctl --failed*"
    - "journalctl *"        # --vacuum-*/--rotate/--flush are caught by always_confirm
    - "loginctl list*"
    - "timedatectl"
    - "timedatectl status"
    # runit / openrc / launchd
    - "sv status *"
    - "rc-status*"
    - "rc-service * status"
    - "launchctl list*"
    - "launchctl print*"
    # macOS
    - "sw_vers*"
    - "system_profiler *"
    - "diskutil list*"
    - "diskutil info*"
    - "vm_stat"
    - "netstat -an*"
    - "top -l 1*"
    - "log show*"
    # packages (query forms)
    - "pacman -Q*"
    - "pacman -Si*"
    - "pacman -Ss*"
    - "pacman -Fl*"
    - "checkupdates"
    - "apt list*"
    - "apt show*"
    - "apt-cache *"
    - "dpkg -l*"
    - "dpkg -L*"
    - "dpkg -S*"
    - "dnf list*"
    - "dnf info*"
    - "dnf check-update*"
    - "rpm -q*"
    - "apk info*"
    - "apk list*"
    - "zypper se*"
    - "zypper info*"
    - "brew list*"
    - "brew info*"
    - "brew services list"
    - "brew outdated*"
    - "flatpak list*"
    - "snap list*"
    - "nix-env -q*"
    # containers
    - "docker ps*"
    - "docker images*"
    - "docker logs*"
    - "docker inspect*"
    - "docker stats --no-stream*"
    - "docker system df*"
    - "docker compose ps*"
    - "docker compose logs*"
    - "podman ps*"
    - "podman logs*"
    # privileged read-only (only meaningful when sudo -n works)
    - "sudo -n journalctl *"
    - "sudo -n dmesg*"
    - "sudo -n cat *"
    - "sudo -n tail -n *"
    - "sudo -n ls *"
    - "sudo -n du *"
    - "sudo -n ss *"
    - "sudo -n lsof *"
    - "sudo -n systemctl status*"
    - "sudo -n sv status *"
    - "sudo -n iptables -S*"
    - "sudo -n iptables -L*"
    - "sudo -n nft list*"
    - "sudo -n ufw status*"
    - "sudo -n fdisk -l*"
    - "sudo -n smartctl -a *"
    - "sudo -n smartctl -H *"
    # git (for /etc under version control, dotfiles, etc.)
    - "git status*"
    - "git log*"
    - "git diff*"
  always_confirm:           # NEW: human confirmation required even in yolo/auto
    - "mkfs*"
    - "mkfs.*"
    - "dd *"
    - "wipefs*"
    - "shred *"
    - "parted *"
    - "sgdisk *"
    - "sfdisk *"
    - "fdisk /*"
    - "cryptsetup *"
    - "lvremove *"
    - "vgremove *"
    - "pvremove *"
    - "zpool destroy*"
    - "btrfs device delete*"
    - "rm -rf /"
    - "rm -rf /*"
    - "rm -rf ~"
    - "rm -rf ~/"
    - "rm -rf ~/*"
    - "rm -rf $HOME*"
    - "rm -rf .*"
    - "rm -rf *"          # any recursive force-delete prompts; fine, this agent should rarely rm -rf
    - "chmod -R *"
    - "chown -R *"
    - "chattr *"
    - "iptables -F*"
    - "iptables --flush*"
    - "iptables -X*"
    - "iptables -P *"
    - "nft flush*"
    - "ufw disable"
    - "ufw reset"
    - "firewall-cmd --panic-on"
    - "systemctl stop ssh*"
    - "systemctl disable ssh*"
    - "systemctl mask *"
    - "systemctl isolate *"
    - "systemctl daemon-reexec"
    - "journalctl --vacuum*"
    - "journalctl *--vacuum*"
    - "journalctl --rotate*"
    - "journalctl --flush*"
    - "passwd *"
    - "usermod *"
    - "userdel *"
    - "groupdel *"
    - "visudo*"
    - "shutdown*"
    - "reboot*"
    - "poweroff*"
    - "halt*"
    - "init 0"
    - "init 6"
    - "systemctl reboot*"
    - "systemctl poweroff*"
    - "systemctl halt*"
    - "systemctl kexec*"
    - "kill -9 -1"
    - "kill -KILL -1"
    - "killall *"
    - "pkill -9 *"
    - "crontab -r*"
    - "truncate *"
    - "swapoff -a"
    - "umount -a*"
    - "pacman -Rns*"
    - "pacman -Rdd*"
    - "pacman -Sc*"
    - "pacman -Scc*"
    - "apt purge*"
    - "apt autoremove*"
    - "apt-get purge*"
    - "apt-get autoremove*"
    - "dnf remove*"
    - "dnf autoremove*"
    - "brew uninstall*"
    - "docker system prune*"
    - "docker volume prune*"
    - "docker volume rm*"
    - "docker compose down*"
    - "nixos-rebuild *"
    - "grub-install*"
    - "update-grub*"
    - "efibootmgr *"
    - "modprobe -r *"
    - "rmmod *"

search: true
max_turns: 300
```

`always_confirm` matching must ignore a leading privilege wrapper: `sudo`,
`sudo -n`, `sudo -E`, `sudo -u X`, `doas`, `run0`. Otherwise `sudo dd ...`
slips past the class. See §3.

The `read: dirs: ["/"]` grant is host-wide read for `read_file`/`grep`/`glob`
(`cmd/session.go:213` already maps agent read dirs into `ToolPermissions`).
Writes still go through path approval, which prompts per file/directory (the
web and TUI approval dialogs offer once/file/directory choices). That is the
desired granularity for editing `/etc/...`.

### 2. `system.md` (full draft)

```markdown
You are the sysadmin agent: a calm, methodical operator who debugs and
administers the machine term-llm is running on.

User: {{user}}. Home: {{home}}. Platform: {{platform}}.

# Host

The following snapshot was taken when this prompt was rendered. Treat it as
ground truth for what kind of machine this is; re-check anything that can
change (load, disk, services) with a command before acting on it.

{{host_facts}}

{{host_notes}}

# Scope

You operate on the whole host, not on a project. The shell working directory is
irrelevant; always use absolute paths and never rely on `pwd` or relative paths
between commands. Do not treat the launch directory as a workspace and do not
look for project instruction files.

Your tools: `shell` for everything diagnostic and administrative; `read_file`,
`grep`, `glob` for configuration and logs; `edit_file`/`write_file` for
configuration changes (each write prompts the user); `ask_user` when a decision
is genuinely theirs.

# Method

1. **Observe before you change anything.** Reproduce or confirm the symptom
   with a command. Read the unit status and the last log lines before
   restarting a service. Check `df`/`du` before deleting anything.
2. **Narrow fast.** Use the USE method for resource problems (utilisation,
   saturation, errors) and the RED method for services (rate, errors,
   duration). Prefer one decisive command over five vague ones.
3. **Use the host's own conventions.** The snapshot tells you the init system
   and package manager; do not run `systemctl` on a runit box or `apt` on
   Arch. On macOS use `launchctl`, `log show`, `brew`.
4. **State the plan before mutating.** Say what you will change, why, and how
   to undo it. Then do it. Do not restart, reinstall, or delete as a first
   move.
5. **Back up before editing.** Before editing any file outside your own home
   directory: `cp -a <file> <file>.bak.YYYYMMDD-HHMM`. Mention the backup path.
6. **Reload before restart.** Prefer `reload`/`try-reload-or-restart` where the
   service supports it. Validate configuration first (`nginx -t`,
   `sshd -t`, `visudo -c`, `apachectl configtest`, `named-checkconf`).
7. **Never lock yourself out.** Changes to sshd, firewalls, network interfaces,
   PAM, sudoers, and disk layout require an explicit plan, a rollback step,
   and user confirmation. Keep an existing SSH session open in your plan
   whenever the change touches remote access.
8. **Do not wait on blocking commands.** No `tail -f`, `journalctl -f`,
   `top`, `less`, `watch`, or interactive editors. Use `-n`, `--no-pager`,
   `-b`, `--since`.
9. **Output hygiene.** Add `--no-pager` to `systemctl`/`journalctl`, cap
   output with `head`/`tail -n`, and prefer machine-readable flags (`-j`,
   `--json`, `-P`, `--no-legend`) when parsing.

# Privilege

- Read-only diagnostics run as the current user. Passwordless `sudo -n` is
  reported in the snapshot; use `sudo -n` only when it is available and the
  command genuinely needs root.
- If a privileged command fails with a password prompt, do not retry and do
  not ask for the password. Show the exact command and ask the user to run it
  or to grant passwordless sudo for that command.
- Package installation, service enable/disable, user/group changes, firewall
  and mount changes are mutations: describe, confirm, then run.
- Destructive classes (filesystem creation, raw disk writes, recursive
  ownership/permission changes, firewall flushes, SSH shutdown, reboots) are
  always confirmed by the human, in every approval mode. Do not try to route
  around that with `sh -c`, `xargs`, `find -exec`, aliases, or scripts.

# Record keeping

term-llm journals every command and file write you make on this host. At the
end of any task that changed the machine, summarise: what changed (paths,
packages, units), backup locations, and the exact undo steps. If the user
keeps host notes, offer to record durable facts you discovered (unusual
layouts, "do not touch" services, hardware quirks) in the host notes file
shown above rather than repeating the discovery next time.

# Style

Direct and specific. Lead with the finding, then the evidence, then the fix.
Quote the exact lines from logs that matter, not whole dumps. Numbers with
units. If you are guessing, say so and say what would confirm it.
```

The `embedded_test.go` "capability-aware directory guidance" table currently
requires every shell-capable builtin to contain the relative-path guidance
sentence. That sentence is wrong for this agent. Add an explicit exception
entry for `sysadmin` asserting the *absolute-path* guidance instead.

### 3. Core changes

#### 3a. `{{host_facts}}` and `{{host_notes}}` template variables

New package `internal/hostfacts`:

```go
type Facts struct {
    Hostname, OS, Distro, DistroVersion, Kernel, Arch string
    Init            string   // systemd, runit, openrc, launchd, s6, sysvinit, unknown
    PackageManagers []string // present on PATH, in preference order
    User            string
    UID             int
    IsRoot          bool
    SudoNoPassword  *bool    // nil = sudo missing or probe skipped
    Container       string   // "", docker, podman, lxc, wsl
    Virtualization  string   // from systemd-detect-virt when available
    UptimeSeconds   int64
    Load1, Load5, Load15 float64
    MemTotalBytes, MemAvailableBytes uint64
    Disks           []DiskUsage // "/", "/home", "/var" when distinct mounts
    Shell           string
    TermLLMVersion  string
    CollectedAt     time.Time
    Warnings        []string // probes that failed, one line each
}

func Collect(ctx context.Context) Facts          // bounded by ctx; never panics
func (f Facts) Render() string                   // compact fenced block, ~1 KB
```

Sources, all with a hard budget (2 s total, like `gitProbeTimeout`):

- `/etc/os-release`, `unix.Uname`, `/proc/1/comm` (or `launchctl` on darwin),
  `/proc/uptime`, `/proc/loadavg`, `/proc/meminfo`, `unix.Statfs`, `/.dockerenv`,
  `/run/.containerenv`, `/proc/1/cgroup`, `/proc/version` (`microsoft` → WSL).
- `exec.LookPath` for pacman/apt/dnf/zypper/apk/brew/nix/port/flatpak/snap.
- `sudo -n true` with a 1 s timeout, stdin closed, `SUDO_ASKPASS` unset. Skip
  when running as root or when `sudo` is absent.
- `systemd-detect-virt` when present, 1 s timeout.
- darwin: `sw_vers -productVersion`, `sysctl -n hw.memsize`, `vm_stat` skipped
  (approximate is fine; say "n/a").

Rendered form (deterministic key order, so tests can assert on it):

```
host: jarvis  (Linux 7.2.6-arch2-1, x86_64)  distro: Arch Linux
init: runit  pkg: pacman  container: docker
user: agent (uid 1000)  root: no  sudo -n: yes
up: 3d 4h  load: 0.42 0.51 0.48  mem: 12.1G avail / 31.3G
disk: / 71% used (58G free)  /home 71% used (58G free)
term-llm: v0.9.51  collected: 2026-09-22T09:14:03+10:00
warnings: systemd-detect-virt not found
```

Wiring in `internal/agents/template.go`: add `HostFacts`, `HostNotes` to
`TemplateContext`; compute in `newTemplateContextInDir` only when
`vars["host_facts"]`/`vars["host_notes"]` are referenced (same pattern as
`agents`/`handover_dir`); add both to the expansion switch. Cache the collected
facts process-wide for 60 s so repeated prompt renders (web reload, compaction,
subagent spawns) do not re-run probes.

`{{host_notes}}` reads `$XDG_CONFIG_HOME/term-llm/hosts/<hostname>.md` when it
exists and renders it under a `# Host notes (user-maintained)` heading;
otherwise it renders a one-line hint naming the path. Hostname is lowercased
and taken from `os.Hostname()` with domain stripped.

#### 3b. `workspace: none`

Agent field `Workspace string` (`""`/`"auto"` default, `"none"`). When `none`:

- `cmd/session.go` does not set the primary workspace value on the tool
  config, so `registry.go:77` never binds a proposal and
  `ensurePrimaryWorkspaceAccess` is a no-op for every path.
- `manage_workspace` is not exposed (reuse the yolo omission path from PR #1103).
- Shell `ShellDir()` still resolves to the launch directory so relative
  `working_dir` values keep working; the prompt tells the model to use absolute
  paths.
- Validate: only `""`, `auto`, `none`.

The planner must confirm which of `cmd/session.go`, `cmd/runner.go`
(jobs/serve), `cmd/chat.go`, `cmd/ask.go`, and `cmd/loop.go` set the primary
workspace and cover all of them; a sysadmin session over web/Telegram/jobs
must behave identically.

#### 3c. `shell.always_confirm`

`ShellConfig.AlwaysConfirm []string` (patterns validated like `allow`), mapped
into `ToolPermissions.ShellAlwaysConfirm`. In `checkShellApprovalWithContext`:

1. Normalise the command: split into sequence and pipe parts exactly as
   `matchAnyShellPattern` does; for each part, strip one leading privilege
   wrapper (`sudo` plus its short flags/`-u user`, `doas`, `run0`).
2. If any part matches an `always_confirm` pattern, skip the yolo fast path,
   skip static allowlists, session caches, project approvals, and Guardian, and
   go straight to the human prompt. `ProceedAlways`/pattern remembering is
   disabled for these prompts: the answer is once-only.
3. If no prompt transport exists (jobs, MCP, non-TTY), deny with
   `ErrPermissionDenied` and a message that names the matched pattern.

Order of precedence becomes: `always_confirm` > yolo > exact scripts > allow
patterns > session/guardian/project approvals > prompt. Also apply the check to
`CheckSharedShellApprovalWithContext` so the shared terminal cannot bypass it.

`agents.Merge` appends `pref.ShellAlwaysConfirm` (new config preference field
under `agents.preferences.<agent>.shell_always_confirm`) so users can extend but
not shrink the list without shadowing the agent.

#### 3d. `journal: true`

Agent field `Journal bool`. When set, the tool registry attaches a
`journal.Writer` that appends JSON lines to
`<appdata.GetDataDir()>/journal/<hostname>.jsonl` (0600, directory 0700):

```json
{"ts":"2026-09-22T09:20:11+10:00","session":"20260922-...","agent":"sysadmin",
 "kind":"shell","cwd":"/home/agent","command":"systemctl restart nginx",
 "approval":"prompt","exit_code":0,"duration_ms":412}
{"ts":"...","session":"...","agent":"sysadmin","kind":"write","tool":"edit_file",
 "path":"/etc/nginx/nginx.conf","bytes":4120}
```

`approval` values: `allowlist`, `script`, `session`, `project`, `guardian`,
`prompt`, `always_confirm`, `yolo`. Shell entries are written after execution
(exit code known); write entries after a successful write. Journal write
failures are logged and never fail the tool call. Read-only tools are not
journaled. The journal is append-only; no CLI in this change (a later
`term-llm journal` subcommand is an obvious follow-up).

### 4. Registration and docs

- `internal/agents/embedded.go`: add `"sysadmin"` to `builtinAgentNames`.
- `internal/agents/embedded_test.go`: table entries (tools list present,
  max_turns 300, has shell allow, search on, no spawn), absolute-path guidance
  exception, always_confirm non-empty, `journal` and `workspace: none` set.
- `docs-site/content/guides/agents.md`: table row, "host-scoped agents"
  subsection documenting `workspace: none`, `journal`, `shell.always_confirm`,
  `{{host_facts}}`, `{{host_notes}}`, and the host notes path.
- `docs-site/content/reference/built-in-tools.md`: shell approval precedence
  including `always_confirm`.
- `docs-site/content/reference/configuration.md`: `agents.preferences.<agent>.shell_always_confirm`.

### 5. Tests

- `internal/hostfacts`: unit tests with fake `/proc` and `/etc/os-release`
  fixtures via an injectable root path; init detection table (systemd, runit,
  openrc, launchd, unknown); render golden; budget exhaustion returns partial
  facts with warnings, never an error; `sudo` probe skipped as root.
- `internal/agents/template_test.go`: `{{host_facts}}`/`{{host_notes}}` are
  only computed when referenced; notes file lookup honours `XDG_CONFIG_HOME`;
  missing notes renders the hint.
- `internal/tools`: `always_confirm` beats yolo; `sudo -n dd ...` and
  `doas mkfs.ext4 ...` match; a pipeline whose second stage matches still
  prompts; allowlisted command that also matches `always_confirm` prompts; no
  prompt transport → denied with pattern in the error; remembered-pattern
  choice is refused for always-confirm prompts; shared terminal path covered.
- `cmd`/registry: `workspace: none` results in no primary workspace proposal
  and no `manage_workspace` tool spec; a read under cwd does not prompt when
  `read.dirs` covers it.
- Journal: entries for prompted shell, allowlisted shell, and `edit_file`;
  read tools produce nothing; unwritable journal dir does not fail the tool.
- Builtin agent tests as in §4.

### 6. Verification (manual, before PR)

From a directory that is not a repo, with `approval.default_mode` unset:

```sh
term-llm ask @sysadmin "what init system is this and which services failed"
term-llm chat @sysadmin      # then: "why is /home at 71%?" → expects du/df without prompts
term-llm chat --yolo @sysadmin  # then ask it to run `dd if=/dev/zero of=/tmp/x bs=1M count=1` → expects a prompt anyway
```

Confirm: no workspace confirmation prompt appears; `{{host_facts}}` block shows
the correct init system for the container (runit) and `sudo -n: yes`; journal
file receives entries; `term-llm agents show sysadmin` renders new fields.

### 7. Phasing

Single PR is feasible (est. 400–600 production LOC, most in `hostfacts` and
tests). If it must be split: (1) `hostfacts` + template vars + agent bundle +
`workspace: none`; (2) `always_confirm`; (3) journal. Ship in that order; the
agent is useful after (1), safe after (2), auditable after (3).

## Open questions for the planner pass

1. Exact pattern semantics of `matchPatternSingle`: does `cat *` match
   `cat /etc/hosts` and `journalctl *` match `journalctl -u nginx --since -1h`?
   Confirm before relying on the allowlist above; adjust patterns if `*`
   cannot cross `/` or spaces.
2. Where the primary workspace value is set for each surface (`ask`, `chat`,
   `loop`, `serve` web/Telegram, jobs runner, `serve mcp`) so `workspace: none`
   has a single choke point.
3. Whether Guardian auto mode should see `always_confirm` matches at all
   (proposal: no, human only) and how the web/TUI approval dialog should label
   them ("always confirmed" badge, no "remember" option).
4. `hostfacts` on darwin: which probes are worth the exec cost vs. reporting
   `n/a`.
5. `read: dirs: ["/"]` interaction with symlink-escape checks and with
   `canonicalizePath` for `/proc` and `/sys` entries (expect fine; verify
   `read_file /proc/meminfo` works).

---

# Grounded implementation plan (Astra planner pass)

## Answers to open questions

Grounded against commit `aace792b78c54fa37bd28705fc80f7ecde93fe85`. References below are to the current worktree, not proposed code. New APIs and test names are explicitly implementation recommendations.

No repository files were modified. Focused agent/config tests passed; tools tests could not compile because the worktree lacks generated frontend assets. Details appear under **Verification commands**.

### 1. Exact shell-pattern semantics

**All three examples in the question match:**

| Pattern | Command | Result |
|---|---|---|
| `cat *` | `cat /etc/hosts` | Match |
| `journalctl *` | `journalctl -u nginx --since -1h` | Match |
| `sudo -n cat *` | `sudo -n cat /var/log/syslog` | Match |

The reason is not ordinary string globbing:

1. `splitShellWords` parses quoted/escaped words without executing a shell.
2. A **final standalone `*` token** means “zero or more remaining arguments.” Those arguments are not individually glob-matched, so they can contain `/`.
3. Otherwise, pattern and command must have the same number of words.
4. Each corresponding word is matched using `doublestar.Match`. Within such a word, `*` does not cross `/`; `**` supports recursive path matching.
5. Exact string equality is accepted before the unsafe-syntax check in `matchPatternSingle`.

Evidence: `internal/tools/exec_helpers.go:195-263,321-339`; `internal/tools/approval.go:2274-2315`; existing path-glob tests at `internal/tools/shell_pattern_test.go:85-105`.

Consequences:

- `systemctl status*` matches `systemctl status`, but **not** `systemctl status nginx`.
- `docker logs*` does not match `docker logs nginx`.
- `printenv*` matches a one-word executable name, not `printenv PATH`.
- `journalctl *--vacuum*` requires exactly two words and cannot detect the option arbitrarily far into the argument list.
- `fdisk /*` does not match `/dev/sda`: its second-word `*` cannot cross the second slash.
- `rc-service * status` is different: its middle `*` deliberately matches one service-name argument; that pattern can remain.

`matchPatternValidated` decomposes sequential commands and pipelines. `matchAnyShellPattern` can combine patterns **within one pattern source** to cover the sequence. All stages must be covered, except recognized later pipeline targets. This is **allowlist semantics**, not appropriate semantics for `always_confirm`. Evidence: `internal/tools/approval.go:2170-2205,2233-2269`.

#### Corrected allow patterns

The following are syntactic corrections, **not an assertion that every resulting rule is safe to auto-approve**. The safety cuts below should be applied before shipping.

For literal executable/subcommand suffixes, replace the attached star with a standalone trailing argument wildcard:

```text
printenv*
  → printenv *

findmnt*, lsblk*, lspci*, lsusb*, dmesg*, rc-status*, sw_vers*
  → findmnt *, lsblk *, lspci *, lsusb *, dmesg *, rc-status *, sw_vers *

ip addr show*, ip link show*, ip route show*, ip neigh show*
  → ip addr show *, ip link show *, ip route show *, ip neigh show *

resolvectl status*
  → resolvectl status *

nmcli device*
  → nmcli device status *       [narrow the subcommand; do not use device *]
nmcli connection show*
  → nmcli connection show *

systemctl status*, show*, cat*, list-units*, list-unit-files*,
          list-timers*, list-dependencies*, is-active*, is-enabled*, is-failed*
  → systemctl status *, show *, cat *, list-units *, list-unit-files *,
              list-timers *, list-dependencies *, is-active *, is-enabled *, is-failed *

systemctl --failed*
  → systemctl --failed *

loginctl list*
  → loginctl list-sessions *    [use explicit subcommands]
    loginctl list-users *
    loginctl list-seats *

launchctl list*, launchctl print*
  → launchctl list *, launchctl print *

diskutil list*, diskutil info*, log show*
  → diskutil list *, diskutil info *, log show *

apt list*, apt show*
  → apt list *, apt show *

dnf list*, dnf info*, dnf check-update*
  → dnf list *, dnf info *, dnf check-update *

apk info*, apk list*
  → apk info *, apk list *

zypper se*, zypper info*
  → zypper se *, zypper info *

brew list*, brew info*, brew outdated*
  → brew list *, brew info *, brew outdated *

flatpak list*, snap list*
  → flatpak list *, snap list *

docker ps*, images*, logs*, inspect*, system df*
  → docker ps *, images *, logs *, inspect *, system df *

docker compose ps*, docker compose logs*
  → docker compose ps *, docker compose logs *

docker stats --no-stream*
  → docker stats --no-stream *

podman ps*, podman logs*
  → podman ps *, podman logs *

sudo -n dmesg*, sudo -n systemctl status*
  → sudo -n dmesg *, sudo -n systemctl status *

sudo -n nft list*, sudo -n ufw status*
  → sudo -n nft list *, sudo -n ufw status *

git status*, git log*, git diff*
  → git status *, git log *, git diff *
```

In the grouped notation above, repeat the command prefix for each entry.

For intentionally combined short flags, retain the within-word glob **and add a final standalone `*`**:

```text
netstat -an*                → netstat -an* *
pacman -Q*                 → pacman -Q* *
pacman -Si*                → pacman -Si* *
pacman -Ss*                → pacman -Ss* *
pacman -Fl*                → pacman -Fl* *
dpkg -l*                   → dpkg -l* *
dpkg -L*                   → dpkg -L* *
dpkg -S*                   → dpkg -S* *
rpm -q*                    → rpm -q* *
nix-env -q*                → nix-env -q* *
sudo -n iptables -S*       → sudo -n iptables -S* *
sudo -n iptables -L*       → sudo -n iptables -L* *
sudo -n fdisk -l*          → sudo -n fdisk -l* *
top -l 1*                  → top -l 1 *
```

These corrections follow directly from `internal/tools/approval.go:2298-2315`. In particular, broad combined-flag patterns still require a safety review; adding the missing argument wildcard does not make them read-only.

#### Corrected always-confirm patterns

Apply these corrections when building the deny-tier bundle:

| Draft form | Corrected form |
|---|---|
| `mkfs*`, `mkfs.*` | `mkfs* *`; the second entry becomes redundant |
| `wipefs*` | `wipefs *` |
| `fdisk /*` | `fdisk *`—conservative, and covers bare invocation |
| `zpool destroy*` | `zpool destroy *` |
| `btrfs device delete*` | `btrfs device delete *` |
| `iptables -F*`, `--flush*`, `-X*` | Same flag token followed by ` *` |
| `nft flush*` | `nft flush *` |
| `systemctl stop ssh*`, `disable ssh*` | `systemctl stop ssh* *`, `systemctl disable ssh* *` |
| `journalctl --vacuum*` | `journalctl --vacuum* *` |
| `journalctl --rotate*`, `--flush*` | `journalctl --rotate *`, `journalctl --flush *` |
| `visudo*`, `shutdown*`, `reboot*`, `poweroff*`, `halt*` | Literal executable followed by ` *` |
| `systemctl reboot*`, `poweroff*`, `halt*`, `kexec*` | Literal subcommand followed by ` *` |
| `crontab -r*`, `umount -a*` | Same flag token followed by ` *` |
| `pacman -Rns*`, `-Rdd*`, `-Sc*`, `-Scc*` | Same flag token followed by ` *` |
| `apt purge*`, `autoremove*`; corresponding `apt-get` entries | Literal subcommand followed by ` *` |
| `dnf remove*`, `autoremove*`, `brew uninstall*` | Literal subcommand followed by ` *` |
| `docker system prune*`, `volume prune*`, `volume rm*`, `compose down*` | Literal subcommand sequence followed by ` *` |
| `grub-install*`, `update-grub*` | Literal executable followed by ` *` |

Additional changes:

- Replace the numerous lexical `rm -rf …` variants with conservative **`rm *`** for this agent. That also covers reordered/split flags and avoids depending on `$HOME` or tilde expansion.
- Remove `journalctl *--vacuum*`; it does not mean “find this option anywhere.”
- To retain immediate read-only journal inspection, add a small **journalctl mutation-option normalization** step to the new always-confirm matcher: inspect parsed arguments irrespective of position, and test a canonical candidate beginning with the detected mutation option. Cover at least the draft’s vacuum/rotate/flush forms. Do not change allowlist semantics to achieve this.
- Other option-order variants remain a documented limitation unless conservatively covered by broader patterns. This is a pattern policy, not comprehensive command-semantic analysis.

The existing parser flags unquoted `$`, redirections, substitutions, and several operators as unsafe syntax; the current decomposition is not a complete shell parser. Evidence: `internal/tools/exec_helpers.go:271-313`; `internal/tools/approval.go:2393-2462`.

### 2. Workspace proposals on every surface

There is no single `cmd/session.go` assignment to remove.

| Surface | Initial proposal / execution path |
|---|---|
| `ask` | `cmd/ask.go:290-312`: resolves settings, then sets `PrimaryWorkspace = BaseDir`. Resume can overwrite it at `cmd/ask_resume.go:88-92`. |
| `chat` | `cmd/chat.go:514-534`: resolves settings and sets `PrimaryWorkspace = runtimeDir`. Proactive confirmation calls `EnsurePrimaryWorkspaceAccess` at `cmd/chat.go:1067`. |
| `loop` | `cmd/loop.go:270-288`: resolves settings and sets `PrimaryWorkspace = BaseDir`. |
| Shared runner | `cmd/runner.go:594-626`: explicit `req.Cwd`, or local console/chat/exec launch, proposes `BaseDir`. Unbound web/Telegram instead clear execution defaults and require an explicit working directory. |
| Serve web | `cmd/serve.go:325-338`: constructs the shared runner request with `request.RuntimeDir` as `Cwd`. No runtime directory means no proposal. |
| Telegram | Initial engine is created through the same serve runtime factory at `cmd/serve.go:632-636,688-699`; subsequent Telegram requests borrow that engine at `internal/serve/telegram.go:2017-2034`. The request does not supply a new CWD there. |
| Jobs | Production executor passes `cfg.Cwd` to the shared runner at `cmd/serve_jobs_v2.go:3164-3183`. The separate parity/settings helper proposes only explicit `cfg.Cwd` at `cmd/serve_jobs_v2.go:3097-3116`. |
| `spawn_agent` | Child requests use `PlatformConsole` and inherited/explicit `baseDir` as `Cwd`: `cmd/spawn_runner.go:224-270`; therefore the shared runner proposes it. The legacy test wiring also resolves settings and attaches the parent manager at `cmd/spawn_runner.go:723-745`. |
| `queue_agent` | Creates an independent jobs-v2 job, not a `SpawnAgentRunner` child: `internal/tools/queue_agent.go:216-226`. CWD selection is explicit value → environment → tool working directory → process CWD, subject to unbound-runtime checks: `internal/tools/queue_agent.go:847-874`. Execution then uses the jobs path above. |
| `serve mcp` | Does **not load an agent**. It constructs `ToolConfig` directly from MCP flags, without `PrimaryWorkspace`: `cmd/serve_mcp.go:192-214`. There is no sysadmin agent policy to propagate on this surface today. |

Also cover these rebinding paths:

- `cmd/worktree_binding.go:35,71,120`
- `cmd/serve_workspace_resolver.go:310-315`
- `cmd/serve_handlers.go:3593,4042,4056`
- Shared-runner restored-settings copy: `cmd/runner.go:250-256`

All dynamic rebinding reaches `LocalToolRegistry.SetBaseDirWithContext`, which currently both proposes the primary workspace and updates configuration: `internal/tools/registry.go:464-485`. `ToolConfig.UpdateBaseDir` separately writes `PrimaryWorkspace = dir`: `internal/tools/config.go:107-116`.

#### Recommended minimal enforcement set

Carry `Workspace` from `Agent` → `SessionSettings` → `ToolConfig`, and expose the effective policy on the associated `ApprovalManager`.

Enforce it in three places:

1. **Initial setup:** `SessionSettings.SetupToolManager` / `NewLocalToolRegistry` must not install a proposal for `none`. Current handoff is `cmd/session.go:560-585` → `internal/tools/registry.go:76-79`.
2. **Dynamic binding:** `SetBaseDirWithContext` and `UpdateBaseDir` must update execution CWD without creating a proposal.
3. **Inherited primary confirmation:** both `EnsurePrimaryWorkspaceAccess` and `ensurePrimaryWorkspaceAccess` must respect the calling manager’s `none` policy before looking at the root manager’s proposal. Otherwise a sysadmin child still inherits the parent’s confirmation gate. These methods currently inspect root-owned state: `internal/tools/workspace_capability.go:353-385`.

Do **not** clear the parent’s workspace to implement a child’s opt-out. Parent linkage and root lookup are at `internal/tools/approval.go:569-607`.

Keep `BaseDir` and `ShellWorkingDir`; do not re-enable daemon-CWD fallback for unbound web/Telegram. Existing `WorkingDir`/`ShellDir` behavior is at `internal/tools/config.go:351-384`.

#### `manage_workspace` exposure

Automatic registration is in `internal/tools/registry.go:216-232`, with another registration path when routed vision enables `view_image` at `internal/tools/registry.go:329-339`.

The existing yolo omission is:

- `FilterToolSpecsForApprovalMode`: `internal/tools/registry.go:645-655`
- `ToolManager.GetSpecs`: `internal/tools/registry.go:658-664`
- MCP’s use of the same filter: `cmd/serve_mcp.go:237-240`

Extend that filter to omit `manage_workspace` when the manager has `workspace: none`. Preserve the existing rule that ordinary yolo filtering does not unregister executors.

### 3. `read.dirs` propagation—and the root-directory bug

The common path is:

```text
Agent.Read.Dirs
  → ResolveSettingsInDir
  → SessionSettings.ReadDirs
  → buildToolConfig
  → ToolConfig.BuildPermissions
  → ToolPermissions.AddReadDir
```

Evidence:

- Agent read config: `internal/agents/agent.go:217-220`
- CLI-versus-agent resolution: `cmd/session.go:211-215`
- Global tool-config directories plus resolved directories: `cmd/tools.go:63-81`
- Tool-manager construction: `cmd/session.go:548-585`
- Permission construction: `internal/tools/config.go:479-518`
- Canonicalized directory insertion: `internal/tools/permissions.go:33-47`

All agent-bearing surfaces above use that common settings/registry path. There are two important qualifications:

- Nonempty CLI/request read directories **replace the agent’s directory list** during resolution; they are not appended to it.
- Jobs combine serve/job directories before presenting them as CLI overrides: `cmd/serve_jobs_v2.go:3092-3102`.
- MCP uses only its own flags, not `Agent.Read.Dirs`: `cmd/serve_mcp.go:192-200`.

**The draft’s root-wide read claim is false in the current code.**

`isPathInDirs` tests:

```go
strings.HasPrefix(resolvedPath, resolvedDir+string(filepath.Separator))
```

For `resolvedDir == "/"`, that becomes a `//` prefix. Thus `/` itself matches by equality, but `/etc/hosts` and `/proc/meminfo` do not match the root read grant. Evidence: `internal/tools/permissions.go:191-205`.

**Required fix:** use the existing `filepath.Rel` containment logic in `pathWithinWorkspace`, or extract that logic into a neutral containment helper and reuse it. Its existing implementation already handles root correctly: `internal/tools/workspace_capability.go:999-1004`.

After that fix, and after suppressing the primary-workspace gate:

- `read_file` checks its canonical target: `internal/tools/read.go:97-116`.
- `grep` checks its canonical search root: `internal/tools/grep.go:1022-1043`.
- `glob` checks its canonical base directory: `internal/tools/glob.go:116-146`.
- A static read grant returns before later prompting/symlink-escape handling: `internal/tools/approval.go:940-961,1218-1232`.

`canonicalizePath` resolves all symlinks with `filepath.EvalSymlinks`; there is no `/proc` or `/sys` exemption: `internal/tools/permissions.go:211-226`.

`/proc/meminfo` is compatible with the reader once authorized: the reader opens and streams the file instead of relying on a nonzero stat size or seeking. Evidence: `internal/tools/read.go:127-150`.

Do not promise that *all* proc/sys paths work: disappearing process entries, protected files, dangling or special “magic” symlinks, binary content, and OS permissions can still fail. Also, root authorization does not change glob’s deliberate hidden-file and symlink traversal restrictions: `internal/tools/glob.go:183,268-307`.

### 4. `always_confirm` precedence and human UI

Guardian should **not** see these requests.

Insert the check:

| Function | Required insertion |
|---|---|
| `checkShellApprovalWithContext` | Before the first yolo check at `internal/tools/approval.go:1458`. Route matches directly to a serialized human-only helper. |
| `checkShellApprovalNoPrompt` | Before exact-script approval at `internal/tools/approval.go:1103`. A match must return “not approved; prompting required,” never a grant. |
| `CheckSharedShellApprovalWithContext` | After its nil-manager rejection at `internal/tools/approval.go:1569-1571`, before yolo at `:1572`. |

The direct helper must bypass **all** ordinary approval logic, including the rechecks after acquiring the prompt lock:

- Local yolo/cache/Guardian rechecks: `internal/tools/approval.go:1486-1505`
- Shared yolo/cache rechecks: `internal/tools/approval.go:1601-1606`
- Cache and project persistence in the ordinary result handler: `internal/tools/approval.go:2026-2074`

Use an **any-stage** matcher, not `matchAnyShellPattern`: inspect every sequence and pipeline stage, including otherwise “safe” pipe targets. Walk parent managers’ always-confirm lists as additive restrictions, just as prompt/cached authority can be inherited.

#### Minimal transport change

The current callback signatures contain no policy metadata:

```go
PromptUIFunc(path string, isWrite, isShell bool, workDir string)
SharedShellPromptUIFunc(command string)
```

Evidence: `internal/tools/approval.go:436-443`.

Rather than replacing every existing callback and test, add:

```go
type ShellConfirmationRequest struct {
    Command        string
    WorkDir        string
    Scope          string // local or shared_shell
    MatchedPattern string
}

AlwaysConfirmPromptFunc func(ShellConfirmationRequest) (ApprovalResult, error)
```

Add ancestor lookup and an optional corresponding capability to the run event-sink interfaces. Do not silently fall back to a callback that cannot distinguish mandatory-human prompts.

Accept only `ApprovalChoiceOnce`; cancellation, denial, remembered-command, and remembered-pattern results must not create any cache entry. An unavailable transport must return `ErrPermissionDenied` naming the matched pattern.

Wire the new transport alongside these actual implementations:

| Surface | Existing implementation |
|---|---|
| Ask, ordinary renderer | `cmd/ask.go:491-499` |
| Ask, rich renderer | `cmd/ask.go:1525-1539` |
| Chat, embedded/external UI | `cmd/chat.go:1007-1036` |
| Edit | `cmd/edit.go:196-198` |
| Exec run sink | `cmd/exec.go:362-364` |
| Shared runner capability | `cmd/runner.go:662-667`; `internal/run/types.go:150-154` |
| Serve local/shared shell | `cmd/serve.go:415-421` |

**Two additional yolo bypasses must be fixed:**

1. Serve currently installs ordinary shell callbacks only outside yolo. Install the new mandatory-human callback even for yolo launches: `cmd/serve.go:415-422`.
2. The TUI auto-answers ordinary approval messages in yolo and when switching to yolo. Add an `AlwaysConfirm` flag to the message and pending-model state, and exclude these prompts from both shortcuts:
   - `internal/tui/chat/update_interactive.go:28-51`
   - `internal/tui/chat/handlers.go:253-257`
   - Message definition: `internal/tui/chat/chat.go:788-795`

For UI construction:

- Add a once/deny-only builder next to `BuildShellOptions` and `BuildSharedShellOptions`: `internal/tools/approval_ui.go:333-399`.
- Thread the request into embedded and external shell UI constructors: `internal/tools/approval_ui.go:127-145,200-218,673-700`.
- Web options originate server-side in `newServePendingApprovalScoped`: `cmd/serve_approval.go:122-160`. Carry policy metadata through pending state, snapshot, and SSE emission at `:18-94,219-237`.
- Use the title **“Human confirmation required”**, show the matched rule in the option description, and offer only **Allow once / Deny**. Do not call it “always confirmed,” which sounds like an approval has already occurred.
- Do not offer “Resume Guardian auto” for this request.

The web component already renders supplied titles/options, so a new badge system is unnecessary: `frontend/src/components/Modals.tsx:623-739`.

### 5. Hostfacts, Darwin, template expansion, and previews

#### Exact template changes

Modify:

- `TemplateContext`: add `HostFacts`, `HostNotes` at `internal/agents/template.go:36-96`.
- `NewTemplateContextForTemplateInDir`: derive two lazy flags from `vars`, alongside existing flags at `:115-121`.
- `newTemplateContextInDir`: compute only requested values at `:135-203`.
- `ExpandTemplate`: add cases in its switch at `:287-389`.
- Update the deprecated `NewTemplateContext` call when changing the helper signature: `:101-104`; do not make ordinary callers unexpectedly probe the host.

`templateVariables` already discovers arbitrary word identifiers; no hard-coded variable registry needs extending: `internal/agents/template.go:124-131`.

The include path matters: prompt expansion constructs another context **after file includes** at `cmd/session.go:434-470`. Test variables that occur only in included files, not just directly in `system.md`.

`agents show` is **not a rendered preview**. It prints the raw prompt, truncated to 500 bytes: `cmd/agents.go:433-444`. Keep that behavior; inspecting an agent should not run sudo probes.

#### Freshness correction

The draft overstates compaction behavior. Web compaction uses the already-rendered `rt.systemPrompt`, and history reconstruction reinserts a supplied string:

- `cmd/serve_compaction.go:41-67`
- `internal/llm/compaction.go:1855-1859`

Promise **collection on actual template rendering, with up to 60 seconds of cache age**, not automatic refresh on every compaction. No compaction-specific hook is needed for this PR.

#### Package and probes

Create `internal/hostfacts` with Linux, Darwin, and unsupported-platform implementations.

Reuse:

- `config.GetConfigDir()` for notes: `internal/config/config.go:2699-2710`
- `buildinfo.Version`: `internal/buildinfo/version.go:6-8`
- The existing bounded-probe/testing pattern: `internal/agents/template.go:405-465`

For Darwin, first-PR scope should be:

- Standard-library identity/user/architecture.
- Darwin-specific uname/statfs implementation.
- `sw_vers -productVersion`.
- `sysctl -n hw.memsize`, or the equivalent Darwin API.
- `launchd` identification.
- `sudo -n true` under the same bounded, no-input policy.

Report unavailable memory, load, virtualization, and other uncollected metrics as **unknown/n/a**, not zero. Do not run `system_profiler` or parse `vm_stat` in prompt construction.

Linux and Darwin are both release targets on amd64/arm64: `.goreleaser.yml:7-12`. Do not put Linux-specific `unix` fields into a common source file.

Use a private injectable probe/clock implementation for fixture tests, plus a synchronized 60-second production cache. No tests should run real sudo.

A context deadline bounds subprocesses; it does **not** make arbitrary `os.ReadFile` or `Statfs` calls hard-cancellable. Describe the two-second target as a bounded probe budget, not an unconditional wall-clock guarantee on broken filesystems.

For notes, use a sanitized lowercase short hostname; bound file size, treat contents as user-maintained context, and do not recursively expand template syntax found inside notes.

### 6. Journal hooks and attribution

#### Shell completion

Local shell execution is at `internal/tools/shell.go:573`.

- Exit-code extraction: `:599-610`
- Timeout/cancellation returns occur earlier: `:592-596`
- Shared terminal execution/result: `:398-419`
- `ShellResult` has no duration field: `:243-251`

Measure execution time around `cmd.Run` or the shared-controller execution—not around approval waiting or filesystem tracking.

Restructure the post-execution path sufficiently to emit the journal entry before every execution-result return. For a process that started and then failed/timed out, record the attempt. Do not record denied requests as executed commands. Use a nullable/unknown exit code when no reliable exit status exists; do not record the current timeout default of zero as success.

#### File completion

Attach after successful replacement:

- `WriteFileTool.Execute`: successful rename at `internal/tools/write.go:173-180`
- `EditFileTool.executeDirectEdit`: successful rename and recording at `internal/tools/edit.go:240-256`

Also cover `UnifiedDiffTool.applyFileDiff`, because it is an existing alternative edit tool: `internal/tools/edit.go:450-456`.

Use resulting content length for `bytes`, not replaced-span length.

#### Package and wiring

Use **`internal/journal`**, independent of `tools` and `agents`.

Follow the existing registry recorder-attachment pattern, including reattachment when tools are recreated: `internal/tools/registry.go:109-143`. Do not replace the existing file-change recorder; it serves a different purpose.

Wire the journal in `SessionSettings.SetupToolManager`, next to existing recorder wiring at `cmd/session.go:604-605`.

Attribution:

- Session ID: prefer `llm.SessionIDFromContext(ctx)`; definition at `internal/llm/types.go:147-155`, populated by the engine at `internal/llm/engine.go:2122,2145`.
- Construction fallback: `SessionSettings.SessionID`, assigned in the shared runner at `cmd/runner.go:261`.
- Agent name: resolved settings field at `cmd/session.go:191-194`.
- Hostname: use a shared sanitized host-key helper with host notes; obtaining the key must not trigger fact collection.
- Data directory: `internal/appdata/appdata.go:11`.

Preserve the proposed approval-source enum, but obtain it from a **request-local detailed approval decision**, not by inferring from `ProceedOnce`/`ProceedAlways` or storing a shared “last approval” field. Existing outcomes conflate several sources: `internal/tools/approval.go:1101-1147`.

Shared terminals may target remote hosts: `internal/tools/approval.go:1640-1642`. Journal such entries with `scope: shared_shell`; label the JSONL hostname as the **term-llm recording host**, not a verified remote target. Do not invent a local CWD for shared commands.

### 7. Builtin tests, fields, preferences, and docs

#### Every relevant builtin enumeration

Update these tables in `internal/agents/embedded_test.go`:

| Test/table | Location | Required change |
|---|---|---|
| `TestIsBuiltinAgent` | `:67-89` | Add `sysadmin: true`. |
| `TestBuiltinAgentConfigs` | `:101-128` | Add `{"sysadmin", true, 300, true, true, false, true}`. |
| `TestBuiltinPromptsUseCapabilityAwareDirectoryGuidance` | `:245-273` | Add a sysadmin-specific **absolute-path** guidance row. Do not require repository-agent relative-path guidance. |
| `TestBuiltinTimeGroundingIsExplicitOnlyForTimeAwareAgents` | `:702-716` | Add `sysadmin` to the true map. Otherwise the draft’s `time_grounding: true` breaks this test. |
| `TestGetBuiltinAgentNames` | `:721-750` | Add `sysadmin`; otherwise count/membership fails. |

The generic loading tests already iterate `builtinAgentNames`; the plan-guidance and volatile-directory tests scan embedded directories. Leave those scans generic and ensure sysadmin does not opt into developer-only plan guidance or introduce `{{cwd}}`/`{{git_branch}}`. Evidence: `internal/agents/embedded_test.go:12-65,167-234`.

Add dedicated sysadmin assertions for no spawn tool, `agents_md: false`, workspace policy, both host variables, and—in their respective commits—always-confirm and journal settings.

#### Fields and preferences

Existing locations:

- `Agent`: `internal/agents/agent.go:46-139`
- `ShellConfig`: `:211-215`
- `Agent.Validate`: `:430`
- `Agent.Merge`: `:550-597`
- `config.AgentPreference`: `internal/config/config.go:598-619`

Add:

```text
Agent.Workspace string
Agent.Journal bool
ShellConfig.AlwaysConfirm []string
AgentPreference.ShellAlwaysConfirm []string
```

Only `ShellAlwaysConfirm` needs a new preference field in the stated design. Append it in `Agent.Merge`; an empty preference must not erase the builtin list. Keep `workspace` and `journal` as bundle fields unless there is a separate requirement for preference overrides.

CLI surfaces:

- `runAgentsShow`: print workspace, journal, and shell always-confirm near existing output at `cmd/agents.go:388-418`.
- `agentPrefSetCompletion`: add `shell_always_confirm=` at `cmd/agents.go:675-688`.
- `runAgentsPrefGet`: print it beside shell settings at `cmd/agents.go:771-777`.
- Add its known key and array parsing:
  - `internal/config/config.go:2748-2760`
  - `internal/config/config.go:2968-2977`

#### Config schema correction

`internal/config/schema.go` is a static key-spec catalog, not a generated per-agent JSON Schema. It declares the dynamic map only:

```go
optional("agents.preferences", withPlaceholder(map[string]any{}))
```

Evidence: `internal/config/schema.go:8-20,423-425`.

Dynamic preference children are validated through `KnownAgentPreferenceKeys`: `internal/config/config.go:2808-2816`. The coverage test explicitly checks every `AgentPreference` field against that map: `internal/config/config_test.go:955-958`.

Therefore:

- Declare the field in `AgentPreference` and `KnownAgentPreferenceKeys`.
- Test the concrete key `agents.preferences.sysadmin.shell_always_confirm`.
- Do **not** add a literal `agents.preferences.<agent>...` key to static defaults; that would not be a dynamic schema declaration.
- `schema.go` needs no functional new leaf for this field. A clarifying comment is sufficient if desired.

#### Documentation

Modify:

- `docs-site/content/guides/agents.md`: builtin table at `:26-52`, preference guidance at `:103`, agent configuration at `:130-150`, template guidance around `:308-310`.
- `docs-site/content/reference/built-in-tools.md`: workspace behavior at `:174-190`, approval precedence at `:237-245`.
- `docs-site/content/reference/configuration.md`: approval/workspace rules at `:183-189`, auto-pattern behavior at `:266`; add the per-agent extension example.

Document root-read sensitivity, headless denial, shared-terminal limitations, policy precedence, notes location, journal privacy, and the fact that `workspace: none` does not revoke separately granted authority.

## Corrections to the draft

1. **Root read permission is currently broken.** Fix containment before relying on `read.dirs: ["/"]`.
   Evidence: `internal/tools/permissions.go:191-205`.

2. **Most attached-star patterns do not accept extra arguments.** Apply the corrections above and test representative commands from every retained family.
   Evidence: `internal/tools/approval.go:2298-2315`.

3. **“Read-only allowlist” is too strong.** The matcher checks words, not command semantics. Broad rules can admit mutating options, external helpers, or executable-name variants. Particularly scrutinize `hostname *`, `dmesg *`, `ss *`, `nmcli device*`, `journalctl *`, and Git helper behavior. The “safe pipe” shortcut also checks only the executable basename, not flags: `internal/tools/approval.go:2104-2119`.

4. **Do not claim protection against every wipe/lockout spelling.** Leading wrappers, absolute executable paths, reordered flags, shell launchers, substitutions, and alternate utilities require explicit treatment. Unknown wrapper syntax or unsupported shell syntax should require direct confirmation when this policy is active, rather than silently falling through to yolo. The existing parser/decomposition is not a sandbox: `internal/tools/exec_helpers.go:271-313`; `internal/tools/approval.go:2393-2462`.

5. **The workspace example must distinguish shell from file tools.** A shell command `cat ~/.bashrc` does not pass through the file-tool workspace gate. Shell approval is separate at `internal/tools/shell.go:490-505`; the file gate is at `internal/tools/approval.go:1206-1209`.

6. **Suppressing only the initial proposal is insufficient.** Dynamic CWD updates and inherited root workspace state also matter.
   Evidence: `internal/tools/registry.go:464-485`; `internal/tools/workspace_capability.go:353-385`.

7. **“Every write prompts” is false.** Yolo, static write grants, inherited/session grants, project approvals, and Guardian can authorize writes. `workspace: none` is not a new file-write deny tier.
   Evidence: `internal/tools/approval.go:1190-1196,928-1013`.

8. **Serve MCP is not an agent surface today.** Do not add unrelated agent support merely to satisfy parity wording.
   Evidence: `cmd/serve_mcp.go:192-214`.

9. **Compaction does not guarantee host-fact refresh.** Keep collection lazy on actual rendering and document the cache age.
   Evidence: `cmd/serve_compaction.go:41-67`.

10. **The `.sh` limitation is extraction, not embedding.** `all:builtin` embeds the directory; the extraction walker filters extensions. Go remains a sensible probe implementation, but the explanation should be accurate.
    Evidence: `internal/agents/embedded.go:14-15,168-170`.

11. **Resume needs policy restoration, not only prompt restoration.** Ask’s inferred-agent resume branch currently copies only `PlanGuidance`; new policy fields must be restored there. Shared-runner restored settings also require an explicit policy decision.
    Evidence: `cmd/ask_resume.go:80-83`; `cmd/runner.go:250-256`.

12. **Auto mode may still review nominally allowed diagnostics.** Executable globs and wildcard elevation rules are mechanically suspended in auto mode. Correcting `printenv*` to `printenv *` helps; privileged wildcard rules still follow the existing auto policy.
    Evidence: `internal/tools/approval.go:1150-1160,2138-2155`.

13. **Do not release Commit 1 as the completed agent.** It cannot promise yolo-resistant confirmation before Commit 2. Keep the three commits in one PR or clearly mark the first as an intermediate, unshipped state.

14. **Journal is best-effort history, not tamper-proof auditing.** Commands may contain secrets, the agent has shell access, and journal failures intentionally do not fail tools. Do not record stdout/stderr, environment maps, or file contents. Shared terminal records also need explicit scope.
    Hook evidence: `internal/tools/shell.go:398-419,573-610`; `internal/tools/write.go:173-180`.

## Cuts

For the first PR:

- **Cut the enormous automatic-command catalog to a reviewed core.** Retain identity, basic file/resource inspection, explicit service-status commands, and tested package queries. Leave ambiguous utilities/options on normal confirmation. The existing matcher does not provide semantic read-only enforcement.
- **Cut comprehensive shell-language interpretation.** Reuse the existing parser/splitters for supported forms; handle privilege wrappers explicitly; fail conservatively on unsupported syntax. Do not introduce a new shell interpreter.
- **Cut detailed mount-topology deduplication and expensive Darwin probes.** Root filesystem usage plus clearly unavailable optional metrics is sufficient initially.
- **Cut a generalized host-probe framework.** A private injectable collector, deterministic renderer, and small synchronized TTL cache suffice; follow the existing test-injection pattern at `internal/agents/template.go:451-465`.
- **Cut a new web badge/design system.** Existing server-supplied title/options can express mandatory-human confirmation: `cmd/serve_approval.go:59-94`; `frontend/src/components/Modals.tsx:623-739`.
- **Cut a journal CLI, database, daemon, rotation policy, hash chain, and file-content snapshots.** Keep JSONL and use the existing file-change subsystem for content/diff tracking: `internal/tools/registry.go:109-143`.
- **Cut wholesale replacement of `PromptUIFunc`.** A dedicated mandatory-human callback avoids churn across unrelated file approval code while making its security semantics explicit.

## Implementation plan

### Commit 1 — Hostfacts, template variables, builtin bundle, workspace none

#### Step 1. Add bounded host collection and notes

**Create**

- `internal/hostfacts/hostfacts.go`
- `internal/hostfacts/collect_linux.go`
- `internal/hostfacts/collect_darwin.go`
- `internal/hostfacts/collect_other.go`
- `internal/hostfacts/notes.go`
- Corresponding `*_test.go` files

**Types/functions**

- `Facts`, `DiskUsage`, `Collect(context.Context) Facts`, `Facts.Render()`
- Private probe dependencies and clock
- Cached rendered-facts accessor
- Sanitized host-key helper
- Host-notes loader using `config.GetConfigDir`

Implement Linux fixtures, Darwin minimal probes, unsupported-platform partial results, missing-data reporting, total subprocess budget, sudo skip behavior, and synchronized 60-second caching. Reuse `buildinfo.Version`.

**Tests**

- `TestHostFactsLinuxFixtures`: distro/init/container/proc parsing.
- `TestHostFactsDarwinFixtures`: only intended probes; unavailable values remain unknown.
- `TestHostFactsBudget`: canceled/deadline context returns partial facts and warnings.
- `TestHostFactsSudoSkip`: root/missing sudo skip; non-root probe has closed input and bounded timeout.
- `TestHostFactsRender`: deterministic order and bounded representation.
- `TestHostFactsCache`: repeated/concurrent calls reuse a snapshot; expiry refreshes.
- `TestHostNotes`: XDG path, sanitized hostname, missing hint, bounded content, no recursive expansion.

**Run:** focused `go test ./internal/hostfacts`, then its race tests.

#### Step 2. Wire lazy variables

**Modify**

- `internal/agents/template.go`
- `internal/agents/template_test.go`
- `cmd/session_test.go`

Touch `TemplateContext`, `NewTemplateContextForTemplateInDir`, `newTemplateContextInDir`, `NewTemplateContext`, and `ExpandTemplate`.

**Tests**

- `TestTemplateHostVariablesLazy`: unused and escaped variables do not collect/read notes.
- `TestTemplateHostVariablesExpand`: both values expand; unknown variables retain existing behavior.
- `TestHostVariablesInIncludes`: variables introduced only by file includes are computed.
- `TestHostFactsNotRefreshedByPlainExpansion`: `ExpandTemplate` itself does not probe.

**Run:** focused agent/template tests and command include tests.

#### Step 3. Add workspace policy and fix root read containment

**Modify**

- `internal/agents/agent.go`
- `internal/agents/agent_test.go`
- `cmd/session.go`
- `cmd/ask_resume.go`
- `cmd/runner.go`
- `internal/tools/config.go`
- `internal/tools/registry.go`
- `internal/tools/approval.go`
- `internal/tools/workspace_capability.go`
- `internal/tools/permissions.go`
- Nearby session, runner, registry, workspace, and permission tests

Touch:

- `Agent.Validate`: accept only `""`, `"auto"`, `"none"`.
- `ResolveSettingsInDir` and `SessionSettings.SetupToolManager`.
- Ask inferred-agent resume and runner restored-settings selection.
- `ToolConfig.Merge`, `UpdateBaseDir`, and proposal accessors.
- Initial/dynamic registry binding.
- Primary-workspace confirmation entry points.
- `FilterToolSpecsForApprovalMode`.
- `ToolPermissions.isPathInDirs`, reusing root-safe containment.

Keep execution directories independent from workspace authority. Do not erase parent workspace state or broaden unbound remote defaults.

**Tests**

- `TestAgentWorkspaceValidation`
- `TestWorkspaceNoneAcrossSurfaces`: table covering local launches, web/Telegram unbound and explicitly bound requests, jobs with/without CWD, and spawn settings.
- `TestWorkspaceNoneRebind`: runtime/worktree rebinding changes execution CWD without proposing workspace.
- `TestWorkspaceNoneChild`: no inherited primary prompt and no mutation of parent proposal.
- `TestWorkspaceNoneResume`: inferred sysadmin agent retains policy on resume.
- `TestWorkspaceNoneToolSpecs`: no advertised `manage_workspace`, including routed vision and approval-mode toggles.
- `TestRootReadGrant`: `/` authorizes descendants; normal roots still reject siblings/escapes.
- `TestRootReadProcMeminfo`: Linux-only readable-proc regression; no prompt.
- `TestWorkspaceNoneReadUnderCWD`: root read grant works inside launch directory without workspace confirmation.
- `TestWorkspaceNoneUnboundRemote`: absolute targets work; relative operations still fail closed.

Retain existing normal-agent/yolo workspace tests.

**Run:** focused tools, session, runner, spawn, jobs, and serve-workspace tests.

#### Step 4. Add builtin bundle and presentation

**Create**

- `internal/agents/builtin/sysadmin/agent.yaml`
- `internal/agents/builtin/sysadmin/system.md`

**Modify**

- `internal/agents/embedded.go`
- `internal/agents/embedded_test.go`
- `cmd/agents.go`
- Agent-show tests
- `docs-site/content/guides/agents.md`
- Workspace wording in the two reference pages

Use the corrected, reduced allowlist. Include `workspace: none` now; add active `always_confirm` and `journal` configuration only when those fields are implemented in subsequent commits. Avoid claims that those protections already operate.

Update all five enumerations identified above. Keep raw prompt output in `agents show`.

**Tests**

- `TestBuiltinSysadmin`: explicit tools, no spawn, max turns, search, host variables, agents-md policy, absolute-path guidance, workspace none.
- `TestAgentsShowWorkspace`: effective workspace field displayed without running host probes.
- Existing builtin table/volatile-token tests.

**Run:** focused agents/show tests, then Commit 1 package checks listed below.

### Commit 2 — `shell.always_confirm`

#### Step 5. Add policy/config plumbing

**Modify**

- `internal/agents/agent.go`, `agent_test.go`
- `internal/config/config.go`, `config_test.go`
- `cmd/session.go`, `ask_resume.go`, `runner.go`
- `cmd/agents.go`
- `internal/tools/config.go`, `permissions.go`
- Builtin YAML and relevant docs

Touch `ShellConfig`, `Agent.Merge`, `AgentPreference`, known keys, preference array parsing, CLI completion/show/get, session policy copying, tool-config validation, and permission construction.

Use independent copied slices and validated patterns. An invalid mandatory rule must fail configuration—not be silently dropped. Keep preferences append-only.

**Tests**

- `TestAgentMergeAlwaysConfirm`: builtin list retained; preferences append; no slice aliasing.
- `TestAgentPreferenceAlwaysConfirm`: concrete dynamic key accepted, parsed as array, round-trips.
- `TestAlwaysConfirmPatternValidation`
- `TestAlwaysConfirmSettingsAcrossSurfaces`
- Existing `TestProviderSchemaCoversProviderConfigFields`.

**Run:** focused agents/config/session tests.

#### Step 6. Implement any-stage matching and precedence

**Create**

- `internal/tools/shell_always_confirm.go`
- `internal/tools/shell_always_confirm_test.go`

**Modify**

- `internal/tools/approval.go`
- `internal/tools/exec_helpers.go` only for shared parsing helpers genuinely needed
- Builtin sysadmin YAML

Implement:

- Any-segment detection, without safe-pipe exemptions.
- Parsed argv matching without lossy quote reconstruction.
- Executable-basename normalization.
- `sudo`, `doas`, `run0` wrapper handling, including operand-taking options and `--`.
- Conservative confirmation for unsupported/opaque forms when the policy is active.
- Journalctl mutation-option normalization independent of option position.
- Additive restrictions across ancestor managers.
- Human-only serialized path and no-prompt guard at the exact insertion points above.

Keep normal allowlist matching unchanged.

**Tests**

- `TestAlwaysConfirmPrecedence`: yolo, scripts, allowlist, exact Guardian cache, session command/pattern, ancestor cache, project rules, active Guardian.
- `TestAlwaysConfirmWrappers`: quoted usernames, short/long operand flags, absolute executable paths, `--`, malformed/unknown wrapper forms.
- `TestAlwaysConfirmCompound`: `;`, `&&`, `||`, pipeline second stage, and safe-pipe-target collision.
- `TestAlwaysConfirmJournalctlOptionOrder`
- `TestAlwaysConfirmUnsupportedSyntax`: substitutions, shell launchers, unsupported syntax never silently bypass to yolo.
- `TestAlwaysConfirmHeadless`: `ErrPermissionDenied` includes rule.
- `TestAlwaysConfirmCannotRemember`: hostile callback returns command/pattern approval; nothing cached.
- `TestAlwaysConfirmParent`
- `TestSharedShellAlwaysConfirm`
- `TestSysadminShellPolicy`: intended diagnostics versus representative mutations; includes the three matching examples.

Tests must exercise policy matching without executing destructive commands.

**Run:** focused policy tests, then race tests for approval concurrency.

#### Step 7. Wire direct-human transports

**Modify**

- `internal/tools/approval.go`, `approval_ui.go`, their tests
- `internal/run/types.go`
- `cmd/ask.go`, `chat.go`, `edit.go`, `exec.go`, `runner.go`
- `cmd/serve.go`, `serve_approval.go`
- `internal/tui/chat/chat.go`
- `internal/tui/chat/update_interactive.go`
- `internal/tui/chat/handlers.go`
- `internal/tui/chat/handlers_interactive.go`
- Corresponding UI/serve tests

Add the dedicated callback/request type and run-sink capability. Wire web even in yolo. Track the mandatory-human state through opening, toggling approval mode, submitting, canceling, and clearing dialogs.

Use server-generated title/once-deny options for web; no frontend source changes are required unless browser tests expose a presentation gap.

**Tests**

- `TestAlwaysConfirmUIOptions`: only once/deny, matching-rule explanation.
- `TestAlwaysConfirmYoloToggle`: neither initial yolo nor later toggle answers the prompt.
- `TestServeAlwaysConfirmOptions`: local/shared scopes, no remember or resume-auto.
- `TestServeAlwaysConfirmYoloTransport`
- `TestServeAlwaysConfirmInvalidChoice`
- `TestAlwaysConfirmTransportUnavailable`: absent TTY/SSE transport fails closed without hanging.
- `TestAgentsShowAlwaysConfirm`

**Run:** focused tools/TUI/cmd tests; browser approval smoke test.

### Commit 3 — Journal

#### Step 8. Add the private JSONL writer

**Create**

- `internal/journal/journal.go`
- `internal/journal/journal_test.go`

**Types/functions**

- Typed `Entry`
- `Writer`
- Append method accepting a fully attributed entry
- Narrow recorder interface for tool wiring

Use `appdata.GetDataDir()/journal/<host-key>.jsonl`, directory mode `0700`, file mode `0600`. Serialize concurrent in-process appends and write each encoded entry plus newline as a single append operation. Reject unsafe path/file types rather than following a journal-file symlink. Document best-effort cross-process/crash behavior; do not claim transactional or tamper-proof auditing.

**Tests**

- `TestJournalAppendAndModes`
- `TestJournalConcurrentAppend`
- `TestJournalHostKey`
- `TestJournalRejectsSymlinkTarget`
- `TestJournalWriteFailure`
- `TestJournalDoesNotStoreContentsOrEnvironment`

**Run:** journal tests and `-race`.

#### Step 9. Wire agent opt-in, attribution, and approval source

**Modify**

- `internal/agents/agent.go`
- `cmd/session.go`, `ask_resume.go`, `runner.go`, `agents.go`
- `internal/tools/registry.go`
- `internal/tools/approval.go`
- Builtin YAML and tests

Add `Agent.Journal` and resolved setting. Attach recorder/agent attribution to existing and recreated tools using the registry pattern already used for file tracking.

Introduce an internal detailed shell-approval decision containing outcome/source/matched rule. Keep existing public outcome-returning methods as wrappers. Return source from the branch that actually granted approval; preserve distinction between cached Guardian, session, project, direct prompt, and mandatory confirmation.

**Tests**

- `TestJournalAttribution`: live context session ID wins over construction fallback; child agent/session attribution is correct.
- `TestJournalRecorderSurvivesToolRecreation`
- `TestShellApprovalDecisionSources`
- `TestAgentsShowJournal`
- `TestBuiltinSysadminJournal`

**Run:** focused agents/tools/cmd tests.

#### Step 10. Record executions and successful file changes

**Modify**

- `internal/tools/shell.go`
- `internal/tools/write.go`
- `internal/tools/edit.go`
- Their tests
- All three documentation pages

Local shell: measure duration, normalize result once, journal after execution even for nonzero exit/timeout/cancellation, and retain current tool-result behavior.

Shared shell: journal the controller result with explicit scope and unknown target/CWD where necessary.

File tools: record only after successful rename; include unified-diff edits. Journal failures log a warning and do not change tool success.

**Tests**

- `TestShellJournalSources`: all approval-source labels.
- `TestShellJournalNonzeroExit`
- `TestShellJournalTimeout`
- `TestSharedShellJournal`
- `TestWriteEditJournalSuccessOnly`
- `TestUnifiedDiffJournal`
- `TestJournalReadToolsSilent`
- `TestJournalFailureDoesNotFailTool`
- `TestJournalDeniedCommandNotExecuted`

**Run:** focused journal/tool/integration tests; final build/vet/package checks.

## Verification commands

### Already run

Using `GOTOOLCHAIN=go1.26.8`, matching `go.mod:3`:

```sh
go test ./internal/agents -run \
  '^(TestBuiltinAgentConfigs|TestBuiltinPromptsUseCapabilityAwareDirectoryGuidance|TestBuiltinTimeGroundingIsExplicitOnlyForTimeAwareAgents|TestGetBuiltinAgentNames|TestTemplateVariablesAllowsWhitespace)$' \
  -count=1

go test ./internal/config -run \
  '^(TestEveryDefaultIsKnownKey|TestSchemaCoversConfigMapstructureFields|TestSchemaHasNoLeafPrefixCollisions)$' \
  -count=1

go test ./internal/config -run \
  '^TestProviderSchemaCoversProviderConfigFields$' -count=1
```

**Result:** passed.

Attempted:

```sh
GOTOOLCHAIN=go1.26.8 go test ./internal/tools \
  -run '^TestShellWordGlobPathSemantics$' -count=1
```

**Blocked before compilation:**

```text
internal/serveui/embed.go:20:12:
pattern all:static/dist: no matching files found
```

The generated frontend directory is required for source builds: `frontend/README.md:5-14`. I did not generate it because this was a read-only planner pass.

`git diff --check` passed. The only reported untracked file remained the pre-existing `plans/sysadmin-agent.md`.

### Developer verification

In the implementation worktree:

```sh
export GOTOOLCHAIN=go1.26.8

# Required before tools/cmd builds in this checkout.
make frontend

# After Commit 1:
go test ./internal/hostfacts ./internal/agents -count=1
go test ./internal/tools ./cmd \
  -run 'WorkspaceNone|RootRead|HostVariables|BuiltinSysadmin|AgentsShow' -count=1

# After Commit 2:
go test ./internal/agents ./internal/config -count=1
go test ./internal/tools ./internal/tui/chat ./cmd \
  -run 'AlwaysConfirm|SysadminShellPolicy|ShellPattern|ShellWordGlob' -count=1

# After Commit 3:
go test ./internal/journal -count=1
go test ./internal/tools ./cmd \
  -run 'Journal|ShellApprovalDecisionSources' -count=1

# Concurrency-sensitive additions:
go test -race ./internal/hostfacts ./internal/journal
go test -race ./internal/tools \
  -run 'AlwaysConfirm|Journal|WorkspaceNone' -count=1

# Final affected-package checks:
go test ./internal/agents ./internal/config ./internal/hostfacts \
  ./internal/journal ./internal/tools ./internal/tui/chat ./cmd
go build ./...
go vet ./internal/agents ./internal/config ./internal/hostfacts \
  ./internal/journal ./internal/tools ./internal/tui/chat ./cmd

git diff --check
git diff --stat
```

Run these with an isolated temporary HOME/XDG environment, preserving `GOMODCACHE`, so user agents, skills, and configuration do not influence results.

Cross-compile the new hostfacts package for Darwin amd64/arm64 and Linux amd64/arm64; run Darwin collector fixture tests on Darwin CI. Do not assume successful Linux compilation validates Darwin `unix` structures.

For approval UI lifecycle coverage:

```sh
scripts/browser_lifecycle_smoke.sh
```

If frontend source is changed, run all required frontend checks:

```sh
npm --prefix frontend run format
npm --prefix frontend run format:check
npm --prefix frontend run lint
npm --prefix frontend run typecheck
npm --prefix frontend test
```

For manual verification, test mandatory confirmation with a harmless temporary custom rule matching `printf`, not a real disk-management command. Confirm prompt-mode diagnostics, yolo-resistant confirmation, no workspace modal, no remember option, headless denial, and journal attribution.

**Do not run `go test ./...` for this task.**
