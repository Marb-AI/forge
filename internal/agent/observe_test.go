package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
)

func TestParseDockerPorts(t *testing.T) {
	known := map[string]bool{"crm": true, "shop": true}
	// Tab-separated: project, service, name, running, bindings.
	out := "crm\tweb\t/crm-web-1\ttrue\t16000>3000/tcp \n" +
		"crm\tapi\t/crm-api-1\tfalse\t16001>8080/tcp 16002>9229/tcp \n" +
		// No compose service label: the name has to be cleaned up instead.
		"shop\t<no value>\t/shop-db-1\ttrue\t16100>5432/tcp \n" +
		// Not a workspace's container — an admin ran it by hand. Not ours to
		// report, let alone tunnel.
		"\t\t/nginx\ttrue\t80>80/tcp \n" +
		"other\tweb\t/other-web-1\ttrue\t9000>3000/tcp \n" +
		// Published nothing.
		"crm\tworker\t/crm-worker-1\ttrue\t\n"

	got := parseDockerPorts(out, known)

	want := map[string][]agentproto.Port{
		"crm": {
			{Name: "web", Host: 16000, Target: 3000, Running: true, Kind: agentproto.KindContainer},
			{Name: "api", Host: 16001, Target: 8080, Running: false, Kind: agentproto.KindContainer},
			{Name: "api", Host: 16002, Target: 9229, Running: false, Kind: agentproto.KindContainer},
		},
		"shop": {
			{Name: "db", Host: 16100, Target: 5432, Running: true, Kind: agentproto.KindContainer},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// A stopped container still holds its host port — `docker start` will bind it — so
// it has to keep being reported, or the port looks free and gets reused.
func TestParseDockerPortsKeepsStoppedContainers(t *testing.T) {
	got := parseDockerPorts("crm\tweb\t/crm-web-1\tfalse\t16000>3000/tcp \n", map[string]bool{"crm": true})
	if len(got["crm"]) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got["crm"][0].Running {
		t.Error("container reported as running")
	}
	if got["crm"][0].Host != 16000 {
		t.Errorf("port = %d", got["crm"][0].Host)
	}
}

func TestContainerLabel(t *testing.T) {
	cases := []struct{ service, name, project, want string }{
		{"web", "/crm-web-1", "crm", "web"},
		// No service label: strip the leading slash, the project prefix (which is
		// the workspace you are already looking at) and the replica index.
		{"", "/crm-web-1", "crm", "web"},
		{"<no value>", "/crm-web-1", "crm", "web"},
		{"", "/crm_web_1", "crm", "web"},
		{"", "/standalone", "crm", "standalone"},
		// A name that is only a prefix and an index must not come out empty.
		{"", "/crm-1", "crm", "1"},
		// A trailing part that is not a number is part of the name.
		{"", "/crm-web-worker", "crm", "web-worker"},
	}
	for _, c := range cases {
		if got := containerLabel(c.service, c.name, c.project); got != c.want {
			t.Errorf("containerLabel(%q, %q, %q) = %q, want %q", c.service, c.name, c.project, got, c.want)
		}
	}
}

func TestParseListeners(t *testing.T) {
	known := map[string]bool{"crm": true}
	owner := func(pid int) string {
		switch pid {
		case 1234, 1240:
			return "crm"
		case 9999:
			return "root"
		}
		return ""
	}
	// `ss -Htlnp`: state, recv-q, send-q, local addr, peer addr, users.
	out := `LISTEN 0 4096 0.0.0.0:16005 0.0.0.0:* users:(("node",pid=1234,fd=23))
LISTEN 0 4096 [::]:16006 [::]:* users:(("python3",pid=1240,fd=7))
LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=9999,fd=3))
LISTEN 0 4096 *:16007 *:* users:(("ghost",pid=4242,fd=3))
LISTEN 0 4096 0.0.0.0:16008 0.0.0.0:*
`
	// 16000 is already accounted for by a container: docker publishes through
	// docker-proxy, which listens as root, so without this every published port
	// appears twice — once attributed, once not.
	claimed := map[int]bool{16000: true}

	got := parseListeners(out, known, claimed, owner)
	want := map[string][]agentproto.Port{
		"crm": {
			{Name: "node", Host: 16005, Running: true, Kind: agentproto.KindProcess},
			{Name: "python3", Host: 16006, Running: true, Kind: agentproto.KindProcess},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseListenersSkipsClaimedPorts(t *testing.T) {
	owner := func(int) string { return "crm" }
	out := "LISTEN 0 4096 0.0.0.0:16000 0.0.0.0:* users:((\"docker-proxy\",pid=1,fd=3))\n"
	got := parseListeners(out, map[string]bool{"crm": true}, map[int]bool{16000: true}, owner)
	if len(got) != 0 {
		t.Errorf("a container's port was reported twice: %+v", got)
	}
}

func TestPortOf(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"0.0.0.0:16000", 16000, true},
		{"127.0.0.1:16001", 16001, true},
		{"[::]:16002", 16002, true},
		{"*:16003", 16003, true},
		{"[::1]:16004", 16004, true},
		{"nonsense", 0, false},
		{"0.0.0.0:notaport", 0, false},
		{"0.0.0.0:0", 0, false},
	}
	for _, c := range cases {
		got, ok := portOf(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("portOf(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUserProcess(t *testing.T) {
	name, pid, ok := userProcess(`LISTEN 0 4096 *:16000 *:* users:(("node",pid=1234,fd=23))`)
	if !ok || name != "node" || pid != 1234 {
		t.Errorf("= %q, %d, %v", name, pid, ok)
	}
	// Several processes on one socket: the first is enough to name it.
	name, pid, ok = userProcess(`LISTEN 0 128 *:80 *:* users:(("nginx",pid=10,fd=6),("nginx",pid=11,fd=6))`)
	if !ok || name != "nginx" || pid != 10 {
		t.Errorf("= %q, %d, %v", name, pid, ok)
	}
	// No users: field at all — ss without root, which is not this agent, but the
	// parser must not invent a process.
	if _, _, ok := userProcess(`LISTEN 0 4096 *:16000 *:*`); ok {
		t.Error("expected no process")
	}
}

// Service names become docker filter arguments, so they are validated rather than
// trusted — the same reason workspace names are.
func TestServiceRe(t *testing.T) {
	for _, ok := range []string{"web", "api-1", "db_main", "Web2", "a.b"} {
		if !serviceRe.MatchString(ok) {
			t.Errorf("%q should be a valid service name", ok)
		}
	}
	for _, bad := range []string{"", "-web", ".hidden", "web service", "web;rm -rf /", "--filter", "a/b", strings.Repeat("x", 64)} {
		if serviceRe.MatchString(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
