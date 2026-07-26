package forge

import (
	"reflect"
	"testing"
)

func TestParseListeningPorts(t *testing.T) {
	// Representative `ss -H -tln` output: ipv4, ipv6, loopback, duplicates.
	in := `LISTEN 0      511          0.0.0.0:3000       0.0.0.0:*
LISTEN 0      4096       127.0.0.1:5432       0.0.0.0:*
LISTEN 0      511             [::]:3000          [::]:*
LISTEN 0      128          0.0.0.0:22         0.0.0.0:*
LISTEN 0      511             [::]:8080          [::]:*`
	want := []int{22, 3000, 5432, 8080}
	got := parseListeningPorts(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseListeningPorts = %v, want %v", got, want)
	}
}

func TestParseListeningPortsEmpty(t *testing.T) {
	if got := parseListeningPorts(""); len(got) != 0 {
		t.Fatalf("expected no ports, got %v", got)
	}
}

// Asking about a host that was never registered is a mistake worth naming. The
// alternative — an empty list — reads as "that server has nothing on it", which
// is a different and much more reassuring answer than the truth.
func TestHostPortUseRejectsAnUnknownHost(t *testing.T) {
	if _, err := HostPortUse("no-such-host"); err == nil {
		t.Error("an unknown alias should be an error, not an empty report")
	}
	// Asking about all of them reports every registered host and nothing else —
	// with none registered, that is an empty report rather than a failure.
	got, err := HostPortUse("")
	if err != nil {
		t.Fatalf("HostPortUse(\"\") = %v", err)
	}
	hosts, err := Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(hosts) {
		t.Errorf("reported %d hosts, %d are registered", len(got), len(hosts))
	}
}
