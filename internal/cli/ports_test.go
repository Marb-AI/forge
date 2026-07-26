package cli

import (
	"reflect"
	"testing"
)

func TestParseSpan(t *testing.T) {
	start, end, err := parseSpan("16000-30000")
	if err != nil || start != 16000 || end != 30000 {
		t.Errorf("= %d, %d, %v", start, end, err)
	}
	if _, _, err := parseSpan(" 16000 - 30000 "); err != nil {
		t.Errorf("spaces should be tolerated: %v", err)
	}
	for _, bad := range []string{"", "16000", "abc-def", "30000-16000", "16000-70000", "80-9000", "16000-16000"} {
		if _, _, err := parseSpan(bad); err == nil {
			t.Errorf("parseSpan(%q) = nil error; want one", bad)
		}
	}
}

func TestParseLsofPorts(t *testing.T) {
	const out = `COMMAND   PID  USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
node     4821 s1lent   23u  IPv4 0x1234      0t0  TCP *:16000 (LISTEN)
ssh      4900 s1lent    5u  IPv4 0x5678      0t0  TCP 127.0.0.1:16104 (LISTEN)
postgres  512 s1lent    7u  IPv6 0x9abc      0t0  TCP [::1]:5432 (LISTEN)
Dock      333 s1lent    9u  IPv4 0xdef0      0t0  TCP *:16000 (LISTEN)
`
	// Only what is inside the range, deduped and sorted — 5432 is outside it and
	// 16000 appears twice.
	if got, want := parseLsofPorts(out, 16000, 30000), []int{16000, 16104}; !reflect.DeepEqual(got, want) {
		t.Errorf("= %v, want %v", got, want)
	}
	if got := parseLsofPorts(out, 20000, 21000); len(got) != 0 {
		t.Errorf("nothing in range should be empty, got %v", got)
	}
	if got := parseLsofPorts("", 16000, 30000); len(got) != 0 {
		t.Errorf("no output = %v, want none", got)
	}
	// Header-only or truncated lines must not panic or produce ports.
	if got := parseLsofPorts("COMMAND PID USER\nnode 1\n", 1, 65535); len(got) != 0 {
		t.Errorf("short lines = %v, want none", got)
	}
}
