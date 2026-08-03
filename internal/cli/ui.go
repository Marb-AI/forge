package cli

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"

	"github.com/Marb-AI/forge/forge"
	"github.com/Marb-AI/forge/internal/ui"
)

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
		if hasBoolFlag(args[1:], "-q", "--quiet") {
			return uiRunning()
		}
		return uiStatus()
	case "port":
		return uiSetPort(rest)
	default:
		return fail("usage: forge ui [start|stop|status|port <port>]")
	}
}

func uiStart() int {
	d, already, err := forge.StartUI()
	if err != nil {
		return fail("%v", err)
	}
	url := ui.URL(d.Port, d.Token)
	if already {
		fmt.Printf("forge ui already running (pid %d)\n  %s\n", d.PID, url)
		return 0
	}
	fmt.Printf("forge ui started\n  %s\n", url)
	openBrowser(url)
	return 0
}

func uiStop() int {
	stopped, err := forge.StopUI()
	if err != nil {
		return fail("stop: %v", err)
	}
	if !stopped {
		fmt.Println("forge ui not running")
		return 0
	}
	fmt.Println("forge ui stopped")
	return 0
}

// uiRunning is `forge ui status -q`: nothing on stdout, the answer in the exit
// code, the way `systemctl is-active -q` gives it. It exists for scripts — the
// installer stops the daemons it finds running and starts those same ones again
// afterwards — because the alternative is grepping a sentence written for a
// human, which is a sentence nobody may then reword.
func uiRunning() int {
	d, err := forge.UIStatus()
	if err != nil || !d.Running {
		return 1
	}
	return 0
}

func uiStatus() int {
	d, err := forge.UIStatus()
	if err != nil {
		return fail("%v", err)
	}
	if !d.Running {
		fmt.Println("forge ui not running (start with: forge ui)")
		return 0
	}
	fmt.Printf("forge ui running (pid %d)\n  %s\n", d.PID, ui.URL(d.Port, d.Token))
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
	if err := forge.SetUIPort(p); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("forge ui port set to %d\n", p)
	// A running daemon already holds the old port, so say what it takes to move it.
	if d, err := forge.UIStatus(); err == nil && d.Running {
		fmt.Println("restart to apply: forge ui stop && forge ui")
	}
	return 0
}

// openBrowser best-effort opens url in the default browser. Failure is silent —
// the URL is always printed too.
//
// It stays in the CLI rather than the core: it is what this front end does with
// an address on a machine that has a browser and a person in front of it, which
// is not something the core knows or should decide.
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
