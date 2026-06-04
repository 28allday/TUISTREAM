// Monitor tab — a refreshing dashboard of the host's health.
//
// Drives, btrfs pools, SMART, CPU + memory, Jellyfin service. The cheap
// probes refresh every 5 seconds via tea.Tick — but only while this tab is
// visible — and SMART runs on its own slower cadence so polling never keeps
// the drives awake. Re-uses the inventory we already track for the other
// tabs to pick which mountpoints/disks to probe.
package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tuistream/internal/drives"
	"tuistream/internal/health"
	"tuistream/internal/jellyfin"
)

// monitorModel holds the latest health snapshot plus the rolling CPU
// state (sampled on its own faster tick).
type monitorModel struct {
	snap *health.Snapshot
	err  error

	// SMART runs on its own slow cadence (smartRefresh) because polling
	// smartctl keeps drives busy — lastSmart is when we last probed,
	// smartCache carries the disk rows across the cheap refreshes in
	// between.
	lastSmart  time.Time
	smartCache []health.DiskHealth

	// CPU sampling: prev holds the last /proc/stat snapshot so the next
	// tick can compute deltas. usage is the most recent % per core +
	// aggregate. history is a per-core ring buffer used to render the
	// sparkline alongside the bar.
	cpuPrev  []health.ProcStatLine
	cpuUsage health.CPUUsage
	cpuHist  [][]float64
}

const cpuHistoryLen = 30

func newMonitorModel() monitorModel { return monitorModel{} }

// WithHealth injects a snapshot into the model directly. For test use; the
// real flow loads via the async loadHealthCmd.
func (m Model) WithHealth(snap *health.Snapshot) Model {
	m.monitor.snap = snap
	return m
}

// WithCPU injects a CPU usage sample + per-core history into the model.
// For test use.
func (m Model) WithCPU(u health.CPUUsage, history [][]float64) Model {
	m.monitor.cpuUsage = u
	m.monitor.cpuHist = history
	return m
}

// --- async loading ---

type healthLoadedMsg struct {
	snap *health.Snapshot
	err  error
	// withSmart records whether this snapshot included a SMART probe, so
	// the Update loop knows to refresh or reuse the cached disk rows.
	withSmart bool
}

type monitorTickMsg struct{}

const monitorRefresh = 5 * time.Second

// smartRefresh is the SMART-probe cadence. Deliberately much slower than
// monitorRefresh: SMART data barely changes second-to-second, and hammering
// smartctl keeps drives awake (and hot). Cheap probes (statfs, /proc,
// systemd) stay on the 5s tick.
const smartRefresh = 60 * time.Second

func monitorTickCmd() tea.Cmd {
	return tea.Tick(monitorRefresh, func(time.Time) tea.Msg {
		return monitorTickMsg{}
	})
}

// Fast tick for CPU only — cheap, no shell-outs.
type cpuTickMsg struct{}

const cpuRefresh = 1 * time.Second

func cpuTickCmd() tea.Cmd {
	return tea.Tick(cpuRefresh, func(time.Time) tea.Msg {
		return cpuTickMsg{}
	})
}

type cpuSampleMsg struct {
	sample []health.ProcStatLine
	usage  health.CPUUsage
}

// cpuSampleCmd reads /proc/stat in the background and returns a delta
// against prev. First sample (prev nil) returns empty usage — the next
// tick produces real numbers.
func cpuSampleCmd(prev []health.ProcStatLine) tea.Cmd {
	return func() tea.Msg {
		now, err := health.ReadProcStat()
		if err != nil {
			return cpuSampleMsg{}
		}
		return cpuSampleMsg{sample: now, usage: health.DeltaCPU(prev, now)}
	}
}

// loadHealthCmd derives the health Inputs from the current inventory and
// kicks off a snapshot in the background. If inventory hasn't loaded
// yet we still take a snapshot — system/jellyfin/cpu sections work
// without it. withSmart=false drops the physical disks from the probe so
// smartctl isn't run; the Update loop re-attaches the cached rows.
func loadHealthCmd(inv *drives.Inventory, withSmart bool) tea.Cmd {
	in := healthInputsFrom(inv)
	if !withSmart {
		in.PhysicalDisks = nil
	}
	return func() tea.Msg {
		s := health.Take(in)
		return healthLoadedMsg{snap: &s, withSmart: withSmart}
	}
}

func healthInputsFrom(inv *drives.Inventory) health.Inputs {
	if inv == nil {
		return health.Inputs{}
	}
	var in health.Inputs
	seenDisk := map[string]bool{}
	for _, d := range inv.All {
		// Capacities: every mounted filesystem we might care about.
		if d.MountPoint != "" {
			in.Capacities = append(in.Capacities, health.CapacityTarget{
				Mountpoint: d.MountPoint,
				Label:      capacityLabel(d),
				FSType:     d.FSType,
			})
		}
		// btrfs pools (managed-ours only; others belong to the user
		// or are system).
		if d.FSType == "btrfs" && d.Role == drives.RoleManagedOurs {
			label := d.Label
			if label == "" {
				label = filepath.Base(d.MountPoint)
			}
			in.BtrfsMounts = append(in.BtrfsMounts, health.BtrfsTarget{
				Mountpoint: d.MountPoint,
				Label:      label,
			})
		}
		// Physical disks (real spinning rust / SSDs only — skip
		// zram, loop, dm-mapper).
		if d.Type == "disk" && !skipDiskForSmart(d.Name) && !seenDisk[d.Name] {
			seenDisk[d.Name] = true
			in.PhysicalDisks = append(in.PhysicalDisks, health.DiskTarget{
				Device:    d.Path,
				Model:     d.Model,
				Transport: d.Transport,
			})
		}
	}
	return in
}

func skipDiskForSmart(name string) bool {
	return drives.IsPseudoDisk(name)
}

func capacityLabel(d drives.Drive) string {
	if d.Label != "" {
		return d.Label
	}
	switch d.MountPoint {
	case "/":
		return "system"
	case "/boot":
		return "boot"
	}
	return filepath.Base(d.MountPoint)
}

// --- view ---

func (mn monitorModel) view(m Model) string {
	if mn.err != nil {
		return centeredCard(
			titleStyle.Render("Couldn't read system health") + "\n\n" +
				mn.err.Error(),
		)
	}
	if mn.snap == nil {
		return centeredCard(headerStyle.Render("Loading health snapshot…"))
	}
	snap := mn.snap

	heading := titleStyle.Render("System health")
	subhead := headerStyle.Render(
		"Refreshing every " + monitorRefresh.String() +
			" · SMART every " + smartRefresh.String() +
			" · last update " + snap.Time.Format("15:04:05"))

	w := cardWidth(m.width)
	return lipgloss.JoinVertical(lipgloss.Center,
		"",
		centered(heading, w),
		centered(subhead, w),
		"",
		renderCPUCard(mn.cpuUsage, mn.cpuHist, snap.System.CPUCount),
		"",
		renderSystemCard(snap),
		"",
		renderCapacityCard(snap),
		"",
		renderBtrfsCard(snap),
		"",
		renderDiskCard(snap),
	)
}

// renderCPUCard is the live activity visualizer: aggregate bar at top,
// per-core row of "Cn  XX%  ▇▇▇▇░░░░  ▁▂▃▄▅▆▇█▇▆▅▄▃▂▁" — % readout,
// 12-cell bar, 30-sample sparkline. Refreshed every 1 second.
func renderCPUCard(u health.CPUUsage, history [][]float64, cores int) string {
	rows := []string{titleStyle.Render("CPU activity") + headerStyle.Render(
		fmt.Sprintf("   — %d cores · sampled every %s", cores, cpuRefresh))}
	rows = append(rows, "")
	if u.Aggregate == 0 && len(u.Cores) == 0 {
		rows = append(rows, headerStyle.Render(
			"Waiting for second sample…  (first reading uses no prior delta)"))
		return centeredFrame(strings.Join(rows, "\n"))
	}
	// Aggregate at the top, wider bar.
	rows = append(rows, "  "+padRight("ALL", 3)+"  "+pctColumn(u.Aggregate)+
		"  "+coloredBar(u.Aggregate, 28))
	if len(u.Cores) > 0 {
		rows = append(rows, "")
	}
	for i, pct := range u.Cores {
		label := fmt.Sprintf("C%d", i)
		var hist []float64
		if i < len(history) {
			hist = history[i]
		}
		rows = append(rows, "  "+padRight(label, 3)+"  "+pctColumn(pct)+
			"  "+coloredBar(pct, 12)+"  "+sparkline(hist, cpuHistoryLen))
	}
	return centeredFrame(strings.Join(rows, "\n"))
}

// humanAge turns a SMART power-on-hours count into a readable drive age:
// years (1 decimal) for ≥1 year, days for ≥2 days, else raw hours. A drive
// reporting 70466 hours reads as "8.0y" instead of an opaque "70466h".
func humanAge(hours int) string {
	switch {
	case hours <= 0:
		return "—"
	case hours >= 8766: // 365.25 * 24
		return fmt.Sprintf("%.1fy", float64(hours)/8766)
	case hours >= 48:
		return fmt.Sprintf("%dd", hours/24)
	default:
		return fmt.Sprintf("%dh", hours)
	}
}

// pctColumn renders a 5-char "  XX%" column, colored against load.
func pctColumn(pct float64) string {
	style := roleAvailableStyle
	switch {
	case pct >= 90:
		style = roleSystemStyle
	case pct >= 70:
		style = roleInUseStyle
	}
	return style.Render(fmt.Sprintf("%4.0f%%", pct))
}

// coloredBar is the inline % bar used by the CPU card. Same colour
// bucket logic as the capacity bar.
func coloredBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100 * float64(width))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	style := roleAvailableStyle
	switch {
	case pct >= 90:
		style = roleSystemStyle
	case pct >= 70:
		style = roleInUseStyle
	}
	return style.Render(bar)
}

// sparkline renders up to `max` samples as a single line of unicode
// block elements. Missing samples render as a space so the line stays
// the same width as it fills up. Anything non-zero shows at least the
// smallest block so a mostly-idle box still has a visible trace.
func sparkline(history []float64, max int) string {
	const blocks = " ▁▂▃▄▅▆▇█"
	chars := []rune(blocks)
	out := make([]rune, max)
	for i := range out {
		out[i] = ' '
	}
	start := 0
	if len(history) > max {
		start = len(history) - max
	}
	off := max - (len(history) - start)
	for i, v := range history[start:] {
		var idx int
		switch {
		case v <= 0:
			idx = 0
		case v >= 100:
			idx = len(chars) - 1
		default:
			// Bucket 0 is "no activity"; anything > 0 must show at
			// least block 1 so the trace is visible at low load.
			idx = int(v/12.5) + 1
			if idx >= len(chars) {
				idx = len(chars) - 1
			}
		}
		out[off+i] = chars[idx]
	}
	return string(out)
}

func renderSystemCard(s *health.Snapshot) string {
	jVerb := roleSystemStyle.Render("not installed")
	switch {
	case s.Jellyfin.Active:
		jVerb = roleAvailableStyle.Render("active")
	case s.Jellyfin.Installed:
		jVerb = roleInUseStyle.Render("installed, stopped")
	}

	loadCol := func(v float64) string {
		// Colour a load average against CPU count: green if <= cores,
		// red if > 2x cores.
		cpu := float64(s.System.CPUCount)
		switch {
		case v >= 2*cpu:
			return roleSystemStyle.Render(fmt.Sprintf("%.2f", v))
		case v >= cpu:
			return roleInUseStyle.Render(fmt.Sprintf("%.2f", v))
		}
		return roleAvailableStyle.Render(fmt.Sprintf("%.2f", v))
	}

	memColour := roleAvailableStyle
	switch {
	case s.System.MemPct >= 90:
		memColour = roleSystemStyle
	case s.System.MemPct >= 75:
		memColour = roleInUseStyle
	}

	rows := []string{titleStyle.Render("System")}
	rows = append(rows,
		fmt.Sprintf("Load avg:   %s · %s · %s   over %d cores",
			loadCol(s.System.LoadAvg1),
			loadCol(s.System.LoadAvg5),
			loadCol(s.System.LoadAvg15),
			s.System.CPUCount))
	rows = append(rows,
		fmt.Sprintf("Memory:     %s used / %s total   (%s)",
			health.HumanBytes(s.System.MemUsedKB*1024),
			health.HumanBytes(s.System.MemTotalKB*1024),
			memColour.Render(fmt.Sprintf("%.0f%%", s.System.MemPct))))
	rows = append(rows,
		fmt.Sprintf("Uptime:     %s", health.HumanUptime(s.System.UptimeS)))
	rows = append(rows, "Jellyfin:   "+jVerb)
	if s.Jellyfin.Active {
		for i, u := range jellyfin.WebURLs() {
			label := "Web URL:"
			if i > 0 {
				label = ""
			}
			rows = append(rows, fmt.Sprintf("%-11s %s", label, devNameStyle.Render(u)))
		}
	}
	return centeredFrame(strings.Join(rows, "\n"))
}

func renderCapacityCard(s *health.Snapshot) string {
	rows := []string{titleStyle.Render("Disk space")}

	// Hide rows we can't measure (swap, failed statfs) — they're just noise
	// to someone checking how full their media drives are.
	var shown []health.Capacity
	for _, c := range s.Capacities {
		if c.Total == 0 || strings.EqualFold(c.FSType, "swap") {
			continue
		}
		shown = append(shown, c)
	}
	if len(shown) == 0 {
		rows = append(rows, headerStyle.Render("Nothing mounted yet."))
		return centeredFrame(strings.Join(rows, "\n"))
	}

	for _, c := range shown {
		name := capacityName(c)
		bar := capacityBar(c.PctUsed, 20)
		rows = append(rows, "  "+devNameStyle.Render(padRight(truncate(name, 20), 21))+bar)
		detail := fmt.Sprintf("%s free of %s  ·  %s used",
			health.HumanBytes(c.Avail),
			health.HumanBytes(c.Total),
			health.HumanBytes(c.Used))
		rows = append(rows, "     "+headerStyle.Render(detail))
	}
	return centeredFrame(strings.Join(rows, "\n"))
}

// capacityName picks the friendliest label for a mounted filesystem: its
// health Label if set, otherwise a plain word for the well-known system
// mounts, otherwise the last path segment.
func capacityName(c health.Capacity) string {
	if c.Label != "" {
		return c.Label
	}
	switch c.Mountpoint {
	case "/":
		return "System drive"
	case "/boot", "/boot/efi", "/efi":
		return "Boot partition"
	}
	return filepath.Base(c.Mountpoint)
}

// capacityBar renders a 20-cell percentage bar with colour buckets:
// green <70, amber 70-90, red >=90.
func capacityBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100 * float64(width))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	style := roleAvailableStyle
	switch {
	case pct >= 90:
		style = roleSystemStyle
	case pct >= 70:
		style = roleInUseStyle
	}
	return style.Render(bar) + fmt.Sprintf("  %5.1f%%", pct)
}

func renderBtrfsCard(s *health.Snapshot) string {
	rows := []string{titleStyle.Render("Storage pool health  (Btrfs)")}
	if len(s.Btrfs) == 0 {
		rows = append(rows, headerStyle.Render("No storage pools — single-drive setups don't need one."))
		return centeredFrame(strings.Join(rows, "\n"))
	}
	for i, p := range s.Btrfs {
		if i > 0 {
			rows = append(rows, "")
		}
		state := roleAvailableStyle.Render("✓ healthy")
		if !p.Healthy {
			state = roleSystemStyle.Render("✗ ATTENTION")
		}
		rows = append(rows, devNameStyle.Render(p.Label)+
			"  "+headerStyle.Render(p.Mountpoint)+"   "+state)
		if p.Note != "" {
			rows = append(rows, "  "+roleSystemStyle.Render(p.Note))
		}
		if len(p.Devices) == 0 {
			rows = append(rows, "  "+headerStyle.Render("(no devices reported)"))
			continue
		}
		rows = append(rows, "  "+headerStyle.Render(fmt.Sprintf(
			"%-18s %6s %6s %6s %6s %6s %s",
			"Device", "write", "read", "flush", "corrupt", "gen", "state",
		)))
		for _, d := range p.Devices {
			devState := roleAvailableStyle.Render("ok")
			if d.Missing {
				devState = roleSystemStyle.Render("MISSING")
			} else if d.WriteErrs+d.ReadErrs+d.FlushErrs+d.CorruptErrs+d.GenErrs > 0 {
				devState = roleSystemStyle.Render("errors")
			}
			rows = append(rows, fmt.Sprintf(
				"  %-18s %6d %6d %6d %6d %6d %s",
				d.Path, d.WriteErrs, d.ReadErrs, d.FlushErrs,
				d.CorruptErrs, d.GenErrs, devState,
			))
		}
	}
	return centeredFrame(strings.Join(rows, "\n"))
}

func renderDiskCard(s *health.Snapshot) string {
	rows := []string{titleStyle.Render("Drive health  (SMART)")}
	if len(s.Disks) == 0 {
		rows = append(rows, headerStyle.Render("No physical disks detected."))
		return centeredFrame(strings.Join(rows, "\n"))
	}
	header := "  " +
		padRight("Drive", 18) + " " +
		padRight("Connection", 11) + " " +
		padRight("Health", 8) + " " +
		padLeft("Temp", 6) + " " +
		padLeft("Age", 8) + " " +
		padLeft("Realloc", 8) + " " +
		padLeft("Pending", 8)
	rows = append(rows, headerStyle.Render(header))
	for _, d := range s.Disks {
		var smart string
		switch {
		case d.Standby:
			// Drive is asleep and we deliberately didn't wake it — a good
			// sign, not a missing reading.
			smart = headerStyle.Render("asleep")
		case !d.SmartReady:
			smart = roleInUseStyle.Render("n/a")
		case d.Passed:
			smart = roleAvailableStyle.Render("✓ good")
		default:
			smart = roleSystemStyle.Render("✗ FAILING")
		}
		temp := "—"
		if d.TempC > 0 {
			tcol := roleAvailableStyle
			if d.TempC >= 55 {
				tcol = roleSystemStyle
			} else if d.TempC >= 45 {
				tcol = roleInUseStyle
			}
			temp = tcol.Render(fmt.Sprintf("%d°C", d.TempC))
		}
		hrs := humanAge(d.PowerOnHr)
		realloc := fmt.Sprintf("%d", d.Reallocated)
		pending := fmt.Sprintf("%d", d.Pending)
		if d.Reallocated > 0 {
			realloc = roleSystemStyle.Render(realloc)
		}
		if d.Pending > 0 {
			pending = roleSystemStyle.Render(pending)
		}
		name := d.Model
		if name == "" {
			name = filepath.Base(d.Device)
		}
		row := "  " +
			padRight(truncate(name, 18), 18) + " " +
			padRight(plainTransport(d.Transport), 11) + " " +
			padRight(smart, 8) + " " +
			padLeft(temp, 6) + " " +
			padLeft(hrs, 8) + " " +
			padLeft(realloc, 8) + " " +
			padLeft(pending, 8)
		rows = append(rows, row)
	}
	return centeredFrame(strings.Join(rows, "\n"))
}

// padRight/padLeft pad a possibly-styled string to a visible-character
// width. Plain fmt %-*s counts ANSI escape bytes, breaking alignment.
func padRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func padLeft(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}
