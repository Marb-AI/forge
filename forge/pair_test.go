package forge

import (
	"strings"
	"testing"

	"github.com/Marb-AI/forge/config"
)

func aPairing() Pairing {
	return Pairing{Hosts: []config.Host{
		{Alias: "box", User: "root", Addr: "10.0.0.1"},
		{Alias: "behind", User: "dev", Addr: "10.0.1.9", Port: 2222, Jump: "root@bastion"},
	}}
}

// A pairing survives being carried between two machines as one line of text —
// which is what a person can retype, and what a QR code will hold when there is
// a camera to read it with.
func TestAPairingSurvivesBeingCarried(t *testing.T) {
	encoded, err := aPairing().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(encoded, " \n\r+/=") {
		t.Errorf("the pairing has characters that do not survive being pasted: %q", encoded)
	}

	dir := t.TempDir()
	swapState(t, dir)

	added, known, err := AcceptPairing(encoded)
	if err != nil {
		t.Fatalf("accepting a pairing this package made: %v", err)
	}
	if len(known) != 0 {
		t.Errorf("a device that knew nothing already knew %v", known)
	}
	if strings.Join(added, ",") != "behind,box" {
		t.Errorf("learned %v, want both servers", added)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Everything needed to reach a server, including the route through a bastion:
	// a device with no ~/.ssh has nowhere else that could be written down.
	h := cfg.Hosts["behind"]
	if h == nil {
		t.Fatal("the host behind a bastion did not arrive")
	}
	if h.User != "dev" || h.Addr != "10.0.1.9" || h.Port != 2222 || h.Jump != "root@bastion" {
		t.Errorf("it arrived as %+v, missing something it cannot be reached without", *h)
	}
}

// A server this device already knows is left exactly as it is. The two may
// disagree — a jump route added here, an address changed there — and pairing is
// how a device hears about servers it has never met, not how one machine's idea
// of a server replaces another's.
func TestPairingLeavesWhatThisDeviceAlreadyKnows(t *testing.T) {
	dir := t.TempDir()
	swapState(t, dir)

	mine := &config.Host{Alias: "box", User: "root", Addr: "10.0.0.1", Jump: "root@my-bastion"}
	if err := updateConfig(func(c *config.Config) error {
		c.Hosts = map[string]*config.Host{"box": mine}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	encoded, err := aPairing().Encode()
	if err != nil {
		t.Fatal(err)
	}
	added, known, err := AcceptPairing(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(known, ",") != "box" || strings.Join(added, ",") != "behind" {
		t.Errorf("added %v and knew %v; want behind added and box known", added, known)
	}

	cfg, _ := loadConfig()
	if got := cfg.Hosts["box"].Jump; got != "root@my-bastion" {
		t.Errorf("the route this device had was replaced by %q — somebody set that here", got)
	}
}

// Text that is not a pairing is refused rather than half-applied. What arrives
// here was retyped by a person off another screen.
func TestSomethingThatIsNotAPairingIsRefused(t *testing.T) {
	dir := t.TempDir()
	swapState(t, dir)

	noAlias, err := Pairing{Hosts: []config.Host{{User: "root", Addr: "10.0.0.1"}}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := Pairing{}.Encode()
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "not base64 at all!!", "aGVsbG8", noAlias, empty} {
		if _, _, err := AcceptPairing(bad); err == nil {
			t.Errorf("%q was accepted as a pairing", bad)
		}
	}
	cfg, _ := loadConfig()
	if len(cfg.Hosts) != 0 {
		t.Errorf("something was recorded anyway: %v", cfg.Hosts)
	}
}

// The alias a server is filed under is the map key, whatever the record inside
// says. A pairing built from a config where the two disagree would otherwise
// name a server something the sending device does not call it.
func TestTheAliasIsTheOneTheServerIsFiledUnder(t *testing.T) {
	cfg := &config.Config{Hosts: map[string]*config.Host{
		"box": {Alias: "stale-name", User: "root", Addr: "10.0.0.1"},
	}}
	list := hostList(cfg)
	if len(list) != 1 || list[0].Alias != "box" {
		t.Errorf("the pairing calls it %+v, want the key it is filed under", list)
	}
}

// A key that is more than one line is refused before any server is asked: the
// far end appends it to a file that decides who may log in, and the second line
// could be anything.
func TestOnlyOneKeyIsEverLetIn(t *testing.T) {
	dir := t.TempDir()
	swapState(t, dir)

	for _, bad := range []string{
		"",
		"   ",
		"ssh-ed25519 AAAAmine me@laptop\nssh-ed25519 AAAAtheirs them@elsewhere",
	} {
		if _, err := LetDeviceIn(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// An alias with nothing behind it is not a server this device knows.
//
// A nil record is what a hand-edited config leaves, and every other reader here
// already treats it as unreachable rather than present — hostList skips it,
// askHosts counts it as not answering. Pairing is the one operation that could
// fill it in, and calling it "already known" would be the one that does not.
func TestPairingFillsInAnAliasWithNothingBehindIt(t *testing.T) {
	dir := t.TempDir()
	swapState(t, dir)

	if err := updateConfig(func(c *config.Config) error {
		c.Hosts = map[string]*config.Host{"box": nil}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	encoded, err := aPairing().Encode()
	if err != nil {
		t.Fatal(err)
	}
	added, known, err := AcceptPairing(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 0 {
		t.Errorf("an empty record counted as a server this device knows: %v", known)
	}
	if strings.Join(added, ",") != "behind,box" {
		t.Errorf("added %v, want both — box had a name and nothing behind it", added)
	}

	cfg, _ := loadConfig()
	if h := cfg.Hosts["box"]; h == nil || h.Addr != "10.0.0.1" {
		t.Errorf("box is still %v — the one operation that could have healed it did not", h)
	}
}
