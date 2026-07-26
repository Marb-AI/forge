package forge

import (
	"reflect"
	"testing"
)

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
