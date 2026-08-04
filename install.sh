#!/bin/sh
# Forge installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/Marb-AI/forge/main/install.sh | sh
#
# Downloads the release binary for your OS/arch into ~/.forge/bin and symlinks it
# onto your PATH. Re-run any time to upgrade. Windows users: download the .exe
# from the releases page.
#
# Env overrides:
#   FORGE_VERSION    tag to install (default: latest)
#   FORGE_HOME       where the binary + config live (default: ~/.forge)
#   FORGE_LINK_DIR   PATH dir to symlink into (default: /usr/local/bin)
#   FORGE_SKIP_AGENT_UPDATE=1  do not touch the servers this machine knows
set -eu

REPO="Marb-AI/forge"
BIN="forge"
FORGE_HOME="${FORGE_HOME:-$HOME/.forge}"
INSTALL_DIR="$FORGE_HOME/bin"
LINK_DIR="${FORGE_LINK_DIR:-/usr/local/bin}"
VERSION="${FORGE_VERSION:-latest}"

# --- detect platform -------------------------------------------------------
case "$(uname -s)" in
	Linux)  OS=linux ;;
	Darwin) OS=darwin ;;
	*) echo "forge: unsupported OS '$(uname -s)' (install.sh supports Linux and macOS)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
	x86_64|amd64)  ARCH=amd64 ;;
	arm64|aarch64) ARCH=arm64 ;;
	*) echo "forge: unsupported architecture '$(uname -m)'" >&2; exit 1 ;;
esac
ASSET="$BIN-$OS-$ARCH"

if [ "$VERSION" = latest ]; then
	URL="https://github.com/$REPO/releases/latest/download/$ASSET"
else
	URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
fi

# --- downloader ------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
else
	echo "forge: need curl or wget" >&2; exit 1
fi

# --- fetch, before anything is disturbed ------------------------------------
# Everything that can fail lives here, and nothing is running differently because
# of it yet: a bad tag, an asset GitHub has not published, a dropped connection.
# This used to come after the daemons were stopped, which meant a failed download
# left them stopped — and the next run, finding nothing up, started nothing back.
# One 404 that way costs a working UI and a set of port tunnels.
echo "forge: installing $VERSION for $OS/$ARCH"
mkdir -p "$INSTALL_DIR"
TARGET="$INSTALL_DIR/$BIN"
# Fetched beside the target and renamed onto it further down, never written over
# it: this is an upgrade as often as an install, and a binary that is executing
# cannot be opened for writing (ETXTBSY) — which is exactly what a running
# `forge ui` daemon is. A rename is atomic and leaves the running one on its old
# inode.
NEW="$TARGET.new.$$"
if ! fetch "$URL" "$NEW"; then
	rm -f "$NEW"
	echo "forge: download failed ($URL)" >&2
	echo "       (a private repo needs a public release, or fetch the asset manually)" >&2
	exit 1
fi
chmod +x "$NEW"

# macOS: a cross-compiled Go binary carries a linker ad-hoc signature that Apple
# Silicon (AMFI) can reject, killing the process with "killed: 9". Re-signing it
# locally with codesign (present on every Mac) produces a signature AMFI accepts.
# Signed before it is put in place, so what lands is already runnable.
if [ "$OS" = darwin ] && command -v codesign >/dev/null 2>&1; then
	codesign --force --sign - "$NEW" >/dev/null 2>&1 && echo "forge: re-signed for macOS"
fi

# --- stop what is running, with the build that started it ------------------
# Only now, with the new binary already on disk. Stopped with the OLD one,
# deliberately: it is not about the file being busy — the rename below handles
# that — it is that a daemon should be stopped by its own build, since stopping
# reads a pidfile and sends a signal, and a release that changed either would
# leave a new binary unable to stop an old daemon, which then keeps the port
# while nothing can start.
#
# Only what is actually running is noted, so nothing that was deliberately down
# gets started at the end.
UI_WAS_UP=no
FWD_WAS_UP=no
if [ -x "$TARGET" ]; then
	"$TARGET" ui status -q 2>/dev/null && UI_WAS_UP=yes
	"$TARGET" forwarding status -q 2>/dev/null && FWD_WAS_UP=yes
	if [ "$UI_WAS_UP" = yes ]; then
		echo "forge: stopping the running UI"
		"$TARGET" ui stop >/dev/null 2>&1 || true
	fi
	if [ "$FWD_WAS_UP" = yes ]; then
		echo "forge: stopping the forwarding supervisor"
		"$TARGET" forwarding stop >/dev/null 2>&1 || true
	fi
fi

# --- put it in place -------------------------------------------------------
mv -f "$NEW" "$TARGET"
echo "forge: binary -> $TARGET"

# --- symlink onto PATH -----------------------------------------------------
LINK="$LINK_DIR/$BIN"
if [ "$LINK" = "$TARGET" ]; then
	# Link dir is the install dir — the binary is already there, don't self-link.
	LINK=""
	echo "forge: add $INSTALL_DIR to your PATH:"
	echo "         export PATH=\"$INSTALL_DIR:\$PATH\""
elif mkdir -p "$LINK_DIR" 2>/dev/null && [ -w "$LINK_DIR" ]; then
	ln -sf "$TARGET" "$LINK"
	echo "forge: linked -> $LINK"
elif command -v sudo >/dev/null 2>&1; then
	echo "forge: linking into $LINK_DIR (needs sudo)"
	sudo mkdir -p "$LINK_DIR"
	sudo ln -sf "$TARGET" "$LINK"
	echo "forge: linked -> $LINK"
else
	LINK=""
	echo
	echo "forge: could not write $LINK_DIR — add the binary to your PATH, e.g.:"
	echo "         export PATH=\"$INSTALL_DIR:\$PATH\""
fi

# --- done ------------------------------------------------------------------
echo
echo "forge: installed $("$TARGET" version 2>/dev/null || echo "$VERSION")"

# --- keep the servers in step ----------------------------------------------
# The agent on each server is half of this release: it rides inside this binary
# and speaks a vocabulary that grows with it, so a client newer than the agent it
# talks to is not a smaller Forge, it is an unreliable one. Updating it is a
# copy, not a provision — nothing is installed, configured or restarted.
#
# Only for a machine that already has hosts registered, so a fresh install
# reaches out to nothing; never fatal, because a server being off is not a reason
# for an install to fail; and skippable, because `curl | sh` in a container or a
# CI job has no business connecting to production.
if [ -f "$FORGE_HOME/config.json" ] && [ -z "${FORGE_SKIP_AGENT_UPDATE:-}" ]; then
	echo "forge: updating the agent on the servers this machine knows"
	echo "       (skip with FORGE_SKIP_AGENT_UPDATE=1)"
	"$TARGET" host update || echo "forge: some hosts were not updated — run 'forge host update' when they are back"

	# A workspace made before port blocks existed has none, and one is given here
	# rather than by hand. Idempotent: a workspace that has a block keeps it, and
	# after the first run this does nothing at all.
	"$TARGET" ports assign >/dev/null 2>&1 || true
fi

# --- start again what was running ------------------------------------------
# The new build, and only the daemons that were up before it.
if [ "$UI_WAS_UP" = yes ]; then
	echo "forge: starting the UI again"
	"$TARGET" ui start >/dev/null 2>&1 || echo "forge: the UI did not come back — start it with 'forge ui'"
fi
if [ "$FWD_WAS_UP" = yes ]; then
	echo "forge: starting the forwarding supervisor again"
	"$TARGET" forwarding start >/dev/null 2>&1 || echo "forge: forwarding did not come back — start it with 'forge forwarding start'"
fi

echo "forge: done. Config lives in $FORGE_HOME (created on first use)."
if [ -n "$LINK" ] && command -v "$BIN" >/dev/null 2>&1; then
	echo "forge: run 'forge help' to get started."
else
	echo "forge: run '$TARGET help' to get started."
fi
