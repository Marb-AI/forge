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
// installs git, tmux, iproute2 (ss), docker + compose, and forge-agent; locks
// the firewall to SSH-only; and disables SSH password auth. Everything is
// idempotent — already-present tools are reported, not reinstalled.
//
// Must connect as root or a passwordless-sudo user (it installs system packages
// and edits sshd/iptables). This path is not exercised on the dev machine — it
// needs a real Linux host; test on a throwaway box first.
func hostPrepare(args []string) int {
	alias, rest := extractFlag(args, "alias")
	noFirewall := hasBoolFlag(rest, "--no-firewall")
	noHarden := hasBoolFlag(rest, "--no-ssh-harden")
	rest = dropFlags(rest, "--no-firewall", "--no-ssh-harden")

	if len(rest) < 1 || alias == "" {
		return fail("usage: forge host prepare <ssh-target> --alias=<alias> [--no-firewall] [--no-ssh-harden]")
	}
	user, addr, port, err := config.ParseSSHTarget(rest[0])
	if err != nil {
		return fail("%v", err)
	}
	target := sshx.Target{User: user, Addr: addr, Port: port}

	// Probe: arch, uid, package manager — in one round trip.
	out, err := sshx.Capture(target.Args(
		"uname -m; id -u; { command -v apt-get || command -v dnf || command -v yum || echo none; }",
	)...)
	if err != nil {
		return fail("cannot reach %s: %v", target.User+"@"+addr, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return fail("unexpected probe output: %q", string(out))
	}
	arch, uid := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	pkgMgr := filepath.Base(strings.TrimSpace(lines[2]))

	goarch, err := unameToGoArch(arch)
	if err != nil {
		return fail("%v", err)
	}
	iproutePkg, ok := iproutePackage(pkgMgr)
	if !ok {
		return fail("unsupported distro: no apt-get/dnf/yum found (%q)", lines[2])
	}
	isRoot := uid == "0"

	agentSrc, agentLabel, agentClose, err := agentReader(goarch)
	if err != nil {
		return fail("%v", err)
	}
	defer agentClose()

	fmt.Printf("preparing %s@%s (arch %s, %s)\n", user, addr, arch, pkgMgr)

	// 1) Upload the agent binary to /tmp; the provisioning script (as root)
	//    installs it into place.
	fmt.Printf("→ uploading forge-agent (%s)\n", agentLabel)
	if err := sshx.RunWithInput(agentSrc, target.Args("cat > /tmp/forge-agent")...); err != nil {
		return fail("upload failed: %v", err)
	}

	// 2) Run the provisioning script as root.
	script := buildPrepareScript(pkgMgr, iproutePkg, port, user, isRoot, !noFirewall, !noHarden)
	runner := "bash -s"
	if !isRoot {
		runner = "sudo bash -s"
	}
	fmt.Println("→ provisioning (idempotent) …")
	if err := sshx.RunWithInput(strings.NewReader(script), target.Args(runner)...); err != nil {
		return fail("provisioning failed: %v", err)
	}

	// 3) Register the host now that it is ready.
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	cfg.Hosts[alias] = &config.Host{Alias: alias, User: user, Addr: addr, Port: port}
	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}

	fmt.Printf("\nhost %q ready.\n", alias)
	fmt.Printf("  next: forge workspace create <name> %s\n", alias)
	return 0
}

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
func buildPrepareScript(pkgMgr, iproutePkg string, sshPort int, user string, isRoot, firewall, harden bool) string {
	var b strings.Builder
	b.WriteString(prepareBase)
	if !isRoot {
		b.WriteString(sudoersSection)
	}
	if firewall {
		b.WriteString(firewallSection)
	}
	if harden {
		b.WriteString(sshHardenSection)
	}
	b.WriteString("echo '[forge] host prepared.'\n")

	r := strings.NewReplacer(
		"__PKG__", pkgMgr,
		"__IPROUTE__", iproutePkg,
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

// sudoersSection lets a non-root admin invoke the agent without a password.
const sudoersSection = `printf '%s\n' '__USER__ ALL=(root) NOPASSWD: /usr/local/bin/forge-agent' > /etc/sudoers.d/forge
chmod 0440 /etc/sudoers.d/forge
visudo -cf /etc/sudoers.d/forge >/dev/null && echo "[forge] sudoers configured for __USER__"
`

// firewallSection locks inbound traffic to SSH only (iptables), including
// Docker's published ports which otherwise bypass the INPUT chain. Rules are
// added with -C||-A so re-running is a no-op, and persisted across reboot.
const firewallSection = `echo "[forge] configuring firewall (iptables): SSH-only inbound ..."
ensure iptables iptables iptables
iptables -P INPUT ACCEPT
iptables -C INPUT -i lo -j ACCEPT 2>/dev/null || iptables -A INPUT -i lo -j ACCEPT
iptables -C INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || iptables -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -C INPUT -p tcp --dport __SSHPORT__ -j ACCEPT 2>/dev/null || iptables -A INPUT -p tcp --dport __SSHPORT__ -j ACCEPT
iptables -C INPUT -p icmp -j ACCEPT 2>/dev/null || iptables -A INPUT -p icmp -j ACCEPT
iptables -P INPUT DROP
iptables -P OUTPUT ACCEPT
# Docker publishes on 0.0.0.0 and bypasses INPUT via FORWARD; block external
# access to published ports in DOCKER-USER (localhost tunnels are unaffected).
if iptables -L DOCKER-USER -n >/dev/null 2>&1; then
  EXTIF=$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1)
  if [ -n "$EXTIF" ]; then
    iptables -C DOCKER-USER -i "$EXTIF" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN 2>/dev/null || iptables -I DOCKER-USER -i "$EXTIF" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
    iptables -C DOCKER-USER -i "$EXTIF" -j DROP 2>/dev/null || iptables -A DOCKER-USER -i "$EXTIF" -j DROP
    echo "[forge] docker published ports firewalled on $EXTIF"
  fi
fi
if [ "$PKG" = apt-get ]; then
  echo 'iptables-persistent iptables-persistent/autosave_v4 boolean true' | debconf-set-selections
  echo 'iptables-persistent iptables-persistent/autosave_v6 boolean true' | debconf-set-selections
  DEBIAN_FRONTEND=noninteractive apt-get install -y iptables-persistent >/dev/null 2>&1 || true
  mkdir -p /etc/iptables && iptables-save > /etc/iptables/rules.v4
else
  pkg_install iptables-services 2>/dev/null || true
  iptables-save > /etc/sysconfig/iptables 2>/dev/null || true
  systemctl enable iptables 2>/dev/null || true
fi
echo "[forge] firewall active (SSH-only inbound)"
`

// sshHardenSection disables password auth (keys only), but only if an
// authorized_keys already exists, so we never lock ourselves out.
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
