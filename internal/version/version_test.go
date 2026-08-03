package version

import (
	"strings"
	"testing"
)

// An unstamped build says "dev" and never a version number. A machine running
// something that calls itself a release when nobody released it is exactly the
// confusion this package exists to end.
func TestAnUnstampedBuildIsNotARelease(t *testing.T) {
	if Version != "dev" {
		t.Skipf("this build was stamped %q, so there is nothing to check here", Version)
	}
	if s := String(); !strings.HasPrefix(s, "dev") {
		t.Errorf("String() = %q, want it to start with the version", s)
	}
}

// What the line says, in the three shapes it comes in. Checked through format
// rather than String because `go test` records no revision, which would leave
// the two interesting cases untested.
func TestTheLineSaysWhatWasBuilt(t *testing.T) {
	for _, c := range []struct {
		name     string
		release  string
		rev      string
		modified bool
		want     string
	}{
		{"a release", "v1.2.3", "a1b2c3d4e5f6", false, "v1.2.3 (a1b2c3d4e5f6)"},
		{"a dirty tree says so", "dev", "a1b2c3d4e5f6", true, "dev (a1b2c3d4e5f6, modified)"},
		// No revision at all: nothing invented, and no empty brackets either.
		{"nothing recorded", "v1.2.3", "", false, "v1.2.3"},
	} {
		if got := format(c.release, c.rev, c.modified); got != c.want {
			t.Errorf("%s: format = %q, want %q", c.name, got, c.want)
		}
	}
}

// The revision is cut short enough to read out loud, when there is one.
func TestTheRevisionIsShort(t *testing.T) {
	rev, _ := Commit()
	if rev == "" {
		t.Skip("no VCS stamp in this build (go test does not record one)")
	}
	if len(rev) != 12 {
		t.Errorf("Commit() = %q, want it cut to 12 characters", rev)
	}
}
