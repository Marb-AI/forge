package cli

import (
	"reflect"
	"testing"
)

func TestParsePublishedPorts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []int
	}{
		{"single", "0.0.0.0:3000->3000/tcp", []int{3000}},
		{
			"ipv4 and ipv6 dedup",
			"0.0.0.0:3000->3000/tcp, :::3000->3000/tcp",
			[]int{3000},
		},
		{
			"multiple lines",
			"0.0.0.0:3000->3000/tcp\n0.0.0.0:5173->5173/tcp",
			[]int{3000, 5173},
		},
		{
			"unpublished skipped",
			"3000/tcp, 0.0.0.0:8080->8080/tcp",
			[]int{8080},
		},
		{"empty", "", nil},
		{"mapped different host port", "0.0.0.0:8081->80/tcp", []int{8081}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePublishedPorts(c.in)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parsePublishedPorts(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
