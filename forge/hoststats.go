package forge

import (
	"sort"
	"strings"
	"sync"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
)

// HostStat is one registered server's resource usage, for the panel under the
// file tree.
//
// A host that could not be measured still gets a row — Reachable false, Note
// saying why. Dropping it would make a server that went down look like one you
// never registered, and the moment a server stops answering is exactly when you
// want to see it in the list.
type HostStat struct {
	Host      string `json:"host"` // the alias, as registered
	Addr      string `json:"addr"`
	Reachable bool   `json:"reachable"`
	// Note is the few words the row itself shows when a host could not be measured
	// ("unreachable"); Detail is the longer version for the tooltip, including what
	// to do about it. Two fields because the row is a sidebar wide — a note long
	// enough to explain the problem squeezes out the name of the server having it.
	Note   string `json:"note,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Zero means "not measured" for each of these — no real host has zero cores or
	// zero bytes of RAM — so the browser can show "—" instead of a confident 0%.
	CPUPercent float64 `json:"cpu_percent"`
	CPUCores   int     `json:"cpu_cores"`
	MemTotal   uint64  `json:"mem_total"`
	MemUsed    uint64  `json:"mem_used"`
	DiskPath   string  `json:"disk_path"`
	DiskTotal  uint64  `json:"disk_total"`
	DiskUsed   uint64  `json:"disk_used"`
	Uptime     int64   `json:"uptime"`
}

// HostStats measures every registered server — CPU, memory, disk — for the
// browser's servers panel.
//
// Every host is asked, not just the ones we keep workspaces on. The panel is a
// view of the machines you own; a server sitting empty still has a disk that can
// fill up, and it is registered precisely so you can put something on it.
//
// The hosts are asked concurrently. Serially, the panel would be as slow as the
// sum of every server's SSH handshake, and one unreachable host would hold the
// whole list up for the full connect timeout — with a fan-out, the slowest host
// costs what the slowest host costs and no more.
func HostStats() ([]HostStat, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	aliases := make([]string, 0, len(cfg.Hosts))
	for alias, h := range cfg.Hosts {
		if h != nil {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)

	out := make([]HostStat, len(aliases))
	var wg sync.WaitGroup
	for i, alias := range aliases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = hostStat(alias, cfg.Hosts[alias])
		}()
	}
	wg.Wait()
	return out, nil
}

// hostStat measures one server. A failure is part of the answer, not an error:
// the row says the machine could not be measured and why, which is worth more
// than its absence.
func hostStat(alias string, h *config.Host) HostStat {
	stat := HostStat{Host: alias, Addr: h.Addr}
	var res agentproto.HostStats
	if err := callAgent(h, &res, "host-stats"); err != nil {
		stat.Note, stat.Detail = statsNote(err)
		return stat
	}
	stat.Reachable = true
	stat.CPUPercent, stat.CPUCores = res.CPUPercent, res.CPUCores
	stat.MemTotal, stat.MemUsed = res.MemTotal, res.MemUsed
	stat.DiskPath, stat.DiskTotal, stat.DiskUsed = res.DiskPath, res.DiskTotal, res.DiskUsed
	stat.Uptime = res.Uptime
	return stat
}

// statsNote turns the failure into the shortest true thing the panel can say, and
// the longer version its tooltip can afford.
//
// The case worth telling apart is a host whose forge-agent predates this feature:
// the machine is up and perfectly healthy, it just doesn't know the question, and
// "unreachable" would send you looking for a network problem that isn't there.
// The agent answers an op it doesn't know with `unknown op "host-stats"`.
func statsNote(err error) (note, detail string) {
	if strings.Contains(err.Error(), "unknown op") {
		return "agent too old", "forge-agent on this server predates host stats — re-run `forge host prepare` to update it"
	}
	return "unreachable", "could not ask this server: " + err.Error()
}
