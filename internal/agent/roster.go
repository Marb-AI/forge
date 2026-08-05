package agent

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// Which workspaces on this host are Forge's.
//
// Today that answer lives in one laptop's config.json, and it is the reason a
// second device shows an empty Forge: the host's own directory holds every
// account under /home/workspaces — a colleague's, one made by hand — and nothing
// on the machine says which of them Forge created. So the list of workspaces has
// had to come from whichever computer made them.
//
// That is the thing standing between Forge and a phone, and it is not a
// synchronisation problem. The record belongs to the machine the workspaces are
// on: it is the only place that is true for everyone who looks, it is there when
// the laptop is shut, and two devices cannot disagree with it.
//
// # Where it lives, and why root owns it
//
// /etc/forge, beside the host-wide git identity and gh credential that
// `host prepare` already puts there. Root-owned and root-writable only, which is
// a boundary rather than tidiness: a workspace user who could add a name to this
// file could have Forge treat any account on the box as a workspace of its own —
// open a shell as it, tunnel its ports, hand it a Claude session. The agent runs
// as root and is the only thing that writes here.

// rosterPath is the file, under the directory `host prepare` already owns.
func rosterPath() string { return filepath.Join(hostKeyDir, "workspaces.json") }

// rosterLockPath is what serialises the read-modify-write, and it is a second
// file rather than the record itself: the record is replaced by rename, so a
// lock held on it would be a lock on an inode nobody is using by the time the
// next writer looks.
func rosterLockPath() string { return filepath.Join(hostKeyDir, "workspaces.lock") }

// withRosterLock runs fn with this host's record held against every other agent
// on the machine.
//
// Two agents run at once whenever two things happen at once — a workspace being
// created from a laptop while another is deleted from the browser is two SSH
// sessions and two processes — and each of them reads the record, changes its
// copy and writes it back. Atomic replacement makes the loser's write whole; it
// does not make it correct. Without this the workspace one of them recorded is
// simply gone.
func withRosterLock(fn func() error) error {
	if err := os.MkdirAll(hostKeyDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(rosterLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	// Released by the close above as well, but said here because the order
	// matters: fn's rename must land before anybody else reads.
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// roster is what that file holds: the names Forge created here, and nothing
// else. A list rather than a map because it is a set of names and JSON has no
// sets; sorted on the way out so the file is stable and a diff of it means
// something.
type roster struct {
	Workspaces []string `json:"workspaces"`
}

// readRoster loads the record. A host that has never had one is not an error:
// it is every host that existed before this, and the answer for it is "none
// recorded", which the client's own config still covers.
func readRoster() (roster, error) {
	var r roster
	data, err := os.ReadFile(rosterPath())
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	return r, nil
}

// readRosterIfAny is readRoster plus the one thing a client cannot infer from
// its contents: whether this host keeps a record at all. An empty record and no
// record read the same otherwise, and they mean opposite things.
func readRosterIfAny() (roster, bool, error) {
	r, err := readRoster()
	if err != nil {
		return r, false, err
	}
	_, statErr := os.Stat(rosterPath())
	return r, statErr == nil, nil
}

// writeRoster replaces the record, atomically. Callers hold the lock.
//
// The temporary file is named by the OS rather than fixed: a fixed one is shared
// by every agent on the host, so two of them would write into the same file and
// rename each other's half into place. The lock makes that unlikely; a unique
// name makes it impossible, which is the difference worth having in the file
// that decides what Forge owns.
func writeRoster(r roster) error {
	sort.Strings(r.Workspaces)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hostKeyDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(hostKeyDir, "workspaces-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename below has taken it

	// 0600: nothing but root has business reading which accounts Forge owns, and
	// CreateTemp's own 0600 is not promised by anything but its documentation.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	// Closed before the rename, not after: renaming a file still open for writing
	// publishes it with whatever has not been flushed still missing.
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), rosterPath())
}

// recordWorkspace adds a name to the record if it is not already there.
//
// Idempotent, because the two callers both have reason to say it twice: creation
// can be retried after a failure further down, and adopting an existing
// workspace is a client repeating what it already believes.
func recordWorkspace(name string) error {
	return withRosterLock(func() error {
		r, err := readRoster()
		if err != nil {
			return err
		}
		if slices.Contains(r.Workspaces, name) {
			return nil
		}
		r.Workspaces = append(r.Workspaces, name)
		return writeRoster(r)
	})
}

// forgetWorkspace removes a name. Also idempotent: a delete of something never
// recorded is a delete that has already happened, as far as this file is
// concerned.
func forgetWorkspace(name string) error {
	return withRosterLock(func() error {
		r, err := readRoster()
		if err != nil {
			return err
		}
		i := slices.Index(r.Workspaces, name)
		if i < 0 {
			return nil
		}
		r.Workspaces = slices.Delete(r.Workspaces, i, i+1)
		return writeRoster(r)
	})
}

// opAdopt records a workspace this host already has.
//
// The migration, and the only way the record can be filled in for a workspace
// that predates it: the host cannot tell its own accounts apart, so the client
// that made them has to say which are its. Refused for a name with no workspace
// behind it — a record naming accounts that do not exist would be worse than an
// empty one, because it would be believed.
func opAdopt(args []string) int {
	fs := flag.NewFlagSet("workspace-adopt", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return emitError("invalid workspace name %q", *name)
	}
	if _, err := os.Stat(filepath.Join(baseDir, *name)); err != nil {
		return emitError("no workspace %q on this host", *name)
	}
	if err := recordWorkspace(*name); err != nil {
		return emitError("record %q: %v", *name, err)
	}
	return emit(agentproto.OK{OK: true})
}
