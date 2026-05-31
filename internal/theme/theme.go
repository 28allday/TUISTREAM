// Package theme loads the active Omarchy colour scheme so the TUI matches the
// rest of the desktop. Omarchy writes a per-theme colors.toml (accent,
// foreground, background, color0-15); we read the active one and fall back to a
// sensible built-in palette on headless / non-Omarchy systems.
package theme

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Theme is the small set of colours the TUI needs, as "#rrggbb" strings.
type Theme struct {
	Accent string
	Fg     string
	Bg     string
	Dim    string
	Muted  string
	Good   string
	Bad    string
}

// Default is the palette used when no Omarchy theme is found (e.g. a headless /
// omaterm box reached over SSH). It uses ANSI palette indices rather than fixed
// hex, so the colours track whatever theme the connecting terminal uses.
func Default() Theme {
	return Theme{
		Accent: "4", // blue
		Fg:     "7", // foreground / white
		Bg:     "0", // background / black
		Dim:    "7",
		Muted:  "8", // bright black / grey
		Good:   "2", // green
		Bad:    "1", // red
	}
}

// Load returns the active Omarchy theme's colours, or Default() if unavailable.
func Load() Theme {
	// Under `sudo` HOME is /root, which never has an Omarchy theme. Prefer
	// the calling user's home (via SUDO_USER) so the TUI matches the
	// desktop you actually launched it from.
	home := callerHome()
	if home == "" {
		return Default()
	}
	path := filepath.Join(home, ".config", "omarchy", "current", "theme", "colors.toml")
	kv, ok := parse(path)
	if !ok {
		return Default()
	}
	d := Default()
	pick := func(def string, keys ...string) string {
		for _, k := range keys {
			if v := kv[k]; v != "" {
				return v
			}
		}
		return def
	}
	return Theme{
		Accent: pick(d.Accent, "accent", "color4"),
		Fg:     pick(d.Fg, "foreground", "color7"),
		Bg:     pick(d.Bg, "background", "color0"),
		Dim:    pick(d.Dim, "color7", "foreground"),
		Muted:  pick(d.Muted, "color8", "color7"),
		Good:   pick(d.Good, "color2"),
		Bad:    pick(d.Bad, "color1"),
	}
}

// callerHome resolves the home directory of the user who launched tuistream,
// looking through `sudo` if necessary. Returns "" if nothing resolves.
func callerHome() string {
	if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
		if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
			return u.HomeDir
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// parse reads simple `key = "#hex"` lines from an Omarchy colors.toml. It is a
// minimal parser (no TOML dependency) sufficient for that flat file.
func parse(path string) (map[string]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	kv := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"`)
		if strings.HasPrefix(val, "#") {
			kv[key] = val
		}
	}
	if len(kv) == 0 {
		return nil, false
	}
	return kv, true
}
