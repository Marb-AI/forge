package ui

import (
	"regexp"
	"strings"
	"testing"
)

// narrowBlock returns the stylesheet's narrow-screen rules.
func narrowBlock(t *testing.T) string {
	t.Helper()
	css := embeddedAsset(t, "app.css")
	// The block runs to the closing brace of the media query, which is the one
	// "}" at column 0 after it.
	re := regexp.MustCompile(`(?ms)^@media \(max-width: 720px\) \{.*?^\}`)
	block := re.FindString(css)
	if block == "" {
		t.Fatal("app.css has no narrow-screen block, so the page is desktop-only")
	}
	return block
}

// The breakpoint is written twice — once in the stylesheet, once in the script
// that decides which view opens — and a page whose two halves disagreed about
// what "narrow" means would put a terminal on a phone at some widths and open a
// chat on a desktop at others.
func TestTheStylesheetAndTheScriptAgreeOnNarrow(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	if !strings.Contains(js, `"(max-width: 720px)"`) {
		t.Error("app.js does not use the stylesheet's breakpoint; the two will drift")
	}
	narrowBlock(t) // fails if the stylesheet's half is missing
}

// The terminal and the shells do not exist on a phone.
//
// Not tidying-up: a TUI behind a software keyboard, redrawn across a mobile
// link, is not something anyone would choose, and a button that opens one is a
// tap wasted and then a reload. Chat is what the chat is for.
func TestAPhoneIsOfferedNoTerminal(t *testing.T) {
	block := narrowBlock(t)

	for _, gone := range []string{
		"#terminal",
		"#sshpanel",
		`.rail-btn[data-action="ssh"]`,
		`.rail-btn[data-action="host"]`,
		`.rail-btn[data-action="local"]`,
	} {
		if !strings.Contains(block, gone) {
			t.Errorf("%s is still offered on a narrow screen", gone)
		}
	}
	// And the chat must not be among them: it is the one that stays.
	if strings.Contains(block, "#chatpanel { display: none") {
		t.Error("the chat is hidden on a phone, which leaves no way to work at all")
	}
}

// What answers "what is this workspace doing" moves out of the way rather than
// going: on a phone that answer matters more, not less.
func TestTheWorkspacePaneIsOffCanvasRatherThanGone(t *testing.T) {
	block := narrowBlock(t)
	index := embeddedAsset(t, "index.html")

	if strings.Contains(block, "#filepane { display: none") {
		t.Fatal("the file pane is dropped on a phone rather than moved aside")
	}
	// transform rather than display, so it animates and keeps its scroll position:
	// a tree scrolled to find something is a tree still in use.
	if !strings.Contains(block, "transform: translateX(-100%)") {
		t.Error("the pane is not pushed off screen, so it covers the stage from the start")
	}
	if !strings.Contains(index, `id="panetoggle"`) {
		t.Error("nothing opens the pane once it is off canvas")
	}
	if !strings.Contains(jsFunc(t, embeddedAsset(t, "app.js"), "initNarrow"), "pane-open") {
		t.Error("the toggle is not wired")
	}
}

// A window dragged narrow, or a phone turned, must end up in a state that
// exists: no off-canvas pane on a wide screen, and nothing waiting for a
// terminal that is not there on a small one.
func TestChangingWidthLandsSomewhereThatExists(t *testing.T) {
	body := jsFunc(t, embeddedAsset(t, "app.js"), "initNarrow")

	if !strings.Contains(body, `mq.addEventListener("change"`) {
		t.Error("nothing reacts to the window crossing the breakpoint")
	}
	if !strings.Contains(body, "apply();") {
		t.Error("the initial width is never applied, so a page loaded narrow opens wrong")
	}
}

// iOS zooms a page when a field with text under 16px takes focus, and does not
// zoom back. The composer is the only field a phone types into.
func TestTheComposerDoesNotMakeIOSZoom(t *testing.T) {
	block := narrowBlock(t)

	i := strings.Index(block, "#chatinput")
	if i < 0 {
		t.Fatal("the composer has no narrow-screen rule")
	}
	if !strings.Contains(block[i:], "font-size: 16px") {
		t.Error("the composer is under 16px on a phone, so focusing it zooms the page " +
			"and nothing zooms it back")
	}
}
