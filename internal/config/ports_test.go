package config

import (
	"reflect"
	"testing"
	"time"
)

func TestPortRangeOrDefaults(t *testing.T) {
	c := &Config{}
	want := PortRange{Start: DefaultPortStart, End: DefaultPortEnd, Block: DefaultPortBlock}
	if got := c.PortRangeOr(); got != want {
		t.Errorf("empty config = %+v, want %+v", got, want)
	}
	// Partially configured is completed, not rejected: `forge ports range` can set
	// the span without restating the block size.
	c.PortRange = PortRange{Start: 20000, End: 21000}
	if got := c.PortRangeOr(); got.Block != DefaultPortBlock || got.Start != 20000 {
		t.Errorf("partial = %+v", got)
	}
	c.PortRange = PortRange{Block: 50}
	if got := c.PortRangeOr(); got.Block != 50 || got.Start != DefaultPortStart {
		t.Errorf("block only = %+v", got)
	}
}

func TestPortRangeBlocks(t *testing.T) {
	// Blocks are contiguous and non-overlapping: an off-by-one puts two
	// neighbouring workspaces on the same port.
	r := PortRange{Start: 16000, End: 16299, Block: 100}
	if got, want := r.Blocks(), []int{16000, 16100, 16200}; !reflect.DeepEqual(got, want) {
		t.Errorf("blocks = %v, want %v", got, want)
	}

	// A tail too short for a whole block is not a block — a workspace with fewer
	// ports than its neighbours would be a surprise nobody asked for.
	r = PortRange{Start: 16000, End: 16250, Block: 100}
	if got, want := r.Blocks(), []int{16000, 16100}; !reflect.DeepEqual(got, want) {
		t.Errorf("short tail = %v, want %v", got, want)
	}

	// Exactly one block fits.
	r = PortRange{Start: 16000, End: 16099, Block: 100}
	if got, want := r.Blocks(), []int{16000}; !reflect.DeepEqual(got, want) {
		t.Errorf("exact fit = %v, want %v", got, want)
	}

	// Nothing fits.
	r = PortRange{Start: 16000, End: 16098, Block: 100}
	if got := r.Blocks(); len(got) != 0 {
		t.Errorf("too small = %v, want none", got)
	}

	// The defaults are wide enough that running out is not a case anyone hits.
	d := (&Config{}).PortRangeOr()
	if n := len(d.Blocks()); n < 100 {
		t.Errorf("default range holds %d blocks; it is meant to be generous", n)
	}
}

func TestPortRangeSurvivesSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	c := &Config{
		Hosts:      map[string]*Host{},
		Ports:      map[string]map[string][]int{},
		Workspaces: map[string]string{},
		PortRange:  PortRange{Start: 20000, End: 30000, Block: 50},
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.PortRange != c.PortRange {
		t.Errorf("range = %+v, want %+v", got.PortRange, c.PortRange)
	}
}

// A zero or negative block size must not hang: the loop steps by it.
func TestPortRangeBlocksTerminatesOnZeroBlock(t *testing.T) {
	done := make(chan []int, 1)
	go func() { done <- PortRange{Start: 16000, End: 30000, Block: 0}.Blocks() }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("zero block = %v, want none", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Blocks() did not terminate with a zero block size")
	}
	if got := (PortRange{Start: 16000, End: 30000, Block: -100}).Blocks(); len(got) != 0 {
		t.Errorf("negative block = %v, want none", got)
	}
	if got := (PortRange{Start: 0, End: 30000, Block: 100}).Blocks(); len(got) != 0 {
		t.Errorf("zero start = %v, want none", got)
	}
}

func TestPortReservations(t *testing.T) {
	now := time.Now()
	c := &Config{}

	c.ReservePortBlock("crm", "srv", 16000, now)
	c.ReservePortBlock("shop", "srv", 16100, now)
	if got := len(c.ActiveReservations(now)); got != 2 {
		t.Fatalf("active = %d, want 2", got)
	}

	// Reserving again for the same workspace replaces rather than stacks — a
	// retried creation must not leave its first block stranded.
	c.ReservePortBlock("crm", "srv", 16200, now)
	live := c.ActiveReservations(now)
	if len(live) != 2 {
		t.Fatalf("after re-reserve = %d, want 2", len(live))
	}
	for _, r := range live {
		if r.Workspace == "crm" && r.Start != 16200 {
			t.Errorf("crm still holds %d", r.Start)
		}
	}

	c.ReleasePortBlock("crm")
	if got := c.ActiveReservations(now); len(got) != 1 || got[0].Workspace != "shop" {
		t.Errorf("after release = %+v", got)
	}

	// A reservation left behind by a creation that died stops counting, so its
	// block is not stranded forever.
	if got := c.ActiveReservations(now.Add(ReservationTTL + time.Minute)); len(got) != 0 {
		t.Errorf("expired = %+v, want none", got)
	}

	// Releasing something that was never reserved is a no-op, not a panic.
	c.ReleasePortBlock("never-existed")
}

func TestPortReservationsSurviveSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	c := &Config{
		Hosts:      map[string]*Host{},
		Ports:      map[string]map[string][]int{},
		Workspaces: map[string]string{},
	}
	c.ReservePortBlock("crm", "srv", 16000, time.Now())
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// It must survive a round trip through the file: the whole point is that a
	// SECOND process sees the reservation the first one wrote.
	if len(got.PortReservations) != 1 || got.PortReservations[0].Start != 16000 {
		t.Errorf("reservations = %+v", got.PortReservations)
	}
}
