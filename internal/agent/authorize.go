package agent

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// Letting another device in.
//
// A second device has a key of its own — that is the whole of `forge setup` —
// and no way to put it anywhere: it cannot reach the servers, which is precisely
// what it is asking for. The device that is already in has to do it, and this is
// what it calls.
//
// # Two places, not one
//
// The login the agent runs as, and every workspace. They are different accounts
// and both are needed: the admin login is how Forge asks a host anything, and
// the workspaces are where a Claude session, a shell and every tunnelled port
// actually live. A key in only the first produces a Forge that lists everything
// and opens nothing.
//
// That asymmetry is not hypothetical — it is exactly the manual step the
// transport change left behind, where a workspace's authorized_keys was written
// once, when it was created, with whatever key that machine had then.
//
// Only workspaces this host records as Forge's are touched. An account somebody
// else made is not ours to hand out access to, whoever is asking.

// opAuthorizeKey adds a public key to this host's login and to every workspace
// Forge records here.
//
// Idempotent: a key already present is left where it is rather than appended
// again, so pairing a device twice does not grow the file, and re-running it
// after a partial failure finishes the job.
func opAuthorizeKey(args []string) int {
	fs := flag.NewFlagSet("authorize-key", flag.ContinueOnError)
	keyB64 := fs.String("key", "", "base64-encoded SSH public key line")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	raw, err := base64.StdEncoding.DecodeString(*keyB64)
	if err != nil || len(raw) == 0 {
		return emitError("invalid --key")
	}
	key := strings.TrimSpace(string(raw))
	if err := looksLikeAKey(key); err != nil {
		return emitError("%v", err)
	}

	// The login first. If this is the only thing that works, the device can at
	// least reach the host and be told what went wrong with the rest.
	home, owner, err := adminAccount()
	if err != nil {
		return emitError("%v", err)
	}
	if err := authorizeIn(home, owner, key); err != nil {
		return emitError("the host login: %v", err)
	}

	r, err := readRoster()
	if err != nil {
		return emitError("read the workspace record: %v", err)
	}
	var opened []string
	for _, name := range r.Workspaces {
		home := filepath.Join(baseDir, name)
		if _, err := os.Stat(home); err != nil {
			continue // recorded but gone; not this command's problem to report
		}
		if err := authorizeIn(home, name+":"+name, key); err != nil {
			return emitError("workspace %q: %v", name, err)
		}
		opened = append(opened, name)
	}
	return emit(agentproto.AuthorizeResult{Workspaces: opened})
}

// looksLikeAKey rejects anything that is not one authorized_keys line.
//
// Not validation of the key itself — the agent is not going to parse ed25519 —
// but of its shape, because this text is appended to a file that decides who may
// log in. A newline in it would append a second entry nobody asked for, and that
// entry could be anything.
func looksLikeAKey(key string) error {
	if key == "" {
		return fmt.Errorf("the key is empty")
	}
	if strings.ContainsAny(key, "\n\r") {
		return fmt.Errorf("the key spans more than one line, which would authorise more than one key")
	}
	fields := strings.Fields(key)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") &&
		!strings.HasPrefix(fields[0], "sk-") {
		return fmt.Errorf("%q does not look like an authorized_keys line", key)
	}
	return nil
}

// authorizeIn appends the key to one account's authorized_keys, creating the
// file and its directory if they are not there, and leaves the account owning
// what it made.
func authorizeIn(home, owner, key string) error {
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(sshDir, "authorized_keys")

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == key {
			return nil // already in; appending again would only grow the file
		}
	}

	// Appended, never rewritten: this file is how everything else gets in, and a
	// rewrite that failed halfway would lock out whoever was already there —
	// including the device running this command.
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body+key+"\n"), 0o600); err != nil {
		return err
	}
	if err := chownTo(owner, sshDir); err != nil {
		return err
	}
	return nil
}

// chownTo hands a directory to an account, and is a variable so the tests can
// stand in for it: they run on a machine where these accounts do not exist, and
// chown is not something to skip in production. sshd refuses an authorized_keys
// file the account does not own — StrictModes — so a key written without this
// would be a key that silently does not work, which is the failure this whole
// command exists to avoid.
var chownTo = func(owner, dir string) error {
	if out, err := run("chown", "-R", owner, dir); err != nil {
		return fmt.Errorf("chown: %v: %s", err, out)
	}
	return nil
}

// adminAccount is the login a client reaches this host by: its home, and its
// name as chown wants it.
//
// Not $HOME, which is the trap here. On a host whose admin is not root the agent
// runs under sudo, so it is root by the time it asks — and $HOME is root's. A key
// written there would authorise an account nobody connects as, and the device
// being paired would be told it worked and then fail to log in.
//
// SUDO_USER is who sudo was invoked by, which is exactly the login `host prepare`
// registered. Without it this is a root login and root is the answer.
func adminAccount() (home, owner string, err error) {
	name := os.Getenv("SUDO_USER")
	if name == "" {
		u, err := user.Current()
		if err != nil {
			return "", "", fmt.Errorf("cannot tell which account this is: %v", err)
		}
		return u.HomeDir, u.Username + ":" + u.Username, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return "", "", fmt.Errorf("the login %q is not on this host: %v", name, err)
	}
	return u.HomeDir, name + ":" + name, nil
}
