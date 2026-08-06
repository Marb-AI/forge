package ui

import (
	"strings"
	"testing"
)

// The preview sits above the chat and below the shell.
//
// Above the chat because it is what you are looking at instead of it. Below the
// shell for the same reason the chat is: a shell is something you open *beside*
// whatever is on screen, and a preview that covered it would make the ssh button
// do nothing while a page was open.
func TestThePreviewSitsBetweenTheChatAndTheShell(t *testing.T) {
	css := embeddedAsset(t, "app.css")

	chat := zIndexOf(t, css, "#chatpanel")
	pv := zIndexOf(t, css, "#preview")
	shell := zIndexOf(t, css, "#sshpanel")

	if !(chat < pv && pv < shell) {
		t.Errorf("chat %d, preview %d, shell %d — want the preview over the chat and "+
			"under the shell", chat, pv, shell)
	}
}

// An address bar that would load anything is a window into the internet inside
// Forge's own neighbourhood. It takes loopback and nothing else.
func TestThePreviewOnlyOpensLoopback(t *testing.T) {
	body := withoutComments(jsFunc(t, embeddedAsset(t, "app.js"), "previewURL"))

	if !strings.Contains(body, "LOOPBACK.has(u.hostname)") {
		t.Fatal("previewURL does not check where it is pointing")
	}
	// And the check is on the parsed hostname, not on the text: "http://
	// 127.0.0.1@evil.example" is a string that contains 127.0.0.1 and is not it.
	if !strings.Contains(body, "new URL(") {
		t.Error("the address is matched as text rather than parsed, which a URL can dress up")
	}
	if !strings.Contains(body, "u.protocol") {
		t.Error("the scheme is not checked, so javascript: would open in the frame")
	}

	js := withoutComments(embeddedAsset(t, "app.js"))
	for _, host := range []string{"127.0.0.1", "localhost"} {
		if !strings.Contains(js, `"`+host+`"`) {
			t.Errorf("LOOPBACK does not include %s", host)
		}
	}
}

// The frame runs somebody's application, so it needs scripts. What it must not
// get is the ability to navigate the window it is in — a page in a frame
// deciding what the whole of Forge shows.
func TestThePreviewFrameRunsAppsButNotTheWindow(t *testing.T) {
	index := embeddedAsset(t, "index.html")

	i := strings.Index(index, `id="pv-frame"`)
	if i < 0 {
		t.Fatal("there is no preview frame")
	}
	frame := index[i:]
	if j := strings.Index(frame, ">"); j > 0 {
		frame = frame[:j]
	}

	if !strings.Contains(frame, "sandbox=") {
		t.Fatal("the preview frame is not sandboxed at all")
	}
	for _, needed := range []string{"allow-scripts", "allow-same-origin"} {
		if !strings.Contains(frame, needed) {
			t.Errorf("the frame is missing %s, so a dev server cannot run in it", needed)
		}
	}
	if strings.Contains(frame, "allow-top-navigation") {
		t.Error("the framed page can navigate the whole window, which is Forge's window")
	}
}

// Hiding and closing are different, and the difference is the tunnel.
//
// ✕ hides: the frame keeps its page, its scroll position and whatever the app in
// it is holding. Only closing empties it, which is what stops the page running
// and stops it asking for the port.
func TestHidingAPreviewIsNotClosingIt(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	hide := withoutComments(jsFunc(t, js, "hidePreview"))
	if strings.Contains(hide, "src") {
		t.Error("hiding the preview touches the frame's src, which throws the page away")
	}
	if !strings.Contains(hide, "hidden = true") {
		t.Error("hiding the preview does not hide it")
	}

	close := withoutComments(jsFunc(t, js, "closePreview"))
	if !strings.Contains(close, "about:blank") {
		t.Error("closing the preview leaves the page running and the port in use")
	}
}

// On a phone the browser is not the other option: opening a tunnelled port there
// puts Forge in the background, and the tunnel goes with it — so the link would
// lead to a page that cannot load. There, a port opens here.
func TestAPhoneOpensAPortInForge(t *testing.T) {
	body := withoutComments(jsFunc(t, embeddedAsset(t, "app.js"), "portTarget"))

	i := strings.Index(body, "isNarrow()")
	if i < 0 {
		t.Fatal("the ports panel offers the same thing on a phone as on a desktop")
	}
	if !strings.Contains(body[i:], "showPreview(") {
		t.Error("a narrow screen does not open the port inside Forge")
	}
	// And a desktop keeps the link it had, with Forge as the other choice rather
	// than a replacement.
	if !strings.Contains(body, `a.target = "_blank"`) {
		t.Error("the desktop lost its plain link to the port")
	}
}
