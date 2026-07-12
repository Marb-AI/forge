# Forge

Persistent remote **Claude Code** workspaces over SSH.

Run Claude Code on a powerful server that never sleeps, in isolated workspaces
you reach from your laptop — or your phone. Each workspace keeps its Claude
session running in the background (in tmux), so it survives SSH disconnects, a
closed laptop lid, even your machine rebooting. Reattach and Claude is exactly
where you left it.

Drive it from the terminal, or from a [browser UI](#browser-ui) (`forge ui`) —
tabs per workspace, the live Claude session, a read-only file tree, and a shell
that pops over the top. Same SSH and tmux underneath, either way.

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

- installs git, make, tmux, **docker + compose**, and **gh** (Debian/Ubuntu and Fedora/RHEL),
- creates the host's **git identity** — an ed25519 SSH key — and prints its public
  half, so you can register it on GitHub. An existing key is kept, never
  regenerated, and re-running `prepare` prints it again,
- locks the firewall to **SSH-only** — nothing else reachable from the internet,
  including Docker's published ports,
- disables SSH password auth (keys only), guarded so it can't lock you out.

Opt out of those last two with `--no-firewall` / `--no-ssh-harden`.

Then authenticate `gh` once for the whole server — it's interactive, so it gets
its own command rather than living inside `prepare`:

```sh
forge host gh-login myserver
```

Both credentials — the SSH key and the `gh` login — are host-wide and copied into
every workspace at `create`. You register the key on GitHub once and log `gh` in
once per server, not once per workspace.

> It makes real system changes (packages, iptables, sshd) — test on a throwaway
> host first.

Then create a workspace and open its persistent Claude session:

```sh
forge workspace create crm myserver     # an isolated workspace "crm"
forge workspace crm claude              # open Claude — survives disconnects
forge workspace crm ssh                 # a plain shell inside the workspace
```

Each workspace gets its own Claude Code install; the first time you open it,
Claude asks you to accept the workspace and log in (once per workspace —
isolated, no shared state). After that it launches with **Remote Control**, so
the session also appears in the Claude mobile/web app named after the workspace
(`marbai-01`, `marbai-02`, … cluster together) — steer it from your phone, or
reattach over SSH from your laptop. It's always right where you left it.

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

Forge gives you the environment; you clone into it. A workspace inherits the
host's git identity (the key `prepare` printed), so if you registered that key on
GitHub the clone just works. A full first run:

```sh
forge workspace create shop myserver             # new workspace "shop"

# In the workspace shell, clone, set your commit identity, bring the project up:
forge workspace shop ssh
#     git clone git@github.com:you/shop.git
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

> **Why the workspace has its own key.** `forge workspace <name> ssh` also
> forwards your local SSH agent, which is handy in an interactive shell. But a
> forwarded agent cannot serve the Claude session: tmux outlives the SSH
> connection that started it, so the forwarded socket is stale on reattach — and
> gone entirely once your laptop sleeps, which is the case Forge exists for. The
> key copied in at `workspace create` is on disk in the workspace, so Claude can
> clone, pull and push with your laptop shut.

> **One key per host, not per workspace.** Every workspace on a host shares the
> host's identity. That matches the boundary Forge actually draws: workspaces are
> scoped (own `$HOME`, own tmux server, own compose project), not isolated —
> workspace users are in the `docker` group, so they can already reach each
> other's files. It also keeps registration to one step: a GitHub *deploy key* is
> bound to a single repo, but this key registered as an *account* SSH key works
> for every repo in every workspace.

> **`gh` is installed but not logged in.** Authenticating is an interactive
> browser/token flow, so it can't happen during `prepare`. The first time Claude
> needs it, it'll say so — `forge workspace <name> ssh`, then `gh auth login`.

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
  forge workspace <name> claude checkpoint        save a handoff to memory, then restart from it
  forge workspace <name> expose <port>            tunnel one port, foreground (Ctrl-C stops)

Forwarding
  forge forwarding start [name]                   find the project's ports, save, keep them tunnelled
  forge forwarding stop
  forge forwarding status
  forge spawn                                      keep tunnels alive in the background (idempotent)

UI
  forge ui                                         start the browser UI and open it (= ui start)
  forge ui stop
  forge ui status
  forge ui port <port>                             change the port it listens on

Info
  forge show ports [host]                          ports in use on the server (paste to Claude)
```

`claude renew` = stop + fresh start; use it to clear a bloated context window.
`claude checkpoint` (run from another terminal while the session is idle) asks
Claude to write a handoff to its memory, waits for it, then restarts the session
so it continues from memory with a fresh context — for long-running work.
Your login to Claude persists in the workspace, so `renew`/`stop` never touch it.

It waits for two things, not one: the confirmation token, and then the pane going
quiet. The token only means Claude *believes* it's finished — it may print it and
carry on writing the memory index. Restarting on the token alone truncates exactly
the handoff the checkpoint exists to preserve.

**Copying text out of a session.** The workspace's tmux has `mouse on`, so
dragging selects and copies straight to your local clipboard (over SSH, via the
OSC 52 escape), and the wheel scrolls back through history. The trade-off is that
your terminal's own selection now needs **Shift** (or **Option** in some
terminals) held down, since a plain drag belongs to tmux.

---

## Browser UI

```sh
forge ui
```

Starts a small local server and opens it. Everything the CLI does to a workspace,
you can do here — it is a second front end over the same SSH and tmux, calling the
same code, not a reimplementation that quietly drifts.

- **Tabs** across the top, one per workspace, with a live status dot. **+** opens
  a wizard that creates a workspace — and can register a whole new server first,
  streaming the `host prepare` run so you watch it install rather than guess.
- **The Claude session** fills the middle, as a real terminal. It's the same tmux
  session `forge workspace <name> claude` attaches to, so closing the tab just
  detaches — Claude keeps working. Its clickable options work too: a mouse click
  is just more input, and it takes the same path as typing.
- **Checkpoint, restart, stop/start** on the right, wired to the commands of the
  same name. A stopped session doesn't quietly come back the moment you click its
  tab — it shows a **Start** button, and starting it is exactly
  `forge workspace <name> claude`.
- **A read-only file tree** on the left, rooted at the workspace and unable to
  leave it. Files carry their language's icon; click one and it opens over the
  terminal with syntax highlighting. Read-only is the point: Claude writes the
  code, you inspect it. Dotfiles at the root (plus `.git` and `.claude` anywhere)
  hide behind the eye toggle.
- **An SSH shell** that opens *over* the terminal — the same box a file opens in,
  so the tree and the rail stay put and Claude never gets reflowed. Hiding it
  **keeps the shell running**: you come back to the same shell, same directory,
  same half-finished command.
- **Settings** holds the things you'd otherwise drop to the CLI for, and the ones
  worth thinking about first: deleting a workspace, removing a server, and the UI
  port.
- Light and dark themes.

**Nothing destructive happens on one click.** Stop, restart, checkpoint, removing
a server and deleting a workspace each explain what exactly is about to be lost
before they do it. Deleting a workspace runs `userdel -r` on the server — the
user and its entire home, every file and every uncommitted change in it — so that
one makes you type the workspace's name.

**The port.** It defaults to `47615` — deliberately obscure, so it won't collide
with a dev server. Change it in Settings or with `forge ui port <port>`; the choice
is saved in `~/.forge/config.json`, and a running UI needs a restart to pick it up:

```sh
forge ui port 8099
forge ui stop && forge ui
```

**No login, and none needed.** The server binds to `127.0.0.1` only, so nothing
off your machine can reach it. It still checks the `Host` header (so a rebound
DNS name can't get in), gates every request on a random token that `forge ui` puts
in the URL it opens and then keeps in a Strict-SameSite cookie, and refuses
cross-origin writes — which is what stops another tab in your browser from driving
your workspaces. No password to manage.

**Single binary, still.** The HTML, JS and CSS — xterm.js, highlight.js and the
file-type icons included — are compiled into `forge` itself. There is nothing to
install and no build step; `make` is still just Go. (`FORGE_UI_DEV=<repo>` serves
them from disk while working on the UI.)

> The terminal needs a local pty, which Windows doesn't provide, so the browser UI
> is macOS and Linux only for now. The rest of the Windows client is unaffected.

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
