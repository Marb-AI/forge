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

# --- download --------------------------------------------------------------
echo "forge: installing $VERSION for $OS/$ARCH"
mkdir -p "$INSTALL_DIR"
TARGET="$INSTALL_DIR/$BIN"
if ! fetch "$URL" "$TARGET"; then
	echo "forge: download failed ($URL)" >&2
	echo "       (a private repo needs a public release, or fetch the asset manually)" >&2
	exit 1
fi
chmod +x "$TARGET"
echo "forge: binary -> $TARGET"

# macOS: a cross-compiled Go binary carries a linker ad-hoc signature that Apple
# Silicon (AMFI) can reject, killing the process with "killed: 9". Re-signing it
# locally with codesign (present on every Mac) produces a signature AMFI accepts.
if [ "$OS" = darwin ] && command -v codesign >/dev/null 2>&1; then
	codesign --force --sign - "$TARGET" >/dev/null 2>&1 && echo "forge: re-signed for macOS"
fi

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
echo "forge: done. Config lives in $FORGE_HOME (created on first use)."
if [ -n "$LINK" ] && command -v "$BIN" >/dev/null 2>&1; then
	echo "forge: run 'forge help' to get started."
else
	echo "forge: run '$TARGET help' to get started."
fi
