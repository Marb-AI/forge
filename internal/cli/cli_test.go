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

func TestUnameToGoArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"} {
		got, err := unameToGoArch(in)
		if err != nil || got != want {
			t.Errorf("unameToGoArch(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
	if _, err := unameToGoArch("mips"); err == nil {
		t.Error("expected error for unsupported arch")
	}
}

func TestIproutePackage(t *testing.T) {
	if p, ok := iproutePackage("apt-get"); !ok || p != "iproute2" {
		t.Errorf("apt-get -> %q,%v", p, ok)
	}
	if p, ok := iproutePackage("dnf"); !ok || p != "iproute" {
		t.Errorf("dnf -> %q,%v", p, ok)
	}
	if _, ok := iproutePackage("apk"); ok {
		t.Error("expected apk unsupported")
	}
}

func TestFormatAndJoinPorts(t *testing.T) {
	if formatPorts(nil) == "" {
		t.Error("expected a placeholder for no ports")
	}
	if got := formatPorts([]int{3000, 8080}); got != "3000 8080" {
		t.Errorf("formatPorts = %q", got)
	}
	if got := joinInts([]int{1, 2, 3}); got != "1 2 3" {
		t.Errorf("joinInts = %q", got)
	}
}
