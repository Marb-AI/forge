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
- disables SSH password auth (keys only), guarded so it can't lock you out,
- schedules a **nightly Docker clean-up** at 03:00 (a systemd timer), because a
  build server fills its disk up and a full disk breaks every workspace at once.
  That's 03:00 in the *server's* timezone — UTC on a stock VPS, so an hour or two
  either side of your own small hours.

Opt out of those last three with `--no-firewall` / `--no-ssh-harden` /
`--no-docker-prune`.

**What the clean-up removes** — deliberately little, because the cost of being too
eager is a workspace that has to rebuild from scratch in the morning:

| | |
|---|---|
| **Dangling images** | the layer sets a rebuild left behind. *Not* `prune -a`, which would also delete tagged images that no container happens to be running right now — i.e. the images of every workspace you aren't currently using. |
| **Build cache** | usually the biggest win. |
| **Stopped containers: no** | 23 MB against 3.9 GB of cache — nothing — and removing one takes its writable layer, so a stack you stopped for the night would need `up` rather than `start` in the morning. |
| **Volumes: never** | that is where your data lives. |

Everything is filtered to `until=24h`, so nothing built today is touched, and an
image a running container uses is never a candidate in the first place.

Check on it from the server itself — as root, or the admin user you prepared with.
(`forge workspace <name> ssh` drops you in as the *workspace* user, which has no
sudo, so this isn't the way in.)

```sh
ssh root@<ip> systemctl list-timers forge-docker-prune   # when it next runs
ssh root@<ip> journalctl -u forge-docker-prune -n 20     # what it reclaimed
ssh root@<ip> forge-docker-prune                         # run it now
```

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
| **Forwarding** | keeps your dev servers tunnelled to `localhost`, following what they publish and auto-reconnecting through blips and reboots |

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
#     …Claude picks host ports from the workspace's own block — it knows the
#        range without being told, so there is nothing to paste…
#     make dev            # or `docker compose up`, whatever the project uses

# Tunnel the dev servers and open the session:
forge spawn                                         # keep tunnels alive in the background
forge forwarding status                             # per-tunnel state
forge workspace shop claude                         # open Claude
```

There is no step here that finds the ports. `spawn` watches what the workspaces
publish and tunnels it — bring a service up on the server and it is on your
`localhost` a few seconds later, take it down and the tunnel goes with it.

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
  forge host prepare <ssh-target> --alias=<alias> [--no-firewall] [--no-ssh-harden] [--no-docker-prune]
                                                  provision a bare server + register it
  forge host add <ssh-target> --alias=<alias>     register an already-prepared server
  forge host list
  forge host remove <alias>

Workspaces
  forge workspace create <name> <host-alias>
  forge workspace delete <name>
  forge workspace list                            NAME  HOST  CLAUDE  (your workspaces; the
                                                  status is the Claude session's)

  forge workspace <name> ssh [--no-agent]         shell as the workspace user (agent forwarded by default)
  forge workspace <name> claude                   open the Claude session (attach-or-create)
  forge workspace <name> claude renew             fresh session (reset context / save tokens)
  forge workspace <name> claude stop              stop the session
  forge workspace <name> claude checkpoint        save a handoff to memory, then restart from it
  forge workspace <name> expose <port>            tunnel one port, foreground (Ctrl-C stops)

Forwarding
  forge forwarding start                          restart the supervisor now (it polls on its own)
  forge forwarding stop
  forge forwarding status
  forge spawn                                      keep tunnels alive in the background (idempotent)

UI
  forge ui                                         start the browser UI and open it (= ui start)
  forge ui stop
  forge ui status
  forge ui port <port>                             change the port it listens on

Ports
  forge ports                                      which workspace owns which block
  forge ports range [<start>-<end>] [--block=N]     the span blocks are allocated from
  forge ports assign [name]                        give a block to workspaces without one

Info
  forge show ports [host]                          ports in use on the server
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
dragging selects and copies straight to your local clipboard, and the wheel
scrolls back through history. The trade-off is that your terminal's own selection
now needs **Shift** (or **Option** in some terminals) held down, since a plain
drag belongs to tmux.

The copy itself is Forge's job, not your terminal's. A workspace is a headless
Linux box — no X, no Wayland, no `xclip` — so nothing there has a clipboard: a
session that copies (a tmux yank, Claude's **press `c`** on a login URL) can only
hand the text to the terminal as an OSC 52 escape and hope. Terminals are a coin
flip on that: Terminal.app has never implemented OSC 52 and has no setting to
turn on, Warp now denies it by default ([CVE-2026-48725][osc52-cve] — a remote
host could silently read or overwrite your clipboard), iTerm2 ships it off, while
Ghostty, WezTerm and kitty allow it. So Forge reads its own SSH output, catches
the escape itself, and puts the text on your clipboard with the local OS tool.
Any terminal, same behaviour — and the browser UI does the same with
`navigator.clipboard`.

Clipboard **reads** (a session asking what you last copied) are refused, not
answered. Claude runs in these sessions with permission prompts off.

[osc52-cve]: https://github.com/warpdotdev/warp/security/advisories/GHSA-wgqj-4c26-7c4g

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
- **The topic** at the top of the left pane: one line saying what this workspace
  is working on, written by Claude itself. You never type one. A hook asks Claude
  to label the workspace whenever the label is missing or older than the session,
  so it appears on its own and follows the work; a topic from before the current
  session is dimmed rather than hidden, because on a stopped workspace "what it
  was last doing" is the thing you came to find out. Hover a tab to read its topic
  without switching to it. Inside the workspace it's just a command —
  `forge-topic <words…>` sets it, `forge-topic` with nothing clears it.
- **A read-only file tree** on the left, rooted at the workspace and unable to
  leave it. Files carry their language's icon; click one and it opens over the
  terminal with syntax highlighting. Read-only is the point: Claude writes the
  code, you inspect it. Dotfiles at the root (plus `.git` and `.claude` anywhere)
  hide behind the eye toggle.
- **The Claude panel**, above the servers: one line per Claude login,
  `5h 92% 7d 56%`, because a rate limit belongs to an *account* — and three workspaces
  signed into the same login are all drawing down the same five-hour window. The
  percentage ambers at three-quarters and reddens at ninety, the same thresholds as
  a disk, and the fullest login sorts to the top, so you see the one about to stop
  working before a turn dies mid-sentence. Nothing is summed: the window is one
  number reported identically by every workspace on the login, so the line shows
  the freshest report — and dims when that report is old, because these figures
  only move while a workspace's Claude is running. A login on API credits has no
  such windows at all and says so rather than showing two zeroes that would imply
  an allowance it doesn't have. Which workspaces draw on a login, when each window
  resets, and how old the reading is are in the tooltip; the panel itself stays
  three lines for three logins. Numbers come from Claude's own status line, which
  Forge installs in each workspace — chaining any status line that was already
  there rather than replacing it.
- **The login, the server and the context** as chips under the topic: the topic says
  what a workspace is doing, these say whose allowance it spends, whose disk it
  fills, and how full its context window is. Context lives here rather than in the
  panel below because it belongs to one session — several workspaces on one login
  have several different contexts, and a global figure would be a number about
  nothing.
- **The ports panel** under the tree: what this workspace publishes, one row per
  port — `web:16000`, and clicking it opens `127.0.0.1:16000`. That is the whole
  feature: you never look a port up, and you never wonder whether it's reachable,
  because the row only offers a link while the tunnel is actually up. Everything
  else is a click-to-copy of `127.0.0.1:<port>`, which is what you'd paste into
  `curl` or a redirect URI anyway — including for a service that plainly isn't
  HTTP, since a link to Postgres is a dead click. A stopped container keeps its
  row (its port is still reserved) and offers **start**; a running one offers
  **stop**. Not `up` — creating containers means knowing which compose file,
  which profiles, whether the repo really starts with `make dev`, and a button
  that guesses is worse than no button. A dev server started by hand, with no
  container around it, is listed and tunnelled like anything else but has no
  buttons: there's nothing Forge could start it back up with. A port published
  outside the workspace's block says so rather than looking broken, and a port
  something on *your* machine is squatting names the process holding it.
- **The servers panel** under the tree: every registered machine with its CPU,
  memory and disk usage, refreshed every ten seconds. It answers the question you
  otherwise ssh in to ask — which box has room, and why one feels slow — and turns
  amber at three-quarters full, red at ninety percent. Collapse it and it stops
  polling; each round costs one SSH round trip per server.
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

**Tunnels follow the containers.** The set isn't fixed when `spawn` starts: every
few seconds Forge asks each server what its workspaces publish and reconciles
against it, so a `docker compose up` on the server puts the port on your
`localhost` on its own, and a `down` takes it away. Tunnels that haven't changed
are left strictly alone — rebuilding them on a timer would drop every connection
through them. Only ports inside a workspace's block are carried, which is exactly
what that workspace was promised.

**A port taken on your laptop says so, by name.** If something local already holds
`16104`, that one tunnel reports `blocked` and names the process — `node (pid
4821)` — rather than failing anonymously or taking the other tunnels with it. It
keeps retrying, so stopping the squatter brings it up within a second, with
nothing to restart.

When no server answers at all — a shut lid, a dead network — Forge keeps the
tunnels it has rather than tearing them all down. They'd come back on the next
poll, but every connection through them would have died in between.

**Ports: every workspace owns a block.** Forge allocates from one range —
`16000–30000` by default — and gives each workspace 100 consecutive host ports of
it, once, at creation. That block never moves, so a port written into a compose
file, an OAuth redirect URI or a CORS whitelist stays correct forever.

The block is what removes you from the loop. Claude is *told* its range, in the
workspace's own Claude memory, so it picks a port without asking anyone and without
you pasting anything — and `forge-ports`, inside the workspace, says which of the
range is already taken. You never look up a port again.

Blocks are unique across **every** server you've registered, not just within one.
That's what lets a workspace's host port double as the port on your laptop: no
mapping to remember, and no chance that two servers hand out the same number and
collide on the machine tunnelling both. It's also why allocating refuses to guess
past an unreachable host — its blocks are unknown, not absent.

```sh
forge ports                       # which workspace owns which block
forge ports range 16000-30000     # the span to allocate from (--block=N per workspace)
forge ports assign                # give a block to workspaces made before this existed
```

**Parallel work stays isolated.** Each workspace scopes `docker compose` to its
own project name automatically, so the same repo cloned into several workspaces
(for parallel Claude sessions) doesn't collide.

---

## Workflows

**One project per workspace.** Simplest model: a workspace is one Claude session
on one project. Name the workspace after the project.

**Parallel sessions on one repo.** Create several workspaces from the same repo
(`crm`, `crm-2`, `crm-feature`). Compose projects and networks won't collide, and
neither will their ports: each workspace holds its own block, so the same repo
brought up in three of them lands on three different sets of host ports with no
per-workspace editing, and each set is tunnelled independently without being
asked.

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
platforms). One dependency — `creack/pty`, which the browser UI's terminal needs;
everything else is the Go standard library. The UI's vendored JS/CSS is checked
in and embedded, so there is no node step.</sub>
