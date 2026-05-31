package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"tuistream/internal/drives"
	"tuistream/internal/step"
)

// Sub-stages of the Add-drive flow. Each lives inside stageAddDrive at the
// setupModel level, switched via addDrive.subStage.
type addDriveSubStage int

const (
	addPick      addDriveSubStage = iota // multi-select drives (1 → single mode, 2+ → RAID)
	addFormat                            // single-drive mode: keep existing FS or pick mkfs target
	addRaidLevel                         // multi-drive mode: pick RAID level (1, 0, 5, 10)
	addName                              // text-input the friendly directory label
	addConfirm                           // last-chance Yes/No before mounting
)

// addDriveState carries the Add-drive sub-flow's state across Update calls.
type addDriveState struct {
	subStage       addDriveSubStage
	candidates     []drives.Drive
	pickIdx        int            // cursor row in the picker
	pickSelected   []bool         // parallel to candidates: which rows are ticked
	selected       drives.Drive   // single-drive mode: the chosen drive
	pool           []drives.Drive // multi-drive mode: the chosen drives
	formatOpts     []formatOption
	formatIdx      int
	formatAs       drives.FormatChoice
	raidLevels     []drives.RAIDLevel
	raidIdx        int
	raidLevel      drives.RAIDLevel
	wipeWholeDisks bool // RAID confirm: zap entire parent disks (default true)
	wipeWholeDisk  bool // single-drive confirm: zap the partition's parent disk (default false)
	name           textinput.Model
	confirmIdx     int    // 0 = Yes, 1 = No
	inspection     string // human-readable summary of existing files (Keep path only)
	hasContent     bool   // true when the kept filesystem already holds real data
}

// isPool returns true when the Add-drive run is in multi-drive (RAID) mode.
func (s *addDriveState) isPool() bool { return len(s.pool) >= 2 }

// formatOption is one row in the addFormat sub-stage's menu.
type formatOption struct {
	choice drives.FormatChoice
	label  string
	hint   string
}

// newAddDriveState seeds the textinput with a sensible default label and
// returns the initial state for the Add-drive flow.
func newAddDriveState(cands []drives.Drive) addDriveState {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "MediaDrive"
	ti.CharLimit = 32
	ti.Width = 32
	ti.Focus()
	return addDriveState{
		subStage:     addPick,
		candidates:   cands,
		pickSelected: make([]bool, len(cands)),
		name:         ti,
	}
}

// defaultLabelFor picks an initial label for the textinput: the drive's
// filesystem label if it exists and is clean, else "MediaDrive".
func defaultLabelFor(d drives.Drive) string {
	if d.Label != "" {
		// Sanitise: replace spaces with hyphens, drop anything weird.
		cleaned := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '-', r == '_':
				return r
			case r == ' ':
				return '-'
			}
			return -1
		}, d.Label)
		if cleaned != "" {
			return cleaned
		}
	}
	return "MediaDrive"
}

// --- key handling ---

func setupAddDriveKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	st := &m.setup.addDrive
	switch st.subStage {
	case addPick:
		return addDrivePickKey(m, msg.String())
	case addFormat:
		return addDriveFormatKey(m, msg.String())
	case addRaidLevel:
		return addDriveRaidLevelKey(m, msg.String())
	case addName:
		return addDriveNameKey(m, msg)
	case addConfirm:
		return addDriveConfirmKey(m, msg.String())
	}
	return m, nil, false
}

func addDrivePickKey(m Model, key string) (Model, tea.Cmd, bool) {
	st := &m.setup.addDrive
	switch key {
	case "up", "k":
		if st.pickIdx > 0 {
			st.pickIdx--
		}
		return m, nil, true
	case "down", "j":
		if st.pickIdx < len(st.candidates)-1 {
			st.pickIdx++
		}
		return m, nil, true
	case " ", "space":
		// Toggle selection on the cursor row. When toggling ON, auto-deselect
		// any peer that would conflict: selecting a whole disk drops any of
		// its child partitions, and vice versa.
		if len(st.pickSelected) > st.pickIdx {
			newState := !st.pickSelected[st.pickIdx]
			st.pickSelected[st.pickIdx] = newState
			if newState {
				current := st.candidates[st.pickIdx]
				for i, d := range st.candidates {
					if i == st.pickIdx || !st.pickSelected[i] {
						continue
					}
					// Conflict: current is a partition whose parent is `d`.
					if d.Type == "disk" && current.ParentDisk == d.Name {
						st.pickSelected[i] = false
					}
					// Conflict: current is a disk and `d` is one of its partitions.
					if current.Type == "disk" && d.ParentDisk == current.Name {
						st.pickSelected[i] = false
					}
				}
			}
		}
		return m, nil, true
	case "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		if len(st.candidates) == 0 {
			m.flash = "No eligible drives — see the inventory for why."
			m.setup.stage = stageIdle
			return m, nil, true
		}
		// Collect the toggled drives. If nothing's toggled, treat the cursor
		// row as a one-drive single-mode pick.
		var chosen []drives.Drive
		for i, ok := range st.pickSelected {
			if ok {
				chosen = append(chosen, st.candidates[i])
			}
		}
		if len(chosen) == 0 {
			chosen = []drives.Drive{st.candidates[st.pickIdx]}
		}

		// If multiple selections collapse onto a single parent disk, the
		// user is really asking to use that whole disk — RAID on a disk
		// against itself isn't a thing, and would fail at mkfs.btrfs AFTER
		// we've already wiped it. Auto-collapse to whole-disk single-drive
		// mode instead.
		if len(chosen) >= 2 {
			parents := drives.UniqueParents(chosen)
			if len(parents) == 1 && m.inventory != nil {
				if wholeDisk := m.inventory.FindDisk(parents[0]); wholeDisk != nil {
					st.selected = *wholeDisk
					st.pool = nil
					st.formatOpts = buildFormatOptions(*wholeDisk) // no "keep" — disk has no FS
					st.formatIdx = 0
					st.formatAs = st.formatOpts[0].choice
					st.wipeWholeDisk = true // forced — wiping is implicit for whole disks
					st.subStage = addFormat
					m.flash = fmt.Sprintf(
						"All %d picks were on %s — switched to whole-disk mode.",
						len(chosen), wholeDisk.Path)
					return m, nil, true
				}
			}
		}

		if len(chosen) == 1 {
			// Single-drive mode — existing flow.
			st.selected = chosen[0]
			st.pool = nil
			st.formatOpts = buildFormatOptions(st.selected)
			st.formatIdx = 0
			st.formatAs = st.formatOpts[0].choice
			st.subStage = addFormat
			return m, nil, true
		}

		// Multi-drive mode — go to RAID level picker. Default wipe to ON;
		// it's the right answer for RAID and the confirm screen makes the
		// consequences fully visible before the user signs off.
		st.pool = chosen
		st.selected = drives.Drive{}
		st.raidLevels = drives.AvailableLevels(len(chosen))
		st.raidIdx = 0
		st.raidLevel = st.raidLevels[0]
		st.wipeWholeDisks = true
		st.subStage = addRaidLevel
		return m, nil, true
	}
	return m, nil, false
}

func addDriveRaidLevelKey(m Model, key string) (Model, tea.Cmd, bool) {
	st := &m.setup.addDrive
	switch key {
	case "up", "k":
		if st.raidIdx > 0 {
			st.raidIdx--
		}
		return m, nil, true
	case "down", "j":
		if st.raidIdx < len(st.raidLevels)-1 {
			st.raidIdx++
		}
		return m, nil, true
	case "esc":
		st.subStage = addPick
		return m, nil, true
	case "enter":
		st.raidLevel = st.raidLevels[st.raidIdx]
		st.subStage = addName
		// Default label for a pool: "MediaPool"
		st.name.SetValue("MediaPool")
		st.name.CursorEnd()
		return m, nil, true
	}
	return m, nil, false
}

// buildFormatOptions returns the format menu for `d`. If the drive already
// has a supported filesystem we offer "Keep existing" first; if it's empty
// or has an unsupported FS we go straight to mkfs choices.
func buildFormatOptions(d drives.Drive) []formatOption {
	var opts []formatOption
	hasUsableFS := false
	switch d.FSType {
	case "ext4", "btrfs", "xfs", "exfat", "ntfs", "vfat":
		hasUsableFS = true
	}
	if hasUsableFS {
		opts = append(opts, formatOption{
			choice: drives.FormatKeep,
			label:  fmt.Sprintf("Keep existing %s filesystem and files", d.FSType),
			hint:   "No data is touched. Recommended unless you want a fresh start.",
		})
	}
	opts = append(opts,
		formatOption{
			choice: drives.FormatBtrfs,
			label:  "Format as btrfs",
			hint:   "Modern default. Snapshots, scrubs, easy to grow into a multi-disk pool later.",
		},
		formatOption{
			choice: drives.FormatExt4,
			label:  "Format as ext4",
			hint:   "Most compatible. Pick if you read this drive on Linux only and want zero surprises.",
		},
		formatOption{
			choice: drives.FormatXFS,
			label:  "Format as xfs",
			hint:   "Great with very large media files and lots of metadata.",
		},
	)
	return opts
}

func addDriveFormatKey(m Model, key string) (Model, tea.Cmd, bool) {
	st := &m.setup.addDrive
	switch key {
	case "up", "k":
		if st.formatIdx > 0 {
			st.formatIdx--
		}
		return m, nil, true
	case "down", "j":
		if st.formatIdx < len(st.formatOpts)-1 {
			st.formatIdx++
		}
		return m, nil, true
	case "esc":
		st.subStage = addPick
		return m, nil, true
	case "enter":
		st.formatAs = st.formatOpts[st.formatIdx].choice
		st.subStage = addName
		st.name.SetValue(defaultLabelFor(st.selected))
		st.name.CursorEnd()
		return m, nil, true
	}
	return m, nil, false
}

func addDriveNameKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	st := &m.setup.addDrive
	keyStr := msg.String()
	switch keyStr {
	case "esc":
		st.subStage = addPick
		return m, nil, true
	case "enter":
		if !validLabel(st.name.Value()) {
			m.flash = "Use only letters, digits, '-' and '_' in the label."
			return m, nil, true
		}
		// When we're keeping an existing single-drive filesystem, preview what's
		// on it (via a temporary read-only mount) so the confirm screen can
		// reassure the user their data is safe — and so we can decide whether to
		// seed starter folders. Skipped for formats (about to be erased anyway)
		// and pools (always freshly mkfs'd).
		st.inspection = ""
		st.hasContent = false
		if !st.isPool() && st.formatAs == drives.FormatKeep && st.selected.FSType != "" {
			st.inspection, st.hasContent = drives.InspectDevice(st.selected.Path)
		}
		st.subStage = addConfirm
		st.confirmIdx = 0
		return m, nil, true
	case "tab", "shift+tab":
		// Let the root model switch tabs.
		return m, nil, false
	}
	var cmd tea.Cmd
	st.name, cmd = st.name.Update(msg)
	return m, cmd, true
}

func addDriveConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	st := &m.setup.addDrive
	switch key {
	case "left", "h":
		st.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		st.confirmIdx = 1
		return m, nil, true
	case "w":
		// Toggle the wipe-whole-disk option in both modes — but only
		// meaningful for single-drive when we're actually going to format.
		if st.isPool() {
			st.wipeWholeDisks = !st.wipeWholeDisks
		} else if st.formatAs != drives.FormatKeep {
			st.wipeWholeDisk = !st.wipeWholeDisk
		}
		return m, nil, true
	case "y", "Y":
		st.confirmIdx = 0
		return runAddDrive(m)
	case "n", "N", "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		if st.confirmIdx == 0 {
			return runAddDrive(m)
		}
		m.setup.stage = stageIdle
		return m, nil, true
	}
	return m, nil, false
}

func runAddDrive(m Model) (Model, tea.Cmd, bool) {
	st := &m.setup.addDrive
	var (
		plan  []step.Step
		err   error
		title string
	)
	if st.isPool() {
		plan, err = drives.PoolPlan(drives.PoolOptions{
			Drives:         st.pool,
			Level:          st.raidLevel,
			Label:          st.name.Value(),
			User:           m.username,
			WipeWholeDisks: st.wipeWholeDisks,
		})
		title = fmt.Sprintf("Creating btrfs %s pool at /media/%s/%s",
			st.raidLevel, m.username, st.name.Value())
	} else {
		plan, err = drives.MountPlan(drives.MountOptions{
			Drive:         st.selected,
			Label:         st.name.Value(),
			User:          m.username,
			FormatAs:      st.formatAs,
			WipeWholeDisk: st.wipeWholeDisk && st.formatAs != drives.FormatKeep,
			// Seed library folders unless we're keeping a drive that already
			// holds the user's own content — don't clutter their layout.
			SeedStarterFolders: !(st.formatAs == drives.FormatKeep && st.hasContent),
			// When keeping a drive that already has media, grant jellyfin read
			// over the WHOLE existing tree — a non-recursive grant would leave
			// pre-existing files unreadable if they were copied with tight perms.
			RecursiveACL: st.formatAs == drives.FormatKeep && st.hasContent,
		})
		title = fmt.Sprintf("Adding media drive at /media/%s/%s",
			m.username, st.name.Value())
	}
	if err != nil {
		m.flash = "Couldn't plan the mount: " + err.Error()
		m.setup.stage = stageIdle
		return m, nil, true
	}
	m.setup.stage = stageIdle
	m.run = planRun{
		title: title,
		steps: plan,
		index: 0,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabSetup
	return m, runStepCmd(plan[0]), true
}

// validLabel: alnum + dash + underscore, 1..32 chars.
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// --- rendering ---

func renderAddDrive(m Model) string {
	st := &m.setup.addDrive
	switch st.subStage {
	case addPick:
		return renderAddDrivePick(st)
	case addFormat:
		return renderAddDriveFormat(st)
	case addRaidLevel:
		return renderAddDriveRaidLevel(st)
	case addName:
		return renderAddDriveName(m.username, st)
	case addConfirm:
		if st.isPool() {
			return renderAddDriveConfirmPool(m.username, st, m.inventory)
		}
		return renderAddDriveConfirm(m.username, st, m.inventory)
	}
	return ""
}

func renderAddDriveConfirmPool(user string, st *addDriveState, inv *drives.Inventory) string {
	mp := filepath.Join("/media", user, st.name.Value())
	var rows []string
	rows = append(rows, titleStyle.Render(fmt.Sprintf("Create btrfs %s pool?", st.raidLevel)))
	rows = append(rows, "")
	rows = append(rows, "Selected partitions:")
	for _, d := range st.pool {
		rows = append(rows, "  · "+devNameStyle.Render(d.Path)+"  ("+d.Size+", "+
			dashIfEmpty(d.FSType)+", "+dashIfEmpty(d.Label)+")")
	}
	rows = append(rows, "")

	// Wipe-scope toggle. This is the critical bit: explain exactly what
	// each choice means in concrete terms so the user can't accidentally
	// destroy data they didn't expect to.
	parents := drives.UniqueParents(st.pool)
	scope := "Use only the selected partitions"
	scopeBody := []string{
		headerStyle.Render(
			"Only the selected partitions are wiped. Other partitions on the same disks are untouched."),
		roleSystemStyle.Render(
			"  ⚠ Risk: stray RAID/LVM/FS signatures on the unused parts of the parent disks can"),
		roleSystemStyle.Render(
			"     confuse btrfs about pool membership at next boot."),
	}
	if st.wipeWholeDisks {
		scope = "Wipe ENTIRE parent disks (recommended for RAID)"
		scopeBody = []string{
			headerStyle.Render(
				"Every byte of these whole disks is erased before mkfs.btrfs:"),
		}
		for _, p := range parents {
			dev := "/dev/" + p
			// Build the full set of partitions on this parent and split
			// them into "picked by user" and "collateral wipe".
			pickedSet := map[string]bool{}
			for _, d := range st.pool {
				if d.ParentDisk == p {
					pickedSet[d.Path] = true
				}
			}
			var picked, collateral []string
			if inv != nil {
				for _, c := range inv.ChildrenOf(p) {
					desc := fmt.Sprintf("%s (%s, %s%s)",
						c.Path, c.Size, dashIfEmpty(c.FSType),
						labelSuffix(c.Label))
					if pickedSet[c.Path] {
						picked = append(picked, desc)
					} else {
						collateral = append(collateral, desc)
					}
				}
			}
			scopeBody = append(scopeBody,
				roleSystemStyle.Render("  "+dev+"  →  ERASE")+
					headerStyle.Render("  contains:"))
			for _, line := range picked {
				scopeBody = append(scopeBody, headerStyle.Render("    · "+line+"  (picked)"))
			}
			for _, line := range collateral {
				scopeBody = append(scopeBody, roleSystemStyle.Render("    · "+line+"  ⚠ also erased"))
			}
		}
		scopeBody = append(scopeBody,
			headerStyle.Render("mkfs.btrfs will use the whole disks ("+
				strings.Join(prefixDevSlice(parents), ", ")+") directly."))
	}
	mark := "[ ]"
	if st.wipeWholeDisks {
		mark = roleSystemStyle.Render("[✓]")
	}
	rows = append(rows, "Wipe scope:   "+mark+" "+scope+"   "+headerStyle.Render("(w to toggle)"))
	for _, l := range scopeBody {
		rows = append(rows, "              "+l)
	}
	rows = append(rows, "")

	rows = append(rows, "RAID level:   "+roleAvailableStyle.Render(string(st.raidLevel))+
		"  ("+st.raidLevel.Tolerates()+")")
	rows = append(rows, "mkfs:         "+
		fmt.Sprintf("mkfs.btrfs -d %s -m %s", st.raidLevel, st.raidLevel.MetadataProfile()))
	rows = append(rows, "Will appear:  "+devNameStyle.Render(mp))
	rows = append(rows, "ACL grant:    jellyfin user gets read access")
	rows = append(rows, "fstab:        UUID-based, mounts the pool via the first device")
	rows = append(rows, "Starter dirs: "+roleAvailableStyle.Render("[✓]")+" "+
		fmt.Sprintf("%d folders created under JellyfinMedia/", len(drives.StarterFolders)))
	rows = append(rows, "              "+headerStyle.Render(strings.Join(drives.StarterFolders, ", ")))
	rows = append(rows, "")
	yes := "  Yes, create pool  "
	no := "  Cancel  "
	if st.confirmIdx == 0 {
		yes = roleAvailableStyle.Render("▸" + yes)
		no = "  " + no
	} else {
		yes = "  " + yes
		no = roleSystemStyle.Render("▸" + no)
	}
	rows = append(rows, yes+"   "+no)
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("←/→ move · w wipe scope · enter confirm · y/n shortcut · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func labelSuffix(l string) string {
	if l == "" {
		return ""
	}
	return ", label " + strconv.Quote(l)
}

func prefixDevSlice(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "/dev/" + n
	}
	return out
}

func renderAddDriveFormat(st *addDriveState) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Filesystem"))
	rows = append(rows, "")
	rows = append(rows, fmt.Sprintf("Drive: %s  (%s)",
		devNameStyle.Render(st.selected.Path), st.selected.Size))
	state := dashIfEmpty(st.selected.FSType)
	if st.selected.Label != "" {
		state = fmt.Sprintf("%s, label %q", state, st.selected.Label)
	}
	rows = append(rows, "Current state: "+state)
	rows = append(rows, "")
	for i, opt := range st.formatOpts {
		marker := "  "
		title := opt.label
		body := headerStyle.Render(opt.hint)
		if i == st.formatIdx {
			marker = roleAvailableStyle.Render("▸ ")
			title = roleAvailableStyle.Render(opt.label)
		}
		rows = append(rows, marker+title)
		rows = append(rows, "    "+body)
		rows = append(rows, "")
	}
	if st.formatOpts[st.formatIdx].choice != drives.FormatKeep {
		rows = append(rows, roleSystemStyle.Render(
			"⚠ Formatting ERASES every file on this drive."))
		rows = append(rows, "")
	}
	rows = append(rows, footerStyle.Render("↑/↓ move · enter select · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderAddDrivePick(st *addDriveState) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Add media drive(s)"))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(
		"Pick ONE drive for a single mount, or TWO+ for a btrfs RAID pool."))
	rows = append(rows, headerStyle.Render(
		"Space toggles a row. Enter advances. With nothing toggled, enter picks the cursor row."))
	rows = append(rows, "")
	if len(st.candidates) == 0 {
		rows = append(rows, roleSystemStyle.Render("No eligible drives detected."))
		rows = append(rows,
			"Plug in a drive that isn't part of the boot disk and isn't already mounted.")
		rows = append(rows, "")
		rows = append(rows, footerStyle.Render("esc to cancel · r to refresh"))
		return centeredCard(strings.Join(rows, "\n"))
	}
	rows = append(rows, headerStyle.Render(fmt.Sprintf(
		"     %-18s %-7s %-12s %-14s %s",
		"Device", "Size", "Filesystem", "Label", "Model / kind",
	)))
	count := 0
	for i, d := range st.candidates {
		box := "[ ]"
		if st.pickSelected[i] {
			box = roleAvailableStyle.Render("[✓]")
			count++
		}
		fsCol := dashIfEmpty(d.FSType)
		labelCol := dashIfEmpty(d.Label)
		modelCol := d.Model
		if d.Type == "disk" {
			fsCol = "—"
			labelCol = "—"
			extra := "WHOLE DISK"
			if d.Model != "" {
				extra = "WHOLE DISK · " + d.Model
			}
			if d.Transport != "" {
				extra += " (" + d.Transport + ")"
			}
			modelCol = extra
		}
		row := fmt.Sprintf(
			"  %s %-18s %-7s %-12s %-14s %s",
			box, d.Path, d.Size,
			truncate(fsCol, 12),
			truncate(labelCol, 14),
			modelCol,
		)
		if i == st.pickIdx {
			row = roleAvailableStyle.Render("▸") + row[1:]
		}
		rows = append(rows, row)
	}
	rows = append(rows, "")
	switch count {
	case 0:
		rows = append(rows, headerStyle.Render("0 selected — enter will pick the cursor row (single-drive mode)."))
	case 1:
		rows = append(rows, "1 selected — "+roleAvailableStyle.Render("single-drive mode"))
	default:
		rows = append(rows, fmt.Sprintf("%d selected — ", count)+
			roleAvailableStyle.Render("RAID pool mode"))
	}
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("↑/↓ move · space toggle · enter continue · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderAddDriveRaidLevel(st *addDriveState) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Pool configuration"))
	rows = append(rows, "")
	rows = append(rows, fmt.Sprintf("%d drives selected:", len(st.pool)))
	for _, d := range st.pool {
		rows = append(rows, "  · "+devNameStyle.Render(d.Path)+"  ("+d.Size+", "+dashIfEmpty(d.FSType)+")")
	}
	rows = append(rows, "")
	rows = append(rows, roleSystemStyle.Render(
		"⚠ All selected drives will be ERASED and combined into a single btrfs filesystem."))
	rows = append(rows, "")
	for i, lvl := range st.raidLevels {
		marker := "  "
		head := raidLevelTitle(lvl)
		hint := raidLevelHint(lvl)
		caveat := raidLevelCaveat(lvl)
		if i == st.raidIdx {
			marker = roleAvailableStyle.Render("▸ ")
			head = roleAvailableStyle.Render(head)
		}
		rows = append(rows, marker+head)
		rows = append(rows, "    "+headerStyle.Render(hint))
		if caveat != "" {
			rows = append(rows, "    "+roleSystemStyle.Render(caveat))
		}
		rows = append(rows, "")
	}
	rows = append(rows, footerStyle.Render("↑/↓ move · enter select · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func raidLevelTitle(l drives.RAIDLevel) string {
	switch l {
	case drives.RAID0:
		return "RAID 0 (stripe)"
	case drives.RAID1:
		return "RAID 1 (mirror)"
	case drives.RAID5:
		return "RAID 5 (single parity)"
	case drives.RAID10:
		return "RAID 10 (mirror + stripe)"
	}
	return string(l)
}

func raidLevelHint(l drives.RAIDLevel) string {
	switch l {
	case drives.RAID0:
		return "Maximum capacity, no redundancy. One drive failing loses the whole pool."
	case drives.RAID1:
		return "Half the capacity, every byte exists twice. Any single drive can fail. Safest default."
	case drives.RAID5:
		return "One drive's worth of parity. Tolerates a single drive failure. Better capacity than RAID 1."
	case drives.RAID10:
		return "Mirrors striped together. One drive in each mirror can fail. Great for big libraries."
	}
	return ""
}

func raidLevelCaveat(l drives.RAIDLevel) string {
	if l == drives.RAID5 {
		return "Caveat: btrfs RAID 5/6 has a known write-hole issue on unclean shutdowns; ensure your box is on UPS."
	}
	return ""
}

func renderAddDriveName(user string, st *addDriveState) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Name this drive"))
	rows = append(rows, "")
	hdr := fmt.Sprintf("Selected: %s  (%s, %s)",
		devNameStyle.Render(st.selected.Path), st.selected.Size, dashIfEmpty(st.selected.FSType))
	if st.isPool() {
		hdr = fmt.Sprintf("Pool of %d drives, btrfs %s", len(st.pool), st.raidLevel)
	}
	rows = append(rows, hdr)
	rows = append(rows, "")
	rows = append(rows, "This becomes the folder name under /media/"+user+"/")
	rows = append(rows, headerStyle.Render(
		"e.g. \"MediaDrive\" → /media/"+user+"/MediaDrive"))
	rows = append(rows, "")
	rows = append(rows, "  "+st.name.View())
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("type a label · enter confirm · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderAddDriveConfirm(user string, st *addDriveState, inv *drives.Inventory) string {
	mp := filepath.Join("/media", user, st.name.Value())
	var rows []string
	rows = append(rows, titleStyle.Render("Mount this drive?"))
	rows = append(rows, "")
	rows = append(rows, fmt.Sprintf("Drive:        %s  (%s, %s)",
		devNameStyle.Render(st.selected.Path), st.selected.Size, dashIfEmpty(st.selected.FSType)))
	if st.formatAs != drives.FormatKeep {
		rows = append(rows, "Format:       "+
			roleSystemStyle.Render(fmt.Sprintf("ERASE and mkfs.%s", st.formatAs)))
	} else {
		rows = append(rows, "Format:       keep existing "+dashIfEmpty(st.selected.FSType))
	}

	// Existing-data preview — only on the Keep path, where there's data to
	// reassure about. Built from a temporary read-only mount at name→confirm.
	if st.formatAs == drives.FormatKeep && st.inspection != "" {
		label := "Existing data: "
		head := roleAvailableStyle.Render("kept as-is — nothing is erased")
		if !st.hasContent {
			head = headerStyle.Render("drive is empty")
		}
		rows = append(rows, label+head)
		for _, l := range strings.Split(strings.TrimRight(st.inspection, "\n"), "\n") {
			rows = append(rows, "              "+headerStyle.Render(l))
		}
	}

	// Wipe scope — three cases:
	//   1. Whole-disk Drive: wipe is mandatory, no toggle. List its partitions.
	//   2. Partition Drive, formatting: toggle between partition-only and whole-disk.
	//   3. Keep existing fs: no wipe line at all.
	if st.formatAs != drives.FormatKeep {
		wholeDiskDrive := st.selected.Type == "disk"
		if wholeDiskDrive {
			rows = append(rows, "Wipe scope:   "+roleSystemStyle.Render("[✓]")+
				" Entire disk "+devNameStyle.Render(st.selected.Path)+
				" — "+headerStyle.Render("mandatory (you picked a whole disk)"))
			if inv != nil {
				children := inv.ChildrenOf(st.selected.Name)
				if len(children) > 0 {
					rows = append(rows, "              "+
						roleSystemStyle.Render("Partitions on this disk that will be ERASED:"))
				}
				for _, c := range children {
					line := fmt.Sprintf("    · %s (%s, %s%s)",
						c.Path, c.Size, dashIfEmpty(c.FSType), labelSuffix(c.Label))
					rows = append(rows, "              "+roleSystemStyle.Render(line))
				}
			}
		} else if st.selected.ParentDisk != "" {
			mark := "[ ]"
			scope := "Format only the picked partition (" + st.selected.Path + ")"
			var detail []string
			if st.wipeWholeDisk {
				mark = roleSystemStyle.Render("[✓]")
				parent := "/dev/" + st.selected.ParentDisk
				scope = "Wipe ENTIRE parent disk " + parent + " first"
				detail = append(detail, headerStyle.Render(fmt.Sprintf(
					"Every byte of %s is erased; mkfs.%s and fstab use the whole disk.",
					parent, st.formatAs)))
				if inv != nil {
					children := inv.ChildrenOf(st.selected.ParentDisk)
					if len(children) > 0 {
						detail = append(detail, roleSystemStyle.Render("  Partitions on this disk that will be ERASED:"))
					}
					for _, c := range children {
						line := fmt.Sprintf("    · %s (%s, %s%s)",
							c.Path, c.Size, dashIfEmpty(c.FSType), labelSuffix(c.Label))
						if c.Path == st.selected.Path {
							detail = append(detail, headerStyle.Render(line+"  (picked)"))
						} else {
							detail = append(detail, roleSystemStyle.Render(line+"  ⚠ also erased"))
						}
					}
				}
			}
			rows = append(rows, "Wipe scope:   "+mark+" "+scope+"   "+headerStyle.Render("(w to toggle)"))
			for _, l := range detail {
				rows = append(rows, "              "+l)
			}
		}
	}

	rows = append(rows, "Will appear:  "+devNameStyle.Render(mp))
	aclLine := "jellyfin user gets read access"
	if st.formatAs == drives.FormatKeep && st.hasContent {
		aclLine = "jellyfin granted read access across ALL existing files (recursive)"
	}
	rows = append(rows, "ACL grant:    "+aclLine)
	rows = append(rows, "fstab:        new UUID-based entry added (backup taken first)")
	if st.formatAs == drives.FormatKeep && st.hasContent {
		rows = append(rows, "Starter dirs: "+headerStyle.Render("[ ] skipped — drive already has content"))
	} else {
		rows = append(rows, "Starter dirs: "+roleAvailableStyle.Render("[✓]")+" "+
			fmt.Sprintf("%d folders created under JellyfinMedia/", len(drives.StarterFolders)))
		rows = append(rows, "              "+headerStyle.Render(strings.Join(drives.StarterFolders, ", ")))
	}
	rows = append(rows, "")
	yes := "  Yes, mount it  "
	no := "  Cancel  "
	if st.confirmIdx == 0 {
		yes = roleAvailableStyle.Render("▸" + yes)
		no = "  " + no
	} else {
		yes = "  " + yes
		no = roleSystemStyle.Render("▸" + no)
	}
	rows = append(rows, yes+"   "+no)
	rows = append(rows, "")
	footer := "←/→ move · enter confirm · y/n shortcut · esc back"
	if st.formatAs != drives.FormatKeep {
		footer = "←/→ move · w wipe scope · enter confirm · y/n shortcut · esc back"
	}
	rows = append(rows, footerStyle.Render(footer))
	return centeredCard(strings.Join(rows, "\n"))
}
