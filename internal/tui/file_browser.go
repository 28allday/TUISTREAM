// File browser sub-model used by the Manage tab. Browses a directory tree
// anchored at a fixed root (a drive's mountpoint) and refuses to escape it.
//
// Key bindings:
//
//	↑/↓         move cursor
//	enter       dive into a folder; on a file or with "select-here" sentinel, select
//	right       same as enter, but always dives (no select-here behaviour)
//	left / esc  go up; at the root, esc cancels
//	space       (caller-defined) often used to select the current cwd directly
//	g / G       jump to top / bottom
//	pgup / pgdn page
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// browseResult is what update() reports back to the caller after each key.
type browseResult int

const (
	browseStillBrowsing browseResult = iota
	browseCancelled
	browseSelected
)

// fileBrowser holds the browser's mutable state. Owned by a caller — e.g.
// the Manage tab embeds two of these (src + dst).
type fileBrowser struct {
	root string // absolute path the browser is anchored at; cwd never escapes it.
	cwd  string // current directory being shown
	rows []browseEntry
	idx  int

	// Window for paging — recomputed on each render based on terminal height.
	// We track these so PgUp/PgDn move sensibly.
	viewportRows int

	// Result fields, populated on browseSelected.
	selectedPath string
}

type browseEntry struct {
	name  string
	path  string
	isDir bool
	size  int64
}

// newFileBrowser opens `root` and lands the user in it.
func newFileBrowser(root string) (fileBrowser, error) {
	fb := fileBrowser{root: root, cwd: root, viewportRows: 12}
	if err := fb.load(); err != nil {
		return fileBrowser{}, err
	}
	return fb, nil
}

// SelectedPath is the absolute path of the picked file/folder. Only valid
// after the browser has returned browseSelected.
func (fb *fileBrowser) SelectedPath() string { return fb.selectedPath }

func (fb *fileBrowser) load() error {
	entries, err := os.ReadDir(fb.cwd)
	if err != nil {
		return err
	}
	fb.rows = fb.rows[:0]
	for _, e := range entries {
		// Hide dotfiles by default — most media drives have a .Trash-1000 etc.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		size := int64(0)
		isDir := e.IsDir()
		if err == nil {
			size = info.Size()
		}
		fb.rows = append(fb.rows, browseEntry{
			name:  e.Name(),
			path:  filepath.Join(fb.cwd, e.Name()),
			isDir: isDir,
			size:  size,
		})
	}
	sort.SliceStable(fb.rows, func(i, j int) bool {
		// Directories first, then files, both case-insensitive alpha.
		if fb.rows[i].isDir != fb.rows[j].isDir {
			return fb.rows[i].isDir
		}
		return strings.ToLower(fb.rows[i].name) < strings.ToLower(fb.rows[j].name)
	})
	fb.idx = 0
	return nil
}

// update routes a key to the browser. Returns the result + an optional Cmd.
//
// Tab / Shift-Tab are intentionally NOT handled here — the caller checks
// for them first and passes them to the root model so the user can switch
// tabs mid-browse without losing their place.
func (fb *fileBrowser) update(msg tea.KeyMsg) (browseResult, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if fb.idx > 0 {
			fb.idx--
		}
	case "down", "j":
		if fb.idx < len(fb.rows)-1 {
			fb.idx++
		}
	case "pgup":
		fb.idx -= fb.viewportRows
		if fb.idx < 0 {
			fb.idx = 0
		}
	case "pgdown":
		fb.idx += fb.viewportRows
		if fb.idx > len(fb.rows)-1 {
			fb.idx = len(fb.rows) - 1
		}
	case "home", "g":
		fb.idx = 0
	case "end", "G":
		fb.idx = len(fb.rows) - 1
	case "left", "h", "backspace":
		// Step up out of cwd. At the root, this is a no-op (use esc to cancel).
		if fb.cwd == fb.root {
			return browseStillBrowsing, nil
		}
		fb.cwd = filepath.Dir(fb.cwd)
		_ = fb.load()
	case "esc":
		return browseCancelled, nil
	case "right", "l":
		// Always dives if on a folder; on a file this is a no-op.
		if len(fb.rows) == 0 {
			return browseStillBrowsing, nil
		}
		cur := fb.rows[fb.idx]
		if cur.isDir {
			fb.cwd = cur.path
			_ = fb.load()
		}
	case "enter":
		if len(fb.rows) == 0 {
			return browseStillBrowsing, nil
		}
		cur := fb.rows[fb.idx]
		if cur.isDir {
			fb.cwd = cur.path
			_ = fb.load()
		} else {
			fb.selectedPath = cur.path
			return browseSelected, nil
		}
	case "s", "S":
		// Select the cursor row itself (works for folders too).
		if len(fb.rows) == 0 {
			return browseStillBrowsing, nil
		}
		fb.selectedPath = fb.rows[fb.idx].path
		return browseSelected, nil
	}
	return browseStillBrowsing, nil
}

// view renders the browser as a fixed-size pane. Always reserves
// viewportRows rows of body space so the card doesn't shrink/grow as the
// user navigates between sparse and dense directories.
func (fb *fileBrowser) view(title, footerHint string) string {
	const nameCol = 56 // name column width before the right-aligned size

	var rows []string
	rows = append(rows, titleStyle.Render(title))
	rows = append(rows, "")
	rows = append(rows, devNameStyle.Render(fb.cwd))
	rows = append(rows, "")

	// Compute the visible window so the cursor stays inside it.
	start := 0
	if fb.idx >= fb.viewportRows {
		start = fb.idx - fb.viewportRows + 1
	}
	end := start + fb.viewportRows
	if end > len(fb.rows) {
		end = len(fb.rows)
	}

	bodyRows := 0
	if len(fb.rows) == 0 {
		rows = append(rows, "  "+headerStyle.Render("(empty directory)"))
		bodyRows = 1
	} else {
		for i := start; i < end; i++ {
			row := fb.rows[i]
			name := row.name
			if row.isDir {
				name = devNameStyle.Render(name + "/")
			}
			size := ""
			if !row.isDir {
				size = humanSize(row.size)
			}
			// Pad the name column so all sizes right-align.
			plainName := row.name
			if row.isDir {
				plainName += "/"
			}
			padding := nameCol - len(plainName)
			if padding < 1 {
				padding = 1
			}
			body := name + strings.Repeat(" ", padding) +
				headerStyle.Render(fmt.Sprintf("%6s", size))
			rows = append(rows, markerLine(i == fb.idx, body))
			bodyRows++
		}
	}
	// Reserve unused viewport rows with blank space so the card height
	// is constant.
	for ; bodyRows < fb.viewportRows; bodyRows++ {
		rows = append(rows, "")
	}

	// Footer: row counter + key hints, always on the same line for a
	// stable card height.
	counter := ""
	if len(fb.rows) > 0 {
		counter = fmt.Sprintf("%d of %d   ·   ", fb.idx+1, len(fb.rows))
	}
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render(counter+footerHint))
	// NOTE: deliberately NOT centeredCard — the row widths change as you
	// navigate (filenames, cwd path), so centring would make the list jitter
	// horizontally while scrolling. A file listing reads best left-aligned; the
	// card itself is still centred on the page by the root View.
	return cardStyle.Render(strings.Join(rows, "\n"))
}

// humanSize formats a byte count with the smallest sensible suffix.
func humanSize(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
		tb = 1024 * gb
	)
	switch {
	case n >= tb:
		return fmtSize(float64(n)/tb, "T")
	case n >= gb:
		return fmtSize(float64(n)/gb, "G")
	case n >= mb:
		return fmtSize(float64(n)/mb, "M")
	case n >= kb:
		return fmtSize(float64(n)/kb, "K")
	}
	return fmt.Sprintf("%dB", n)
}

// fmtSize: one decimal under 10, none above. "1.2G", "44M", "512K".
func fmtSize(v float64, suffix string) string {
	if v < 10 {
		s := fmt.Sprintf("%.1f", v)
		s = strings.TrimSuffix(s, ".0")
		return s + suffix
	}
	return fmt.Sprintf("%.0f", v) + suffix
}
