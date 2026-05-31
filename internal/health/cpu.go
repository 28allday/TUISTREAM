// CPU sampling for the Monitor tab. /proc/stat reports cumulative ticks
// per CPU since boot; usage % is the delta of (total - idle) over the
// delta of total between two snapshots.
//
// The Monitor tab samples once per second, holds the previous reading on
// the model, and renders the delta — first sample after startup shows 0
// because there's nothing to subtract against yet.
package health

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// ProcStatLine is one row of /proc/stat (either the "cpu" aggregate or a
// single "cpuN" core). All values are cumulative kernel ticks since boot.
type ProcStatLine struct {
	Index   int // -1 for the aggregate line
	User    uint64
	Nice    uint64
	System  uint64
	Idle    uint64
	IOWait  uint64
	IRQ     uint64
	SoftIRQ uint64
	Steal   uint64
	Total   uint64 // sum of all of the above
	IdleAll uint64 // Idle + IOWait
}

// CPUUsage is the per-tick result: an aggregate % and per-core %s in
// CPU-index order. Empty when we don't yet have a previous sample.
type CPUUsage struct {
	Aggregate float64
	Cores     []float64
}

// ReadProcStat parses /proc/stat. Stops at the first non-cpu line for
// speed (we don't need intr/ctxt/btime/etc).
func ReadProcStat() ([]ProcStatLine, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ProcStatLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		idx := -1
		if fields[0] != "cpu" {
			if n, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu")); err == nil {
				idx = n
			}
		}
		var ps ProcStatLine
		ps.Index = idx
		vals := fields[1:]
		readU64 := func(i int) uint64 {
			if i >= len(vals) {
				return 0
			}
			v, _ := strconv.ParseUint(vals[i], 10, 64)
			return v
		}
		ps.User = readU64(0)
		ps.Nice = readU64(1)
		ps.System = readU64(2)
		ps.Idle = readU64(3)
		ps.IOWait = readU64(4)
		ps.IRQ = readU64(5)
		ps.SoftIRQ = readU64(6)
		ps.Steal = readU64(7)
		for _, v := range vals {
			n, _ := strconv.ParseUint(v, 10, 64)
			ps.Total += n
		}
		ps.IdleAll = ps.Idle + ps.IOWait
		out = append(out, ps)
	}
	return out, sc.Err()
}

// DeltaCPU computes usage between two /proc/stat snapshots. Returns an
// empty CPUUsage if `prev` is empty (first sample after startup).
func DeltaCPU(prev, now []ProcStatLine) CPUUsage {
	if len(prev) == 0 {
		return CPUUsage{}
	}
	prevByIdx := map[int]ProcStatLine{}
	for _, p := range prev {
		prevByIdx[p.Index] = p
	}
	var u CPUUsage
	for _, n := range now {
		p, ok := prevByIdx[n.Index]
		if !ok {
			continue
		}
		pct := cpuPct(p, n)
		if n.Index == -1 {
			u.Aggregate = pct
		} else {
			u.Cores = append(u.Cores, pct)
		}
	}
	return u
}

func cpuPct(prev, now ProcStatLine) float64 {
	if now.Total <= prev.Total {
		return 0
	}
	dTotal := now.Total - prev.Total
	var dIdle uint64
	if now.IdleAll > prev.IdleAll {
		dIdle = now.IdleAll - prev.IdleAll
	}
	if dTotal == 0 {
		return 0
	}
	pct := float64(dTotal-dIdle) / float64(dTotal) * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
