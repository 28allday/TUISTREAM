// Manage-tab action flow: copy, delete, rename. All three drive a small
// state machine (manageStage) plus a shared file browser, then hand the
// final command off to the shared step runner on the root Model.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"tuistream/internal/drives"
	"tuistream/internal/step"
)

// manageStage is the current sub-state of the Manage tab. manageIdle is the
// default list view; manageActionMenu is the post-`c`/`d`/`n` flow. The
// "running" + "done" splashes live on the root Model and are shared with
// Setup.
type manageStage int

const (
	manageIdle manageStage = iota

	// Copy flow
	manageCopySrcDrive  // pick source (external) drive — skipped if exactly 1
	manageCopySrcBrowse // browse source for file/folder to copy
	manageCopyDstDrive  // pick destination (managed) drive — skipped if exactly 1
	manageCopyDstBrowse // browse destination for the folder to copy INTO
	manageCopyConfirm

	// Delete flow
	manageDeleteDrive  // pick a managed drive — skipped if exactly 1
	manageDeleteBrowse // browse managed drive to pick the path to delete
	manageDeleteConfirm

	// Rename flow
	manageRenameDrive  // pick a managed drive — skipped if exactly 1
	manageRenameBrowse // browse managed drive to pick the path to rename
	manageRenameInput  // textinput for the new basename
	manageRenameConfirm

	// Mount / unmount flows (headless boxes, no auto-mount daemon)
	manageMountPick
	manageMountConfirm
	manageUnmountPick
	manageUnmountConfirm
)

// manageAction is the action the user picked. It just disambiguates the
// drive-picker + browse sub-stages, which would otherwise need 3x as many
// enum values.
type manageActionKind int

const (
	manageActNone manageActionKind = iota
	manageActCopy
	manageActDelete
	manageActRename
	manageActMount
	manageActUnmount
)

// manageAction carries the action-in-flight state. Populated incrementally
// as the user advances through the sub-stages.
type manageAction struct {
	kind manageActionKind

	// Drive picker state — used both for the source-drive and dest-drive
	// stages of Copy, and for the single drive stage of Delete/Rename.
	driveChoices []drives.Drive
	driveIdx     int

	// Resolved drives.
	srcDrive drives.Drive // copy source
	dstDrive drives.Drive // copy destination, or the target of delete/rename

	// File browser per side. For Copy we use two; for Delete/Rename only dst.
	srcBrowse fileBrowser
	dstBrowse fileBrowser

	// Resolved paths.
	srcPath string // absolute path on the source side (Copy)
	dstPath string // absolute path on the destination side (Copy = folder; Delete/Rename = target itself)

	// Rename-only: textinput for the new basename + confirmation index.
	rename     textinput.Model
	confirmIdx int // 0 = Yes, 1 = No

	// Mount-only state.
	mountReadOnly bool   // toggled with 'w' on the confirm screen; default true
	mountPoint    string // computed mountpoint for the chosen drive
}

// manageModel holds Manage-tab-specific state.
type manageModel struct {
	stage  manageStage
	active manageAction
}

func newManageModel() manageModel { return manageModel{stage: manageIdle} }

// --- key handling ---

func handleManageKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	key := msg.String()
	switch m.manage.stage {
	case manageIdle:
		return manageIdleKey(m, key)

	case manageCopySrcDrive:
		return manageDrivePickerKey(m, key, manageCopySrcBrowse, manageActCopy, true)
	case manageCopySrcBrowse:
		return manageCopySrcBrowseKey(m, msg)
	case manageCopyDstDrive:
		return manageDrivePickerKey(m, key, manageCopyDstBrowse, manageActCopy, false)
	case manageCopyDstBrowse:
		return manageCopyDstBrowseKey(m, msg)
	case manageCopyConfirm:
		return manageCopyConfirmKey(m, key)

	case manageDeleteDrive:
		return manageDrivePickerKey(m, key, manageDeleteBrowse, manageActDelete, false)
	case manageDeleteBrowse:
		return manageDeleteBrowseKey(m, msg)
	case manageDeleteConfirm:
		return manageDeleteConfirmKey(m, key)

	case manageRenameDrive:
		return manageDrivePickerKey(m, key, manageRenameBrowse, manageActRename, false)
	case manageRenameBrowse:
		return manageRenameBrowseKey(m, msg)
	case manageRenameInput:
		return manageRenameInputKey(m, msg)
	case manageRenameConfirm:
		return manageRenameConfirmKey(m, key)

	case manageMountPick:
		return manageMountPickKey(m, key)
	case manageMountConfirm:
		return manageMountConfirmKey(m, key)
	case manageUnmountPick:
		return manageUnmountPickKey(m, key)
	case manageUnmountConfirm:
		return manageUnmountConfirmKey(m, key)
	}
	return m, nil, false
}

func manageIdleKey(m Model, key string) (Model, tea.Cmd, bool) {
	if m.inventory == nil {
		return m, nil, false
	}
	switch key {
	case "c":
		return startCopy(m)
	case "d":
		return startDelete(m)
	case "n":
		return startRename(m)
	case "m":
		return startMount(m)
	case "e":
		return startUnmount(m)
	}
	return m, nil, false
}

// --- Copy: kickoff + drive picker ---

func startCopy(m Model) (Model, tea.Cmd, bool) {
	ext := m.inventory.External()
	if len(ext) == 0 {
		m.flash = "No external drives mounted. Plug one in and press 'r'."
		return m, nil, true
	}
	managed := m.inventory.Managed()
	if len(managed) == 0 {
		m.flash = "No managed media drives. Add one from the Setup tab first."
		return m, nil, true
	}
	m.manage.active = manageAction{kind: manageActCopy, driveChoices: ext}
	if len(ext) == 1 {
		m.manage.active.srcDrive = ext[0]
		fb, err := newFileBrowser(ext[0].MountPoint)
		if err != nil {
			m.flash = "Couldn't open " + ext[0].MountPoint + ": " + err.Error()
			return m, nil, true
		}
		m.manage.active.srcBrowse = fb
		m.manage.stage = manageCopySrcBrowse
		return m, nil, true
	}
	m.manage.stage = manageCopySrcDrive
	return m, nil, true
}

// manageDrivePickerKey is shared by every "pick a drive" sub-stage. The
// `next` arg is the sub-stage to move to after the user picks. `isCopySrc`
// tells us whether to seed srcDrive (true) or dstDrive (false).
func manageDrivePickerKey(m Model, key string, next manageStage, kind manageActionKind, isCopySrc bool) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	switch key {
	case "up", "k":
		if a.driveIdx > 0 {
			a.driveIdx--
		}
		return m, nil, true
	case "down", "j":
		if a.driveIdx < len(a.driveChoices)-1 {
			a.driveIdx++
		}
		return m, nil, true
	case "esc":
		return manageCancel(m), nil, true
	case "enter":
		if a.driveIdx < 0 || a.driveIdx >= len(a.driveChoices) {
			return m, nil, true
		}
		chosen := a.driveChoices[a.driveIdx]
		fb, err := newFileBrowser(chosen.MountPoint)
		if err != nil {
			m.flash = "Couldn't open " + chosen.MountPoint + ": " + err.Error()
			return manageCancel(m), nil, true
		}
		if isCopySrc {
			a.srcDrive = chosen
			a.srcBrowse = fb
		} else {
			a.dstDrive = chosen
			a.dstBrowse = fb
		}
		_ = kind // kept for future per-action divergence
		m.manage.stage = next
		return m, nil, true
	}
	return m, nil, false
}

// manageCancel returns the user to the idle Manage view and clears any
// in-flight action state.
func manageCancel(m Model) Model {
	m.manage.stage = manageIdle
	m.manage.active = manageAction{}
	return m
}

// --- Copy: source browser ---

func manageCopySrcBrowseKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	if k := msg.String(); k == "tab" || k == "shift+tab" {
		return m, nil, false
	}
	res, cmd := a.srcBrowse.update(msg)
	switch res {
	case browseStillBrowsing:
		return m, cmd, true
	case browseCancelled:
		return manageCancel(m), nil, true
	case browseSelected:
		// User picked something to copy. Advance to destination drive.
		a.srcPath = a.srcBrowse.SelectedPath()
		managed := m.inventory.Managed()
		a.driveChoices = managed
		a.driveIdx = 0
		if len(managed) == 1 {
			a.dstDrive = managed[0]
			fb, err := newFileBrowser(managed[0].MountPoint)
			if err != nil {
				m.flash = "Couldn't open " + managed[0].MountPoint + ": " + err.Error()
				return manageCancel(m), nil, true
			}
			a.dstBrowse = fb
			m.manage.stage = manageCopyDstBrowse
			return m, nil, true
		}
		m.manage.stage = manageCopyDstDrive
		return m, nil, true
	}
	return m, cmd, true
}

// --- Copy: destination browser ---

func manageCopyDstBrowseKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	if k := msg.String(); k == "tab" || k == "shift+tab" {
		return m, nil, false
	}
	// In the dst browser, Enter on a folder dives in (default), Space SELECTS
	// the current directory as the destination — we want "copy INTO this folder".
	if msg.String() == " " || msg.String() == "space" {
		a.dstPath = a.dstBrowse.cwd
		m.manage.stage = manageCopyConfirm
		a.confirmIdx = 0
		return m, nil, true
	}
	res, cmd := a.dstBrowse.update(msg)
	switch res {
	case browseStillBrowsing:
		return m, cmd, true
	case browseCancelled:
		return manageCancel(m), nil, true
	case browseSelected:
		// Selecting an item in the destination browser means "copy into this
		// folder" — so if it's a file we still target its parent dir.
		sel := a.dstBrowse.SelectedPath()
		if isDir(sel) {
			a.dstPath = sel
		} else {
			a.dstPath = filepath.Dir(sel)
		}
		m.manage.stage = manageCopyConfirm
		a.confirmIdx = 0
		return m, nil, true
	}
	return m, cmd, true
}

// --- Copy: confirm + run ---

func manageCopyConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "left", "h":
		m.manage.active.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		m.manage.active.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		m.manage.active.confirmIdx = 0
		return runCopy(m)
	case "n", "N", "esc":
		return manageCancel(m), nil, true
	case "enter":
		if m.manage.active.confirmIdx == 0 {
			return runCopy(m)
		}
		return manageCancel(m), nil, true
	}
	return m, nil, false
}

func runCopy(m Model) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	src := a.srcPath
	dstDir := a.dstPath
	// Guard against copying onto self.
	if src == dstDir || strings.HasPrefix(dstDir, src+string(os.PathSeparator)) {
		m.flash = "Refusing to copy a folder into itself."
		return manageCancel(m), nil, true
	}
	plan := []step.Step{
		{
			Title: "Ensure destination exists: " + dstDir,
			Cmd:   exec.Command("mkdir", "-p", dstDir),
		},
		{
			Title: fmt.Sprintf("rsync %s → %s/", filepath.Base(src), dstDir),
			Cmd:   exec.Command("rsync", "-a", "--info=progress2", src, dstDir+string(os.PathSeparator)),
		},
		{
			Title: "Reapply ACL on destination",
			Cmd:   exec.Command("setfacl", "-R", "-m", "u:jellyfin:rX", dstDir),
		},
	}
	m.run = planRun{
		title: "Copying " + src,
		steps: plan,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabManage
	return m, runStepCmd(plan[0]), true
}

// --- Delete: kickoff + browser ---

func startDelete(m Model) (Model, tea.Cmd, bool) {
	managed := m.inventory.Managed()
	if len(managed) == 0 {
		m.flash = "No managed media drives to delete from."
		return m, nil, true
	}
	m.manage.active = manageAction{kind: manageActDelete, driveChoices: managed}
	if len(managed) == 1 {
		m.manage.active.dstDrive = managed[0]
		fb, err := newFileBrowser(managed[0].MountPoint)
		if err != nil {
			m.flash = "Couldn't open " + managed[0].MountPoint + ": " + err.Error()
			return m, nil, true
		}
		m.manage.active.dstBrowse = fb
		m.manage.stage = manageDeleteBrowse
		return m, nil, true
	}
	m.manage.stage = manageDeleteDrive
	return m, nil, true
}

func manageDeleteBrowseKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	if k := msg.String(); k == "tab" || k == "shift+tab" {
		return m, nil, false
	}
	res, cmd := a.dstBrowse.update(msg)
	switch res {
	case browseStillBrowsing:
		return m, cmd, true
	case browseCancelled:
		return manageCancel(m), nil, true
	case browseSelected:
		sel := a.dstBrowse.SelectedPath()
		if sel == "" {
			m.flash = "Pick a file or folder first."
			return m, nil, true
		}
		if !isUnderRoot(sel, a.dstDrive.MountPoint) {
			m.flash = "Refusing to delete outside the drive root."
			return manageCancel(m), nil, true
		}
		if sel == a.dstDrive.MountPoint {
			m.flash = "Refusing to delete the drive root."
			return m, nil, true
		}
		a.dstPath = sel
		m.manage.stage = manageDeleteConfirm
		a.confirmIdx = 1 // default No for destructive op
		return m, nil, true
	}
	return m, cmd, true
}

func manageDeleteConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "left", "h":
		m.manage.active.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		m.manage.active.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		m.manage.active.confirmIdx = 0
		return runDelete(m)
	case "n", "N", "esc":
		return manageCancel(m), nil, true
	case "enter":
		if m.manage.active.confirmIdx == 0 {
			return runDelete(m)
		}
		return manageCancel(m), nil, true
	}
	return m, nil, false
}

func runDelete(m Model) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	if a.dstPath == "" || a.dstPath == a.dstDrive.MountPoint {
		m.flash = "Refusing to delete the drive root."
		return manageCancel(m), nil, true
	}
	plan := []step.Step{
		{
			Title: "Delete: " + a.dstPath,
			Cmd:   exec.Command("rm", "-rf", "--", a.dstPath),
		},
	}
	m.run = planRun{
		title: "Deleting " + a.dstPath,
		steps: plan,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabManage
	return m, runStepCmd(plan[0]), true
}

// --- Rename: kickoff, browser, input, confirm ---

func startRename(m Model) (Model, tea.Cmd, bool) {
	managed := m.inventory.Managed()
	if len(managed) == 0 {
		m.flash = "No managed media drives to rename in."
		return m, nil, true
	}
	m.manage.active = manageAction{kind: manageActRename, driveChoices: managed}
	if len(managed) == 1 {
		m.manage.active.dstDrive = managed[0]
		fb, err := newFileBrowser(managed[0].MountPoint)
		if err != nil {
			m.flash = "Couldn't open " + managed[0].MountPoint + ": " + err.Error()
			return m, nil, true
		}
		m.manage.active.dstBrowse = fb
		m.manage.stage = manageRenameBrowse
		return m, nil, true
	}
	m.manage.stage = manageRenameDrive
	return m, nil, true
}

func manageRenameBrowseKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	if k := msg.String(); k == "tab" || k == "shift+tab" {
		return m, nil, false
	}
	res, cmd := a.dstBrowse.update(msg)
	switch res {
	case browseStillBrowsing:
		return m, cmd, true
	case browseCancelled:
		return manageCancel(m), nil, true
	case browseSelected:
		sel := a.dstBrowse.SelectedPath()
		if sel == "" || sel == a.dstDrive.MountPoint {
			m.flash = "Pick a file or folder (not the drive root)."
			return m, nil, true
		}
		a.dstPath = sel
		ti := textinput.New()
		ti.Prompt = ""
		ti.CharLimit = 128
		ti.Width = 48
		ti.SetValue(filepath.Base(sel))
		ti.CursorEnd()
		ti.Focus()
		a.rename = ti
		m.manage.stage = manageRenameInput
		return m, nil, true
	}
	return m, cmd, true
}

func manageRenameInputKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	switch msg.String() {
	case "esc":
		return manageCancel(m), nil, true
	case "enter":
		newName := strings.TrimSpace(a.rename.Value())
		if newName == "" || strings.ContainsRune(newName, '/') {
			m.flash = "Name can't be empty or contain '/'."
			return m, nil, true
		}
		if newName == filepath.Base(a.dstPath) {
			m.flash = "New name is the same — nothing to do."
			return m, nil, true
		}
		newPath := filepath.Join(filepath.Dir(a.dstPath), newName)
		if _, err := os.Stat(newPath); err == nil {
			m.flash = "A file/folder named '" + newName + "' already exists here."
			return m, nil, true
		}
		m.manage.stage = manageRenameConfirm
		a.confirmIdx = 0
		return m, nil, true
	case "tab", "shift+tab":
		return m, nil, false // let the root model handle tab-switch
	}
	var cmd tea.Cmd
	a.rename, cmd = a.rename.Update(msg)
	return m, cmd, true
}

func manageRenameConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "left", "h":
		m.manage.active.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		m.manage.active.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		m.manage.active.confirmIdx = 0
		return runRename(m)
	case "n", "N", "esc":
		return manageCancel(m), nil, true
	case "enter":
		if m.manage.active.confirmIdx == 0 {
			return runRename(m)
		}
		return manageCancel(m), nil, true
	}
	return m, nil, false
}

func runRename(m Model) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	newName := strings.TrimSpace(a.rename.Value())
	newPath := filepath.Join(filepath.Dir(a.dstPath), newName)
	plan := []step.Step{
		{
			Title: fmt.Sprintf("Rename %s → %s", filepath.Base(a.dstPath), newName),
			Cmd:   exec.Command("mv", "--", a.dstPath, newPath),
		},
	}
	m.run = planRun{
		title: "Renaming " + a.dstPath,
		steps: plan,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabManage
	return m, runStepCmd(plan[0]), true
}

// --- Mount: kickoff, picker, confirm, run ---

func startMount(m Model) (Model, tea.Cmd, bool) {
	cands := m.inventory.Mountable()
	if len(cands) == 0 {
		m.flash = "No mountable drives detected. Plug one in and press 'r'."
		return m, nil, true
	}
	m.manage.active = manageAction{
		kind:          manageActMount,
		driveChoices:  cands,
		mountReadOnly: true,
	}
	m.manage.stage = manageMountPick
	return m, nil, true
}

func manageMountPickKey(m Model, key string) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	switch key {
	case "up", "k":
		if a.driveIdx > 0 {
			a.driveIdx--
		}
		return m, nil, true
	case "down", "j":
		if a.driveIdx < len(a.driveChoices)-1 {
			a.driveIdx++
		}
		return m, nil, true
	case "esc":
		return manageCancel(m), nil, true
	case "enter":
		if a.driveIdx < 0 || a.driveIdx >= len(a.driveChoices) {
			return m, nil, true
		}
		a.srcDrive = a.driveChoices[a.driveIdx]
		a.mountPoint = filepath.Join("/run/media", m.username, mountSlug(a.srcDrive))
		m.manage.stage = manageMountConfirm
		a.confirmIdx = 0
		return m, nil, true
	}
	return m, nil, false
}

func manageMountConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	switch key {
	case "left", "h":
		a.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		a.confirmIdx = 1
		return m, nil, true
	case "w":
		a.mountReadOnly = !a.mountReadOnly
		return m, nil, true
	case "y", "Y":
		a.confirmIdx = 0
		return runMount(m)
	case "n", "N", "esc":
		return manageCancel(m), nil, true
	case "enter":
		if a.confirmIdx == 0 {
			return runMount(m)
		}
		return manageCancel(m), nil, true
	}
	return m, nil, false
}

func runMount(m Model) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	mountOpts := "ro"
	if !a.mountReadOnly {
		mountOpts = "rw"
	}
	// Many removable drives (USB-attached MS-DOS-style partition tables on
	// vfat / exfat) prefer the calling user owning the files. Pass uid/gid
	// where the FS supports those mount options.
	uid, gid := callerUIDGID(m.username)
	switch a.srcDrive.FSType {
	case "vfat", "exfat", "ntfs", "ntfs3":
		mountOpts += fmt.Sprintf(",uid=%d,gid=%d", uid, gid)
	}
	plan := []step.Step{
		{
			Title: "Create mountpoint: " + a.mountPoint,
			Cmd:   exec.Command("mkdir", "-p", a.mountPoint),
		},
		{
			Title: fmt.Sprintf("mount -o %s %s → %s", mountOpts, a.srcDrive.Path, a.mountPoint),
			Cmd:   exec.Command("mount", "-o", mountOpts, a.srcDrive.Path, a.mountPoint),
		},
	}
	m.run = planRun{
		title: "Mounting " + a.srcDrive.Path,
		steps: plan,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabManage
	return m, runStepCmd(plan[0]), true
}

// --- Unmount: kickoff, picker, confirm, run ---

func startUnmount(m Model) (Model, tea.Cmd, bool) {
	ext := m.inventory.External()
	if len(ext) == 0 {
		m.flash = "No external drives are mounted. Nothing to eject."
		return m, nil, true
	}
	m.manage.active = manageAction{
		kind:         manageActUnmount,
		driveChoices: ext,
	}
	m.manage.stage = manageUnmountPick
	return m, nil, true
}

func manageUnmountPickKey(m Model, key string) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	switch key {
	case "up", "k":
		if a.driveIdx > 0 {
			a.driveIdx--
		}
		return m, nil, true
	case "down", "j":
		if a.driveIdx < len(a.driveChoices)-1 {
			a.driveIdx++
		}
		return m, nil, true
	case "esc":
		return manageCancel(m), nil, true
	case "enter":
		if a.driveIdx < 0 || a.driveIdx >= len(a.driveChoices) {
			return m, nil, true
		}
		a.srcDrive = a.driveChoices[a.driveIdx]
		a.mountPoint = a.srcDrive.MountPoint
		m.manage.stage = manageUnmountConfirm
		a.confirmIdx = 0
		return m, nil, true
	}
	return m, nil, false
}

func manageUnmountConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	switch key {
	case "left", "h":
		a.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		a.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		a.confirmIdx = 0
		return runUnmount(m)
	case "n", "N", "esc":
		return manageCancel(m), nil, true
	case "enter":
		if a.confirmIdx == 0 {
			return runUnmount(m)
		}
		return manageCancel(m), nil, true
	}
	return m, nil, false
}

func runUnmount(m Model) (Model, tea.Cmd, bool) {
	a := &m.manage.active
	plan := []step.Step{
		{
			Title: "sync — flush pending writes",
			Cmd:   exec.Command("sync"),
		},
		{
			Title: "umount " + a.mountPoint,
			Cmd:   exec.Command("umount", "-v", a.mountPoint),
		},
	}
	// If the mountpoint is one we created under /run/media/<user>/, tidy
	// it up. The kernel can hold the dentry briefly after umount (rmdir
	// returns EBUSY) and stale "ghost" mount entries from drives that
	// were unplugged without unmounting also cause EBUSY — so this step
	// is best-effort: sleeps briefly, tries rmdir, and never fails the
	// plan. The eject is considered successful as soon as umount returns
	// 0.
	if strings.HasPrefix(a.mountPoint, "/run/media/") {
		plan = append(plan, step.Step{
			Title: "Tidy mountpoint (best-effort): " + a.mountPoint,
			Cmd: exec.Command("sh", "-c", fmt.Sprintf(
				"sleep 0.3; rmdir %q 2>/dev/null; true",
				a.mountPoint,
			)),
		})
	}
	m.run = planRun{
		title: "Ejecting " + a.srcDrive.Path,
		steps: plan,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabManage
	return m, runStepCmd(plan[0]), true
}

// mountSlug picks a stable, filesystem-safe directory name for a drive:
// the label if it's clean, else "<device>-<uuid-prefix>".
func mountSlug(d drives.Drive) string {
	if d.Label != "" {
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
	if d.UUID != "" && len(d.UUID) >= 8 {
		return filepath.Base(d.Path) + "-" + d.UUID[:8]
	}
	return filepath.Base(d.Path)
}

// callerUIDGID resolves a username to its numeric uid/gid. Falls back to
// 1000/1000 if the lookup fails — same default Arch's first-user setup uses.
func callerUIDGID(username string) (int, int) {
	if username == "" {
		return 1000, 1000
	}
	u, err := user.Lookup(username)
	if err != nil {
		return 1000, 1000
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 1000, 1000
	}
	return uid, gid
}

// --- small helpers ---

func isDir(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.IsDir()
}

func isUnderRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}
