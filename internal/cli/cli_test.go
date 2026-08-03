package cli

import (
	"reflect"
	"testing"
)

func TestExtractFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantVal  string
		wantRest []string
	}{
		{"equals form", []string{"target", "--alias=srv"}, "srv", []string{"target"}},
		{"space form", []string{"--alias", "srv", "target"}, "srv", []string{"target"}},
		{"single dash equals", []string{"target", "-alias=srv"}, "srv", []string{"target"}},
		{"before positional", []string{"--alias=srv", "target"}, "srv", []string{"target"}},
		{"absent", []string{"target"}, "", []string{"target"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			val, rest := extractFlag(c.args, "alias")
			if val != c.wantVal || !reflect.DeepEqual(rest, c.wantRest) {
				t.Fatalf("extractFlag(%v) = (%q,%v), want (%q,%v)", c.args, val, rest, c.wantVal, c.wantRest)
			}
		})
	}
}

// A flag that was typed as empty and a flag that was not typed are different
// answers, for the one flag where they mean different things: `--jump=` clears a
// host's route, no --jump at all keeps the recorded one.
func TestExtractFlagSetSaysWhetherItWasThere(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantVal   string
		wantGiven bool
	}{
		{"absent", []string{"target"}, "", false},
		{"equals form, empty", []string{"target", "--jump="}, "", true},
		{"equals form", []string{"target", "--jump=bastion"}, "bastion", true},
		{"space form", []string{"--jump", "bastion", "target"}, "bastion", true},
		{"single dash equals, empty", []string{"target", "-jump="}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			val, given, rest := extractFlagSet(c.args, "jump")
			if val != c.wantVal || given != c.wantGiven {
				t.Fatalf("extractFlagSet(%v) = (%q,%v), want (%q,%v)", c.args, val, given, c.wantVal, c.wantGiven)
			}
			if !reflect.DeepEqual(rest, []string{"target"}) {
				t.Fatalf("the positional did not survive: %v", rest)
			}
		})
	}
}

func TestHasBoolFlag(t *testing.T) {
	if !hasBoolFlag([]string{"-A", "x"}, "-A", "--agent") {
		t.Error("expected -A found")
	}
	if hasBoolFlag([]string{"x"}, "--no-agent") {
		t.Error("did not expect flag")
	}
}

func TestDropFlags(t *testing.T) {
	got := dropFlags([]string{"a", "--no-firewall", "b", "--no-ssh-harden"}, "--no-firewall", "--no-ssh-harden")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("dropFlags = %v", got)
	}
}

func TestJoinInts(t *testing.T) {
	if got := joinInts([]int{1, 2, 3}); got != "1 2 3" {
		t.Errorf("joinInts = %q", got)
	}
}
