# Forge

A lightweight Go CLI that manages persistent remote **Claude Code** workspaces
over SSH.

One powerful VPS behind a firewall (only SSH is open). On it, several isolated
workspaces — each a normal Linux user with its own home, git config, SSH keys,
and one persistent Claude session running inside **tmux** so it never dies on
disconnect. Forge is the thin client that drives all of it from your laptop (and,
because the session lives on the server, you can reattach from anywhere — even a
phone with an SSH app).

It leans on standard Linux primitives instead of reinventing them:

- **tmux** for persistent sessions (not a custom session manager)
- **SSH** for transport (not a custom protocol or an internet-facing daemon)
- **Linux users** for isolation (not containers)
- **SSH tunnels** for exposing dev servers (not a reverse proxy)

Zero external dependencies — standard library only. See
[`docs/IMPLEMENTATION_PLAN.md`](docs/IMPLEMENTATION_PLAN.md) for the full design
and the reasoning behind every decision.

> **Status: scaffold.** Config, command routing, SSH/tmux composition, the
> forwarding supervisor, and `show ports` work and are tested locally. The
> server-side agent (`useradd`/`tmux`/filesystem) is implemented but needs a
> real Linux host to exercise end to end.

---

## Concepts

| Thing | Is |
| --- | --- |
| **Host** | a registered server, reached only over SSH |
| **Workspace** | a Linux user on a host (`crm` → home `/home/workspaces/crm`), with its own keys, git config, and one Claude session |
| **Claude session** | a tmux session named `claude` owned by the workspace user; survives disconnects |
| **Forwarding** | a background supervisor on your laptop that keeps `ssh -L` tunnels alive, reconnecting on any blip |

Two binaries from one module:

- **`forge`** — the local CLI (laptop).
- **`forge-agent`** — a privileged helper on the server, invoked over SSH per
  operation (never a daemon). Only used for workspace lifecycle (create / delete
  / list / status); everything else is direct SSH as the workspace user.

---

## Build

```sh
make build          # bin/forge and bin/forge-agent
make agent-linux    # cross-compile the agent for the server (linux amd64/arm64)
```

## Server setup (once per host)

The server needs standard tools: `docker` (with users in the `docker` group),
`tmux`, `iproute2` (`ss`), and the usual `useradd`/`userdel`/`usermod`/`runuser`.

1. Put the agent on the server and let the admin SSH user run it as root without
   a password:

   ```sh
   scp bin/forge-agent-linux-amd64 you@server:/usr/local/bin/forge-agent
   ssh you@server 'sudo chmod +x /usr/local/bin/forge-agent'
   # /etc/sudoers.d/forge:
   #   you ALL=(root) NOPASSWD: /usr/local/bin/forge-agent
   ```

2. Register it from your laptop:

   ```sh
   forge host add you@server --alias=myserver
   ```

Forge installs your SSH **public key** (from `~/.ssh/*.pub`, or `FORGE_PUBKEY`)
into each workspace user's `authorized_keys`, so you can SSH in as the workspace
user directly.

---

## Quick start

```sh
forge host add you@1.2.3.4 --alias=myserver     # register a server
forge workspace create crm myserver             # a Linux user "crm" on myserver
forge workspace crm claude                      # persistent Claude session (attach-or-create)
forge workspace crm ssh                          # a plain shell in the workspace
```

Then add the project (see below), expose its dev servers, and start coding.

To make forwarding survive laptop reboots without any OS integration, drop one
line in your shell rc — `spawn` is idempotent, so every later shell is a fast
no-op:

```sh
forge spawn >/dev/null 2>&1
```

---

## Add a new project

Forge gives you the environment; it does **not** clone for you (automatic git
clone is a future idea). Because a fresh workspace has no git credentials, decide
how it authenticates first. A full first run:

```sh
forge workspace create shop myserver             # new Linux user "shop"

# --- git auth: pick one ---------------------------------------------------
# (a) Forward your SSH agent for the clone — no key left on the server:
forge workspace shop ssh -A
#     git clone git@github.com:you/shop.git
#
# (b) Or give the workspace its own deploy key (tidier, no forwarding):
forge workspace shop ssh
#     ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519 -N ''
#     cat ~/.ssh/id_ed25519.pub        # add as a deploy key on the repo, then clone
# --------------------------------------------------------------------------

# In the workspace shell, set your commit identity and bring it up:
#     cd shop
#     git config user.name  "You"
#     git config user.email "you@example.com"
forge show ports myserver                         # paste the taken ports to Claude,
#     …Claude edits .env / compose to pick free host ports…
#     make dev            # or `docker compose up`, whatever the project uses

# Tunnel the dev servers and open the session:
forge forwarding start shop                        # scan the project's docker ports, tunnel them
forge spawn                                         # ensure the tunnel supervisor runs
forge forwarding status                             # per-tunnel state
forge workspace shop claude                         # persistent Claude session
```

> Agent forwarding (`-A`) trusts the server with your forwarded keys for the
> duration of the session — fine for your own single-user box, which the model
> already trusts. Prefer a per-repo deploy key if you'd rather not forward.

For running the **same** repo in several parallel workspaces, or a
**backend + frontend** across two repos, see *Workflows & best practices* below.

## Command reference

```
Hosts
  forge host add <ssh-target> --alias=<alias>    e.g. you@1.2.3.4[:port]
  forge host list
  forge host remove <alias>

Workspaces
  forge workspace create <name> <host-alias>
  forge workspace delete <name>
  forge workspace list                            NAME  HOST  STATUS

  forge workspace <name> ssh [-A]                 shell as the workspace user (-A forwards your SSH agent)
  forge workspace <name> claude                   attach-or-create the Claude session
  forge workspace <name> claude renew             kill + fresh session (reset context/tokens)
  forge workspace <name> claude stop              kill the session
  forge workspace <name> expose <port>            one-off ssh -L, foreground (Ctrl-C stops)

Forwarding
  forge forwarding start [name]                   scan docker ports, save, (re)spawn supervisor
  forge forwarding stop
  forge forwarding status
  forge spawn                                      ensure the tunnel supervisor is up (idempotent)

Info
  forge show ports [host]                          listening + forwarded ports (paste to Claude)
```

`claude renew` = `stop` + fresh start; use it to clear a bloated context window.
Your login to Claude persists in the workspace's `~/.claude`, so `renew`/`stop`
never touch authentication.

---

## How it works (the parts worth knowing)

**Persistent sessions.** `forge workspace crm claude` runs
`tmux new -A -s claude claude` as the `crm` user — attach if it exists, else
create. The client SSH connection can die; tmux keeps Claude alive server-side,
you just reattach.

**Forwarding is supervised.** A bare `ssh -L` neither waits for a down server nor
reconnects. Forge runs **one supervised `ssh -L` per port**, each with a
1-second retry, so a blip or a server reboot self-heals within a second. A port
whose service is *down* is fine — `-L` binds locally regardless and returns
connection-refused until the service is up, then just works. An **auth failure**
is terminal (retrying a bad key never helps): that tunnel stops and is reported
in `forge forwarding status`.

**Ports are reported, not allocated.** Claude sets the ports on the project
(editing `.env` / compose). Forge just tells you what's taken — `forge show
ports` prints the union of what's actually listening (`ss`) and what Forge is
forwarding. Paste it to Claude: *"pick host ports that avoid these."*

**Per-workspace env.** At create, Forge writes `~/.forge/env` in the workspace
(sourced from `.bashrc` and from Forge's launch commands, so it applies to
interactive shells, scripts, and `make` targets alike). It sets:

```
COMPOSE_PROJECT_NAME=<workspace>
```

so `docker compose` scopes its project (and, in tooling that names its network
off `COMPOSE_PROJECT_NAME`, the network too) to the workspace — parallel clones
stay isolated automatically. Add your own entries to this file if you like.

---

## Workflows & best practices

**One project per workspace.** Keep the model simple: a workspace maps to one
Claude session on one project. Name the workspace after the project.

**Multisession dev (clones of one repo).** Create several workspaces from the
same repo (`crm`, `crm-2`, `crm-feature`). Each gets its own
`COMPOSE_PROJECT_NAME`, so compose projects/networks don't collide. Host ports
still need to differ — parameterize them in each repo's `.env`
(`${API_HOST_PORT:-8000}` style) and let Claude pick free values from
`forge show ports`. Then `forge forwarding start <name>` tunnels each clone's
ports independently.

**Backend + frontend across two repos.** Two repos, each with its own
Makefile/compose, run as **separate projects** — don't force them under one
name. How they talk depends on where the frontend runs:

- **FE is a container** → put it on the same docker network as the backend
  (e.g. an `external` network both attach to) and reach the backend by service
  name. The backend API needs **no** host port.
- **FE runs on the host** (a Metro/Expo/Vite dev server, etc.) → it reaches the
  backend over HTTP, so the backend API **must** be published on a host port and
  tunnelled with `forge forwarding`; point the FE's API URL at that
  `localhost:<port>`.

Backend-internal services (db, gRPC, …) always talk by service name over the
docker network and never need publishing.

**Let the project own its lifecycle.** `make dev`, `make logs`, `make migrate`,
restarts, backups — those live in the repo, not in Forge. Forge only provides
*access* to the environment.

**Guardrail.** Running `claude` directly in a workspace shell prints a hint to
use `forge workspace <name> claude` instead — a bare launch would die on
disconnect. (A power user can still bypass with `command claude`.)

---

## Layout

```
cmd/forge         local CLI (laptop)
cmd/forge-agent   privileged helper (server, invoked over SSH)
internal/         config, sshx, supervisor, agent, command handlers
docs/             design of record
```

## Non-goals

Forge does not manage Docker/Compose lifecycle, Kubernetes, deployments, logs,
restarts, backups, snapshots, CI/CD, or build pipelines. Those belong to each
project's own tooling.
