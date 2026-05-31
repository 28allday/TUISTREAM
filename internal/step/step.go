// Package step defines a single canonical "labelled command" type that the
// install / mount / firewall packages all return, and that the TUI feeds to
// tea.ExecProcess one entry at a time.
package step

import "os/exec"

// Step is one logical action of a multi-step plan — a human-readable Title
// shown in the TUI's progress view, plus the command actually executed.
type Step struct {
	Title string
	Cmd   *exec.Cmd
}
