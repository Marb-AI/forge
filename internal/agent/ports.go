package agent

import (
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// opPorts reports what every workspace currently publishes, so the client can
// tunnel it without being told and the UI can show it.
//
// Nothing is stored. Both sources are read fresh each time, because both are
// already the truth: Docker knows what it published, and the kernel knows what is
// listening. A file recording it could only be a stale copy of one of them.
//
// Two sources, because one is not enough. Docker covers published containers,
// including STOPPED ones — whose ports are still spoken for. But a dev server the
// workspace ran directly (`npm run dev`, a Metro or Vite server) is not a container
// at all, and is exactly the sort of thing a developer wants reachable, so plain
// listeners owned by a workspace user count too.
func opPorts() int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return emit(agentproto.PortsResult{Workspaces: map[string]agentproto.WorkspacePorts{}})
		}
		return emitError("read %s: %v", baseDir, err)
	}

	known := map[string]bool{}
	out := map[string]agentproto.WorkspacePorts{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		known[e.Name()] = true
		out[e.Name()] = agentproto.WorkspacePorts{Block: readMetadata(e.Name()).PortBlock}
	}

	claimed := map[int]bool{} // host ports a container already accounts for
	for ws, ports := range dockerPorts(known) {
		wp := out[ws]
		wp.Ports = append(wp.Ports, ports...)
		out[ws] = wp
		for _, p := range ports {
			claimed[p.Host] = true
		}
	}
	for ws, ports := range listenerPorts(known, claimed) {
		wp := out[ws]
		wp.Ports = append(wp.Ports, ports...)
		out[ws] = wp
	}

	// A workspace with neither a block nor a port has nothing to say; dropping it
	// keeps the payload about what exists rather than about what does not.
	for ws, wp := range out {
		if wp.Block == nil && len(wp.Ports) == 0 {
			delete(out, ws)
			continue
		}
		sort.Slice(wp.Ports, func(i, j int) bool { return wp.Ports[i].Host < wp.Ports[j].Host })
		out[ws] = wp
	}
	return emit(agentproto.PortsResult{Workspaces: out})
}

// dockerInspectFormat pulls everything needed about one container out of a single
// `docker inspect`, tab-separated: the compose project (which is the workspace, see
// writeEnvFile), the compose service, the container's own name, whether it is
// running, and each binding as host>container.
//
// Inspect rather than `docker ps --format {{.Ports}}` because ps prints an empty
// PORTS column for a stopped container, whose port is nonetheless still taken.
const dockerInspectFormat = `{{index .Config.Labels "com.docker.compose.project"}}` +
	"\t" + `{{index .Config.Labels "com.docker.compose.service"}}` +
	"\t" + `{{.Name}}` +
	"\t" + `{{.State.Running}}` +
	"\t" + `{{range $p, $bs := .HostConfig.PortBindings}}{{range $bs}}{{.HostPort}}>{{$p}} {{end}}{{end}}`

// dockerPorts returns the published ports of every container belonging to a known
// workspace, keyed by workspace.
//
// A docker that cannot be read yields nothing rather than an error: this is one of
// two sources on a sweep that runs every few seconds, and a host whose docker is
// briefly unhappy should report the listeners it can still see, not fail wholesale.
// The cost is a tunnel that appears a few seconds late, which is what the next
// sweep fixes anyway.
func dockerPorts(known map[string]bool) map[string][]agentproto.Port {
	ids, err := run("docker", "ps", "-aq")
	if err != nil || strings.TrimSpace(ids) == "" {
		return nil
	}
	args := append([]string{"inspect", "--format", dockerInspectFormat}, strings.Fields(ids)...)
	out, err := run("docker", args...)
	if err != nil {
		return nil
	}
	return parseDockerPorts(out, known)
}

// parseDockerPorts turns the inspect output into ports per workspace. Split out
// from the docker call so the parsing — which is where the mistakes live — is
// testable without a daemon.
func parseDockerPorts(out string, known map[string]bool) map[string][]agentproto.Port {
	ports := map[string][]agentproto.Port{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		project, service, name, running, bindings := fields[0], fields[1], fields[2], fields[3], fields[4]
		// The compose project is the workspace (COMPOSE_PROJECT_NAME is set to the
		// workspace name). A container belonging to no workspace — something an admin
		// ran by hand — is not ours to report, let alone tunnel.
		if !known[project] {
			continue
		}
		label := containerLabel(service, name, project)
		for _, b := range strings.Fields(bindings) {
			hostStr, targetStr, ok := strings.Cut(b, ">")
			if !ok {
				continue
			}
			host, err := strconv.Atoi(hostStr)
			if err != nil || host <= 0 {
				continue
			}
			// The container side arrives as "3000/tcp"; the protocol is not something
			// a tunnel or a link can act on, so only the number is kept.
			target, _ := strconv.Atoi(strings.SplitN(targetStr, "/", 2)[0])
			ports[project] = append(ports[project], agentproto.Port{
				Name:    label,
				Host:    host,
				Target:  target,
				Running: running == "true",
				Kind:    agentproto.KindContainer,
			})
		}
	}
	return ports
}

// containerLabel is what to call a container in one column of a narrow panel.
//
// The compose service name is the good answer ("web"). Falling back to the
// container name means undoing what compose did to it: "/crm-web-1" carries a
// leading slash, the project prefix — which is the workspace you are already
// looking at — and a replica index.
func containerLabel(service, name, project string) string {
	if s := sanitizeText(service, agentproto.LabelMaxRunes); s != "" && s != "<no value>" {
		return s
	}
	n := strings.TrimPrefix(name, "/")
	n = strings.TrimPrefix(n, project+"-")
	n = strings.TrimPrefix(n, project+"_")
	if i := strings.LastIndexAny(n, "-_"); i > 0 {
		if _, err := strconv.Atoi(n[i+1:]); err == nil {
			n = n[:i] // a trailing replica index says nothing
		}
	}
	return sanitizeText(n, agentproto.LabelMaxRunes)
}

// listenerPorts returns the ports listening under a workspace user's own uid —
// a dev server started directly, with no container around it.
//
// Ports a container already accounts for are skipped: Docker publishes through
// docker-proxy, which listens as root, so every published port would otherwise
// appear twice, once correctly attributed and once not attributed at all.
func listenerPorts(known map[string]bool, claimed map[int]bool) map[string][]agentproto.Port {
	out, err := run("ss", "-Htlnp")
	if err != nil {
		return nil
	}
	return parseListeners(out, known, claimed, ownerOf)
}

// parseListeners reads `ss -Htlnp`. owner maps a pid to the user running it, taken
// as an argument so the parsing can be tested without processes to look at.
func parseListeners(out string, known map[string]bool, claimed map[int]bool, owner func(pid int) string) map[string][]agentproto.Port {
	ports := map[string][]agentproto.Port{}
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		port, ok := portOf(fields[3])
		if !ok || claimed[port] || seen[port] {
			continue
		}
		name, pid, ok := userProcess(line)
		if !ok {
			continue
		}
		ws := owner(pid)
		if !known[ws] {
			continue
		}
		seen[port] = true
		ports[ws] = append(ports[ws], agentproto.Port{
			Name: sanitizeText(name, agentproto.LabelMaxRunes),
			Host: port,
			// A plain process is always running — it could not be listening otherwise
			// — and has no inside, so no target port.
			Running: true,
			Kind:    agentproto.KindProcess,
		})
	}
	return ports
}

// portOf pulls the port out of an ss local address: "0.0.0.0:16000",
// "127.0.0.1:16001", "[::]:16002", "*:16003".
func portOf(addr string) (int, bool) {
	colon := strings.LastIndex(addr, ":")
	if colon < 0 {
		return 0, false
	}
	p, err := strconv.Atoi(addr[colon+1:])
	if err != nil || p <= 0 {
		return 0, false
	}
	return p, true
}

// userProcess pulls the process name and pid out of an ss line's users: field,
// which looks like: users:(("node",pid=1234,fd=23))
func userProcess(line string) (name string, pid int, ok bool) {
	i := strings.Index(line, `users:(("`)
	if i < 0 {
		return "", 0, false
	}
	rest := line[i+len(`users:(("`):]
	name, rest, ok = strings.Cut(rest, `"`)
	if !ok || name == "" {
		return "", 0, false
	}
	j := strings.Index(rest, "pid=")
	if j < 0 {
		return "", 0, false
	}
	digits := rest[j+len("pid="):]
	end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
	if end >= 0 {
		digits = digits[:end]
	}
	pid, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}
	return name, pid, true
}

// ownerOf returns the username running a pid, or "" if it cannot be determined.
// Attribution by uid rather than by a compose label, so anything a workspace runs
// counts — not only what it ran through compose.
func ownerOf(pid int) string {
	uid, err := procUID(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return ""
	}
	u, err := user.LookupId(uid)
	if err != nil {
		return ""
	}
	return u.Username
}

// Container actions the agent will perform. Deliberately only these two.
//
// `docker compose up` is NOT here and should not be: it CREATES containers, which
// takes knowing the project — which compose file, which profiles, whether the repo
// actually starts with `make dev` instead. Forge does not know any of that, and a
// button that guesses is worse than no button, because you cannot tell what it
// did. Starting and stopping something that already exists needs none of it: the
// container is an object with a name, and the answer is the same whatever built it.
const (
	actionStart = "start"
	actionStop  = "stop"
)

// opContainer starts or stops one of a workspace's containers.
//
// The container is named by its compose SERVICE, which is what the UI shows, and
// resolved here through the project label. Taking a container id from the caller
// would let any request reach any container on the host, including another
// workspace's; a service name is scoped to the project by construction, and the
// project is the workspace.
func opContainer(args []string) int {
	fs := flag.NewFlagSet("workspace-container", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	service := fs.String("service", "", "compose service name")
	action := fs.String("action", "", "start or stop")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return emitError("invalid workspace name %q", *name)
	}
	if *action != actionStart && *action != actionStop {
		return emitError("action must be %q or %q", actionStart, actionStop)
	}
	if !serviceRe.MatchString(*service) {
		return emitError("invalid service name %q", *service)
	}
	if _, err := os.Stat(filepath.Join(baseDir, *name)); err != nil {
		return emitError("no such workspace %q", *name)
	}

	// Two questions, deliberately separate: does this service have containers at
	// all, and do any of them need the action. Collapsing them would answer "the
	// container is already stopped" with the same error as "there is no such
	// container", and only one of those is something to tell the user about.
	all, err := serviceContainers(*name, *service)
	if err != nil {
		return emitError("%v", err)
	}
	if len(all) == 0 {
		// The button was drawn from an observation, so an empty answer means the
		// container went away underneath it. Worth saying.
		return emitError("workspace %q has no container for service %q", *name, *service)
	}
	todo, err := serviceContainers(*name, *service, actionable[*action]...)
	if err != nil {
		return emitError("%v", err)
	}
	if len(todo) == 0 {
		// Already where it was asked to be. Success: the caller wanted a state, not
		// a state transition, and reporting a failure for reaching it anyway would
		// make a harmless double-click look broken.
		return emit(agentproto.OK{OK: true})
	}
	if out, err := run("docker", append([]string{*action}, todo...)...); err != nil {
		return emitError("docker %s: %v: %s", *action, err, tailLines(out, 3))
	}
	return emit(agentproto.OK{OK: true})
}

// actionable is the container states each action has work to do on. Docker ORs
// repeated status filters, so these select exactly the containers that are not
// already where the action would put them.
//
// "dead" is in neither list: it cannot be started and there is nothing to stop.
var actionable = map[string][]string{
	actionStop:  {"running", "paused", "restarting"},
	actionStart: {"exited", "created"},
}

// serviceContainers lists the ids of a workspace's containers for one compose
// service, optionally narrowed to a set of states.
//
// The compose project label is what scopes this to the workspace: a service name
// alone is not unique across the host, and taking a container id from the caller
// would let a request reach any container on the machine.
func serviceContainers(workspace, service string, states ...string) ([]string, error) {
	args := []string{"ps", "-aq",
		"--filter", "label=com.docker.compose.project=" + workspace,
		"--filter", "label=com.docker.compose.service=" + service,
	}
	for _, st := range states {
		args = append(args, "--filter", "status="+st)
	}
	out, err := run("docker", args...)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %v: %s", err, out)
	}
	return strings.Fields(out), nil
}

// serviceRe bounds what can be passed as a compose service. These become docker
// filter arguments, so they are validated rather than trusted — the same reason
// workspace names are.
var serviceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
