package config

import (
	"reflect"
	"testing"
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
