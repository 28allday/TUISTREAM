package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"tuistream/internal/drives"
	"tuistream/internal/jellyfin"
)

// setupModel holds Setup-tab-specific state: the current sub-stage (idle,
// confirm modal, add-drive flow). The "running" and "done" splash live on
// the root Model so they're shared with the Manage tab.
type setupModel struct {
	stage          setupStage
	confirmIdx     int             // 0 = Yes, 1 = No (used by confirm modals)
	firewallTarget bool            // true = "we're about to open", false = "we're about to close"
	addDrive       addDriveState   // state for the multi-stage Add-drive flow
	importPool     importPoolState // state for the import-existing-pool flow
	showTech       bool            // 'd' toggle: false = friendly drive list, true = raw lsblk table

	// Move-Jellyfin-storage flow.
	moveChoices []drives.Drive // managed media drives to choose from
	movePickIdx int            // cursor in the picker
	moveTarget  drives.Drive   // the chosen destination drive
}

func newSetupModel() setupModel { return setupModel{stage: stageIdle} }

func (s setupModel) view(m Model) string {
	if m.inventoryErr != nil {
		return centeredCard(
			titleStyle.Render("Couldn't read drives") + "\n\n" +
				m.inventoryErr.Error(),
		)
	}
	if m.inventory == nil {
		return centeredCard(headerStyle.Render("Loading drives…"))
	}

	switch s.stage {
	case stageConfirmInstall:
		return renderConfirmInstall(s.confirmIdx)
	case stageConfirmUninstall:
		return renderConfirmUninstall(m.status, s.confirmIdx)
	case stageConfirmFirewall:
		return renderConfirmFirewall(m.firewall, m.setup.firewallTarget, s.confirmIdx)
	case stageAddDrive:
		return renderAddDrive(m)
	case stageImportPool:
		return renderImportPool(m)
	case stageMoveJellyfinPick:
		return renderMoveJellyfinPick(m)
	case stageConfirmMoveJellyfin:
		return renderMoveJellyfinConfirm(m)
	}

	status := renderStatusLine(m.status)
	fw := renderFirewallLine(m.firewall)
	jellyfinMoved := jellyfin.LoadStorageState().Moved
	hasDetachedPool := len(m.inventory.DetachedPools()) > 0
	actions := renderSetupActionBar(m.status.IsInstalled(), m.firewall.AllOpen(), m.status.ServiceActive, jellyfinMoved, hasDetachedPool, cardWidth(m.width))

	var heading, subhead, inv string
	if s.showTech {
		heading = titleStyle.Render("Drives detected on this system")
		subhead = summaryLine(m.inventory)
		inv = renderInventory(m.inventory)
	} else {
		body, ready, media := renderInventoryFriendly(m.inventory)
		heading = titleStyle.Render("Your drives")
		subhead = friendlyDriveSummary(ready, media)
		inv = body
	}
	hint := headerStyle.Render(techToggleHint(s.showTech))

	w := cardWidth(m.width)
	return lipgloss.JoinVertical(lipgloss.Center,
		"",
		centered(heading, w),
		centered(subhead, w),
		"",
		centered(status, w),
		centered(fw, w),
		"",
		inv,
		centered(hint, w),
		"",
		centered(actions, w),
	)
}

// techToggleHint is the dim one-liner under the drive list pointing at the
// 'd' toggle, worded for whichever view is currently showing.
func techToggleHint(showTech bool) string {
	if showTech {
		return "  [d] back to the simple view"
	}
	return "  [d] show technical details"
}

// friendlyDriveSummary is the plain-English subheading under "Your drives",
// e.g. "1 drive ready to use · none set up for media yet".
func friendlyDriveSummary(ready, media int) string {
	var parts []string
	switch ready {
	case 0:
		parts = append(parts, "no spare drives ready")
	case 1:
		parts = append(parts, "1 drive ready to use")
	default:
		parts = append(parts, fmt.Sprintf("%d drives ready to use", ready))
	}
	switch media {
	case 0:
		parts = append(parts, "none set up for media yet")
	case 1:
		parts = append(parts, "1 set up for media")
	default:
		parts = append(parts, fmt.Sprintf("%d set up for media", media))
	}
	return headerStyle.Render(strings.Join(parts, "  ·  "))
}

// renderInventoryFriendly draws the default, jargon-free drive list: one row
// per physical disk with a friendly name, human size, and a plain-English
// status. Virtual devices (zram / loop) are hidden entirely. Returns the
// rendered frame plus counts of "ready" and "media" disks for the subheading.
func renderInventoryFriendly(inv *drives.Inventory) (string, int, int) {
	// Device paths that can be turned into a media drive (whole disks and/or
	// individual partitions), so a disk reads "Ready to use" when it OR one of
	// its partitions qualifies.
	cand := map[string]bool{}
	for _, c := range inv.Candidates() {
		cand[c.Path] = true
	}

	var lines []string
	ready, media := 0, 0
	for _, d := range inv.All {
		if d.Type != "disk" {
			continue
		}
		// Swap-in-RAM, loopback and eMMC firmware areas aren't drives the user
		// thinks about.
		if drives.IsPseudoDisk(d.Name) {
			continue
		}
		text, style, detail, kind := diskFriendlyStatus(inv, d, cand)
		switch kind {
		case "ready":
			ready++
		case "media":
			media++
		}
		name := devNameStyle.Render(fmt.Sprintf("%-24s", truncate(friendlyDriveName(d), 24)))
		size := valueStyle.Render(fmt.Sprintf("%-8s", prettySize(d.Size)))
		lines = append(lines, "  "+name+" "+size+" "+style.Render(text))
		if detail != "" {
			lines = append(lines, "     "+headerStyle.Render(detail))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, headerStyle.Render("No drives detected."))
	}
	return centeredFrame(strings.Join(lines, "\n")), ready, media
}

// diskFriendlyStatus summarises a physical disk into a single plain-English
// status: the colour style to render it in, a dim one-line detail (or ""),
// and a "kind" tag ("ready"/"media"/"system"/"inuse") used for counting.
// Priority: system disk → already-our-media → ready → in-use.
func diskFriendlyStatus(inv *drives.Inventory, d drives.Drive, cand map[string]bool) (text string, style lipgloss.Style, detail, kind string) {
	if d.Role == drives.RoleSystem {
		return "⛌ System drive (protected)", roleSystemStyle, "Holds the operating system — left alone", "system"
	}

	children := inv.ChildrenOf(d.Name)

	// Already set up by us as media (whole-disk or one of its partitions).
	if d.Role == drives.RoleManagedOurs {
		return "★ TUISTREAM media", roleAvailableStyle, "Mounted at " + d.MountPoint, "media"
	}
	for _, c := range children {
		if c.Role == drives.RoleManagedOurs {
			return "★ TUISTREAM media", roleAvailableStyle, "Mounted at " + c.MountPoint, "media"
		}
	}

	// Spare and pickable. Both a whole disk and its partition can qualify; pick
	// the most informative candidate (a labelled / formatted partition beats the
	// bare disk) so the detail line says something useful.
	var best *drives.Drive
	consider := func(c drives.Drive) {
		if !cand[c.Path] {
			return
		}
		if best == nil || readyScore(c) > readyScore(*best) {
			cc := c
			best = &cc
		}
	}
	consider(d)
	for _, c := range children {
		consider(c)
	}
	if best != nil {
		return "✓ Ready to use", roleAvailableStyle, readyDetail(*best, d), "ready"
	}

	// Everything else is occupied — keep it short and reassuring.
	if d.Role == drives.RolePoolMember {
		return "• In use (storage pool)", roleInUseStyle, "", "inuse"
	}
	for _, c := range children {
		if c.Role == drives.RolePoolMember {
			return "• In use (storage pool)", roleInUseStyle, "", "inuse"
		}
	}
	if mp := firstMount(d, children); mp != "" {
		return "• In use", roleInUseStyle, "Mounted at " + mp, "inuse"
	}
	return "• In use", roleInUseStyle, "", "inuse"
}

// readyScore ranks ready candidates so the friendly view describes the most
// meaningful one: a labelled partition outranks a formatted-but-unlabelled one,
// which outranks a bare/blank device.
func readyScore(d drives.Drive) int {
	s := 0
	if d.FSType != "" {
		s++
	}
	if d.Label != "" {
		s += 2
	}
	return s
}

// readyDetail builds the dim hint under a "Ready to use" row: how it's
// connected, plus its label (if any) or a note that a blank disk gets
// formatted on add. `disk` supplies the transport, which lsblk only reports
// on the top-level device, not on partitions.
func readyDetail(target, disk drives.Drive) string {
	bits := []string{plainTransport(disk.Transport)}
	switch {
	case target.Label != "":
		bits = append(bits, "labelled “"+target.Label+"”")
	case target.FSType == "":
		bits = append(bits, "blank — formatted when you add it")
	}
	return strings.Join(bits, " · ")
}

// firstMount returns the first mountpoint found on the disk or its children.
func firstMount(d drives.Drive, children []drives.Drive) string {
	if d.MountPoint != "" {
		return d.MountPoint
	}
	for _, c := range children {
		if c.MountPoint != "" {
			return c.MountPoint
		}
	}
	return ""
}

// friendlyDriveName is the human label for a disk: its model if lsblk knows
// it, otherwise the bare kernel name (e.g. "sda").
func friendlyDriveName(d drives.Drive) string {
	if d.Model != "" {
		return d.Model
	}
	return d.Name
}

// prettySize expands lsblk's compact size ("3.6T", "4G") into "3.6 TB" /
// "4 GB". Anything unexpected passes through unchanged.
func prettySize(s string) string {
	if s == "" {
		return "—"
	}
	switch s[len(s)-1] {
	case 'K', 'M', 'G', 'T', 'P', 'E':
		return s[:len(s)-1] + " " + s[len(s)-1:] + "B"
	}
	return s
}

// plainFS normalises the filesystem names users are most likely to be
// confused by (vfat → FAT32, ntfs3 → NTFS) and leaves the rest as-is, since
// ext4 / btrfs / xfs are recognisable to anyone setting up a media server.
func plainFS(fs string) string {
	switch strings.ToLower(fs) {
	case "vfat", "fat", "fat32":
		return "FAT32"
	case "exfat":
		return "exFAT"
	case "ntfs", "ntfs3":
		return "NTFS"
	case "":
		return "unformatted"
	default:
		return fs
	}
}

// plainTransport renders a disk's bus in friendly words.
func plainTransport(tran string) string {
	switch strings.ToLower(tran) {
	case "usb":
		return "USB"
	case "nvme":
		return "NVMe SSD"
	case "sata", "ata":
		return "SATA"
	case "":
		return "internal"
	default:
		return strings.ToUpper(tran)
	}
}

// renderMoveJellyfinPick lets the user choose which managed media drive to
// relocate Jellyfin's storage onto, when there's more than one.
func renderMoveJellyfinPick(m Model) string {
	st := &m.setup
	var rows []string
	rows = append(rows, titleStyle.Render("Move Jellyfin storage — pick a drive"))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render("Jellyfin's library DB, metadata and artwork will move onto this drive."))
	rows = append(rows, "")
	for i, d := range st.moveChoices {
		name := d.Label
		if name == "" {
			name = d.MountPoint
		}
		body := fmt.Sprintf("%-20s %-8s ", truncate(name, 20), prettySize(d.Size)) +
			headerStyle.Render("→ "+d.MountPoint)
		rows = append(rows, markerLine(i == st.movePickIdx, body))
	}
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("↑/↓ move · enter pick · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

// renderMoveJellyfinConfirm is the last-chance screen before migrating.
func renderMoveJellyfinConfirm(m Model) string {
	st := &m.setup
	target := st.moveTarget.MountPoint + "/JellyfinData"
	var rows []string
	rows = append(rows, titleStyle.Render("Move Jellyfin storage to the media drive?"))
	rows = append(rows, "")
	rows = append(rows, "Frees space on your OS drive by relocating Jellyfin's library")
	rows = append(rows, "database, downloaded metadata and artwork onto the media drive.")
	rows = append(rows, "")
	rows = append(rows, "Data:   "+devNameStyle.Render(target+"/data")+
		headerStyle.Render("   (library DB, metadata, artwork)"))
	rows = append(rows, "Cache:  "+devNameStyle.Render(target+"/cache")+
		headerStyle.Render("   (transcodes/thumbnails regenerate)"))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render("Jellyfin keeps using /var/lib/jellyfin, but that path is bind-mounted"))
	rows = append(rows, headerStyle.Render("to the media drive via fstab. The service is stopped, data copied,"))
	rows = append(rows, headerStyle.Render("the OS-drive copy reclaimed, then Jellyfin restarts. systemd mounts"))
	rows = append(rows, headerStyle.Render("the drive before Jellyfin starts, so ordering is automatic."))
	rows = append(rows, "")
	rows = append(rows, roleSystemStyle.Render("⚠ The media drive must stay attached — if it's ever missing,"))
	rows = append(rows, roleSystemStyle.Render("  Jellyfin won't start until it's back."))
	rows = append(rows, "")
	yes := "  Yes, move it  "
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
	rows = append(rows, footerStyle.Render("←/→ move · enter confirm · y/n shortcut · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

// renderInventory builds the table-style overview of every disk and partition
// with its role coloured according to whether it's available, off-limits, etc.
func renderInventory(inv *drives.Inventory) string {
	header := headerStyle.Render(fmt.Sprintf(
		"  %-20s %-7s %-12s %-14s %s",
		"Device", "Size", "Filesystem", "Label", "Used for",
	))

	var rows []string
	rows = append(rows, header)

	for i, d := range inv.All {
		switch d.Type {
		case "disk":
			label := fmt.Sprintf("▸ %s  %s  %s",
				devNameStyle.Render(d.Path),
				d.Size,
				diskDescriptor(d),
			)
			if d.Role == drives.RoleSystem {
				label += "  " + roleSystemStyle.Render("[SYSTEM DISK — off-limits]")
			}
			if i > 0 {
				rows = append(rows, "")
			}
			rows = append(rows, label)
			// When the disk has a filesystem directly on it (no partition
			// table), show that on a sub-line — otherwise the user has no
			// indication that mkfs / mount actually landed.
			if d.FSType != "" && d.Role != drives.RoleSystem {
				rows = append(rows, renderWholeDiskFSRow(d))
			}

		case "part", "crypt":
			rows = append(rows, renderPartitionRow(d))
		}
	}

	return centeredFrame(strings.Join(rows, "\n"))
}

func renderWholeDiskFSRow(d drives.Drive) string {
	fs := dashIfEmpty(d.FSType)
	label := dashIfEmpty(d.Label)
	used := d.RoleDetail
	switch d.Role {
	case drives.RoleAvailable:
		used = roleAvailableStyle.Render("✓ AVAILABLE (formatted but unmounted)")
	case drives.RoleManagedOurs:
		used = roleAvailableStyle.Render(used)
	case drives.RoleMounted, drives.RolePoolMember:
		used = roleInUseStyle.Render(used)
	}
	return fmt.Sprintf(
		"  └─ %-17s %-7s %-12s %-14s %s",
		"(whole-disk fs)",
		d.Size,
		truncate(fs, 12),
		truncate(label, 14),
		used,
	)
}

func renderPartitionRow(d drives.Drive) string {
	fs := dashIfEmpty(d.FSType)
	label := dashIfEmpty(d.Label)

	used := d.RoleDetail
	switch d.Role {
	case drives.RoleAvailable:
		used = roleAvailableStyle.Render("✓ AVAILABLE")
	case drives.RoleSystem:
		used = roleSystemStyle.Render(used)
	case drives.RoleManagedOurs:
		used = roleAvailableStyle.Render(used)
	case drives.RoleMounted, drives.RoleLUKS, drives.RoleLVM, drives.RoleRAID, drives.RoleSwap, drives.RolePoolMember:
		used = roleInUseStyle.Render(used)
	}

	return fmt.Sprintf(
		"  └─ %-17s %-7s %-12s %-14s %s",
		d.Path,
		d.Size,
		truncate(fs, 12),
		truncate(label, 14),
		used,
	)
}

func diskDescriptor(d drives.Drive) string {
	model := d.Model
	if model == "" {
		model = "(unknown)"
	}
	tran := d.Transport
	if tran == "" {
		tran = "?"
	}
	return fmt.Sprintf("%s [%s]", model, tran)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
