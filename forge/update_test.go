package forge

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Marb-AI/forge/internal/sshx"
	"github.com/Marb-AI/forge/internal/version"
)

// scripted is a transport that answers commands from a table and records every
// one it was given, so a test can say what the far end replied and then check
// what it was asked.
type scripted struct {
	mu    sync.Mutex
	ran   []string
	stdin []string
	// fail is per-host: an address in here answers nothing, which is what a
	// server that is off looks like from up here.
	fail map[string]error
	// reply matches on a substring of the command line; the first hit wins.
	reply []reply
}

type reply struct {
	match string
	out   string
	err   error
	once  bool
	used  bool
}

func (s *scripted) answer(match, out string, err error) {
	s.reply = append(s.reply, reply{match: match, out: out, err: err})
}

// answerOnce answers the next matching command and then steps aside, so a test
// can say "this is what it said before, and this is what it says after".
func (s *scripted) answerOnce(match, out string, err error) {
	s.reply = append(s.reply, reply{match: match, out: out, err: err, once: true})
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Run(t sshx.Target, c sshx.Command) error {
	if err, dead := s.fail[t.Addr]; dead {
		return err
	}
	line := strings.Join(c.Remote, " ")
	s.mu.Lock()
	s.ran = append(s.ran, line)
	s.mu.Unlock()
	if c.Stdin != nil {
		data, _ := io.ReadAll(c.Stdin)
		s.mu.Lock()
		s.stdin = append(s.stdin, string(data))
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.reply {
		r := &s.reply[i]
		if !strings.Contains(line, r.match) || (r.once && r.used) {
			continue
		}
		r.used = true
		if r.out != "" && c.Stdout != nil {
			io.WriteString(c.Stdout, r.out)
		}
		return r.err
	}
	return nil
}

func (s *scripted) Open(sshx.Target, sshx.Shell) (sshx.Terminal, error) {
	return nil, errors.New("this backend opens no terminals")
}

func (s *scripted) Forward(sshx.Target, int, int) (sshx.Tunnel, error) {
	return nil, errors.New("this backend carries no ports")
}

// matched reports whether any command matches re.
func (s *scripted) matched(re *regexp.Regexp) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range s.ran {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// did reports whether any command carried needle.
func (s *scripted) did(needle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range s.ran {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// useScripted points the transport at one of these for the length of a test, and
// gives the client an agent binary to upload (a dev build embeds none).
func useScripted(t *testing.T) *scripted {
	t.Helper()
	s := &scripted{fail: map[string]error{}}
	sshx.Use(s)
	t.Cleanup(func() { sshx.Use(nil) })

	fake := filepath.Join(t.TempDir(), "forge-agent-linux-amd64")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho agent\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_AGENT_BIN", fake)
	return s
}

// A host already running this build is left alone. It is the common case once
// the installer does this on every upgrade, and it is what makes "make sure they
// agree" something you can run without thinking about it.
func TestUpdatingAHostThatIsAlreadyCurrentUploadsNothing(t *testing.T) {
	s := useScripted(t)
	s.answer("forge-agent version", fmt.Sprintf(`{"version":%q}`, version.String()), nil)
	if _, err := AddHost("root@1.2.3.4", "srv-current-test", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveHost("srv-current-test") })

	u, err := UpdateAgent("srv-current-test")
	if err != nil {
		t.Fatal(err)
	}
	if u.Changed {
		t.Errorf("reported a change: %+v", u)
	}
	if s.did("cat > ") || s.did("install -m") {
		t.Errorf("uploaded to a host that was already current: %v", s.ran)
	}
}

// And one that is behind gets this build, staged and renamed into place rather
// than copied over itself — a binary that is executing cannot be opened for
// writing, and the agent runs on every poll the UI makes.
func TestUpdatingAHostThatIsBehindReplacesTheAgentByRename(t *testing.T) {
	s := useScripted(t)
	// Old before the swap, this build after it: the scripted answers change in
	// the order they are asked, which is what the operation is checking too.
	s.answerOnce("forge-agent version", `{"version":"v0.0.1 (old)"}`, nil)
	s.answer("forge-agent version", fmt.Sprintf(`{"version":%q}`, version.String()), nil)
	s.answer("uname -m", "x86_64\n", nil)
	if _, err := AddHost("root@1.2.3.4", "srv-behind-test", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveHost("srv-behind-test") })

	u, err := UpdateAgent("srv-behind-test")
	if err != nil {
		t.Fatal(err)
	}
	if u.Err != nil {
		t.Fatal(u.Err)
	}
	if u.Now != version.String() {
		t.Errorf("Now = %q, want what the host reported after the swap", u.Now)
	}
	if !u.Changed || u.Was != "v0.0.1 (old)" {
		t.Errorf("update = %+v, want a change away from the old build", u)
	}
	if !s.did("cat > /tmp/forge-agent.") {
		t.Errorf("nothing was uploaded: %v", s.ran)
	}
	// Renamed into place from a staging name of this run's own — never a fixed
	// one, which two clients updating the same host would fight over.
	staged := regexp.MustCompile(`mv ` + agentPath + `\.[0-9a-f]{12} ` + agentPath + `\b`)
	if !s.matched(staged) {
		t.Errorf("the binary was not renamed into place from a per-run staging file: %v", s.ran)
	}
	// And nothing of ours is left behind, whichever way the install went.
	if !s.did("rm -f /tmp/forge-agent.") {
		t.Errorf("the upload was not cleaned up: %v", s.ran)
	}
	// The upload must be the agent itself, not an empty stream.
	if len(s.stdin) == 0 || !strings.Contains(s.stdin[0], "echo agent") {
		t.Errorf("the uploaded bytes were not the agent: %q", s.stdin)
	}
	// It provisions nothing. This is the whole reason it is not `host prepare`.
	for _, forbidden := range []string{"apt-get", "get.docker.com", "iptables", "ssh-keygen", "sshd"} {
		if s.did(forbidden) {
			t.Errorf("it ran %q — updating the agent installs nothing else", forbidden)
		}
	}
}

// An agent too old to name itself is still replaced: the version verb arrived
// with this feature, so "it cannot answer" is the loudest possible sign that it
// is behind.
func TestAnAgentTooOldToNameItselfIsReplaced(t *testing.T) {
	s := useScripted(t)
	s.answerOnce("forge-agent version", `{"error":"unknown op \"version\""}`, errors.New("exit 1"))
	s.answer("forge-agent version", fmt.Sprintf(`{"version":%q}`, version.String()), nil)
	s.answer("uname -m", "aarch64\n", nil)
	if _, err := AddHost("root@1.2.3.4", "srv-ancient-test", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveHost("srv-ancient-test") })

	u, err := UpdateAgent("srv-ancient-test")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Changed {
		t.Errorf("update = %+v, want it replaced", u)
	}
	if u.Was != "" {
		t.Errorf("Was = %q, want it empty for an agent that could not say", u.Was)
	}
}

// One host being unreachable must not hide the others: the installer runs this
// across every host it knows, and a server that is off is a line of output, not
// a failed install.
func TestOneUnreachableHostDoesNotStopTheRest(t *testing.T) {
	s := useScripted(t)
	s.answer("forge-agent version", fmt.Sprintf(`{"version":%q}`, version.String()), nil)
	// One host is simply off. Everything to that address fails, including the
	// version question this starts with.
	s.fail["5.6.7.8"] = errors.New("connection refused")

	for addr, alias := range map[string]string{"1.2.3.4": "srv-up-test", "5.6.7.8": "srv-down-test"} {
		if _, err := AddHost("root@"+addr, alias, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = RemoveHost(alias) })
	}

	ups, err := UpdateAgents()
	if err != nil {
		t.Fatalf("one host's failure became everybody's: %v", err)
	}

	got := map[string]AgentUpdate{}
	for _, u := range ups {
		got[u.Host] = u
	}
	if u, ok := got["srv-up-test"]; !ok || u.Err != nil {
		t.Errorf("the reachable host was not brought into step: %+v", u)
	}
	if u, ok := got["srv-down-test"]; !ok || u.Err == nil {
		t.Errorf("the host that was off is reported as fine: %+v", u)
	}
}
