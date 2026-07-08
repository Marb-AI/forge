# Forge

A lightweight Go CLI that manages persistent remote **Claude Code** workspaces
over SSH.

One powerful VPS behind a firewall (only SSH is open). On it, several isolated
workspaces — each a normal Linux user with its own home, git config, SSH keys,
and one persistent Claude session running inside **tmux** so it never dies on
disconnect. Forge is the thin client that drives all of it from your laptop.

It leans on standard Linux primitives instead of reinventing them:

- **tmux** for persistent sessions (not a custom session manager)
- **SSH** for transport (not a custom protocol or an internet-facing daemon)
- **Linux users** for isolation (not containers)
- **SSH tunnels** for exposing dev servers (not a reverse proxy)

See [`docs/IMPLEMENTATION_PLAN.md`](docs/IMPLEMENTATION_PLAN.md) for the full
design and the reasoning behind each decision.

> Status: **scaffold**. The structure, config, command routing, SSH/tmux
> composition, and the forwarding supervisor are in place. Some server-side
> agent operations are implemented but need a real Linux host to exercise.

## Build

```sh
make build          # builds bin/forge and bin/forge-agent
make agent-linux    # cross-compiles the agent for the server
```

Zero external dependencies — standard library only.

## Quick start

```sh
# register your server
forge host add root@1.2.3.4 --alias=myserver

# create a workspace (a Linux user "crm" on myserver)
forge workspace create crm myserver

# open a persistent Claude session (attach-or-create, survives disconnect)
forge workspace crm claude

# a plain shell inside the workspace
forge workspace crm ssh

# expose a dev server once, foreground (Ctrl-C to stop)
forge workspace crm expose 3000

# discover the workspace's Docker ports, save them, and keep them tunnelled
forge forwarding start crm
forge spawn          # ensure the background tunnel supervisor is running
forge forwarding status
```

To make forwarding survive reboots without any OS integration, drop one line in
your shell rc — it's idempotent, so every later shell is a fast no-op:

```sh
forge spawn >/dev/null 2>&1
```

## Layout

```
cmd/forge         local CLI (laptop)
cmd/forge-agent   privileged helper (server, invoked over SSH)
internal/         config, sshx, supervisor, agent, command handlers
docs/             design of record
```
