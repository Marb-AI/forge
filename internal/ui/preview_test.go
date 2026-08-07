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
	// The frames are made in JS now, one per open preview, so this is where the
	// sandbox is written down.
	body := withoutComments(jsFunc(t, embeddedAsset(t, "app.js"), "showPreview"))

	i := strings.Index(body, `setAttribute("sandbox"`)
	if i < 0 {
		t.Fatal("the preview frame is not sandboxed at all")
	}
	rule := body[i:]
	if j := strings.Index(rule, ")"); j > 0 {
		rule = rule[:j]
	}
	for _, needed := range []string{"allow-scripts", "allow-same-origin"} {
		if !strings.Contains(rule, needed) {
			t.Errorf("the frame is missing %s, so a dev server cannot run in it", needed)
		}
	}
	if strings.Contains(rule, "allow-top-navigation") {
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
	if strings.Contains(hide, "src") || strings.Contains(hide, "remove()") {
		t.Error("hiding the preview throws its page away")
	}
	if !strings.Contains(hide, "hidden = true") {
		t.Error("hiding the preview does not hide it")
	}

	// And closing removes the frame, which is what stops the page running and
	// stops it asking for the port.
	closing := withoutComments(jsFunc(t, js, "closePreview"))
	if !strings.Contains(closing, "frame.remove()") {
		t.Error("closing the preview leaves the page running and the port in use")
	}
	if !strings.Contains(closing, "preview.open.delete") {
		t.Error("a closed preview stays in the list, so the rail offers a way back to nothing")
	}
}

// Several previews may be open and one is showing. The others keep their page,
// their scroll position and whatever the app in them is holding — which is the
// expensive part, not the frame, and the reason looking at something else does
// not throw them away.
func TestSeveralPreviewsStayAliveWhileOneShows(t *testing.T) {
	body := withoutComments(jsFunc(t, embeddedAsset(t, "app.js"), "showPreview"))

	if !strings.Contains(body, "other.frame.hidden = other !== pv") {
		t.Error("showing one preview does not merely hide the others")
	}
	// Opening the same address twice is looking at it again, not a second copy:
	// an address is what a preview is.
	if !strings.Contains(body, "preview.open.get(url)") {
		t.Error("the same address opens twice, which is two frames on one port")
	}
}

// Every open preview has an entry in the rail, because the ✕ hides the panel and
// the rail is the only way back — and the trash on that entry is the only way to
// end one.
func TestEveryPreviewHasAWayBackAndAWayOut(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	index := embeddedAsset(t, "index.html")

	if !strings.Contains(index, `id="rail-previews"`) {
		t.Fatal("the rail has nowhere to list open previews")
	}
	body := withoutComments(jsFunc(t, js, "renderPreviewRail"))
	if !strings.Contains(body, "showPreview(pv.url") {
		t.Error("a rail entry does not bring its preview back")
	}
	if !strings.Contains(body, "closePreview(pv.url)") {
		t.Error("nothing ends a preview, so a page runs until the tab is reloaded")
	}
	// The kill is inside the entry, so it must not also trigger the entry.
	if !strings.Contains(body, "stopPropagation()") {
		t.Error("closing a preview also opens it")
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

// One address is one preview, however it was written.
//
// The ports panel builds "http://127.0.0.1:16042" and a parsed URL comes back as
// ".../" — the same port, two strings. Keyed by the raw text, opening it both
// ways would be two frames on one tunnel, which is the thing this is supposed to
// prevent.
//
// Normalising at the door also puts the loopback check on every way in. It used
// to be the address bar's alone, and the address bar is not the only door.
func TestOneAddressIsOnePreviewHoweverItWasWritten(t *testing.T) {
	body := withoutComments(jsFunc(t, embeddedAsset(t, "app.js"), "showPreview"))

	if !strings.Contains(body, "previewURL(raw)") {
		t.Fatal("showPreview trusts the string it was handed, so two spellings of one " +
			"address are two previews")
	}
	i := strings.Index(body, "previewURL(raw)")
	j := strings.Index(body, "preview.open.get(")
	if j < i {
		t.Error("the address is looked up before it is normalised, which is the same bug")
	}
	if !strings.Contains(body[i:j], "if (!url) return") {
		t.Error("an address previewURL refuses is opened anyway")
	}
}
