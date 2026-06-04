// Package health gathers a system snapshot for the Monitor tab: drive
// capacities, btrfs RAID error counters, SMART health per physical disk,
// CPU + memory load, and Jellyfin service state.
//
// Every probe is best-effort: if smartctl can't read a USB bridge, or
// btrfs isn't installed yet, or /proc/meminfo changes shape on us, the
// rest of the snapshot still renders. Errors are stuffed into the
// per-row Note field so the UI can show them inline.
package health

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// probeTimeout caps any external health probe. A flaky/half-dead USB bridge
// can make `smartctl` (or `btrfs device stats` on a degraded pool) block
// indefinitely; without a deadline that would freeze the Monitor tab's
// refresh tick. Each probe is best-effort, so on timeout we just report it.
const probeTimeout = 8 * time.Second

// outputWithTimeout runs a command and returns its stdout, killing it (and
// returning a non-nil error) if it doesn't finish within probeTimeout.
func outputWithTimeout(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() == context.DeadlineExceeded {
		return out, ctx.Err()
	}
	return out, err
}

// Snapshot is one point-in-time view of the host. Re-taken on each tick.
type Snapshot struct {
	Time       time.Time
	Capacities []Capacity
	Disks      []DiskHealth
	Btrfs      []BtrfsPool
	System     SystemHealth
	Jellyfin   JellyfinHealth
}

// Capacity is a single mounted filesystem we care about.
type Capacity struct {
	Mountpoint string
	Label      string // friendly name shown to user
	FSType     string
	Total      uint64
	Used       uint64
	Avail      uint64
	PctUsed    float64
	Note       string // populated if statfs fails or returns weird values
}

// DiskHealth is one physical disk's SMART summary.
type DiskHealth struct {
	Device      string // /dev/sda
	Model       string
	Transport   string // sata / usb / nvme
	Standby     bool   // drive was in standby/sleep; we deliberately didn't wake it
	SmartReady  bool
	Passed      bool
	TempC       int
	PowerOnHr   int
	Reallocated int
	Pending     int
	Note        string // e.g. "USB bridge not supported", or smartctl errors
}

// BtrfsPool is one btrfs filesystem mounted on the box (we only care
// about ones we manage). Devices is per physical device in the pool.
type BtrfsPool struct {
	Mountpoint string
	Label      string
	Devices    []BtrfsDevice
	Healthy    bool
	Note       string
}

// BtrfsDevice is one device in a btrfs pool with its persistent error
// counters. Any non-zero counter is suspicious.
type BtrfsDevice struct {
	DevID       int
	Path        string
	WriteErrs   int
	ReadErrs    int
	FlushErrs   int
	CorruptErrs int
	GenErrs     int
	Missing     bool
}

// SystemHealth covers CPU/memory/load.
type SystemHealth struct {
	LoadAvg1   float64
	LoadAvg5   float64
	LoadAvg15  float64
	CPUCount   int
	MemTotalKB uint64
	MemUsedKB  uint64
	MemPct     float64
	UptimeS    int64
}

// JellyfinHealth is just the install + service-active flags. Re-uses
// what the Setup tab already tracks but on its own schedule.
type JellyfinHealth struct {
	Installed   bool
	Active      bool
	ServiceUnit string
}

// Inputs tells Take which mountpoints + disks the host has so we don't
// re-run lsblk inside this package.
type Inputs struct {
	Capacities    []CapacityTarget
	PhysicalDisks []DiskTarget
	BtrfsMounts   []BtrfsTarget
}

type CapacityTarget struct {
	Mountpoint string
	Label      string
	FSType     string
}

type DiskTarget struct {
	Device    string
	Model     string
	Transport string
}

type BtrfsTarget struct {
	Mountpoint string
	Label      string
}

// Take runs every probe and returns the assembled snapshot. Never
// returns an error — failures land in per-row Note fields so the UI can
// degrade gracefully.
func Take(in Inputs) Snapshot {
	s := Snapshot{Time: time.Now()}
	for _, c := range in.Capacities {
		s.Capacities = append(s.Capacities, takeCapacity(c))
	}
	for _, d := range in.PhysicalDisks {
		s.Disks = append(s.Disks, takeDisk(d))
	}
	for _, b := range in.BtrfsMounts {
		s.Btrfs = append(s.Btrfs, takeBtrfs(b))
	}
	s.System = takeSystem()
	s.Jellyfin = takeJellyfin()
	return s
}

// ---------- capacity ----------

func takeCapacity(t CapacityTarget) Capacity {
	c := Capacity{Mountpoint: t.Mountpoint, Label: t.Label, FSType: t.FSType}
	var st syscall.Statfs_t
	if err := syscall.Statfs(t.Mountpoint, &st); err != nil {
		c.Note = "statfs failed: " + err.Error()
		return c
	}
	bs := uint64(st.Bsize)
	c.Total = st.Blocks * bs
	c.Avail = st.Bavail * bs
	c.Used = c.Total - st.Bfree*bs
	if c.Total > 0 {
		c.PctUsed = float64(c.Used) / float64(c.Total) * 100
	}
	return c
}

// ---------- SMART ----------

// smartctlJSON is the subset of `smartctl -j -H -A -i` fields we read.
type smartctlJSON struct {
	ModelName   string `json:"model_name"`
	SmartStatus struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	NVMeLog struct {
		PowerOnHours int `json:"power_on_hours"`
	} `json:"nvme_smart_health_information_log"`
	AtaAttrs struct {
		Table []struct {
			Name string `json:"name"`
			Raw  struct {
				Value int `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	Smartctl struct {
		Messages []struct {
			Severity string `json:"severity"`
			String   string `json:"string"`
		} `json:"messages"`
		ExitStatus int `json:"exit_status"`
	} `json:"smartctl"`
}

func takeDisk(t DiskTarget) DiskHealth {
	d := DiskHealth{Device: t.Device, Model: t.Model, Transport: t.Transport}
	if _, err := exec.LookPath("smartctl"); err != nil {
		d.Note = "smartmontools not installed"
		return d
	}
	// -n standby: if the drive has spun down, do NOT wake it just to read
	// SMART. Without this, every poll spins the drive back up, so it never
	// reaches standby and runs (hot) 24/7. A standby drive is reported as
	// such instead — which is itself a healthy sign.
	args := []string{"-j", "-n", "standby", "-H", "-A", "-i", t.Device}
	// USB bridges often need a device-type hint; without it smartctl
	// bails. Try sat for USB SATA bridges.
	if t.Transport == "usb" {
		args = append([]string{"-d", "sat"}, args...)
	}
	out, err := outputWithTimeout("smartctl", args...)
	if err == context.DeadlineExceeded {
		d.Note = "smartctl timed out (unresponsive USB bridge?)"
		return d
	}
	if len(out) == 0 {
		d.Note = "smartctl produced no output"
		return d
	}
	var parsed smartctlJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		d.Note = "couldn't parse smartctl JSON"
		return d
	}
	// `-n standby` reports a sleeping drive via a message like
	// "Device is in STANDBY mode, exit(2)". Surface that as its own state
	// rather than "SMART not available".
	for _, m := range parsed.Smartctl.Messages {
		up := strings.ToUpper(m.String)
		if strings.Contains(up, "STANDBY") || strings.Contains(up, "SLEEP") {
			d.Standby = true
			d.Note = "in standby — not woken for SMART"
			return d
		}
	}
	if parsed.ModelName != "" && d.Model == "" {
		d.Model = parsed.ModelName
	}
	d.SmartReady = parsed.SmartStatus.Passed || parsed.Temperature.Current > 0 || parsed.PowerOnTime.Hours > 0 || parsed.NVMeLog.PowerOnHours > 0
	d.Passed = parsed.SmartStatus.Passed
	d.TempC = parsed.Temperature.Current
	if parsed.PowerOnTime.Hours > 0 {
		d.PowerOnHr = parsed.PowerOnTime.Hours
	} else {
		d.PowerOnHr = parsed.NVMeLog.PowerOnHours
	}
	for _, attr := range parsed.AtaAttrs.Table {
		switch attr.Name {
		case "Reallocated_Sector_Ct":
			d.Reallocated = attr.Raw.Value
		case "Current_Pending_Sector":
			d.Pending = attr.Raw.Value
		}
	}
	if !d.SmartReady {
		// Pull a useful message out of the smartctl json if it complained.
		for _, m := range parsed.Smartctl.Messages {
			if m.Severity == "error" || m.Severity == "warning" {
				d.Note = strings.TrimSpace(m.String)
				break
			}
		}
		if d.Note == "" {
			d.Note = "SMART not available (USB bridge?)"
		}
	}
	return d
}

// ---------- btrfs ----------

func takeBtrfs(t BtrfsTarget) BtrfsPool {
	p := BtrfsPool{Mountpoint: t.Mountpoint, Label: t.Label, Healthy: true}
	if _, err := exec.LookPath("btrfs"); err != nil {
		p.Note = "btrfs-progs not installed"
		p.Healthy = false
		return p
	}
	// `btrfs device stats <mp>` prints lines like:
	//   [/dev/sda].write_io_errs    0
	out, err := outputWithTimeout("btrfs", "device", "stats", t.Mountpoint)
	if err == context.DeadlineExceeded {
		p.Note = "btrfs device stats timed out (degraded/missing device?)"
		p.Healthy = false
		return p
	}
	if err != nil {
		p.Note = "btrfs device stats failed: " + err.Error()
		p.Healthy = false
		return p
	}
	devs := map[string]*BtrfsDevice{}
	order := []string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "[") {
			continue
		}
		end := strings.Index(line, "]")
		if end < 0 {
			continue
		}
		path := line[1:end]
		rest := strings.TrimSpace(line[end+1:])
		// rest looks like "write_io_errs    0" — split into key + val
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimPrefix(fields[0], ".")
		val, _ := strconv.Atoi(fields[len(fields)-1])
		d, ok := devs[path]
		if !ok {
			d = &BtrfsDevice{Path: path}
			devs[path] = d
			order = append(order, path)
		}
		switch key {
		case "write_io_errs":
			d.WriteErrs = val
		case "read_io_errs":
			d.ReadErrs = val
		case "flush_io_errs":
			d.FlushErrs = val
		case "corruption_errs":
			d.CorruptErrs = val
		case "generation_errs":
			d.GenErrs = val
		}
	}
	for _, path := range order {
		d := devs[path]
		if !deviceExists(path) {
			d.Missing = true
		}
		if d.WriteErrs+d.ReadErrs+d.FlushErrs+d.CorruptErrs+d.GenErrs > 0 || d.Missing {
			p.Healthy = false
		}
		p.Devices = append(p.Devices, *d)
	}
	return p
}

func deviceExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---------- system (cpu + memory + uptime) ----------

func takeSystem() SystemHealth {
	s := SystemHealth{CPUCount: runtime.NumCPU()}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 3 {
			s.LoadAvg1, _ = strconv.ParseFloat(fields[0], 64)
			s.LoadAvg5, _ = strconv.ParseFloat(fields[1], 64)
			s.LoadAvg15, _ = strconv.ParseFloat(fields[2], 64)
		}
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var avail uint64
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			val, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				s.MemTotalKB = val
			case "MemAvailable:":
				avail = val
			}
		}
		if s.MemTotalKB > 0 {
			used := s.MemTotalKB - avail
			s.MemUsedKB = used
			s.MemPct = float64(used) / float64(s.MemTotalKB) * 100
		}
	}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 0 {
			if up, err := strconv.ParseFloat(fields[0], 64); err == nil {
				s.UptimeS = int64(up)
			}
		}
	}
	return s
}

// ---------- jellyfin ----------

func takeJellyfin() JellyfinHealth {
	j := JellyfinHealth{ServiceUnit: "jellyfin.service"}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return j
	}
	if pacmanHasJellyfin() {
		j.Installed = true
	}
	out, _ := exec.Command("systemctl", "is-active", j.ServiceUnit).Output()
	j.Active = strings.TrimSpace(string(out)) == "active"
	return j
}

func pacmanHasJellyfin() bool {
	if _, err := exec.LookPath("pacman"); err != nil {
		return false
	}
	for _, pkg := range []string{"jellyfin-server", "jellyfin", "jellyfin-bin"} {
		err := exec.Command("pacman", "-Qi", pkg).Run()
		if err == nil {
			return true
		}
	}
	return false
}

// ---------- formatting helpers used by the view ----------

// HumanBytes renders a byte count with one decimal for sub-10 values.
// Reusable across the TUI and tests.
func HumanBytes(n uint64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
		tb = 1024 * gb
	)
	switch {
	case n >= tb:
		return fmtBytes(float64(n)/tb, "T")
	case n >= gb:
		return fmtBytes(float64(n)/gb, "G")
	case n >= mb:
		return fmtBytes(float64(n)/mb, "M")
	case n >= kb:
		return fmtBytes(float64(n)/kb, "K")
	}
	return fmt.Sprintf("%dB", n)
}

func fmtBytes(v float64, suffix string) string {
	if v < 10 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + suffix
	}
	return fmt.Sprintf("%.0f", v) + suffix
}

// HumanUptime renders a seconds count as e.g. "3d 4h", "2h 17m", "44m".
func HumanUptime(s int64) string {
	d := s / 86400
	h := (s % 86400) / 3600
	m := (s % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
