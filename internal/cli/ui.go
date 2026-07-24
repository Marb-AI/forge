package cli

import (
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

	if err := startDetached(dir, "ui.log", runUIArg); err != nil {
		return fail("%v", err)
	}
	// The daemon writes its pidfile only once it has bound the port, so waiting
	// for it turns "port already in use" into a real error instead of a browser
	// opening on a dead address.
	if !waitForUI(dir, 3*time.Second) {
		return fail("the UI daemon didn't come up (port %d may be in use)\n  see %s",
			port, filepath.Join(dir, "ui.log"))
	}

	// Read the token back rather than trusting one we made up: the daemon mints
	// it after winning the port and writes it before the pidfile, so this is the
	// token actually being served — even if another `forge ui` raced us.
	url := uiURL(port, readToken(dir))
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
	if err != nil {
		return fail("invalid port %q (want 1-65535)", rest[0])
	}
	if err := setUIPort(p); err != nil {
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

// setUIPort records the port the browser UI should bind to. It only takes effect
// on the next start — a running daemon already holds the old port. Shared by
// `forge ui port` and the UI's settings panel.
func setUIPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %d (want 1-65535)", port)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.UIPort = port
	return cfg.Save()
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
	deps := ui.Deps{
		ListWorkspaces:    listWorkspacesInfo,
		WorkspaceActivity: workspaceActivityInfo,
		WorkspaceTrack:    workspaceTrackInfo,
		TrackInc:          trackInc,
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
		DeleteWorkspace: deleteWorkspace,
		RemoveHost:      removeHost,
		SetUIPort:       setUIPort,
	}
	if err := ui.Serve(dir, cfg.UIPortOr(), deps); err != nil {
		return fail("%v", err)
	}
	return 0
}

// listWorkspacesInfo adapts the shared listing for the browser UI. Same source of
// truth as `forge workspace list`: our config says which workspaces are ours, the
// host says whether Claude is running in them.
func listWorkspacesInfo() ([]ui.WorkspaceInfo, error) {
	list, err := listWorkspaces()
	if err != nil {
		return nil, err
	}
	out := make([]ui.WorkspaceInfo, 0, len(list))
	for _, ws := range list {
		out = append(out, ui.WorkspaceInfo{Name: ws.Name, Host: ws.Host, Status: ws.Status})
	}
	return out, nil
}

// workspaceActivityInfo adapts the agent's activity map into the ui package's own
// type, so ui need not import agentproto (same split as listWorkspacesInfo).
func workspaceActivityInfo() (map[string]ui.Activity, error) {
	act, err := workspacesActivity()
	if err != nil {
		return nil, err
	}
	out := make(map[string]ui.Activity, len(act))
	for name, a := range act {
		out[name] = ui.Activity{State: a.State, TS: a.TS}
	}
	return out, nil
}

// workspaceTrackInfo adapts the agent's session-tracking map into the ui package's
// own type, so ui need not import agentproto (same split as workspaceActivityInfo).
func workspaceTrackInfo() (map[string]ui.Track, error) {
	tr, err := workspacesTrack()
	if err != nil {
		return nil, err
	}
	out := make(map[string]ui.Track, len(tr))
	for name, t := range tr {
		out[name] = ui.Track{SessionStart: t.SessionStart, ActiveSeconds: t.ActiveSeconds}
	}
	return out, nil
}

func uiURL(port int, token string) string {
	if token == "" {
		return fmt.Sprintf("http://127.0.0.1:%d/", port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, token)
}

// readToken reads the token the running daemon minted for itself.
func readToken(dir string) string {
	data, err := os.ReadFile(ui.TokenPath(dir))
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
