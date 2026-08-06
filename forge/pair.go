package forge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Marb-AI/forge/config"
)

// Pairing a second device.
//
// Two things have to cross, and they go in opposite directions. The new device's
// public key has to reach the servers, which only a device already on them can
// arrange. The list of servers has to reach the new device, which has no way to
// discover it — the addresses, the logins and the routes through bastions are
// not written on any of the machines in a form a stranger could ask for.
//
// So pairing is: the new device says its key, the old device lets it in
// everywhere and hands back where "everywhere" is.
//
// # Why there is no server in this
//
// A central place to keep the host list would make this one step instead of two,
// and it would tie together machines that have nothing to do with each other: its
// being down would stop work on every server, none of which needed it. The list
// is small, it changes rarely, and it is exactly the thing a pairing step is
// already carrying.

// Pairing is what an already-paired device hands to a new one: the servers it
// knows, and nothing else.
//
// Not the device key, which never leaves the device that made it — the new one
// has its own, and that is the whole point of asking for its public half rather
// than copying anything. Not the workspaces either: the hosts know those now, so
// a device with this list can ask them itself.
type Pairing struct {
	Hosts []config.Host `json:"hosts"`
}

// LetDeviceIn puts another device's public key on every registered server, and
// returns the servers it can now reach.
//
// The key goes to the host login and to every workspace the host records as
// Forge's — see the agent's authorize-key for why both. A host that could not be
// reached is an error naming it rather than a shorter list: a device told it was
// paired, which then cannot open half its workspaces, has been told something
// untrue.
func LetDeviceIn(pubkey string) (Pairing, error) {
	key := strings.TrimSpace(pubkey)
	if key == "" {
		return Pairing{}, fmt.Errorf("no key to let in")
	}
	if strings.ContainsAny(key, "\n\r") {
		return Pairing{}, fmt.Errorf("a key is one line; this is more than one")
	}
	cfg, err := loadConfig()
	if err != nil {
		return Pairing{}, err
	}
	if len(cfg.Hosts) == 0 {
		return Pairing{}, fmt.Errorf("no servers to let it onto (see: forge host list)")
	}

	enc := base64.StdEncoding.EncodeToString([]byte(key))
	failed := askHosts(cfg.Hosts, everyHost(cfg),
		func(_ string, host *config.Host) (error, error) {
			// The failure is the value, not the error: askHosts drops errors, and
			// here every host's answer is wanted — including "no".
			return callAgent(host, nil, "authorize-key", "-key", enc), nil
		})

	var refused []string
	for alias, err := range failed {
		if err != nil {
			refused = append(refused, fmt.Sprintf("%s (%v)", alias, err))
		}
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return Pairing{}, fmt.Errorf("could not let it onto %s — pair again once they answer, "+
			"which will finish the ones that worked as well", strings.Join(refused, ", "))
	}
	return Pairing{Hosts: hostList(cfg)}, nil
}

// hostList is every registered server, in a stable order so two pairings of the
// same setup produce the same text.
func hostList(cfg *config.Config) []config.Host {
	out := make([]config.Host, 0, len(cfg.Hosts))
	for alias, h := range cfg.Hosts {
		if h == nil {
			continue
		}
		c := *h
		c.Alias = alias // the map key is the truth; the field may predate it
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// Encode turns a pairing into one line of text, which is what a person can carry
// between two machines — and what a QR code will hold when there is a camera to
// read it with.
func (p Pairing) Encode() (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// AcceptPairing records the servers in a pairing and reports what it learned and
// what it already had.
//
// A host this device already knows is left exactly as it is. The two may
// disagree — a jump route added here, an address changed there — and this is not
// the moment to decide which is right: pairing is how a device is told about
// servers it has never heard of, and overwriting what somebody set on this
// machine would make it something else.
func AcceptPairing(encoded string) (added, known []string, err error) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, nil, fmt.Errorf("this is not a pairing: %w", err)
	}
	var p Pairing
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, nil, fmt.Errorf("this is not a pairing: %w", err)
	}
	if len(p.Hosts) == 0 {
		return nil, nil, fmt.Errorf("the pairing names no servers")
	}
	for _, h := range p.Hosts {
		if h.Alias == "" || h.Addr == "" || h.User == "" {
			return nil, nil, fmt.Errorf("the pairing names a server without an alias, address or login")
		}
	}

	err = updateConfig(func(c *config.Config) error {
		if c.Hosts == nil {
			c.Hosts = map[string]*config.Host{}
		}
		for _, h := range p.Hosts {
			if _, have := c.Hosts[h.Alias]; have {
				known = append(known, h.Alias)
				continue
			}
			host := h
			c.Hosts[h.Alias] = &host
			added = append(added, h.Alias)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(added)
	sort.Strings(known)
	return added, known, nil
}
