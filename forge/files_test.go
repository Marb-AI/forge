package forge

import (
	"errors"
	"os/exec"
	"testing"
)

func TestCleanRel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"/", "", true},
		{".", "", true},
		{"src", "src", true},
		{"/src/main.go", "src/main.go", true},
		{"src//main.go", "src/main.go", true},
		{"src/./main.go", "src/main.go", true},
		{"src/sub/../main.go", "src/main.go", true}, // stays inside: fine
		// Escapes: every one of these must be refused.
		{"..", "", false},
		{"../etc/passwd", "", false},
		{"src/../../etc", "", false},
		{"a/../../..", "", false},
	}
	for _, c := range cases {
		got, ok := cleanRel(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("cleanRel(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestShQuote(t *testing.T) {
	// The point is that nothing can break out of the single quotes.
	cases := map[string]string{
		"main.go":       `'main.go'`,
		"my file.txt":   `'my file.txt'`,
		"a;rm -rf /":    `'a;rm -rf /'`,
		"it's":          `'it'\''s'`,
		"$(whoami)":     `'$(whoami)'`,
		"`id`":          "'`id`'",
		"a'; touch x;'": `'a'\''; touch x;'\'''`,
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// The remote snippet and the code that reads its exit status are two halves of
// one agreement, and they are written in different languages. This runs the
// guard's own shell — every literal in it comes from the rc* constants — and
// checks that what comes back is the failure a caller can act on rather than
// "exit status 5".
func TestAStalePathComesBackAsWhatWentWrong(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh to run the guard against")
	}
	dir := t.TempDir()
	if err := exec.Command("mkdir", dir+"/adir").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("touch", dir+"/afile").Run(); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		rel  string
		want string // "-d" or "-f"
		err  error
	}{
		{"a directory that is gone", "gone", "-d", ErrNoSuchPath},
		{"a file that is gone", "gone", "-f", ErrNoSuchPath},
		{"a directory where a file is", "afile", "-d", ErrNotADir},
		{"a file where a directory is", "adir", "-f", ErrNotAFile},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", guardPath(c.rel, c.want)+"true")
			cmd.Env = append(cmd.Environ(), "HOME="+dir)
			if got := fsErr(cmd.Run()); !errors.Is(got, c.err) {
				t.Errorf("guard reported %v, want %v", got, c.err)
			}
		})
	}

	// And a home that cannot be entered at all, which is the one failure that is
	// about the workspace rather than the path.
	cmd := exec.Command("sh", "-c", guardPath("anything", "-f")+"true")
	cmd.Env = append(cmd.Environ(), "HOME="+dir+"/no-such-home")
	if got := fsErr(cmd.Run()); !errors.Is(got, ErrNoHome) {
		t.Errorf("unreachable home reported %v, want %v", got, ErrNoHome)
	}

	// A path that is there and of the right type must not be a failure at all —
	// otherwise the four cases above would pass with a guard that always exits.
	cmd = exec.Command("sh", "-c", guardPath("afile", "-f")+"true")
	cmd.Env = append(cmd.Environ(), "HOME="+dir)
	if err := cmd.Run(); err != nil {
		t.Errorf("the guard refused a file that is there: %v", err)
	}
}

// The exit codes are a private protocol between the snippet and fsErr, so they
// must be distinct — two paths sharing a code would report each other's failure.
func TestTheGuardsExitCodesAreDistinct(t *testing.T) {
	seen := map[int]bool{}
	for _, rc := range []int{rcNoHome, rcNotFound, rcNotAFile, rcNotADir} {
		if seen[rc] {
			t.Errorf("exit code %d is used for two different failures", rc)
		}
		seen[rc] = true
		// Shells reserve 126 and up, and a code above 255 wraps to something else
		// entirely — either would arrive as some other failure.
		if rc < 1 || rc > 125 {
			t.Errorf("exit code %d is not one a shell will hand back unchanged", rc)
		}
	}
}
