package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/proc"
	"github.com/Marb-AI/forge/internal/sshx"
	"github.com/Marb-AI/forge/internal/ui"
)

// runUIArg is the hidden subcommand the detached UI daemon re-execs itself with,
// mirroring runSupervisorArg. `forge ui` launches `forge __run-ui` in the
// background.
const runUIArg = "__run-ui"

// uiCmd handles `forge ui [start|stop|status|port <port>]`. Bare `forge ui`
// means `forge ui start`.
func uiCmd(args []string) int {
	sub := "start"
	rest := args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "start":
		return uiStart()
	case "stop":
		return uiStop()
	case "status":
		return uiStatus()
	case "port":
		return uiSetPort(rest)
	default:
		return fail("usage: forge ui [start|stop|status|port <port>]")
	}
}

func tokenPath(dir string) string { return filepath.Join(dir, "ui.token") }

func uiStart() int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	port := cfg.UIPortOr()

	if pid, ok := pidFromFile(ui.PIDPath(dir)); ok {
		token := readToken(dir)
		fmt.Printf("forge ui already running (pid %d)\n  %s\n", pid, uiURL(port, token))
		return 0
	}

	token, err := newToken()
	if err != nil {
		return fail("%v", err)
	}
	if err := writeToken(dir, token); err != nil {
		return fail("%v", err)
	}
	if err := startDetached(dir, "ui.log", runUIArg); err != nil {
		return fail("%v", err)
	}
	// The daemon writes its pidfile only once it has actually bound the port, so
	// waiting for it turns "port already in use" into a real error instead of a
	// browser opening on a dead address.
	if !waitForUI(dir, 3*time.Second) {
		return fail("the UI daemon didn't come up (port %d may be in use)\n  see %s",
			port, filepath.Join(dir, "ui.log"))
	}

	url := uiURL(port, token)
	fmt.Printf("forge ui started\n  %s\n", url)
	openBrowser(url)
	return 0
}

// waitForUI polls for the daemon's pidfile, which it writes only after a
// successful bind.
func waitForUI(dir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := pidFromFile(ui.PIDPath(dir)); ok {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func uiStop() int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	pid, ok := pidFromFile(ui.PIDPath(dir))
	if !ok {
		fmt.Println("forge ui not running")
		return 0
	}
	if err := proc.Terminate(pid); err != nil {
		return fail("stop: %v", err)
	}
	// Wait for it to actually go: we tell people to run `forge ui stop && forge ui`,
	// and a replacement that starts while the old one still holds the port would
	// fail to bind.
	waitForUIExit(dir, 3*time.Second)
	fmt.Println("forge ui stopped")
	return 0
}

// waitForUIExit gives a signalled daemon a moment to release its pidfile (and
// the port) before a replacement tries to bind.
func waitForUIExit(dir string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := pidFromFile(ui.PIDPath(dir)); !ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func uiStatus() int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	pid, ok := pidFromFile(ui.PIDPath(dir))
	if !ok {
		fmt.Println("forge ui not running (start with: forge ui)")
		return 0
	}
	fmt.Printf("forge ui running (pid %d)\n  %s\n", pid, uiURL(cfg.UIPortOr(), readToken(dir)))
	return 0
}

func uiSetPort(rest []string) int {
	if len(rest) < 1 {
		return fail("usage: forge ui port <port>")
	}
	p, err := strconv.Atoi(rest[0])
	if err != nil || p < 1 || p > 65535 {
		return fail("invalid port %q (want 1-65535)", rest[0])
	}
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	cfg.UIPort = p
	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("forge ui port set to %d\n", p)
	if dir, err := config.Dir(); err == nil {
		if _, ok := pidFromFile(ui.PIDPath(dir)); ok {
			fmt.Println("restart to apply: forge ui stop && forge ui")
		}
	}
	return 0
}

// runUI is the foreground body of the detached UI daemon. It loads config and
// the session token, wires the Forge operations the server needs, and blocks in
// ui.Serve until signalled.
func runUI(_ []string) int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	token := readToken(dir)
	if token == "" {
		return fail("no ui token found — start with: forge ui")
	}

	deps := ui.Deps{
		ListWorkspaces: listWorkspacesInfo,
		HostFor: func(name string) *config.Host {
			// Reload each time so workspaces added while the daemon runs resolve.
			c, err := config.Load()
			if err != nil {
				return nil
			}
			return c.HostFor(name)
		},
		Checkpoint: func(name string, out io.Writer) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			host := c.HostFor(name)
			if host == nil {
				return fmt.Errorf("unknown workspace %q", name)
			}
			return runCheckpoint(sshx.WorkspaceTarget(host, name), agentproto.TmuxSession,
				func(m string) { fmt.Fprintln(out, m) })
		},
		ListHosts: func() ([]string, error) {
			c, err := config.Load()
			if err != nil {
				return nil, err
			}
			aliases := make([]string, 0, len(c.Hosts))
			for a := range c.Hosts {
				aliases = append(aliases, a)
			}
			sort.Strings(aliases)
			return aliases, nil
		},
		CreateWorkspace: createWorkspace,
		PrepareHost:     runHostPrepare,
	}
	if err := ui.Serve(dir, cfg.UIPortOr(), token, deps); err != nil {
		return fail("%v", err)
	}
	return 0
}

// listWorkspacesInfo gathers workspaces across all reachable hosts for the UI.
// Unlike the CLI's `workspace list`, it silently skips unreachable hosts rather
// than printing an "(unreachable)" row — the UI refetches on its own.
func listWorkspacesInfo() ([]ui.WorkspaceInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(cfg.Hosts))
	for a := range cfg.Hosts {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)

	out := []ui.WorkspaceInfo{}
	for _, alias := range aliases {
		var res agentproto.ListResult
		if err := callAgent(cfg.Hosts[alias], &res, "workspace-list"); err != nil {
			continue
		}
		sort.Slice(res.Workspaces, func(i, j int) bool {
			return res.Workspaces[i].Name < res.Workspaces[j].Name
		})
		for _, ws := range res.Workspaces {
			out = append(out, ui.WorkspaceInfo{Name: ws.Name, Host: alias, Status: ws.Status})
		}
	}
	return out, nil
}

func uiURL(port int, token string) string {
	if token == "" {
		return fmt.Sprintf("http://127.0.0.1:%d/", port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, token)
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeToken(dir, token string) error {
	return os.WriteFile(tokenPath(dir), []byte(token), 0o600)
}

func readToken(dir string) string {
	data, err := os.ReadFile(tokenPath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// openBrowser best-effort opens url in the default browser. Failure is silent —
// the URL is always printed too.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
