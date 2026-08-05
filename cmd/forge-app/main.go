// Command forge-app is Forge as a desktop application: the same core, in a
// window of its own instead of a browser tab.
//
// There is deliberately almost nothing here. The core already knows how to run
// inside somebody else's process — ui.Start binds a port, serves, and hands back
// what it takes to reach it — so a desktop shell is a window pointed at that
// address and a promise to stop it afterwards. Everything the window shows is the
// same HTML, the same handlers and the same operations the browser gets.
//
// # Why a window over loopback, and not the assets in-process
//
// Wails can serve the front end straight out of Go memory, with no port at all
// (application.AssetOptions.Handler). It is the obvious thing to reach for and it
// does not work here: on iOS that path ends in a WKURLSchemeHandler, which
// answers a request with didReceiveResponse, then didReceiveData carrying the
// *whole* body, then didFinish. Nothing arrives before the handler returns. Forge
// streams two things it cannot do without — terminals and job output, both
// text/event-stream — and they would buffer forever behind that.
//
// So the boundary stays where the core put it: an HTTP server on loopback, and
// every front end a client of it. That is also what keeps this reversible. This
// file is a shell; swapping it for a different one is a day's work, because
// nothing above it knows which shell it is.
package main

import (
	"fmt"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/Marb-AI/forge/internal/ui"
)

func main() {
	app := application.New(application.Options{
		Name:        "Forge",
		Description: "Claude Code workspaces on your servers",
	})

	// Port 0: the OS picks. A desktop app has no reason to want a particular
	// number — only its own webview has to find it — and asking for one is how it
	// would collide with a `forge ui` daemon already on this machine. The two are
	// meant to be able to run at once.
	//
	// The empty directory is the same "wherever this device keeps its state" the
	// CLI relies on, which on a desktop is ~/.forge. A phone says so itself, with
	// forge.Use, before it ever gets here.
	inst, err := ui.Start("", 0)
	if err != nil {
		// A double-clicked application that writes to stderr and exits has, as far
		// as anyone watching can tell, done nothing at all. There is no window to
		// put this in — the thing a window would show is what failed — so it goes
		// in a dialog, which needs the app running first.
		fail(app, "Forge could not start.\n\n%v", err)
		return
	}

	// Said, not dialogged: the window works, and what does not is the ports panel's
	// links. A modal in front of a Forge that started fine would be the wrong size
	// of complaint, and the panel says which tunnels are up anyway.
	if inst.TunnelErr != nil {
		fmt.Fprintf(os.Stderr, "forge-app: the ports panel's links need tunnels and "+
			"they did not start: %v\n", inst.TunnelErr)
	}

	// The window is the whole app: when it goes, the core it was showing goes with
	// it. Stopping the instance closes the terminals it opened and gives the port
	// back — nothing else will, because an in-process UI has no signal handler and
	// no pidfile for anyone to find it by.
	//
	// Stop is idempotent, which is what lets this be said twice: OnShutdown fires
	// when the application terminates the way applications do, and Run's error
	// path (below) reaches neither it nor a deferred call.
	app.OnShutdown(func() { _ = inst.Stop() })

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "Forge",
		// The URL carries this run's token. It is not written to ~/.forge/ui.token
		// and never leaves this process — the webview is the only thing that has
		// it, which is the point of handing it over rather than storing it.
		URL:    inst.URL(),
		Width:  1440,
		Height: 900,
		// Large enough that the rail, the tree and a terminal all fit; below this
		// the layout stops being worth showing.
		MinWidth:  900,
		MinHeight: 600,
	})

	if err := app.Run(); err != nil {
		// Run's own defers do not reach the shutdown hooks — those belong to the
		// platform's termination path, which a failed Run never gets to — and
		// os.Exit below would skip a deferred call here anyway. So the instance is
		// stopped by hand, and its terminals end as sessions closing rather than as
		// connections a dead process left behind.
		_ = inst.Stop()
		fmt.Fprintf(os.Stderr, "forge-app: %v\n", err)
		os.Exit(1)
	}
}

// fail reports a failure the only way a windowed application can be heard: it
// runs the app with no window, shows one dialog, and quits. Also on stderr, for
// whoever started it from a terminal.
func fail(app *application.App, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "forge-app: "+msg)
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		app.Dialog.Error().SetTitle("Forge").SetMessage(msg).Show()
		app.Quit()
	})
	_ = app.Run()
	os.Exit(1)
}
