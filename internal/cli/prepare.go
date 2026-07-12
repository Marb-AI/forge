package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/internal/agentbin"
	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
)

// hostPrepare provisions a bare server into a Forge host and registers it:
// installs git, make, tmux, iproute2 (ss), docker + compose, gh, and forge-agent;
// creates the host's git identity (an SSH key); locks the firewall to SSH-only;
// and disables SSH password auth. Everything is idempotent — already-present
// tools are reported, not reinstalled, and an existing key is kept.
//
// Must connect as root or a passwordless-sudo user (it installs system packages
// and edits sshd/iptables). This path is not exercised on the dev machine — it
// needs a real Linux host; test on a throwaway box first.
func hostPrepare(args []string) int {
	alias, rest := extractFlag(args, "alias")
	noFirewall := hasBoolFlag(rest, "--no-firewall")
	noHarden := hasBoolFlag(rest, "--no-ssh-harden")
	noPrune := hasBoolFlag(rest, "--no-docker-prune")
	rest = dropFlags(rest, "--no-firewall", "--no-ssh-harden", "--no-docker-prune")

	if len(rest) < 1 || alias == "" {
		return fail("usage: forge host prepare <ssh-target> --alias=<alias> [--no-firewall] [--no-ssh-harden] [--no-docker-prune]")
	}
	if err := runHostPrepare(rest[0], alias, !noFirewall, !noHarden, !noPrune, os.Stdout); err != nil {
		return fail("%v", err)
	}
	return 0
}

// runHostPrepare is the transport-agnostic `host prepare`: it provisions a bare
// server and registers it, writing every line of progress to out. The CLI passes
// os.Stdout; the browser UI passes an SSE stream, so the wizard can show the same
// long provisioning run live instead of a spinner.
func runHostPrepare(sshTarget, alias string, firewall, harden, dockerPrune bool, out io.Writer) error {
	user, addr, port, err := config.ParseSSHTarget(sshTarget)
	if err != nil {
		return err
	}
	target := sshx.Target{User: user, Addr: addr, Port: port}

	// Probe: arch, uid, package manager — in one round trip.
	probe, err := sshx.Capture(target.Args(
		"uname -m; id -u; { command -v apt-get || command -v dnf || command -v yum || echo none; }",
	)...)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", target.User+"@"+addr, err)
	}
	lines := strings.Split(strings.TrimSpace(string(probe)), "\n")
	if len(lines) < 3 {
		return fmt.Errorf("unexpected probe output: %q", string(probe))
	}
	arch, uid := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	pkgMgr := filepath.Base(strings.TrimSpace(lines[2]))

	goarch, err := unameToGoArch(arch)
	if err != nil {
		return err
	}
	iproutePkg, ok := iproutePackage(pkgMgr)
	if !ok {
		return fmt.Errorf("unsupported distro: no apt-get/dnf/yum found (%q)", lines[2])
	}
	sshClientPkg, _ := sshClientPackage(pkgMgr)
	isRoot := uid == "0"

	agentSrc, agentLabel, agentClose, err := agentReader(goarch)
	if err != nil {
		return err
	}
	defer agentClose()

	fmt.Fprintf(out, "preparing %s@%s (arch %s, %s)\n", user, addr, arch, pkgMgr)

	// 1) Upload the agent binary to /tmp; the provisioning script (as root)
	//    installs it into place.
	fmt.Fprintf(out, "→ uploading forge-agent (%s)\n", agentLabel)
	if err := sshx.RunWithInputTo(agentSrc, out, target.Args("cat > /tmp/forge-agent")...); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	// 2) Run the provisioning script as root.
	script := buildPrepareScript(pkgMgr, iproutePkg, sshClientPkg, goarch, port, user, isRoot, firewall, harden, dockerPrune)
	runner := "bash -s"
	if !isRoot {
		runner = "sudo bash -s"
	}
	fmt.Fprintln(out, "→ provisioning (idempotent) …")
	if err := sshx.RunWithInputTo(strings.NewReader(script), out, target.Args(runner)...); err != nil {
		return fmt.Errorf("provisioning failed: %w", err)
	}

	// 3) Read the host's public key back, so a re-run always shows it — whether it
	//    was just generated or has been there all along. The .pub is world-readable
	//    (the private key is not), so this needs no sudo.
	pubkey, err := sshx.Capture(target.Args("cat " + hostKeyPath + ".pub")...)
	if err != nil {
		return fmt.Errorf("cannot read host key %s.pub: %w", hostKeyPath, err)
	}

	// 4) Register the host now that it is ready.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Hosts[alias] = &config.Host{Alias: alias, User: user, Addr: addr, Port: port}
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Fprintf(out, "\nhost %q ready.\n", alias)
	fmt.Fprintf(out, "\n  git identity — register this key on GitHub (Settings → SSH keys) so\n")
	fmt.Fprintf(out, "  workspaces can clone and push without your laptop:\n\n")
	fmt.Fprintf(out, "    %s\n", strings.TrimSpace(string(pubkey)))
	fmt.Fprintf(out, "\n  gh is installed but not authenticated. Log in once for the whole host:\n")
	fmt.Fprintf(out, "    forge host gh-login %s\n", alias)
	fmt.Fprintf(out, "\n  next: forge workspace create <name> %s\n", alias)
	return nil
}

// hostKeyDir holds the host-wide git identity: one key per server, copied into
// each workspace at create. Kept in sync with internal/agent.
//
// One key for the whole host (rather than one per workspace) matches the
// boundary Forge actually draws. Workspace users are in the docker group, so any
// of them can already read every other's files; a per-workspace key would buy no
// real separation. It also keeps registration to a single step: a GitHub deploy
// key is bound to one repo, but this key registered as an account SSH key works
// for every repo in every workspace.
const (
	hostKeyDir  = "/etc/forge"
	hostKeyPath = hostKeyDir + "/id_ed25519"
	// hostGhDir holds the host-wide gh credential, seeded by `forge host gh-login`
	// and copied into each workspace at create. Kept in sync with internal/agent.
	hostGhDir = hostKeyDir + "/gh"
)

// unameToGoArch maps `uname -m` to a Go arch used in the agent binary name.
func unameToGoArch(uname string) (string, error) {
	switch uname {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported CPU architecture %q", uname)
	}
}

func iproutePackage(pkgMgr string) (string, bool) {
	switch pkgMgr {
	case "apt-get":
		return "iproute2", true
	case "dnf", "yum":
		return "iproute", true
	default:
		return "", false
	}
}

// sshClientPackage names the package holding ssh-keygen / ssh-keyscan.
func sshClientPackage(pkgMgr string) (string, bool) {
	switch pkgMgr {
	case "apt-get":
		return "openssh-client", true
	case "dnf", "yum":
		return "openssh-clients", true
	default:
		return "", false
	}
}

// agentReader yields the forge-agent binary for goarch: the version embedded in
// a release build if present, otherwise a locally cross-compiled file. Returns a
// reader, a human label, and a close func.
func agentReader(goarch string) (io.Reader, string, func(), error) {
	if data, err := agentbin.Get(goarch); err == nil && len(data) > 0 {
		return bytes.NewReader(data), "embedded linux/" + goarch, func() {}, nil
	}
	p, err := locateAgentBinary(goarch)
	if err != nil {
		return nil, "", func() {}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, "", func() {}, err
	}
	return f, filepath.Base(p), func() { _ = f.Close() }, nil
}

// locateAgentBinary finds the cross-compiled linux agent for goarch. Override
// with FORGE_AGENT_BIN; otherwise it looks next to the forge binary and in ./bin.
func locateAgentBinary(goarch string) (string, error) {
	name := "forge-agent-linux-" + goarch
	if p := os.Getenv("FORGE_AGENT_BIN"); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("FORGE_AGENT_BIN=%s not found", p)
	}
	var cands []string
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands, filepath.Join(d, name), filepath.Join(d, "..", "bin", name))
	}
	cands = append(cands, filepath.Join("bin", name), name)
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("agent binary %s not found — build it with `make agent-linux` (or set FORGE_AGENT_BIN)", name)
}

// dropFlags removes the given boolean flags from args.
func dropFlags(args []string, flags ...string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if !contains(flags, a) {
			out = append(out, a)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// buildPrepareScript assembles the idempotent remote provisioning script. It
// assumes it runs as root (the caller wraps it in `sudo bash -s` when needed).
func buildPrepareScript(pkgMgr, iproutePkg, sshClientPkg, goarch string, sshPort int, user string, isRoot, firewall, harden, dockerPrune bool) string {
	var b strings.Builder
	b.WriteString(prepareBase)
	b.WriteString(ghSection)
	b.WriteString(sshKeySection)
	if !isRoot {
		b.WriteString(sudoersSection)
	}
	if firewall {
		b.WriteString(firewallSection)
	}
	if harden {
		b.WriteString(sshHardenSection)
	}
	if dockerPrune {
		b.WriteString(dockerPruneSection)
	}
	b.WriteString("echo '[forge] host prepared.'\n")

	r := strings.NewReplacer(
		"__PKG__", pkgMgr,
		"__IPROUTE__", iproutePkg,
		"__SSHCLIENT__", sshClientPkg,
		"__GOARCH__", goarch,
		"__KEYDIR__", hostKeyDir,
		"__KEY__", hostKeyPath,
		"__SSHPORT__", strconv.Itoa(sshPort),
		"__USER__", user,
	)
	return r.Replace(b.String())
}

// prepareBase installs base tools + docker + the agent, idempotently.
const prepareBase = `set -e
PKG="__PKG__"

pkg_install() {
  case "$PKG" in
    apt-get) DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
    dnf)     dnf install -y "$@" ;;
    yum)     yum install -y "$@" ;;
  esac
}
ensure() { # ensure <binary> <name> <package>
  if command -v "$1" >/dev/null 2>&1; then
    echo "[forge] $2 already installed"
  else
    echo "[forge] installing $2 ..."
    pkg_install "$3"
  fi
}

[ "$PKG" = apt-get ] && apt-get update -qq || true

ensure git  git             git
ensure tmux tmux            tmux
ensure ss   "iproute2 (ss)" "__IPROUTE__"
ensure curl curl            curl
ensure make make            make

if command -v docker >/dev/null 2>&1; then
  echo "[forge] docker already installed"
else
  echo "[forge] installing docker (get.docker.com) ..."
  curl -fsSL https://get.docker.com | sh
fi
systemctl enable --now docker 2>/dev/null || true

install -m 0755 /tmp/forge-agent /usr/local/bin/forge-agent
rm -f /tmp/forge-agent
echo "[forge] forge-agent installed"
`

// ghSection installs the GitHub CLI. `gh` is not in Debian's main repos and its
// distro packages lag, so we add GitHub's own repo; if that fails (unknown
// distro, repo unreachable) we fall back to the release tarball. gh is left
// unauthenticated — `gh auth login` is an interactive browser/token flow, so it
// belongs in a workspace shell, not here.
//
// A failure to install gh must not fail the whole prepare: everything else about
// the host still works without it, so each step is guarded and the section ends
// with a warning rather than a non-zero exit.
const ghSection = `GOARCH="__GOARCH__"
if command -v gh >/dev/null 2>&1; then
  echo "[forge] gh already installed"
else
  echo "[forge] installing gh (github cli) ..."
  {
    case "$PKG" in
      apt-get)
        pkg_install ca-certificates curl gnupg
        install -m 0755 -d /etc/apt/keyrings
        curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
          -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
        chmod 0644 /etc/apt/keyrings/githubcli-archive-keyring.gpg
        printf 'deb [arch=%s signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main\n' \
          "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/github-cli.list
        apt-get update -qq
        pkg_install gh
        ;;
      dnf)
        dnf install -y 'dnf-command(config-manager)'
        dnf config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo
        dnf install -y gh
        ;;
      yum)
        yum install -y yum-utils
        yum-config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo
        yum install -y gh
        ;;
    esac
  } >/dev/null 2>&1 || true

  if command -v gh >/dev/null 2>&1; then
    echo "[forge] gh installed (package)"
  else
    echo "[forge] gh: package install failed, trying release tarball ..."
    GH_VER=$(curl -fsSL https://api.github.com/repos/cli/cli/releases/latest 2>/dev/null \
      | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -1) || true
    if [ -n "$GH_VER" ]; then
      TMP=$(mktemp -d)
      GH_DIR="gh_${GH_VER}_linux_${GOARCH}"
      if curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VER}/${GH_DIR}.tar.gz" -o "$TMP/gh.tgz" \
         && tar -xzf "$TMP/gh.tgz" -C "$TMP" \
         && install -m 0755 "$TMP/$GH_DIR/bin/gh" /usr/local/bin/gh; then
        echo "[forge] gh installed (tarball $GH_VER)"
      else
        echo "[forge] WARNING: gh install failed — install it by hand later"
      fi
      rm -rf "$TMP"
    else
      echo "[forge] WARNING: could not resolve latest gh version — skipping gh"
    fi
  fi
fi
`

// sshKeySection creates the host's git identity, once. It is idempotent in the
// strong sense: an existing private key is never regenerated (that would silently
// break every repo it is already registered on), only its .pub is rebuilt if
// missing. The caller reads the .pub back and prints it on every run, so a
// re-prepare still shows you what to register.
//
// The key has no passphrase, deliberately: the whole point is that a Claude
// session in tmux can push while your laptop is off, and an encrypted key would
// need an interactive unlock that nobody is there to type.
//
// github.com's host keys are pre-seeded so an unattended `git clone` never stops
// at the "authenticity of host can't be established" prompt.
const sshKeySection = `ensure ssh-keygen openssh-client "__SSHCLIENT__"
install -m 0755 -d __KEYDIR__
if [ -f "__KEY__" ]; then
  echo "[forge] git identity already present (kept)"
else
  echo "[forge] generating git identity (ed25519, no passphrase) ..."
  # uname -n, not hostname(1): minimal images (Fedora) ship coreutils but not it.
  ssh-keygen -q -t ed25519 -N '' -C "forge@$(uname -n)" -f "__KEY__"
fi
[ -f "__KEY__.pub" ] || ssh-keygen -y -f "__KEY__" > "__KEY__.pub"
chmod 0600 "__KEY__"
chmod 0644 "__KEY__.pub"

if [ ! -s __KEYDIR__/known_hosts ]; then
  ssh-keyscan -t rsa,ecdsa,ed25519 github.com > __KEYDIR__/known_hosts 2>/dev/null \
    && echo "[forge] pre-trusted github.com host keys" \
    || echo "[forge] WARNING: ssh-keyscan github.com failed — first clone may prompt"
fi
chmod 0644 __KEYDIR__/known_hosts 2>/dev/null || true
`

// sudoersSection lets a non-root admin invoke the agent without a password.
const sudoersSection = `printf '%s\n' '__USER__ ALL=(root) NOPASSWD: /usr/local/bin/forge-agent' > /etc/sudoers.d/forge
chmod 0440 /etc/sudoers.d/forge
visudo -cf /etc/sudoers.d/forge >/dev/null && echo "[forge] sudoers configured for __USER__"
`

// firewallSection locks inbound traffic to SSH only on BOTH IPv4 and IPv6 —
// leaving ip6tables open (its default) would expose every service over IPv6, a
// hole that defeats the SSH-only intent. ICMP is allowed (and ICMPv6 is
// mandatory for IPv6 to function at all — NDP/PMTU). Docker's published ports,
// which bypass INPUT via FORWARD, are blocked externally in DOCKER-USER. Rules
// use -C||-A so re-running is a no-op, and both stacks are persisted.
const firewallSection = `echo "[forge] configuring firewall: SSH-only inbound (IPv4 + IPv6) ..."
ensure iptables iptables iptables
EXTIF=$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1)
[ -z "$EXTIF" ] && EXTIF=eth0

fw_apply() { # $1 = iptables|ip6tables, $2 = icmp|ipv6-icmp
  IPT="$1"; ICMP="$2"
  "$IPT" -P INPUT ACCEPT
  "$IPT" -C INPUT -i lo -j ACCEPT 2>/dev/null || "$IPT" -A INPUT -i lo -j ACCEPT
  "$IPT" -C INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || "$IPT" -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  "$IPT" -C INPUT -p "$ICMP" -j ACCEPT 2>/dev/null || "$IPT" -A INPUT -p "$ICMP" -j ACCEPT
  "$IPT" -C INPUT -p tcp --dport __SSHPORT__ -j ACCEPT 2>/dev/null || "$IPT" -A INPUT -p tcp --dport __SSHPORT__ -j ACCEPT
  "$IPT" -P INPUT DROP
  "$IPT" -P OUTPUT ACCEPT
  if "$IPT" -L DOCKER-USER -n >/dev/null 2>&1; then
    "$IPT" -C DOCKER-USER -i "$EXTIF" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN 2>/dev/null || "$IPT" -I DOCKER-USER -i "$EXTIF" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
    "$IPT" -C DOCKER-USER -i "$EXTIF" -j DROP 2>/dev/null || "$IPT" -A DOCKER-USER -i "$EXTIF" -j DROP
  fi
}
fw_apply iptables icmp
fw_apply ip6tables ipv6-icmp
echo "[forge] firewall active on $EXTIF (SSH-only inbound, IPv4 + IPv6)"

if [ "$PKG" = apt-get ]; then
  echo 'iptables-persistent iptables-persistent/autosave_v4 boolean true' | debconf-set-selections
  echo 'iptables-persistent iptables-persistent/autosave_v6 boolean true' | debconf-set-selections
  DEBIAN_FRONTEND=noninteractive apt-get install -y iptables-persistent >/dev/null 2>&1 || true
  mkdir -p /etc/iptables
  iptables-save > /etc/iptables/rules.v4
  ip6tables-save > /etc/iptables/rules.v6
else
  pkg_install iptables-services 2>/dev/null || true
  iptables-save > /etc/sysconfig/iptables 2>/dev/null || true
  ip6tables-save > /etc/sysconfig/ip6tables 2>/dev/null || true
  systemctl enable iptables 2>/dev/null || true
  systemctl enable ip6tables 2>/dev/null || true
fi
`

// sshHardenSection disables password auth (keys only), but only if an
// authorized_keys already exists, so we never lock ourselves out.

// dockerPruneSection installs a nightly Docker clean-up. A build server fills up
// fast — every rebuild orphans the layers it replaced, and BuildKit's cache grows
// without bound — and a full disk breaks every workspace at once.
//
// What it removes is deliberately conservative, because the failure mode of being
// too eager is a workspace that has to rebuild from scratch in the morning:
//
//   - DANGLING images only (no -a). Those are the layer sets a rebuild left
//     behind. `-a` would also delete any tagged image that no container happens to
//     be running right now — which, with several workspaces where not all are up,
//     means quietly deleting the images of every idle project each night.
//   - Build cache, which on a real host is where the growth actually is.
//   - Never volumes. That is where data lives.
//
// Stopped containers are deliberately NOT pruned either. On the host I measured
// they were 23MB against 3.9GB of build cache — nothing — and removing one takes
// its writable layer with it, so a stack you `compose stop`-ed for the night is
// gone in the morning and has to be `up`-ed rather than `start`-ed. Not worth it.
//
// Everything is filtered to 24h, so nothing you built today is touched, and an
// image an actually-running container uses is never a candidate in the first place.
//
// A systemd timer rather than cron: no extra package, it logs to the journal, and
// Persistent=true means a server that was off at 03:00 still runs the clean-up
// when it comes back.
const dockerPruneSection = `echo "[forge] installing nightly docker clean-up (03:00) ..."
cat > /usr/local/bin/forge-docker-prune <<'PRUNE'
#!/bin/sh
# Reclaim disk from Docker. Conservative on purpose — see prepare.go.
set -e
echo "docker disk usage before:"
docker system df 2>/dev/null || exit 0

# Layer sets orphaned by a rebuild. NOT -a: that would delete tagged images no
# container happens to be running, i.e. every idle workspace's images.
docker image prune -f --filter until=24h || true
# BuildKit cache. Usually the biggest win.
docker builder prune -f --filter until=24h || true
# Containers are NOT pruned: worth ~nothing next to the cache, and removing one
# takes its writable layer with it, so a stack stopped for the night would have to
# be re-created in the morning rather than just started.
# Volumes are NOT pruned either. That is where data lives.

echo "docker disk usage after:"
docker system df 2>/dev/null || true
PRUNE
chmod 0755 /usr/local/bin/forge-docker-prune

cat > /etc/systemd/system/forge-docker-prune.service <<'UNIT'
[Unit]
Description=Forge: reclaim Docker disk (dangling images and build cache)
# After, but deliberately NOT Requires. Requires would fail the unit outright on a
# host where Docker was removed or disabled — leaving a timer that is permanently
# red — and it would also *start* Docker at 03:00 on a host where someone had
# stopped it on purpose. The script already exits cleanly when Docker is absent,
# which is the behaviour we want: nothing to clean, so nothing happens.
After=docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/forge-docker-prune
UNIT

cat > /etc/systemd/system/forge-docker-prune.timer <<'UNIT'
[Unit]
Description=Forge: nightly Docker clean-up

[Timer]
# The server's 03:00, not yours — UTC on a stock VPS.
OnCalendar=*-*-* 03:00:00
# A server that was off at 03:00 still runs it once it is back.
Persistent=true
# Don't have every Forge host hammer its disk at the same second.
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload 2>/dev/null || true
systemctl enable --now forge-docker-prune.timer 2>/dev/null || true
echo "[forge] docker clean-up scheduled (systemctl list-timers forge-docker-prune)"
echo "[forge]   run it now:  sudo forge-docker-prune"
`

const sshHardenSection = `KEYFILE=""
[ -s /root/.ssh/authorized_keys ] && KEYFILE=/root/.ssh/authorized_keys
[ -s "$HOME/.ssh/authorized_keys" ] && KEYFILE="$HOME/.ssh/authorized_keys"
if [ -n "$KEYFILE" ]; then
  if [ -d /etc/ssh/sshd_config.d ] && grep -q 'sshd_config.d' /etc/ssh/sshd_config 2>/dev/null; then
    printf '%s\n' 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' 'ChallengeResponseAuthentication no' 'PubkeyAuthentication yes' > /etc/ssh/sshd_config.d/60-forge.conf
  elif ! grep -q '# forge-hardening' /etc/ssh/sshd_config; then
    printf '\n%s\n%s\n%s\n%s\n%s\n' '# forge-hardening' 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' 'ChallengeResponseAuthentication no' 'PubkeyAuthentication yes' >> /etc/ssh/sshd_config
  fi
  if sshd -t 2>/dev/null; then
    systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || service ssh reload 2>/dev/null || true
    echo "[forge] SSH hardened: password auth disabled (keys only)"
  else
    echo "[forge] WARNING: sshd config test failed — left password auth unchanged"
  fi
else
  echo "[forge] WARNING: no authorized_keys for this user — skipping password-auth disable to avoid lockout"
fi
`
