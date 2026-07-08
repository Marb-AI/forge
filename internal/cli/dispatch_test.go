package cli

import "testing"

func TestMainRouting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"help"}, 0},
		{[]string{"bogus"}, 2},
		{[]string{"host"}, 1},
		{[]string{"workspace"}, 1},
		{[]string{"forwarding"}, 1},
		{[]string{"workspace", "list"}, 0},    // no hosts registered
		{[]string{"forwarding", "status"}, 0}, // no supervisor
		{[]string{"show", "ports"}, 0},        // no hosts
	}
	for _, c := range cases {
		if got := Main(c.args); got != c.want {
			t.Errorf("Main(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}

func TestHostAddListRemove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := Main([]string{"host", "add", "root@1.2.3.4", "--alias=srv"}); got != 0 {
		t.Fatalf("host add = %d", got)
	}
	if got := Main([]string{"host", "add", "x@y", "--alias=srv"}); got != 1 {
		t.Errorf("duplicate host add should fail, got %d", got)
	}
	if got := Main([]string{"host", "add", "root@h"}); got != 1 {
		t.Errorf("missing --alias should fail, got %d", got)
	}
	if got := Main([]string{"host", "list"}); got != 0 {
		t.Errorf("host list = %d", got)
	}
	if got := Main([]string{"host", "remove", "nope"}); got != 1 {
		t.Errorf("remove unknown should fail, got %d", got)
	}
	if got := Main([]string{"host", "remove", "srv"}); got != 0 {
		t.Errorf("host remove = %d", got)
	}
}
