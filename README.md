# Forge

Persistent remote **Claude Code** workspaces over SSH.

Run Claude Code on a powerful server that never sleeps, in isolated workspaces
you reach from your laptop — or your phone. Each workspace keeps its Claude
session running in the background (in tmux), so it survives SSH disconnects, a
closed laptop lid, even your machine rebooting. Reattach and Claude is exactly
where you left it.

Forge is a single small binary. On the server it uses nothing exotic — plain
Linux users for isolation, tmux for sessions, SSH tunnels for dev servers — so
there's little to trust or maintain.

> **Status: early.** The laptop side works and is tested. Server provisioning
> (`host prepare`) and workspace management make real system changes and haven't
> been run end to end on a live host yet — **try it on a throwaway server first.**

---

## Install

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/Marb-AI/forge/main/install.sh | sh
```

That drops the right binary for your machine into `~/.forge/bin` and links it onto
your PATH. Re-run any time to upgrade.

By hand, or on Windows: download the binary for your OS/arch from the
[latest release](https://github.com/Marb-AI/forge/releases/latest) and put it on
your PATH (`forge-windows-amd64.exe` on Windows).

---

## Quick start

Point Forge at a **bare** server — connect as **root** or a passwordless-sudo
user, and it provisions everything, no cloud-console clicking:

```sh
forge host prepare root@1.2.3.4 --alias=myserver
```

`prepare` is idempotent and:

- installs git, tmux, and **docker + compose** (Debian/Ubuntu and Fedora/RHEL),
- locks the firewall to **SSH-only** — nothing else reachable from the internet,
  including Docker's published ports,
- disables SSH password auth (keys only), guarded so it can't lock you out.

Opt out of those last two with `--no-firewall` / `--no-ssh-harden`.

> It makes real system changes (packages, iptables, sshd) — test on a throwaway
> host first.

Then create a workspace and open its persistent Claude session:

```sh
forge workspace create crm myserver     # an isolated workspace "crm"
forge workspace crm claude              # open Claude — survives disconnects
forge workspace crm ssh                 # a plain shell inside the workspace
```

Each workspace gets its own Claude Code install; the first time you open it,
Claude prompts you to log in (once per workspace — isolated, no shared state).
After that, reattach any time, from your laptop or a phone with an SSH app, and
Claude is right where you left it.

---

## Concepts

| | |
| --- | --- |
| **Host** | a server you registered, reached only over SSH |
| **Workspace** | an isolated Linux user on a host (`crm`), with its own home, git config, keys, and one Claude session |
| **Claude session** | a background tmux session that keeps Claude alive across disconnects |
| **Forwarding** | keeps your dev servers tunnelled to `localhost`, auto-reconnecting through blips and reboots |

---

## Add a new project

Forge gives you the environment; you clone into it. A fresh workspace has no git
credentials, so pick how it authenticates first. A full first run:

```sh
forge workspace create shop myserver             # new workspace "shop"

# --- git auth: pick one ---------------------------------------------------
# (a) `ssh` forwards your SSH agent by default, so the clone just uses your keys:
forge workspace shop ssh
#     git clone git@github.com:you/shop.git
#
# (b) Or give the workspace its own deploy key (survives into the Claude session,
#     see the note below):
#     ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519 -N ''
#     cat ~/.ssh/id_ed25519.pub        # add as a deploy key on the repo, then clone
# --------------------------------------------------------------------------

# In the workspace shell, set your commit identity and bring the project up:
#     cd shop
#     git config user.name  "You"
#     git config user.email "you@example.com"
forge show ports myserver                         # paste the taken ports to Claude,
#     …Claude edits .env / compose to pick free host ports…
#     make dev            # or `docker compose up`, whatever the project uses

# Tunnel the dev servers and open the session:
forge forwarding start shop                        # find the project's ports, tunnel them
forge spawn                                         # keep tunnels alive in the background
forge forwarding status                             # per-tunnel state
forge workspace shop claude                         # open Claude
```

To keep tunnels alive across laptop reboots, add one line to your shell rc
(`spawn` is idempotent — every later shell is a fast no-op):

```sh
forge spawn >/dev/null 2>&1
```

> **Agent forwarding vs the Claude session.** `forge workspace <name> ssh`
> forwards your SSH agent by default — great for an interactive shell (clone,
> push, pull just work). But it does **not** reach the persistent Claude session
> reliably: tmux outlives the SSH connection, so the forwarded agent goes stale on
> reattach. If **Claude itself** needs to push/pull, give the workspace a **deploy
> key** (option b) — it works regardless of how you connect.

---

## Commands

```
Hosts
  forge host prepare <ssh-target> --alias=<alias> [--no-firewall] [--no-ssh-harden]
                                                  provision a bare server + register it
  forge host add <ssh-target> --alias=<alias>     register an already-prepared server
  forge host list
  forge host remove <alias>

Workspaces
  forge workspace create <name> <host-alias>
  forge workspace delete <name>
  forge workspace list                            NAME  HOST  STATUS

  forge workspace <name> ssh [--no-agent]         shell as the workspace user (agent forwarded by default)
  forge workspace <name> claude                   open the Claude session (attach-or-create)
  forge workspace <name> claude renew             fresh session (reset context / save tokens)
  forge workspace <name> claude stop              stop the session
  forge workspace <name> expose <port>            tunnel one port, foreground (Ctrl-C stops)

Forwarding
  forge forwarding start [name]                   find the project's ports, save, keep them tunnelled
  forge forwarding stop
  forge forwarding status
  forge spawn                                      keep tunnels alive in the background (idempotent)

Info
  forge show ports [host]                          ports in use on the server (paste to Claude)
```

`claude renew` = stop + fresh start; use it to clear a bloated context window.
Your login to Claude persists in the workspace, so `renew`/`stop` never touch it.

---

## How it works

**Sessions never die.** `forge workspace crm claude` runs Claude inside a tmux
session on the server. Your SSH connection can drop; Claude keeps running. You
just reattach.

**Tunnels heal themselves.** A plain SSH tunnel dies on any hiccup and stays
dead. Forge supervises one tunnel per port and reconnects within a second of a
blip or a server reboot — a service that's momentarily down is fine, it just
starts working once it's up. A wrong SSH key is reported instead of retried
forever.

**Ports: reported, not assigned.** Claude sets the ports on your project. Forge
just tells you what's already taken — `forge show ports` lists everything
listening on the server plus what it's forwarding. Paste it to Claude: *"pick
ports that avoid these."*

**Parallel work stays isolated.** Each workspace scopes `docker compose` to its
own project name automatically, so the same repo cloned into several workspaces
(for parallel Claude sessions) doesn't collide.

---

## Workflows

**One project per workspace.** Simplest model: a workspace is one Claude session
on one project. Name the workspace after the project.

**Parallel sessions on one repo.** Create several workspaces from the same repo
(`crm`, `crm-2`, `crm-feature`). Compose projects/networks won't collide; give
each a different host port (parameterize it in the repo's `.env` and let Claude
pick free ones from `forge show ports`), then `forge forwarding start` tunnels
each independently.

**Backend + frontend across two repos.** Run them as separate projects. If the
frontend is a container, put it on the backend's docker network and reach it by
service name — no host port for the API. If the frontend runs on your host (a
Metro/Expo/Vite dev server), publish the API on a host port, tunnel it, and point
the frontend's API URL at that `localhost:<port>`.

**The project owns its lifecycle.** `make dev`, `make logs`, `make migrate`,
restarts, backups — those live in the repo. Forge only gives you access to the
environment.

---

## Non-goals

Forge does not manage Docker/Compose lifecycle, Kubernetes, deployments, logs,
restarts, backups, CI/CD, or build pipelines. Those belong to each project.

---

## License

MIT — see [LICENSE](LICENSE).

<sub>Hacking on Forge itself? `make build` (dev) or `make release` (all
platforms) — Go standard library only, no dependencies.</sub>
