package cli

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
