// Package spindown puts spinning media drives to sleep after a few idle
// minutes. NAS-class drives (WD Red, IronWolf) ship with NO idle timer at
// all — they spin 24/7 and run warm even when nothing touches them.
//
// Spin-down is a DEFAULT, not a feature the user enables: whenever the
// inventory shows spinning media drives, the TUI silently syncs the
// watcher onto the box (see AutoSyncDue). The Setup tab's [s] key is the
// opt-OUT — disabling writes a marker file so the default never fights
// the user.
//
// Mechanism: a tiny systemd service runs `tuistream --spindown-watch`,
// which samples /proc/diskstats and issues `hdparm -y` to any target
// drive that's been idle past the timeout. We deliberately do NOT use the
// drive's own standby timer (`hdparm -S`): common NAS drives — the 10TB
// helium WD Reds included — advertise the timer and then ignore it.
// Forcing standby from the outside works on everything `hdparm -y`
// works on, which we can verify per-drive.
//
// The watcher's own probes never touch the platters: /proc/diskstats and
// sysfs are kernel memory, and `hdparm -C` is an ATA CHECK POWER MODE,
// which neither wakes a sleeping drive nor resets its idle state.
//
// System disks are never targeted: Targets() refuses anything the OS
// lives on, using the same classifier the rest of Setup trusts.
package spindown

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tuistream/internal/drives"
	"tuistream/internal/step"
)

// UnitPath is the systemd service that runs the watcher.
const UnitPath = "/etc/systemd/system/tuistream-spindown.service"

const unitName = "tuistream-spindown.service"

// legacyRulesPath is the v0.2.0-rc udev/`hdparm -S` approach, removed on
// sync: the firmware timer it relied on is ignored by common NAS drives.
const legacyRulesPath = "/etc/udev/rules.d/69-tuistream-spindown.rules"

// OptOutPath marks "the user turned spin-down off on purpose" — its
// presence stops the auto-sync from re-enabling the default behind their
// back. Written by DisablePlan, removed by EnablePlan.
const OptOutPath = "/etc/tuistream/spindown-off"

// TimeoutMinutes is the idle time before a drive is spun down.
const TimeoutMinutes = 3

// WatchInterval is how often the watcher samples /proc/diskstats.
const WatchInterval = 30 * time.Second

// Target is one spinning, non-system physical disk the watcher may touch.
type Target struct {
	Device string // /dev/sda
	Name   string // sda
	Model  string
}

// Targets picks the disks the watcher may touch: spinning (per sysfs),
// top-level, real hardware, and never a disk the OS lives on.
func Targets(inv *drives.Inventory) []Target {
	if inv == nil {
		return nil
	}
	var ts []Target
	for _, d := range inv.All {
		if d.Type != "disk" || drives.IsPseudoDisk(d.Name) {
			continue
		}
		if inv.SystemDisks[d.Name] {
			continue
		}
		if !rotational(d.Name) {
			continue
		}
		ts = append(ts, Target{Device: d.Path, Name: d.Name, Model: d.Model})
	}
	return ts
}

// rotational reports whether a disk spins, per sysfs. Anything we can't
// read is treated as non-rotational so SSDs/NVMe are never targeted by a
// misread.
func rotational(name string) bool {
	b, err := os.ReadFile("/sys/block/" + name + "/queue/rotational")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "1"
}

// Enabled reports whether the watcher service is installed.
func Enabled() bool {
	_, err := os.Stat(UnitPath)
	return err == nil
}

// OptedOut reports whether the user explicitly disabled the default.
func OptedOut() bool {
	_, err := os.Stat(OptOutPath)
	return err == nil
}

// AutoSyncDue reports whether the silent default-apply should run: there
// are spinning media drives, the user hasn't opted out, and the unit on
// disk doesn't match what we'd write (missing, stale, or pointing at a
// binary that has since moved — e.g. after a proper install).
func AutoSyncDue(ts []Target) bool {
	if len(ts) == 0 || OptedOut() {
		return false
	}
	b, err := os.ReadFile(UnitPath)
	if err != nil {
		return true
	}
	return string(b) != unitContent()
}

// unitContent renders the service. ExecStart points at the running
// binary so a dev copy works too; when the binary later moves (proper
// install), the content no longer matches and AutoSyncDue triggers a
// rewrite.
func unitContent() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "/usr/local/bin/tuistream"
	}
	return `[Unit]
Description=TUISTREAM drive spin-down watcher
Documentation=https://github.com/28allday/TUISTREAM

[Service]
ExecStart=` + exe + ` --spindown-watch
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`
}

// EnablePlan installs and starts the watcher, clears any opt-out marker,
// and removes the legacy udev rule from the rc builds.
func EnablePlan(ts []Target) []step.Step {
	return []step.Step{
		{
			Title: "Install spin-down watcher service",
			Cmd: exec.Command("bash", "-c",
				"cat > "+UnitPath+" <<'TUISTREAM_EOF'\n"+unitContent()+"TUISTREAM_EOF"),
		},
		{Title: "Clear spin-down opt-out", Cmd: exec.Command("rm", "-f", OptOutPath)},
		{Title: "Remove legacy udev rule", Cmd: exec.Command("rm", "-f", legacyRulesPath)},
		{Title: "Reload systemd units", Cmd: exec.Command("systemctl", "daemon-reload")},
		{Title: "Start spin-down watcher", Cmd: exec.Command("systemctl", "enable", "--now", unitName)},
	}
}

// DisablePlan stops and removes the watcher and records the opt-out so
// the default never re-applies itself. Drives return to their factory
// behaviour (NAS drives spin 24/7).
func DisablePlan(ts []Target) []step.Step {
	return []step.Step{
		{
			Title: "Stop spin-down watcher",
			Cmd:   exec.Command("bash", "-c", "systemctl disable --now "+unitName+" 2>/dev/null; true"),
		},
		{Title: "Remove watcher service", Cmd: exec.Command("rm", "-f", UnitPath)},
		{Title: "Remove legacy udev rule", Cmd: exec.Command("rm", "-f", legacyRulesPath)},
		{Title: "Reload systemd units", Cmd: exec.Command("systemctl", "daemon-reload")},
		{
			Title: "Record spin-down opt-out",
			Cmd: exec.Command("bash", "-c",
				"mkdir -p "+filepath.Dir(OptOutPath)+" && touch "+OptOutPath),
		},
	}
}

// AutoSync is the silent path for the default: same work as EnablePlan
// but run directly (no step-runner UI), used when the TUI notices the
// watcher is missing or stale. Returns the first error; the TUI surfaces
// it as a flash rather than a failure screen.
func AutoSync(ts []Target) error {
	for _, s := range EnablePlan(ts) {
		if err := s.Cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", s.Title, err)
		}
	}
	return nil
}

// ---------- the watcher itself (`tuistream --spindown-watch`) ----------

// Watch is the daemon loop. Every WatchInterval it re-derives the target
// set (so hotplugged drives are covered without a restart), reads each
// target's I/O counters, and spins down any drive that has been idle past
// the timeout and is still spinning. Logs go to stdout → journald.
func Watch() error {
	timeout := time.Duration(TimeoutMinutes) * time.Minute
	log.Printf("watching for drives idle ≥ %s (sampling every %s)", timeout, WatchInterval)

	type diskState struct {
		sig  string    // last-seen I/O counter signature
		last time.Time // when the signature last changed
	}
	states := map[string]*diskState{}

	for {
		inv, err := drives.Load("")
		if err != nil {
			log.Printf("inventory failed (will retry): %v", err)
			time.Sleep(WatchInterval)
			continue
		}
		for _, t := range Targets(inv) {
			sig := ioSignature(t.Name)
			if sig == "" {
				continue
			}
			st := states[t.Name]
			if st == nil || st.sig != sig {
				states[t.Name] = &diskState{sig: sig, last: time.Now()}
				continue
			}
			if time.Since(st.last) < timeout || !isSpinning(t.Device) {
				continue
			}
			if err := exec.Command("hdparm", "-y", t.Device).Run(); err != nil {
				log.Printf("couldn't spin down %s: %v", t.Device, err)
				// Push last forward so a refusing drive is retried after a
				// full timeout instead of every sample.
				st.last = time.Now()
				continue
			}
			log.Printf("spun down %s (%s) after %s idle", t.Device, t.Model, timeout)
		}
		time.Sleep(WatchInterval)
	}
}

// ioSignature condenses a disk's /proc/diskstats counters that only move
// on real I/O: reads/writes completed and sectors read/written. Fields
// like io_ticks and in_flight churn on their own and are excluded.
func ioSignature(name string) string {
	b, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[2] != name {
			continue
		}
		return f[3] + " " + f[5] + " " + f[7] + " " + f[9]
	}
	return ""
}

// isSpinning reports whether the drive is in active/idle (as opposed to
// standby/sleeping). `hdparm -C` issues CHECK POWER MODE, which doesn't
// wake a sleeping drive. On error we report false — never send a sleep
// command to a drive we can't read.
func isSpinning(device string) bool {
	out, err := exec.Command("hdparm", "-C", device).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "active")
}
