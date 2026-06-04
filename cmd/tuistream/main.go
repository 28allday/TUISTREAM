// Command tuistream is a TUI for setting up and managing a headless Jellyfin
// Media Server on Omarchy / Arch Linux.
//
// Two tabs:
//
//	Setup  — install Jellyfin, attach media drives, firewall, uninstall.
//	Manage — copy files from an external drive to a media drive,
//	         delete files, rename files.
//
// The picker hides any partition that lives on a disk hosting the OS or
// active swap, so the boot drive can never be selected as a media drive.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"tuistream/internal/spindown"
	"tuistream/internal/system"
	"tuistream/internal/theme"
	"tuistream/internal/tui"
)

// version is stamped at build time via:
//
//	-ldflags "-X main.version=v0.1.1"
var version = "dev"

func main() {
	readOnly := flag.Bool("read-only", false,
		"open the TUI without checking for root; only the inventory views work")
	showVersion := flag.Bool("version", false, "print the version and exit")
	spindownWatch := flag.Bool("spindown-watch", false,
		"internal: run the drive idle watcher (started by tuistream-spindown.service)")
	flag.Parse()

	if *showVersion {
		fmt.Println("tuistream", version)
		return
	}

	// Daemon mode: no TUI, just the idle watcher (root needed for hdparm).
	if *spindownWatch {
		if os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "tuistream --spindown-watch must run as root")
			os.Exit(1)
		}
		if err := spindown.Watch(); err != nil {
			fmt.Fprintln(os.Stderr, "tuistream:", err)
			os.Exit(1)
		}
		return
	}

	if !*readOnly && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr,
			"tuistream needs administrator rights to install Jellyfin, edit /etc/fstab, etc.")
		fmt.Fprintln(os.Stderr, "  Run:  sudo tuistream")
		fmt.Fprintln(os.Stderr, "  Or:   tuistream --read-only   (inventory only, no actions)")
		os.Exit(1)
	}

	// Pre-flight: install any userspace tools the flows assume are present.
	// Only attempted when running as root — read-only just skips the check.
	if !*readOnly {
		if err := system.EnsureInstalled(); err != nil {
			fmt.Fprintln(os.Stderr, "tuistream: pacman failed:", err)
			fmt.Fprintln(os.Stderr,
				"  Some actions may not work until the missing packages are installed.")
		}
	}

	m := tui.NewModel(theme.Load())
	// No mouse capture: TUISTREAM is keyboard-driven, and grabbing the mouse
	// would stop the user's terminal/tmux from selecting-to-copy text (e.g.
	// the Jellyfin web URL) or click-opening links. Leaving it off keeps
	// native selection and hyperlink handling working over SSH + tmux.
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tuistream:", err)
		os.Exit(1)
	}
}
