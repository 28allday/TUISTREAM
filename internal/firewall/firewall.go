// Package firewall opens/closes the Jellyfin LAN ports via UFW if present.
//
// We deliberately target UFW only — Omarchy & Arch installs commonly have it.
// If UFW isn't installed, the helpers return a no-op plan with a clear note
// in the title so the TUI surfaces "skipped" instead of silently doing nothing.
package firewall

import (
	"os/exec"
	"strings"
	"tuistream/internal/step"
)

const (
	WebPort       = "8096/tcp" // HTTP web UI / API
	DiscoveryPort = "7359/udp" // Jellyfin client auto-discovery on the LAN
)

// Available reports whether UFW is installed (otherwise plans return no-ops).
func Available() bool {
	_, err := exec.LookPath("ufw")
	return err == nil
}

// State is a snapshot of which Jellyfin ports UFW is currently allowing.
type State struct {
	UFWInstalled  bool
	WebOpen       bool
	DiscoveryOpen bool
}

func LoadState() State {
	s := State{UFWInstalled: Available()}
	if !s.UFWInstalled {
		return s
	}
	out, err := exec.Command("ufw", "status").Output()
	if err != nil {
		return s
	}
	text := string(out)
	s.WebOpen = containsAllow(text, WebPort)
	s.DiscoveryOpen = containsAllow(text, DiscoveryPort)
	return s
}

// AllOpen returns true if both Jellyfin ports are currently allowed.
func (s State) AllOpen() bool { return s.WebOpen && s.DiscoveryOpen }

// AnyOpen returns true if at least one Jellyfin port is allowed (used to
// decide whether the toggle's verb should be "close" or "open").
func (s State) AnyOpen() bool { return s.WebOpen || s.DiscoveryOpen }

func containsAllow(ufwStatus, port string) bool {
	// `ufw status` lines look like:  "8096/tcp                   ALLOW       Anywhere"
	for _, line := range strings.Split(ufwStatus, "\n") {
		if strings.Contains(line, port) && strings.Contains(line, "ALLOW") {
			return true
		}
	}
	return false
}

// OpenPlan returns the steps needed to allow Jellyfin's two LAN ports.
func OpenPlan() []step.Step {
	if !Available() {
		return []step.Step{{Title: "UFW not installed — skipping firewall", Cmd: exec.Command("true")}}
	}
	return []step.Step{
		{Title: "ufw allow " + WebPort, Cmd: exec.Command("ufw", "allow", WebPort)},
		{Title: "ufw allow " + DiscoveryPort, Cmd: exec.Command("ufw", "allow", DiscoveryPort)},
	}
}

// ClosePlan returns the steps needed to remove the two Jellyfin allow rules.
func ClosePlan() []step.Step {
	if !Available() {
		return []step.Step{{Title: "UFW not installed — nothing to close", Cmd: exec.Command("true")}}
	}
	return []step.Step{
		{Title: "ufw delete allow " + WebPort, Cmd: exec.Command("ufw", "delete", "allow", WebPort)},
		{Title: "ufw delete allow " + DiscoveryPort, Cmd: exec.Command("ufw", "delete", "allow", DiscoveryPort)},
	}
}
