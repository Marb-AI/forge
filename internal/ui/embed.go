package ui

import "embed"

// assetsFS holds the browser UI — HTML, JS, CSS and the vendored xterm.js /
// highlight.js. Embedding keeps Forge a single binary: `forge ui` serves these
// straight from the executable, no files to ship alongside it.
//
//go:embed assets
var assetsFS embed.FS
