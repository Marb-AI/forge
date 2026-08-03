package forge

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentbin"
	"github.com/Marb-AI/forge/internal/sshx"
)

// PrepareHost provisions a bare server into a Forge host and registers it:
// installs git, make, tmux, iproute2 (ss), docker + compose, gh, and forge-agent;
// creates the host's git identity (an SSH key); locks the firewall to SSH-only;
// and disables SSH password auth. Everything is idempotent — already-present
// tools are reported, not reinstalled, and an existing key is kept.
//
// Must connect as root or a passwordless-sudo user (it installs system packages
// and edits sshd/iptables). This path is not exercised on the dev machine — it
// needs a real Linux host; test on a throwaway box first.
//
// It takes minutes and the progress is the point, so every line goes to out as it
// happens. The CLI passes os.Stdout; the browser UI passes an SSE stream, so the
// wizard shows the same long provisioning run live instead of a spinner.
//
// jump is the route to the server, empty for one this machine can reach
// directly. A server behind a bastion has to be reachable before it can be
// provisioned, so the route is given here and kept with the host — the same
// value `host add` takes, and the only way onto a private network.
//
// It is a pointer because "not given" and "given as empty" are different
// answers, and this command is re-run: provisioning is idempotent, so a host
// gets prepared again to pick up a new agent or a fixed clean-up, and a flag
// nobody typed must not silently drop the route that has been recorded all
// along. Nil keeps what is on record, a value replaces it, and an empty value
// clears it — the only way to say "this host is reachable directly now".
func PrepareHost(sshTarget, alias string, jump *string, firewall, harden, dockerPrune, pruneImages, pruneVolumes bool, out io.Writer) error {
	user, addr, port, err := config.ParseSSHTarget(sshTarget)
	if err != nil {
		return err
	}
	route, err := routeFor(alias, jump)
	if err != nil {
		return err
	}
	// Built from the host as it will be recorded, rather than assembled here, so
	// the route is read the one way the transport reads every route — a hop with
	// no login named takes this server's.
	host := &config.Host{Alias: alias, User: user, Addr: addr, Port: port, Jump: route}
	target := sshx.AdminTarget(host)

	// Probe: arch, uid, package manager — in one round trip.
	probe, err := target.Output(
		"uname -m; id -u; { command -v apt-get || command -v dnf || command -v yum || echo none; }",
	)
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
	if err := target.Pipe(agentSrc, out, out, "cat > /tmp/forge-agent"); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	// 2) Run the provisioning script as root.
	script := buildPrepareScript(pkgMgr, iproutePkg, sshClientPkg, goarch, port, user, isRoot, firewall, harden, dockerPrune, pruneImages, pruneVolumes)
	runner := "bash -s"
	if !isRoot {
		runner = "sudo bash -s"
	}
	fmt.Fprintln(out, "→ provisioning (idempotent) …")
	if err := target.Pipe(strings.NewReader(script), out, out, runner); err != nil {
		return fmt.Errorf("provisioning failed: %w", err)
	}

	// 3) Read the host's public key back, so a re-run always shows it — whether it
	//    was just generated or has been there all along. The .pub is world-readable
	//    (the private key is not), so this needs no sudo.
	pubkey, err := target.Output("cat " + hostKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("cannot read host key %s.pub: %w", hostKeyPath, err)
	}

	// 4) Register the host now that it is ready. Its own step: preparing a server
	//    takes minutes, and a config loaded before all that would be stale enough
	//    to undo anything else written in the meantime.
	if err := updateConfig(func(c *config.Config) error {
		c.Hosts[alias] = host
		return nil
	}); err != nil {
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

// routeFor settles which route a prepare uses: the one given, or the one already
// recorded for this alias when none was.
//
// Reading the config here rather than in the front ends is what makes the rule
// one rule. A CLI that looked the old route up and passed it along would leave
// the browser's wizard — which sends whatever is in a text field — free to erase
// a route by leaving it blank, and the two would disagree about what an absent
// answer means.
func routeFor(alias string, jump *string) (string, error) {
	if jump == nil {
		cfg, err := loadConfig()
		if err != nil {
			return "", err
		}
		if h := cfg.Hosts[alias]; h != nil {
			return h.Jump, nil
		}
		return "", nil
	}
	if _, err := sshx.ParseJump(*jump); err != nil {
		return "", err
	}
	return *jump, nil
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
	// HostGhDir holds the host-wide gh credential, seeded by `forge host gh-login`
	// and copied into each workspace at create. Kept in sync with internal/agent.
	//
	// Exported for the one step this package cannot do yet: the gh login itself is
	// an interactive browser-or-token flow, so it needs a terminal — and terminals
	// are still the front end's, until they move behind this boundary too.
	HostGhDir = hostKeyDir + "/gh"
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

// buildPrepareScript assembles the idempotent remote provisioning script. It
// assumes it runs as root (the caller wraps it in `sudo bash -s` when needed).
func buildPrepareScript(pkgMgr, iproutePkg, sshClientPkg, goarch string, sshPort int, user string, isRoot, firewall, harden, dockerPrune, pruneImages, pruneVolumes bool) string {
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
		"__IMAGE_PRUNE__", imagePruneLine(pruneImages),
		"__VOLUME_PRUNE__", volumePruneLine(pruneVolumes),
		"__FULL_PCT__", pruneFullPct,
	)
	return r.Replace(b.String())
}

// pruneImagesGrace is how old an unreferenced image must be before the opt-in
// aggressive sweep will remove it. Wide enough (7 days) that every recent rebuild
// — and so the newest build of any repository — is safe, and a stack brought fully
// down for a few days keeps its image.
const pruneImagesGrace = "168h"

// imagePruneLine is the aggressive image sweep injected into the nightly clean-up
// when `--docker-prune-images` is set. It removes tagged images that no container
// (running OR stopped) references and that are older than the grace window — the
// superseded builds a rebuild-to-a-new-tag leaves behind, which the dangling pass
// can't see because they keep their tag. Off by default: without a container to
// hold it, a `compose down`-ed stack's image older than the grace window would go
// too and force a rebuild, so this is a trade you opt into rather than a default.
func imagePruneLine(on bool) string {
	if !on {
		return "# tagged-image sweep disabled (opt in with --docker-prune-images)"
	}
	return `# Superseded builds: unreferenced tagged images past the grace window. A
# rebuild to a NEW tag leaves the old one tagged (so not dangling); this is the
# only pass that reaps it. -a is safe here because an image any container holds —
# running or merely stopped — is never a candidate.
echo "[prune] unreferenced images older than ` + pruneImagesGrace + `:"
docker image prune -a -f --filter until=` + pruneImagesGrace + ` || failed=1`
}

// pruneFullPct is the disk-usage percentage at or above which the nightly
// clean-up reports failure instead of success. A host that is still this full
// after a reclaim has a problem the clean-up cannot solve on its own, and the
// only thing worse than that is not being told.
const pruneFullPct = "90"

// volumePruneLine is the volume pass. Anonymous-only by default; the opt-in
// `--docker-prune-volumes` tier widens it to every unused volume.
//
// The default is safe in a way "prune volumes" generally is not: without `-a`,
// Docker removes only volumes nobody NAMED, which is what an image's `VOLUME`
// directive leaves behind when a container dies on a signal before `--rm` can
// fire. A compose stack's data is always in a named volume, so it is never in
// scope here.
//
// The opt-in tier drops that distinction and takes every unused volume with it,
// which is what a busy build host needs — named scratch volumes (Go caches,
// node_modules trees) were 65 of the 75 GB on the host measured. It is opt-in
// because the same sweep takes the data of a stack stopped with `compose down`,
// which removes the containers but deliberately KEEPS the named volumes.
// `compose stop`, or leaving it running, is unaffected: the containers still
// exist, so their volumes are never unused.
func volumePruneLine(all bool) string {
	if !all {
		return `# Volumes nobody named — an image VOLUME whose container died on a signal, so
# ` + "`--rm`" + ` never reclaimed it. A named volume (any compose stack's data) is out
# of scope without -a. Widen with --docker-prune-volumes.
docker volume prune -f || failed=1`
	}
	return `# Every unused volume, named ones included: the Go caches and node_modules
# trees a build host accumulates. This also takes a ` + "`compose down`" + `-ed stack's
# data, which is why it is opt-in — see volumePruneLine in prepare.go.
echo "[prune] all unused volumes, including named:"
docker volume prune -af || failed=1`
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
//   - DANGLING images only (no -a) by default. Those are the layer sets a rebuild
//     left behind — including the previous image when you rebuild to the SAME tag,
//     which loses its tag and so becomes dangling on the spot. Rebuilding to a NEW
//     tag instead leaves the old build tagged (not dangling), so the default pass
//     can't see it; the opt-in `--docker-prune-images` tier adds a guarded `-a`
//     sweep for exactly that case (see imagePruneLine). It stays opt-in because a
//     stack brought fully down — no container left, running or stopped — would lose
//     its image too once past the grace window.
//   - Build cache, which on a real host is where the growth actually is — ALL of
//     it, see the `until` note below for why no filter survives here.
//   - ANONYMOUS volumes, and by default only those. See the volume note below.
//
// Stopped containers are deliberately NOT pruned either. On the host I measured
// they were 23MB against 3.9GB of build cache — nothing — and removing one takes
// its writable layer with it, so a stack you `compose stop`-ed for the night is
// gone in the morning and has to be `up`-ed rather than `start`-ed. Not worth it.
//
// A systemd timer rather than cron: no extra package, it logs to the journal, and
// Persistent=true means a server that was off at 03:00 still runs the clean-up
// when it comes back.
//
// # Why `--filter until=` is gone from the build-cache pass
//
// It never did what its name suggests. BuildKit's `until` filters on LAST USED,
// not on age, so `until=24h` means "cache nothing has touched in a day" — and on
// a build server, which is the only kind of host this clean-up is for, nearly
// every record is touched daily. The filter therefore spares almost the entire
// cache, which is the opposite of the intent ("nothing you built today is
// touched").
//
// Measured, not assumed. On a build host, with a cache whose records were all
// CREATED more than 30s earlier but LAST USED under a second earlier:
//
//	docker buildx prune --filter until=30s   ->  reclaimed 0 B, every record survived
//	docker buildx prune --filter until=1s    ->  reclaimed the lot
//
// Creation-time semantics would have emptied it in the first call. The same host
// showed what that costs in practice: 95 GB of build cache and a 98%-full disk,
// after this timer had been running nightly and exiting 0 for weeks. An
// unfiltered `-af` on that cache reclaimed 93 GB.
//
// So the build-cache pass is now `-af`: all of it, every night. That is a real
// trade and worth stating plainly — the morning's first build is cold. It is the
// right side of the trade because build cache is the one thing here that is
// purely derived: losing it costs time and nothing else, while the alternative
// this host demonstrated is every workspace down at once on a full disk. A host
// that wants the cache kept declines the whole clean-up with --no-docker-prune.
//
// # Why volumes are no longer "never"
//
// The old rule was "never volumes, that is where data lives", and the reasoning
// behind it is sound — `docker system df` reports volumes as 100% reclaimable
// whenever no container holds them, which is how a copy-pasted prune cron job
// ends up eating a database. But it drew the line one notch too conservatively,
// because Docker already draws a precise one: `docker volume prune` without `-a`
// removes ONLY ANONYMOUS volumes.
//
// An anonymous volume is one nobody named. It cannot be the `compose down`-ed
// stack whose data someone is coming back for, because compose volumes are
// always named; it is what an image's `VOLUME` directive creates when a
// container runs without an explicit mount. `docker run --rm` normally takes it
// away again — but `--rm` is exactly what a SIGNALLED death skips (Ctrl-C, a CI
// timeout, an OOM kill), and what is left then has no name, no label, and once
// its container is gone, nothing to attribute it to.
//
// That is not a corner case; it was the single largest class on the host
// measured. Of 204 dangling volumes holding 75 GB, 138 were anonymous ~40 MB
// postgres data dirs from test runs that died on a signal.
//
// Named dangling volumes — build caches, node_modules trees, a `compose down`-ed
// stack's data — stay untouched by default and move to the opt-in
// `--docker-prune-volumes` tier, because that set genuinely can contain data
// somebody wants back.
//
// # Why it now reports failure
//
// Every pass used to end in `|| true`, so the unit reported success whether it
// had reclaimed 168 GB or nothing at all — which is how a clean-up that had
// stopped working went unnoticed while the disk filled. A safeguard whose
// success path is indistinguishable from its no-op path is not a safeguard, it
// is a report. The script now measures the filesystem before and after, says
// what it reclaimed, and exits non-zero when a pass failed or when the host is
// still over pruneFullPct — the state where somebody needs to know.
const dockerPruneSection = `echo "[forge] installing nightly docker clean-up (03:00) ..."
cat > /usr/local/bin/forge-docker-prune <<'PRUNE'
#!/bin/sh
# Reclaim disk from Docker. See prepare.go for why each pass is the shape it is.
set -e

# Fail the run when the filesystem is still this full afterwards. The number is
# not the point; making "there was nothing left to reclaim and we are nearly
# full" a visible failure rather than a silent success is.
FULL_PCT=__FULL_PCT__

# A summary for humans, deliberately world-READABLE: the journal is root/adm-only
# on a stock host, so without this a workspace user cannot check whether the
# nightly clean-up is still working — which is how the broken one went unnoticed.
LOG=/var/log/forge-docker-prune.log

docker info >/dev/null 2>&1 || { echo "docker not available; nothing to clean"; exit 0; }

root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
used_pct() { df --output=pcent "$root" | tail -1 | tr -dc '0-9'; }
used_kb()  { df --output=used  "$root" | tail -1 | tr -dc '0-9'; }

before_pct=$(used_pct)
before_kb=$(used_kb)
echo "docker disk usage before: ${before_pct}% on ${root}"
docker system df

# "|| failed=1" rather than "|| true": one pass failing must not abort the rest
# under "set -e", but it must not be swallowed either.
failed=0

# Layer sets orphaned by a rebuild. NOT -a: that would delete tagged images no
# container happens to be running, i.e. every idle workspace's images.
docker image prune -f --filter until=24h || failed=1
__IMAGE_PRUNE__
# BuildKit cache, all of it. No "until" filter: it measures LAST USED, so on a
# build server it spares almost everything. See prepare.go.
docker builder prune -af || failed=1
__VOLUME_PRUNE__
# Containers are NOT pruned: worth ~nothing next to the cache, and removing one
# takes its writable layer with it, so a stack stopped for the night would have to
# be re-created in the morning rather than just started.

after_pct=$(used_pct)
after_kb=$(used_kb)
echo "docker disk usage after: ${after_pct}% on ${root}"
docker system df

# The delta can be NEGATIVE: this runs at 03:30 on a host where something else
# may well be building, and that build can write more than the prune freed. That
# is worth seeing rather than clamping to zero — "reclaimed 0 MiB" and "the disk
# grew while we cleaned" are different situations and only one of them means the
# clean-up has nothing left to give. So report it, in words that read correctly.
delta_mb=$(( (before_kb - after_kb) / 1024 ))
if [ "$delta_mb" -lt 0 ]; then
  grew_mb=$(( -delta_mb ))
  summary="grew ${grew_mb} MiB during the run, something else is writing (${before_pct}% -> ${after_pct}% on ${root})"
else
  summary="reclaimed ${delta_mb} MiB (${before_pct}% -> ${after_pct}% on ${root})"
fi
echo "$summary"

# Best effort: a log we cannot write is not worth failing a reclaim over.
{ printf '%s %s\n' "$(date -Is)" "$summary" >> "$LOG" && chmod 644 "$LOG"; } 2>/dev/null || true

if [ "$failed" -ne 0 ]; then
  echo "FAIL: a prune pass failed — see the output above." >&2
  exit 1
fi
if [ "$after_pct" -ge "$FULL_PCT" ]; then
  echo "FAIL: still ${after_pct}% used — ${summary}." >&2
  echo "Docker has no more to give: the space is either outside Docker, or in the" >&2
  echo "images, containers and named volumes this clean-up does not touch." >&2
  echo "Start with: docker system df -v" >&2
  exit 1
fi
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
