package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"tuistream/internal/drives"
)

// Sub-stages of the Import-existing-pool flow, switched via importPoolState.
type importSubStage int

const (
	importPick    importSubStage = iota // choose which detached pool (skipped if only one)
	importName                          // text-input the mountpoint label
	importConfirm                       // last-chance Yes/No before mounting
)

// importPoolState carries the import sub-flow's state across Update calls.
type importPoolState struct {
	subStage   importSubStage
	pools      []drives.DetachedPool
	pickIdx    int
	selected   drives.DetachedPool
	name       textinput.Model
	confirmIdx int    // 0 = Yes, 1 = No
	inspection string // existing-data summary (from a temporary RO mount)
	hasContent bool   // pool already holds real data
}

// startImportPool enters the import flow. With no detached pools it flashes and
// stays on the menu; with exactly one it skips the picker and goes to naming.
func startImportPool(m Model) (Model, tea.Cmd, bool) {
	var pools []drives.DetachedPool
	if m.inventory != nil {
		pools = m.inventory.DetachedPools()
	}
	if len(pools) == 0 {
		m.flash = "No detached btrfs pools found to import."
		return m, nil, true
	}

	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "MediaPool"
	ti.CharLimit = 32
	ti.Width = 32
	ti.Focus()

	st := importPoolState{pools: pools, name: ti}
	if len(pools) == 1 {
		st.selected = pools[0]
		st.subStage = importName
		st.name.SetValue(defaultPoolLabel(pools[0]))
		st.name.CursorEnd()
	} else {
		st.subStage = importPick
	}
	m.setup.importPool = st
	m.setup.stage = stageImportPool
	return m, nil, true
}

// defaultPoolLabel sanitises the pool's btrfs label into a directory name.
func defaultPoolLabel(p drives.DetachedPool) string {
	if l := defaultLabelFrom(p.Label); l != "" {
		return l
	}
	return "MediaPool"
}

// defaultLabelFrom reuses defaultLabelFor's sanitiser on a bare label string.
func defaultLabelFrom(label string) string {
	return defaultLabelFor(drives.Drive{Label: label})
}

// --- key handling ---

func setupImportPoolKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	st := &m.setup.importPool
	switch st.subStage {
	case importPick:
		return importPoolPickKey(m, msg.String())
	case importName:
		return importPoolNameKey(m, msg)
	case importConfirm:
		return importPoolConfirmKey(m, msg.String())
	}
	return m, nil, false
}

func importPoolPickKey(m Model, key string) (Model, tea.Cmd, bool) {
	st := &m.setup.importPool
	switch key {
	case "up", "k":
		if st.pickIdx > 0 {
			st.pickIdx--
		}
		return m, nil, true
	case "down", "j":
		if st.pickIdx < len(st.pools)-1 {
			st.pickIdx++
		}
		return m, nil, true
	case "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		st.selected = st.pools[st.pickIdx]
		st.subStage = importName
		st.name.SetValue(defaultPoolLabel(st.selected))
		st.name.CursorEnd()
		return m, nil, true
	}
	return m, nil, false
}

func importPoolNameKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	st := &m.setup.importPool
	switch msg.String() {
	case "esc":
		// Back to the picker if there was a choice, else out to the menu.
		if len(st.pools) > 1 {
			st.subStage = importPick
		} else {
			m.setup.stage = stageIdle
		}
		return m, nil, true
	case "enter":
		if !validLabel(st.name.Value()) {
			m.flash = "Use only letters, digits, '-' and '_' in the label."
			return m, nil, true
		}
		// Preview the pool's contents via a temporary read-only mount so the
		// confirm screen can reassure the user and we can decide whether to
		// seed starter folders.
		st.inspection, st.hasContent = drives.InspectDevice(st.selected.FirstDevice())
		st.subStage = importConfirm
		st.confirmIdx = 0
		return m, nil, true
	case "tab", "shift+tab":
		return m, nil, false
	}
	var cmd tea.Cmd
	st.name, cmd = st.name.Update(msg)
	return m, cmd, true
}

func importPoolConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	st := &m.setup.importPool
	switch key {
	case "left", "h":
		st.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		st.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		st.confirmIdx = 0
		return runImportPool(m)
	case "n", "N", "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		if st.confirmIdx == 0 {
			return runImportPool(m)
		}
		m.setup.stage = stageIdle
		return m, nil, true
	}
	return m, nil, false
}

func runImportPool(m Model) (Model, tea.Cmd, bool) {
	st := &m.setup.importPool
	plan, err := drives.ImportPoolPlan(drives.ImportPoolOptions{
		Pool:  st.selected,
		Label: st.name.Value(),
		User:  m.username,
		// Don't litter an existing library with empty starter folders.
		SeedStarterFolders: !st.hasContent,
		// Pre-existing pool media may have been copied with tight perms — grant
		// jellyfin read over the whole tree, not just the mount root.
		RecursiveACL: st.hasContent,
	})
	if err != nil {
		m.flash = "Couldn't plan the import: " + err.Error()
		m.setup.stage = stageIdle
		return m, nil, true
	}
	m.setup.stage = stageIdle
	m.run = planRun{
		title: fmt.Sprintf("Importing btrfs pool at /media/%s/%s", m.username, st.name.Value()),
		steps: plan,
		index: 0,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabSetup
	return m, runStepCmd(plan[0]), true
}

// --- rendering ---

func renderImportPool(m Model) string {
	st := &m.setup.importPool
	switch st.subStage {
	case importPick:
		return renderImportPoolPick(st)
	case importName:
		return renderImportPoolName(m.username, st)
	case importConfirm:
		return renderImportPoolConfirm(m.username, st)
	}
	return ""
}

func renderImportPoolPick(st *importPoolState) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Import existing btrfs pool"))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(
		"These pools were found whole and unmounted. Importing mounts one as-is — nothing is erased."))
	rows = append(rows, "")
	for i, p := range st.pools {
		marker := "  "
		head := fmt.Sprintf("%s  (%d devices, %s)",
			p.DisplayLabel(), len(p.Members), devNameStyle.Render(shortUUID(p.UUID)))
		if i == st.pickIdx {
			marker = roleAvailableStyle.Render("▸ ")
			head = roleAvailableStyle.Render(head)
		}
		rows = append(rows, marker+head)
		for _, d := range p.Members {
			rows = append(rows, "      "+headerStyle.Render("· "+d.Path+"  ("+d.Size+")"))
		}
		rows = append(rows, "")
	}
	rows = append(rows, footerStyle.Render("↑/↓ move · enter select · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderImportPoolName(user string, st *importPoolState) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Name this pool"))
	rows = append(rows, "")
	rows = append(rows, fmt.Sprintf("Pool: %s  (%d devices, btrfs)",
		st.selected.DisplayLabel(), len(st.selected.Members)))
	rows = append(rows, "")
	rows = append(rows, "This becomes the folder name under /media/"+user+"/")
	rows = append(rows, headerStyle.Render(
		"e.g. \"MediaPool\" → /media/"+user+"/MediaPool"))
	rows = append(rows, "")
	rows = append(rows, "  "+st.name.View())
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("type a label · enter confirm · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderImportPoolConfirm(user string, st *importPoolState) string {
	mp := filepath.Join("/media", user, st.name.Value())
	var rows []string
	rows = append(rows, titleStyle.Render("Import this pool?"))
	rows = append(rows, "")
	rows = append(rows, "Pool:         "+st.selected.DisplayLabel()+
		"  ("+devNameStyle.Render(shortUUID(st.selected.UUID))+")")
	rows = append(rows, "Member devices:")
	for _, d := range st.selected.Members {
		rows = append(rows, "  · "+devNameStyle.Render(d.Path)+"  ("+d.Size+")")
	}
	rows = append(rows, "Action:       "+roleAvailableStyle.Render("mount as-is — nothing is formatted or erased"))

	if st.inspection != "" {
		label := "Existing data: "
		head := roleAvailableStyle.Render("kept as-is")
		if !st.hasContent {
			head = headerStyle.Render("pool is empty")
		}
		rows = append(rows, label+head)
		for _, l := range strings.Split(strings.TrimRight(st.inspection, "\n"), "\n") {
			rows = append(rows, "              "+headerStyle.Render(l))
		}
	}

	rows = append(rows, "Will appear:  "+devNameStyle.Render(mp))
	acl := "jellyfin user gets read access"
	if st.hasContent {
		acl = "jellyfin granted read access across ALL existing files (recursive)"
	}
	rows = append(rows, "ACL grant:    "+acl)
	rows = append(rows, "fstab:        UUID-based, mounts the pool via the first device")
	starter := roleAvailableStyle.Render("[✓]") + " " +
		fmt.Sprintf("%d folders created under JellyfinMedia/", len(drives.StarterFolders))
	if st.hasContent {
		starter = headerStyle.Render("[ ] skipped — pool already has content")
	}
	rows = append(rows, "Starter dirs: "+starter)
	rows = append(rows, "")
	yes := "  Yes, import it  "
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
	rows = append(rows, footerStyle.Render("←/→ move · enter confirm · y/n shortcut · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

// shortUUID trims a UUID to its first segment for compact display.
func shortUUID(u string) string {
	if i := strings.IndexByte(u, '-'); i > 0 {
		return u[:i] + "…"
	}
	return u
}
