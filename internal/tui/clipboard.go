package tui

import (
	"os"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
)

// copyToClipboard returns a command that copies text to the user's clipboard
// using an OSC 52 escape sequence.
//
// This is the only copy mechanism that works from a TUI running on a HEADLESS
// server reached over SSH: the sequence is interpreted by the user's *local*
// terminal emulator, so the text lands in the clipboard of the machine they're
// sitting at — no X11/Wayland needed on the server. Inside tmux we wrap it for
// passthrough; that requires `set -g allow-passthrough on` (or `set -g
// set-clipboard on`) in the user's tmux, and a terminal that supports OSC 52
// (kitty, ghostty, wezterm, iTerm2, foot, …).
//
// The sequence is zero-width and moves no cursor, so writing it to stderr does
// not disturb Bubble Tea's alt-screen rendering. Best-effort: if the terminal
// ignores OSC 52 the user can still select-to-copy with the mouse.
func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		seq := osc52.New(text)
		if os.Getenv("TMUX") != "" {
			seq = seq.Tmux()
		}
		_, _ = seq.WriteTo(os.Stderr)
		return nil
	}
}
