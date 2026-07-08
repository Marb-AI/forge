# Forge — Implementation Plan

Remote Claude Code workspace manager. A lightweight Go CLI that manages
persistent remote Claude Code workspaces over SSH.

This document is the design of record. It reflects the decisions made during
design discussion; where we deviated from the original brief, the deviation and
its reason are called out explicitly.

---

## 1. What this is (and is not)

Forge gives you **effortless remote Claude Code usage**. You have one powerful
VPS behind a firewall (only SSH is open). On it live several isolated
workspaces, each a normal Linux user, each running one persistent Claude session
inside tmux. Forge is the thin client that drives all of it from your laptop.

**Non-goals** (belong to the individual project, not to Forge): Docker/Compose
lifecycle, Kubernetes, deployments, logs, restarts, backups, snapshots, CI/CD,
build pipelines. Forge only provides *access to the environment*.

---

## 2. Key design decisions

These were settled during design and drive the whole implementation.

### 2.1 Transport: SSH stdio, not a network daemon

The original brief described the remote agent as a daemon on localhost reached
through SSH port forwarding. **We rejected that.** A persistent daemon means a
systemd unit, a port, and a port-forward before every call — needless
complexity. Persistence of the Claude session is provided by **tmux**, not by
the agent, so the agent does not need to be long-lived.

Instead the agent is a **binary invoked over SSH per operation**:

```
ssh <admin>@<host> sudo forge-agent workspace-create --name crm --pubkey <b64>
```

It reads arguments/JSON, does its work, prints JSON to stdout, exits. No daemon,
no open port, no port-forward. SSH remains the only entry point, and the SSH key
*is* the authentication.

### 2.2 Most commands don't touch the agent at all

Each workspace is a Linux user with its own SSH access (the client's public key
is installed into the workspace user's `~/.ssh/authorized_keys` at create time).
So:

| Command | Implementation |
| --- | --- |
| `workspace <n> ssh` | `ssh <n>@<host>` |
| `workspace <n> claude` | `ssh -t <n>@<host> 'tmux new -A -s claude claude'` |
| `workspace <n> expose` | `ssh -L p:localhost:p <n>@<host>` |

`tmux new -A -s claude claude` = attach if the session exists, otherwise create
and run Claude, in a single command. Claude survives SSH disconnects because
tmux owns it server-side.

The **agent is only needed for privileged lifecycle** operations that require
root: create / delete / list / status. Everything else is direct SSH as the
workspace user.

### 2.3 Zero external dependencies

Standard library only — no cobra, no yaml. It fits the "keep it simple" ethos,
the binary needs nothing fetched, and it keeps the tool auditable. Config is
JSON at `~/.forge/config.json`. Command routing is a small hand-written
dispatcher.

### 2.4 Isolation is "against mistakes", not "against attackers"

Workspaces are separated by ordinary Linux permissions — no chroot, no
containers. The server is single-user (you). Workspace users are in the `docker`
group so `docker compose` works; **this makes the isolation soft** (the docker
group is root-equivalent). That is an accepted trade-off for a personal,
single-operator server. The `claude`-command guard (below) is likewise only a
guard against *accidental* misuse.

### 2.5 Claude auth is a one-time setup, outside the normal flow

First time you run `claude` in a workspace you log in once (API key is the
smoothest, neither interactive nor browser-bound); the login persists in the
workspace user's `~/.claude`. `claude renew` and `claude stop` therefore have
**nothing to do with authentication** — `renew` means *kill the session and
start a fresh one to reset the context window* (save tokens), `stop` means kill
it. Both are pure tmux operations.

### 2.6 Forwarding: the one genuinely non-trivial part

A `ssh -L` tunnel is a single foreground process; the tunnel lives exactly as
long as that process. Bare `ssh -L` does **not** wait for a down server and does
**not** reconnect — any blip leaves a dead tunnel. Since there is no "tmux for
tunnels", the *client* must supervise reconnection. This is the real work of the
project; the rest is command composition.

Decisions:

- **`-L` is lazy.** The local port binds at start regardless of whether the
  remote service is up; the remote connection happens on demand. A workspace
  service that is *down* therefore does **not** break its tunnel — you just get
  connection-refused until it comes up, then it works with no action. So we do
  **not** health-check or detect "what is running"; we forward the whole
  configured set and let TCP sort it out on demand.
- **One supervised `ssh -L` process per port.** The only real "1 of 5 fails"
  case is a *local* port collision; per-port isolation makes cascading failure
  impossible by construction and gives per-service status. Cost is negligible
  (an idle SSH connection + keepalive bytes).
- **1-second fixed retry, no backoff** — for a personal tool this is fine and
  gives sub-second recovery when the server returns. **Exception:** an
  authentication failure (`Permission denied (publickey)`) is not
  retry-fixable, so on auth failure we stop that tunnel and report, instead of
  spamming forever.
- **Detection is free via config, not polling.** `forge forwarding start`
  scans the workspace's Docker published ports once, saves them to config, and
  (re)launches the supervisor. You only re-run it when you *add or remove a
  service*.

### 2.7 No OS autostart integration

We deliberately skip launchd / systemd / Windows Task Scheduler — three
platform-specific integrations for a small tool. `forge spawn` simply detaches a
background supervisor process (idempotent: a no-op if already running). Because
it is idempotent it is safe to drop a single line into your shell rc:

```sh
forge spawn >/dev/null 2>&1
```

The first terminal after a reboot brings the supervisor up; every later shell is
a fast no-op. Same effect as autostart, zero installation, portable everywhere a
shell exists. The only OS-specific detail is detaching the process (`Setsid` on
Unix, `DETACHED_PROCESS` on Windows) — a few lines, not three integrations.

---

## 3. Architecture

```
        laptop (client)                         VPS (behind firewall, SSH only)
 ┌──────────────────────────┐                ┌────────────────────────────────┐
 │ forge (CLI)              │                │ forge-agent (invoked via SSH)   │
 │  ├ host add/list/remove  │   ssh          │  (root, lifecycle only)         │
 │  ├ workspace create/…    │ ─────────────▶ │   useradd, home, keys, meta     │
 │  ├ workspace ssh/claude  │   ssh -t       │                                 │
 │  │   (direct as ws user) │ ─────────────▶ │ workspace user "crm"            │
 │  ├ workspace expose      │   ssh -L       │   ~/  git  ~/.claude            │
 │  └ spawn / forwarding    │ ═════════════▶ │   tmux session "claude"         │
 │       └ supervisor ──────┼─ ssh -L (xN) ─▶│   docker compose (project=crm)  │
 └──────────────────────────┘                └────────────────────────────────┘
        ~/.forge/config.json                     /home/workspaces/<name>/
        ~/.forge/forge.pid                        /home/workspaces/<name>/workspace.json
        ~/.forge/status.json
        ~/.forge/forge.log
```

Two binaries from one module:

- **`cmd/forge`** — the local CLI (runs on laptop).
- **`cmd/forge-agent`** — the remote privileged helper (runs on the VPS, invoked
  over SSH, needs root for user management).

---

## 4. Package layout

```
forge/
├── cmd/
│   ├── forge/main.go            # CLI entrypoint → internal/cli
│   └── forge-agent/main.go      # agent entrypoint → internal/agent
├── internal/
│   ├── cli/                     # command dispatch + handlers (laptop side)
│   │   ├── cli.go               #   router + usage
│   │   ├── host.go              #   host add/list/remove
│   │   ├── workspace.go         #   create/delete/list + <name> ssh/claude/expose
│   │   ├── forwarding.go        #   forwarding start/stop/status
│   │   └── spawn.go             #   spawn + hidden __run-supervisor
│   ├── config/config.go         # ~/.forge/config.json load/save, Host, forwards
│   ├── sshx/sshx.go             # build & run ssh commands (interactive + capture)
│   ├── supervisor/supervisor.go # per-port reconnect loop, status.json, pidfile
│   ├── agentproto/agentproto.go # JSON types shared by CLI and agent
│   └── agent/agent.go           # privileged workspace lifecycle (server side)
├── docs/IMPLEMENTATION_PLAN.md
├── go.mod                       # no external deps
├── Makefile
└── README.md
```

---

## 5. Command surface

```
forge host add <ssh-target> --alias=<alias>     # e.g. user@1.2.3.4
forge host list
forge host remove <alias>

forge workspace create <name> <host-alias>
forge workspace delete <name>
forge workspace list                             # NAME  HOST  STATUS

forge workspace <name> ssh                       # shell as the workspace user
forge workspace <name> claude                    # attach-or-create tmux + Claude
forge workspace <name> claude renew              # kill + fresh session (reset context)
forge workspace <name> claude stop               # kill session
forge workspace <name> expose <port>             # one-off foreground ssh -L

forge forwarding start [name]                    # scan docker ports, save, (re)spawn
forge forwarding stop
forge forwarding status                          # per-tunnel state
forge spawn                                       # idempotent: ensure supervisor up
```

`forge workspace <name> <action>` is dispatched by the workspace handler:
`create`/`delete`/`list` are subcommands; anything else is treated as
`<name> <action>`.

---

## 6. Data formats

### 6.1 Client config — `~/.forge/config.json`

```json
{
  "hosts": {
    "myserver": { "alias": "myserver", "user": "root", "addr": "1.2.3.4", "port": 22 }
  },
  "forwards": {
    "myserver": { "crm": [3000, 5173], "api": [8080] }
  }
}
```

`forwards` is host → workspace → ports, populated by `forwarding start`.

### 6.2 Workspace metadata — `/home/workspaces/<name>/workspace.json` (server)

```json
{ "name": "crm", "owner": "crm", "tmux_session": "claude",
  "created_at": "…", "last_used": "…" }
```

Intentionally minimal. `last_used` is best-effort (updated on attach); status is
*not* stored — it is derived live from `tmux has-session`.

### 6.3 Supervisor status — `~/.forge/status.json`

```json
{ "pid": 12345, "updated_at": "…",
  "tunnels": [
    { "host": "myserver", "workspace": "crm", "port": 3000,
      "state": "up", "detail": "" },
    { "host": "myserver", "workspace": "crm", "port": 5173,
      "state": "retrying", "detail": "connection refused" }
  ] }
```

Tunnel states: `up`, `retrying`, `error` (terminal, e.g. auth failure).

---

## 7. Agent protocol

CLI → `ssh <admin>@<host> sudo forge-agent <op> [flags]`. Agent → JSON on stdout,
non-zero exit + JSON `{ "error": "…" }` on failure.

| Op | Args | Returns |
| --- | --- | --- |
| `workspace-create` | `--name`, `--pubkey` (base64) | `{ workspace }` |
| `workspace-delete` | `--name` | `{ ok: true }` |
| `workspace-list` | — | `{ workspaces: [ … ] }` (name, status) |
| `workspace-status` | `--name` | `{ name, status }` |

`workspace-create` does, as root: `useradd -m -d /home/workspaces/<name>`, add to
`docker` group, install the client pubkey into `~/.ssh/authorized_keys`, seed
`.bashrc` with the `claude` guard function, write minimal git config and
`workspace.json`. `status` = `tmux has-session -t claude` run as the workspace
user → `running` / `stopped`.

---

## 8. Workspace environment & the `claude` guard

### 8.1 Environment file

Each workspace gets `~/.forge/env` (written at create time), a `KEY=value` file
that is the single source of truth for the workspace's environment. Today it
holds:

```
COMPOSE_PROJECT_NAME=<name>
```

`COMPOSE_PROJECT_NAME` makes every `docker compose` invocation use the workspace
name as its project name — so the same repo cloned into different workspaces
never collides on container/network/volume names, and it enforces the
project==workspace convention the port scan relies on.

It lives in a file rather than a bare `export` in `.bashrc` because `.bashrc` is
sourced only by *interactive* shells (and most distros' `.bashrc` returns early
for non-interactive ones). A `docker compose` run non-interactively — a script,
`bash -c`, a `make dev` target — would miss a `.bashrc`-only variable. So the
file is sourced from **both** ends: `.bashrc` sources it for interactive shells,
and Forge's own launch commands source it (`set -a; . ~/.forge/env; set +a`)
before starting the Claude/tmux session, covering every invocation path.

> Host-port de-confliction across workspaces/clones (e.g. a `FORGE_PORT_BASE`)
> is a natural future entry in this same file, but the allocation scheme is not
> yet settled — see §10.

### 8.2 The `claude` guard

Added to each workspace user's `.bashrc` at create time. Shadows the binary for
*interactive* shells so a stray `claude` doesn't launch a session that dies on
disconnect:

```sh
claude() {
  echo "⚠  Claude runs managed via tmux so it survives disconnects."
  echo "   Use:  forge workspace <name> claude"
}
```

`forge`'s own invocation calls the binary non-interactively
(`tmux new -A -s claude claude`), where `.bashrc`'s function does not apply, so it
never collides with itself. A power user can still bypass with `command claude` —
obscure enough to be safe, present enough to never lock you out.

---

## 9. Build order (implementation milestones)

1. **Config + host commands** — `config` package, `host add/list/remove`. Fully
   working, testable locally without a server.
2. **sshx + workspace access** — `ssh`, `claude` (+ `renew`/`stop`), `expose`.
   Real `ssh`/`tmux` command composition.
3. **Agent** — `forge-agent` lifecycle ops; wire `workspace create/delete/list`.
4. **Supervisor + spawn + forwarding** — per-port reconnect loop, docker port
   scan, `status.json`, pidfile, detached `spawn`, shell-rc idempotency.
5. **Polish** — `forge forwarding status` output, error messages, README.

Milestones 1–2 are useful on their own; the supervisor (4) is where the design
value concentrates.

---

## 10. Deferred (future ideas, not MVP)

Multiple Claude sessions per workspace, workspace templates / presets, automatic
git clone, managed SSH keys, terminal streaming, session history, a web/mobile
UI. Background/daemonized `expose` with a tunnel registry (MVP `expose` is
foreground, Ctrl-C to stop). OS-level autostart. Implement only on real need.
