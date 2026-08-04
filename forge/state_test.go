package forge

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/sshx"
	"github.com/Marb-AI/forge/keys"
)

// swapState points the core at dir for the duration of one test and puts the
// previous stores back afterwards. The stores are process-wide, so a test that
// left its own behind would hand every test after it a directory that no longer
// exists.
func swapState(t *testing.T, dir string) {
	t.Helper()
	prevCfg, err := Store()
	if err != nil {
		t.Fatal(err)
	}
	prevKeys, err := Keys()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Use(prevCfg, prevKeys) })
	Open(dir)
}

// The point of the seam: an operation writes where the front end said, not where
// the operation would have guessed.
func TestOpenPointsTheCoreAtADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "container", "forge")
	swapState(t, dir)

	if err := SetUIPort(8099); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("the config was not written where the core was pointed: %v", err)
	}
	port, err := UIPort()
	if err != nil {
		t.Fatal(err)
	}
	if port != 8099 {
		t.Errorf("UIPort() = %d; want the 8099 just written to this store", port)
	}
	if got, err := StateDir(); err != nil || got != dir {
		t.Errorf("StateDir() = %q, %v; want %q", got, err, dir)
	}
}

// The servers this device has accepted are kept with the rest of its state, so a
// core pointed at a container records them in the container — not in a ~/.forge
// the transport decided on by itself.
func TestTheTransportRecordsHostKeysWhereTheCoreWasPointed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "container", "forge")
	swapState(t, dir)

	// Pointing the transport at the store does not resolve it: the directory is
	// still not there, and a Forge that connects to nothing leaves nothing behind.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the state directory exists before anything asked for it: %v", err)
	}

	path, err := sshx.KnownHosts()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "known_hosts"); path != want {
		t.Errorf("the transport records host keys in %q, want %q", path, want)
	}
}

// A store of the caller's own — no filesystem in sight. This is the case the
// whole seam exists for: iOS has no home directory, and a front end there hands
// the core its own answer rather than a path.
func TestUseAcceptsAStoreOfTheCallersOwn(t *testing.T) {
	prevCfg, err := Store()
	if err != nil {
		t.Fatal(err)
	}
	prevKeys, err := Keys()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Use(prevCfg, prevKeys) })

	mem := &memStore{cfg: &config.Config{
		Hosts:      map[string]*config.Host{},
		Ports:      map[string]map[string][]int{},
		Workspaces: map[string]string{},
	}}
	if err := Use(mem, keys.NewFileStore(t.TempDir())); err != nil {
		t.Fatal(err)
	}

	if err := SetUIPort(4242); err != nil {
		t.Fatal(err)
	}
	if mem.cfg.UIPort != 4242 {
		t.Errorf("the port went somewhere other than the store it was given: %+v", mem.cfg)
	}
	// And a store with nowhere to put runtime files says so, rather than the core
	// inventing a directory on its behalf. The transport is handed the same
	// answer: a device that cannot write down the servers it accepted will not
	// accept one on sight.
	if _, err := StateDir(); !errors.Is(err, errNoDir) {
		t.Errorf("StateDir() on a store with no directory = %v; want it refused", err)
	}
	if _, err := sshx.KnownHosts(); !errors.Is(err, errNoDir) {
		t.Errorf("sshx.KnownHosts() on a store with no directory = %v; want it refused", err)
	}
}

// memStore is a Store that is not a directory — the shape a phone's would be.
type memStore struct{ cfg *config.Config }

var errNoDir = errors.New("this store has no directory")

func (m *memStore) Load() (*config.Config, error) { return m.cfg, nil }
func (m *memStore) Dir() (string, error)          { return "", errNoDir }
func (m *memStore) Update(change func(*config.Config) error) error {
	return change(m.cfg)
}

// Half a wiring is the mistake this seam must not swallow: the core would fill
// the gap with ~/.forge, and a front end that got one store wrong would look like
// it was working.
func TestUseRefusesHalfAWiring(t *testing.T) {
	dir := t.TempDir()
	swapState(t, dir)

	for _, c := range []struct {
		name string
		cfg  config.Store
		keys keys.Store
	}{
		{"no config store", nil, keys.NewFileStore(t.TempDir())},
		{"no key store", config.NewFileStore(t.TempDir()), nil},
		{"neither", nil, nil},
	} {
		if err := Use(c.cfg, c.keys); err == nil {
			t.Errorf("Use with %s was accepted", c.name)
		}
	}

	// And a refused Use must leave the stores that were there alone, rather than
	// half-applying itself on the way to the error.
	if got, err := StateDir(); err != nil || got != dir {
		t.Errorf("StateDir() = %q, %v after the refusals; want the store from before, %q", got, err, dir)
	}
}

// The device key defaults alongside the config: one directory, one answer to
// "where does this device keep its things".
func TestKeysDefaultBesideTheConfig(t *testing.T) {
	dir := t.TempDir()
	swapState(t, dir)

	k, err := Keys()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Create(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "id.pem")); err != nil {
		t.Errorf("the key was not written beside the config: %v", err)
	}
}

// Every operation used to resolve ~/.forge for itself, which is why the location
// could not be changed. Now exactly one place does, and this is what keeps it that
// way: a new caller of DefaultDir is a new assumption about where state lives, and
// the next platform breaks it.
func TestOnlyTheCoreResolvesTheDefaultDirectory(t *testing.T) {
	for _, f := range grepRepo(t, "config.DefaultDir(") {
		if f == "forge/state.go" {
			continue
		}
		t.Errorf("%s resolves the default state directory; ask the core for its "+
			"store instead — that is what Open and Use are for", f)
	}
}

// And the same for $HOME itself, which is what DefaultDir is made of. The two
// remaining lookups are not about where Forge keeps its state, and each says so
// where it stands; a third would need the same justification.
func TestHomeIsResolvedOnlyWhereItIsExplained(t *testing.T) {
	allowed := map[string]string{
		"config/config.go":  "DefaultDir, the one place ~/.forge is spelled out",
		"forge/terminal.go": "the local shell's working directory — the user's home, not Forge's",
	}
	for _, f := range grepRepo(t, "os.UserHomeDir(") {
		if _, ok := allowed[f]; !ok {
			t.Errorf("%s resolves the home directory; if that is state, it belongs "+
				"behind a store, and if it is not, say what it is instead", f)
		}
	}
}

// grepRepo returns the repository's non-test Go files that mention needle, as
// paths relative to the module root.
func grepRepo(t *testing.T, needle string) []string {
	t.Helper()
	root := ".."
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "dist" || d.Name() == "bin") {
			return fs.SkipDir
		}
		// Tests are exempt: isolating HOME is exactly what a TestMain is for, and a
		// test that names its own directory is the behaviour these rules ask for.
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), needle) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatalf("nothing in the repository mentions %q — the check has stopped "+
			"matching what it was written for", needle)
	}
	return found
}

// The exec'd ssh cannot be handed bytes, so a store backed by files hands over
// its path as well — asked for rather than assumed, because a Keychain has none
// to give and the question has to be askable of any store.
func TestTheExecdSshIsToldWhereTheKeyIs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	swapState(t, dir)

	if _, _, err := Setup(); err != nil {
		t.Fatal(err)
	}
	args := sshx.AdminTarget(&config.Host{User: "root", Addr: "h", Port: 22}).TTYArgs("id")
	want := filepath.Join(dir, "id.pem")
	if !strings.Contains(strings.Join(args, " "), want) {
		t.Errorf("argv does not point at this device's key (%s): %v", want, args)
	}
}
