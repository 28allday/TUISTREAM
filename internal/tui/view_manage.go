package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"tuistream/internal/drives"
)

func (mn manageModel) view(m Model) string {
	if m.inventoryErr != nil {
		return centeredCard(
			titleStyle.Render("Couldn't read drives") + "\n\n" +
				m.inventoryErr.Error(),
		)
	}
	if m.inventory == nil {
		return centeredCard(headerStyle.Render("Loading drives…"))
	}

	switch mn.stage {
	case manageCopySrcDrive:
		return renderManageDrivePicker(&mn.active, "Pick the SOURCE drive",
			"The drive containing the file/folder you want to copy.")
	case manageCopySrcBrowse:
		return mn.active.srcBrowse.view(
			"Pick what to copy from "+mn.active.srcDrive.MountPoint,
			"↑/↓ nav · ↵ open/pick · s pick here · ← up · esc cancel",
		)
	case manageCopyDstDrive:
		return renderManageDrivePicker(&mn.active, "Pick the DESTINATION drive",
			"The managed media drive to copy into.")
	case manageCopyDstBrowse:
		return mn.active.dstBrowse.view(
			"Browse "+mn.active.dstDrive.MountPoint+" — pick destination folder",
			"↑/↓ nav · ↵ open · space copy HERE · s pick cursor · ← up · esc cancel",
		)
	case manageCopyConfirm:
		return renderCopyConfirm(&mn.active)

	case manageDeleteDrive:
		return renderManageDrivePicker(&mn.active, "Pick the drive to delete from",
			"The managed media drive that contains the file/folder you want to delete.")
	case manageDeleteBrowse:
		return mn.active.dstBrowse.view(
			"Pick what to DELETE from "+mn.active.dstDrive.MountPoint,
			"↑/↓ nav · ↵ open · s pick cursor · ← up · esc cancel",
		)
	case manageDeleteConfirm:
		return renderDeleteConfirm(&mn.active)

	case manageRenameDrive:
		return renderManageDrivePicker(&mn.active, "Pick the drive to rename inside",
			"The managed media drive whose file/folder you want to rename.")
	case manageRenameBrowse:
		return mn.active.dstBrowse.view(
			"Pick what to rename in "+mn.active.dstDrive.MountPoint,
			"↑/↓ nav · ↵ open · s pick cursor · ← up · esc cancel",
		)
	case manageRenameInput:
		return renderRenameInput(&mn.active)
	case manageRenameConfirm:
		return renderRenameConfirm(&mn.active)

	case manageMountPick:
		return renderMountPick(&mn.active)
	case manageMountConfirm:
		return renderMountConfirm(&mn.active)
	case manageUnmountPick:
		return renderUnmountPick(&mn.active)
	case manageUnmountConfirm:
		return renderUnmountConfirm(&mn.active)
	}

	// Idle: managed + external + mountable drive lists + action shortcuts.
	heading := titleStyle.Render("Manage media")
	managed := m.inventory.Managed()
	external := m.inventory.External()
	mountable := m.inventory.Mountable()
	subhead := manageSummary(len(managed), len(external), len(mountable))

	mediaList := renderDriveList(
		"Your media drives",
		managed,
		"No media drives yet — add one from the Setup tab.",
	)
	extList := renderDriveList(
		"Plugged-in drives  (copy your library from these)",
		external,
		"Nothing plugged in. Press 'm' to mount a drive, or connect a USB drive.",
	)
	mountList := renderMountableList(mountable)

	actions := renderManageActionBar(len(managed) > 0, len(external) > 0, len(mountable) > 0, cardWidth(m.width))

	w := cardWidth(m.width)
	return lipgloss.JoinVertical(lipgloss.Center,
		"",
		centered(heading, w),
		centered(subhead, w),
		"",
		mediaList,
		"",
		extList,
		"",
		mountList,
		"",
		centered(actions, w),
	)
}

// manageSummary is the plain-English subheading under "Manage media".
func manageSummary(managed, external, mountable int) string {
	parts := []string{
		countLabel(managed, "media drive", "media drives"),
		countLabel(external, "drive plugged in", "drives plugged in"),
	}
	if mountable > 0 {
		parts = append(parts, countLabel(mountable, "drive ready to mount", "drives ready to mount"))
	}
	return headerStyle.Render(strings.Join(parts, "  ·  "))
}

// countLabel renders "0 things" / "1 thing" / "3 things".
func countLabel(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func renderMountableList(list []drives.Drive) string {
	rows := []string{titleStyle.Render("Ready to mount")}
	if len(list) == 0 {
		rows = append(rows, headerStyle.Render(
			"No unmounted drives with a usable filesystem."))
		return centeredFrame(strings.Join(rows, "\n"))
	}
	for _, d := range list {
		labelOrName := d.Label
		if labelOrName == "" {
			labelOrName = filepath.Base(d.Path)
		}
		rows = append(rows, friendlyDriveRow(labelOrName, d, headerStyle.Render(d.Path)))
	}
	return centeredFrame(strings.Join(rows, "\n"))
}

func renderDriveList(title string, list []drives.Drive, emptyMsg string) string {
	rows := []string{titleStyle.Render(title)}
	if len(list) == 0 {
		rows = append(rows, headerStyle.Render(emptyMsg))
		return centeredFrame(strings.Join(rows, "\n"))
	}
	for _, d := range list {
		labelOrName := d.Label
		if labelOrName == "" {
			labelOrName = filepath.Base(d.MountPoint)
		}
		if labelOrName == "" {
			labelOrName = d.Path
		}
		mp := d.MountPoint
		if mp == "" {
			mp = "—"
		}
		rows = append(rows, friendlyDriveRow(labelOrName, d, headerStyle.Render("→ "+mp)))
	}
	return centeredFrame(strings.Join(rows, "\n"))
}

// friendlyDriveRow lays out one drive list row consistently: friendly name,
// human size, plain filesystem name, then a trailing dim detail (mountpoint or
// device path, supplied by the caller).
func friendlyDriveRow(name string, d drives.Drive, trailing string) string {
	return "  " +
		devNameStyle.Render(padRight(truncate(name, 20), 21)) +
		valueStyle.Render(padRight(prettySize(d.Size), 9)) +
		headerStyle.Render(padRight(plainFS(d.FSType), 9)) +
		trailing
}

func renderManageActionBar(hasManaged, hasExternal, hasMountable bool, width int) string {
	var keys []string
	if hasManaged && hasExternal {
		keys = append(keys, "[c] copy")
	} else {
		keys = append(keys, headerStyle.Render("[c] copy"))
	}
	if hasManaged {
		keys = append(keys, "[d] delete", "[n] rename")
	} else {
		keys = append(keys,
			headerStyle.Render("[d] delete"),
			headerStyle.Render("[n] rename"),
		)
	}
	if hasMountable {
		keys = append(keys, "[m] mount")
	} else {
		keys = append(keys, headerStyle.Render("[m] mount"))
	}
	if hasExternal {
		keys = append(keys, "[e] eject")
	} else {
		keys = append(keys, headerStyle.Render("[e] eject"))
	}
	return wrapKeyBar(keys, width)
}

// wrapKeyBar joins keyed shortcut labels with " · ", wrapping cleanly at
// the separator boundary when the line would overflow the available
// width. Never splits a label across lines.
func wrapKeyBar(parts []string, width int) string {
	if len(parts) == 0 {
		return ""
	}
	const sep = "  ·  "
	var lines []string
	var cur string
	curWidth := 0
	for _, p := range parts {
		pw := lipgloss.Width(p)
		needed := pw
		if cur != "" {
			needed += len(sep)
		}
		if cur != "" && curWidth+needed > width-2 {
			lines = append(lines, cur)
			cur = p
			curWidth = pw
			continue
		}
		if cur != "" {
			cur += sep
			curWidth += len(sep)
		}
		cur += p
		curWidth += pw
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	// Render each wrapped line on its own so a center-aligning wrapper can
	// centre each independently. Rendering them as one block would right-pad
	// the shorter line, leaving its text stuck to the left when centred.
	for i, l := range lines {
		lines[i] = footerStyle.Render(l)
	}
	return strings.Join(lines, "\n")
}

func renderManageDrivePicker(a *manageAction, title, sub string) string {
	var rows []string
	rows = append(rows, titleStyle.Render(title))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(sub))
	rows = append(rows, "")
	for i, d := range a.driveChoices {
		labelOrName := d.Label
		if labelOrName == "" {
			labelOrName = filepath.Base(d.MountPoint)
		}
		body := fmt.Sprintf("%-18s %-8s %-9s ", truncate(labelOrName, 18), prettySize(d.Size), plainFS(d.FSType)) +
			headerStyle.Render("→ "+d.MountPoint)
		rows = append(rows, markerLine(i == a.driveIdx, body))
	}
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("↑/↓ move · enter pick · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

// markerLine prefixes a row with a 2-char marker column ("▸ " selected,
// "  " unselected) so picker rows align regardless of styling.
func markerLine(selected bool, body string) string {
	if selected {
		return roleAvailableStyle.Render("▸ ") + body
	}
	return "  " + body
}

func renderCopyConfirm(a *manageAction) string {
	src := a.srcPath
	dst := filepath.Join(a.dstPath, filepath.Base(src))
	var rows []string
	rows = append(rows, titleStyle.Render("Copy this?"))
	rows = append(rows, "")
	rows = append(rows, "From:  "+devNameStyle.Render(src))
	rows = append(rows, "To:    "+devNameStyle.Render(dst))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(
		"rsync runs in the background. Progress goes to /tmp/tuistream/last-step.log — tail it from another shell for a live view."))
	rows = append(rows, "")
	yes := "  Yes, copy  "
	no := "  Cancel  "
	if a.confirmIdx == 0 {
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

func renderDeleteConfirm(a *manageAction) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Delete this?"))
	rows = append(rows, "")
	rows = append(rows, "Target:  "+devNameStyle.Render(a.dstPath))
	rows = append(rows, "")
	rows = append(rows, roleSystemStyle.Render(
		"⚠ This runs `rm -rf` and CANNOT be undone."))
	rows = append(rows, "")
	yes := "  Yes, delete  "
	no := "  Cancel  "
	if a.confirmIdx == 0 {
		yes = roleSystemStyle.Render("▸" + yes)
		no = "  " + no
	} else {
		yes = "  " + yes
		no = roleAvailableStyle.Render("▸" + no)
	}
	rows = append(rows, yes+"   "+no)
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("←/→ move · enter confirm · y/n shortcut · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderRenameInput(a *manageAction) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Rename"))
	rows = append(rows, "")
	rows = append(rows, "Target:  "+devNameStyle.Render(a.dstPath))
	rows = append(rows, "")
	rows = append(rows, "New name (basename only):")
	rows = append(rows, "  "+a.rename.View())
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("type a name · enter confirm · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderMountPick(a *manageAction) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Mount drive"))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render("Pick an unmounted drive to mount under /run/media/."))
	rows = append(rows, "")
	rows = append(rows, "  "+headerStyle.Render(fmt.Sprintf(
		"%-18s %-8s %-9s %s",
		"Device", "Size", "Type", "Label",
	)))
	for i, d := range a.driveChoices {
		label := d.Label
		if label == "" {
			label = "—"
		}
		body := fmt.Sprintf("%-18s %-8s %-9s %s",
			d.Path, prettySize(d.Size), plainFS(d.FSType), label)
		rows = append(rows, markerLine(i == a.driveIdx, body))
	}
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("↑/↓ move · enter pick · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderMountConfirm(a *manageAction) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Mount this drive?"))
	rows = append(rows, "")
	rows = append(rows, "Device:       "+devNameStyle.Render(a.srcDrive.Path)+
		"  ("+prettySize(a.srcDrive.Size)+", "+plainFS(a.srcDrive.FSType)+
		dashLabel(a.srcDrive.Label)+")")
	rows = append(rows, "Mountpoint:   "+devNameStyle.Render(a.mountPoint))
	mode := "[✓] read-only (safe default)"
	if !a.mountReadOnly {
		mode = roleSystemStyle.Render("[ ] read-only — drive will be WRITABLE")
	} else {
		mode = roleAvailableStyle.Render("[✓]") + " read-only (safe default)"
	}
	rows = append(rows, "Mode:         "+mode+"   "+headerStyle.Render("(w to toggle)"))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(
		"This is a transient mount — nothing is written to /etc/fstab. Use 'e' to eject when done."))
	rows = append(rows, "")
	yes := "  Yes, mount  "
	no := "  Cancel  "
	if a.confirmIdx == 0 {
		yes = roleAvailableStyle.Render("▸" + yes)
		no = "  " + no
	} else {
		yes = "  " + yes
		no = roleSystemStyle.Render("▸" + no)
	}
	rows = append(rows, yes+"   "+no)
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("←/→ move · w toggle write · enter confirm · y/n shortcut · esc back"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderUnmountPick(a *manageAction) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Eject drive"))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render("Pick a mounted external drive to unmount."))
	rows = append(rows, "")
	for i, d := range a.driveChoices {
		labelOrName := d.Label
		if labelOrName == "" {
			labelOrName = filepath.Base(d.Path)
		}
		body := fmt.Sprintf("%-18s %-8s %-9s ", truncate(labelOrName, 18), prettySize(d.Size), plainFS(d.FSType)) +
			headerStyle.Render("→ "+d.MountPoint)
		rows = append(rows, markerLine(i == a.driveIdx, body))
	}
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("↑/↓ move · enter pick · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderUnmountConfirm(a *manageAction) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Eject this drive?"))
	rows = append(rows, "")
	rows = append(rows, "Device:       "+devNameStyle.Render(a.srcDrive.Path)+
		"  ("+prettySize(a.srcDrive.Size)+", "+plainFS(a.srcDrive.FSType)+
		dashLabel(a.srcDrive.Label)+")")
	rows = append(rows, "Mountpoint:   "+devNameStyle.Render(a.mountPoint))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(
		"Runs `sync && umount`. If the mountpoint is under /run/media/ the now-empty dir is removed."))
	rows = append(rows, "")
	yes := "  Yes, eject  "
	no := "  Cancel  "
	if a.confirmIdx == 0 {
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

func dashLabel(l string) string {
	if l == "" {
		return ""
	}
	return `, label "` + l + `"`
}

func renderRenameConfirm(a *manageAction) string {
	newName := strings.TrimSpace(a.rename.Value())
	newPath := filepath.Join(filepath.Dir(a.dstPath), newName)
	var rows []string
	rows = append(rows, titleStyle.Render("Rename this?"))
	rows = append(rows, "")
	rows = append(rows, "From:  "+devNameStyle.Render(a.dstPath))
	rows = append(rows, "To:    "+devNameStyle.Render(newPath))
	rows = append(rows, "")
	yes := "  Yes, rename  "
	no := "  Cancel  "
	if a.confirmIdx == 0 {
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
