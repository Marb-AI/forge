package forge

// This device's own key, and the one moment it is made.
//
// Everything Forge does to a server it does over SSH, and until now it did that
// with whatever the machine already had: your agent, your ~/.ssh. That is a
// laptop's answer. A phone has neither, and a key that only exists because
// somebody once ran ssh-keygen is a key Forge cannot promise anything about.
//
// So the key is Forge's, made here, and made ONCE — deliberately, by somebody
// who meant to. Nothing creates one as a side effect of needing it: a key that
// servers already trust must never be replaced by a surprise, and "I asked for
// the public half and it quietly generated a new pair" is exactly that surprise.
// Create says whether it made one; every other caller only ever reads.

// Setup gives this device a key if it has none, and hands back the public half
// either way — the single line that goes into a new server's cloud-init, or onto
// one that is already running.
//
// Safe to run again: the second time it makes nothing and reports the same line.
func Setup() (pubkey string, created bool, err error) {
	k, err := Keys()
	if err != nil {
		return "", false, err
	}
	created, err = k.Create()
	if err != nil {
		return "", false, err
	}
	// Read back rather than returned by Create: the public half is derived from
	// the key on disk, so this is the line a server will actually be shown — and
	// on the run that made nothing, it is the line that was already there.
	pubkey, err = k.PublicKey()
	if err != nil {
		return "", created, err
	}
	return pubkey, created, nil
}
