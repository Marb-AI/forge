// Package version answers "which Forge is this".
//
// It exists because two binaries have to agree and only one of them is on the
// machine you are typing at: the client carries the agent it installs, so a
// server runs whatever the client that last prepared it had embedded. When a
// feature is missing, the first question is which of the two is behind — and
// until now neither could answer, so the only way to tell was to compare
// timestamps on a binary or read the verbs out of a usage line.
//
// Stamped at build time by the Makefile, for the client and the agent in the
// same command, so a release's two halves say the same thing.
package version

import "runtime/debug"

// Version is the release this was built from, set with
// -X github.com/Marb-AI/forge/internal/version.Version=v1.2.3.
//
// "dev" is the honest answer for everything else — a `go build` in a working
// tree is not a release, and calling it one is how a machine ends up running
// something nobody can name.
var Version = "dev"

// Commit reports the revision the build recorded, and whether the tree it came
// from had uncommitted changes. It comes from the build itself rather than the
// Makefile: the toolchain records what it built from, which is the one thing a
// stamp cannot get wrong. Empty when nothing recorded one — a `go run`, a `go
// test`, or a build outside a repository.
func Commit() (rev string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev, modified
}

// String is the one line both binaries print: the release, and the revision
// under it when there is one.
//
//	v1.2.3 (a1b2c3d4e5f6)
//	dev (a1b2c3d4e5f6, modified)
func String() string {
	rev, modified := Commit()
	return format(Version, rev, modified)
}

// format is String with its inputs handed in, so what it prints can be checked
// without a build stamp — `go test` records no revision, so the interesting
// cases are unreachable from String itself.
func format(release, rev string, modified bool) string {
	if rev == "" {
		return release
	}
	s := release + " (" + rev
	if modified {
		s += ", modified"
	}
	return s + ")"
}
