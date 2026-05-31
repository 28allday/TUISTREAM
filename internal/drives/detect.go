// Package drives detects block devices on the host and classifies each one.
//
// The classifier is the safety mechanism that prevents the boot drive from
// ever being offered as a media drive: it walks every candidate partition
// down through any LUKS / LVM / RAID layers to its underlying physical disk
// and refuses to offer it if that disk also hosts /, /boot, swap, or any of
// the other "system" mounts.
package drives

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// probeTimeout caps each block-device probe. A half-connected or failing USB
// drive can make lsblk/findmnt/mountpoint block in uninterruptible kernel I/O,
// which would otherwise freeze launch (detection runs before the first paint).
// On timeout the probe reports an error and detection degrades gracefully.
const probeTimeout = 8 * time.Second

// cmdOutput runs a command with a deadline and returns its stdout. On timeout
// the process is killed and a non-nil error is returned.
func cmdOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// cmdRunOK reports whether a command exits 0 within the deadline.
func cmdRunOK(name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run() == nil
}

// Role labels what a partition is currently being used for. Used both in the
// inventory display and to filter candidates in the picker.
type Role string

const (
	RoleSystem      Role = "system"       // on a disk that hosts /, /boot, swap, etc.
	RoleSwap        Role = "swap"         // swap partition
	RoleLUKS        Role = "luks"         // crypto_LUKS container
	RoleLVM         Role = "lvm-pv"       // LVM physical volume
	RoleRAID        Role = "raid"         // mdadm RAID member
	RoleMounted     Role = "mounted"      // mounted at some non-system path
	RoleAvailable   Role = "available"    // has a filesystem, nowhere mounted — pickable
	RoleEmpty       Role = "empty"        // no filesystem at all
	RoleManagedOurs Role = "managed-ours" // we put this in fstab under /media/<user>/
	RolePoolMember  Role = "pool-member"  // device of a multi-device btrfs pool (mounted via a sibling) — off-limits
)

// Drive represents one block device row from lsblk.
type Drive struct {
	Path         string // /dev/sda1
	Name         string // sda1
	Size         string // "3.6T"
	FSType       string // ext4, btrfs, swap, crypto_LUKS, "" if none
	Label        string // filesystem label, "" if none
	UUID         string // filesystem UUID
	Model        string // device model (top-level disks only)
	Transport    string // sata / usb / nvme (top-level disks only)
	Type         string // disk / part / crypt / lvm
	MountPoint   string // "" if not mounted
	ParentDisk   string // physical disk this lives on (sda, nvme0n1) — "" for top-level disks
	Hotpluggable bool   // best-effort: USB / removable
	Role         Role   // computed
	RoleDetail   string // free-text like "→ /boot" or "encrypted (LUKS)"
}

// Inventory is the full list of drives on the system, in lsblk order, with
// each row's Role and ParentDisk filled in. Calling site uses this for the
// Setup tab's overview panel and for filtering candidates.
type Inventory struct {
	All []Drive

	// SystemDisks is the set of physical disks the OS lives on. Their short
	// names (sda, nvme0n1) — never offered as media targets.
	SystemDisks map[string]bool
}

// Load runs lsblk and findmnt, returns a fully-classified Inventory.
func Load(managedUser string) (*Inventory, error) {
	sysDisks, err := systemDisks()
	if err != nil {
		return nil, err
	}
	managed := managedMounts(managedUser)

	all, err := lsblkAll()
	if err != nil {
		return nil, err
	}

	// First pass: fill ParentDisk for every row.
	for i := range all {
		if all[i].Type == "disk" {
			all[i].ParentDisk = ""
			continue
		}
		all[i].ParentDisk = physicalDiskOf(all[i].Path)
	}

	// Multi-device btrfs filesystems share one UUID across every member device.
	// Map UUID → device count and UUID → "is any member mounted", so the
	// classifier can recognise a pool member that has no mountpoint of its own
	// (e.g. a 2-disk RAID mounted via /dev/sda while /dev/sdb looks idle).
	uuidCount := map[string]int{}
	uuidMounted := map[string]bool{}
	for i := range all {
		u := all[i].UUID
		if u == "" {
			continue
		}
		uuidCount[u]++
		if all[i].MountPoint != "" {
			uuidMounted[u] = true
		}
	}

	// Second pass: classify Role. Membership of a system disk always wins so
	// the UI consistently marks every partition on the boot drive as off-limits,
	// regardless of whether it's mounted, encrypted, or empty.
	for i := range all {
		d := &all[i]
		switch d.Type {
		case "disk":
			if sysDisks[d.Name] {
				d.Role = RoleSystem
				d.RoleDetail = "SYSTEM DISK (off-limits)"
				continue
			}
			// Whole disk with a filesystem directly on it (no partition
			// table) — e.g. after `mkfs.btrfs /dev/sda`. Apply the same
			// mount/managed classification we use for partitions.
			if d.MountPoint != "" {
				if managed[d.MountPoint] {
					d.Role = RoleManagedOurs
					d.RoleDetail = "TUISTREAM media → " + d.MountPoint
				} else {
					d.Role = RoleMounted
					d.RoleDetail = "mounted → " + d.MountPoint
				}
				continue
			}
			if d.FSType != "" {
				if member, detail := btrfsPoolMember(d, uuidCount, uuidMounted); member {
					d.Role = RolePoolMember
					d.RoleDetail = detail
				} else {
					// Formatted but unmounted (e.g. partial setup, manual umount).
					d.Role = RoleAvailable
					d.RoleDetail = "AVAILABLE"
				}
			}
			continue
		case "part", "crypt":
			// fall through
		default:
			continue
		}

		// What the partition "looks like" on its own — used to make the
		// RoleSystem detail informative when we override below.
		intrinsic := ""
		intrinsicRole := Role("")
		switch d.FSType {
		case "swap":
			intrinsic, intrinsicRole = "swap", RoleSwap
		case "crypto_LUKS":
			intrinsic, intrinsicRole = "encrypted (LUKS container)", RoleLUKS
		case "LVM2_member":
			intrinsic, intrinsicRole = "LVM physical volume", RoleLVM
		case "linux_raid_member":
			intrinsic, intrinsicRole = "RAID member", RoleRAID
		}

		// System-disk membership trumps everything else.
		if sysDisks[d.ParentDisk] {
			d.Role = RoleSystem
			switch {
			case d.MountPoint != "":
				d.RoleDetail = "boot disk — mounted at " + d.MountPoint
			case intrinsic != "":
				d.RoleDetail = "boot disk — " + intrinsic
			default:
				d.RoleDetail = "boot disk — off-limits"
			}
			continue
		}

		// Off the boot disk: classify normally.
		if d.MountPoint != "" {
			if managed[d.MountPoint] {
				d.Role = RoleManagedOurs
				d.RoleDetail = "TUISTREAM media → " + d.MountPoint
			} else {
				d.Role = RoleMounted
				d.RoleDetail = "mounted → " + d.MountPoint
			}
			continue
		}

		if intrinsicRole != "" {
			d.Role = intrinsicRole
			d.RoleDetail = intrinsic
			continue
		}

		if d.FSType == "" {
			d.Role = RoleEmpty
			d.RoleDetail = "empty / unformatted"
		} else if member, detail := btrfsPoolMember(d, uuidCount, uuidMounted); member {
			d.Role = RolePoolMember
			d.RoleDetail = detail
		} else {
			d.Role = RoleAvailable
			d.RoleDetail = "AVAILABLE"
		}
	}

	return &Inventory{All: all, SystemDisks: sysDisks}, nil
}

// Candidates returns drives suitable for being added as a media drive.
//
// Three kinds qualify, in lsblk order:
//
//  1. Whole disks that aren't system disks AND have no currently-mounted
//     children. Includes the "freshly-wiped, no partition table" case
//     (where lsblk shows just the bare disk with no children) as well as
//     "blank USB that's never been partitioned".
//  2. Partitions with a real filesystem that aren't mounted anywhere
//     (RoleAvailable).
//  3. Empty/unformatted partitions on a non-system disk (RoleEmpty) —
//     the user can format them during Add.
//
// If a disk and one of its partitions both qualify, both appear; the picker
// auto-deselects any conflicting peer when the user toggles a row.
func (inv *Inventory) Candidates() []Drive {
	var out []Drive
	for _, d := range inv.All {
		switch d.Type {
		case "disk":
			if !inv.diskIsCandidate(d) {
				continue
			}
			out = append(out, d)
		case "part", "crypt":
			if d.Role == RoleAvailable || d.Role == RoleEmpty {
				out = append(out, d)
			}
		}
	}
	return out
}

// IsPseudoDisk reports whether a whole-disk device is a kernel/firmware
// pseudo-device that should never be shown or offered as a usable drive:
// zram swap, loopback mounts, device-mapper targets, and the tiny eMMC
// hardware boot / RPMB areas (mmcblk0boot0, mmcblk0boot1, mmcblk0rpmb …).
// Exported so the TUI and the health monitor apply the exact same filter.
func IsPseudoDisk(name string) bool {
	switch {
	case strings.HasPrefix(name, "zram"),
		strings.HasPrefix(name, "loop"),
		strings.HasPrefix(name, "dm-"):
		return true
	case strings.HasPrefix(name, "mmcblk") &&
		(strings.Contains(name, "boot") || strings.Contains(name, "rpmb")):
		return true
	}
	return false
}

// diskIsCandidate is the gate for whole-disk picker eligibility. We refuse
// system disks, virtual / firmware pseudo-disks (zram / loop / dm / eMMC boot
// areas), and any disk that has ANY currently-mounted child (mount-point != "")
// so the user can't nuke storage that's actively in use.
func (inv *Inventory) diskIsCandidate(d Drive) bool {
	if inv.SystemDisks[d.Name] {
		return false
	}
	if IsPseudoDisk(d.Name) {
		return false
	}
	// A whole-disk btrfs that's a live pool member (mounted via a sibling disk).
	if d.Role == RolePoolMember {
		return false
	}
	for _, c := range inv.All {
		if c.ParentDisk != d.Name {
			continue
		}
		// Any mounted child, or any child that belongs to a btrfs pool, means
		// wiping this disk would destroy storage that's actively in use.
		if c.MountPoint != "" || c.Role == RolePoolMember {
			return false
		}
	}
	return true
}

// btrfsPoolMember reports whether an unmounted, formatted device is actually a
// member of a multi-device btrfs filesystem — recognised because btrfs gives
// every device of one filesystem the same UUID. Two cases qualify:
//
//   - another device sharing this UUID is currently mounted (a live pool
//     reached through a sibling, e.g. a 2-disk RAID mounted via /dev/sda while
//     /dev/sdb shows no mountpoint of its own), or
//   - two or more devices share the UUID (a detached multi-device pool).
//
// Such a device must never be offered for formatting: wiping any one member
// destroys the whole pool. Returns a human-readable detail for the inventory.
func btrfsPoolMember(d *Drive, uuidCount map[string]int, uuidMounted map[string]bool) (bool, string) {
	if d.FSType != "btrfs" || d.UUID == "" {
		return false, ""
	}
	name := d.Label
	if name == "" {
		name = "(unlabelled)"
	}
	switch {
	case uuidMounted[d.UUID]:
		return true, "IN USE — btrfs pool '" + name + "' (mounted via another device)"
	case uuidCount[d.UUID] >= 2:
		return true, "IN USE — btrfs pool '" + name + "' (multi-device, detached)"
	}
	return false, ""
}

// Managed returns drives the installer has previously set up (mounted under
// /media/<user>/). Used by the Manage tab as the destination list.
func (inv *Inventory) Managed() []Drive {
	var out []Drive
	for _, d := range inv.All {
		if d.Role == RoleManagedOurs {
			out = append(out, d)
		}
	}
	return out
}

// FindDisk returns the whole-disk Drive whose short name matches `name`
// (e.g. "sda" or "nvme0n1"), or nil if none. Used by the Add-drive flow when
// auto-collapsing same-parent selections into whole-disk single-drive mode.
func (inv *Inventory) FindDisk(name string) *Drive {
	for i := range inv.All {
		d := &inv.All[i]
		if d.Type == "disk" && d.Name == name {
			return d
		}
	}
	return nil
}

// ChildrenOf returns every partition / crypt mapping whose underlying
// physical disk is `parentName` (e.g. "sda"). Used by the Add-drive confirm
// screen to list what would be erased if WipeWholeDisks is set.
func (inv *Inventory) ChildrenOf(parentName string) []Drive {
	var out []Drive
	for _, d := range inv.All {
		if d.Type == "disk" {
			continue
		}
		if d.ParentDisk == parentName {
			out = append(out, d)
		}
	}
	return out
}

// UniqueParents returns the set of parent-disk names referenced by the
// passed drives, in first-seen order. Empty entries (drives that have no
// resolvable parent, like top-level disks themselves) are skipped.
func UniqueParents(ds []Drive) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range ds {
		if d.ParentDisk == "" || seen[d.ParentDisk] {
			continue
		}
		seen[d.ParentDisk] = true
		out = append(out, d.ParentDisk)
	}
	return out
}

// Mountable returns drives that have a recognisable filesystem on them but
// aren't currently mounted anywhere. Used by the Manage tab's "mount drive"
// flow on headless boxes where no auto-mount daemon runs. System disks and
// partitions on system disks are already excluded by the role classifier.
func (inv *Inventory) Mountable() []Drive {
	supported := map[string]bool{
		"ext4": true, "btrfs": true, "xfs": true,
		"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
		"iso9660": true, "udf": true, "f2fs": true,
	}
	var out []Drive
	for _, d := range inv.All {
		if d.Role != RoleAvailable {
			continue
		}
		if !supported[d.FSType] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// External returns mounted drives that are NOT system mounts and NOT managed
// by us — anything the user has plugged in and that the desktop has
// auto-mounted (e.g. a USB stick at /run/media/<user>/MovieDump). Used by
// the Manage tab as the copy-source list.
func (inv *Inventory) External() []Drive {
	var out []Drive
	for _, d := range inv.All {
		if d.Role != RoleMounted {
			continue
		}
		// Anything mounted under /run/media, /media, /mnt is plausibly external.
		mp := d.MountPoint
		if strings.HasPrefix(mp, "/run/media/") || strings.HasPrefix(mp, "/media/") || strings.HasPrefix(mp, "/mnt/") {
			out = append(out, d)
		}
	}
	return out
}

// ---------------- internals ----------------

// lsblk's JSON output structure. We only model the fields we need.
type lsblkNode struct {
	Name        string      `json:"name"`        // bare name, e.g. "sda1"
	Path        string      `json:"path"`        // full path, e.g. "/dev/sda1"
	Size        string      `json:"size"`        // "3.6T"
	FSType      string      `json:"fstype"`      // ext4, swap, crypto_LUKS, ""
	Label       string      `json:"label"`       // fs label
	UUID        string      `json:"uuid"`        // fs uuid
	Mountpoint  string      `json:"mountpoint"`  // single-mountpoint field (older lsblk)
	Mountpoints []string    `json:"mountpoints"` // newer lsblk uses an array
	Model       string      `json:"model"`       // disk model (top-level only)
	Tran        string      `json:"tran"`        // transport (sata/usb/nvme)
	Type        string      `json:"type"`        // disk/part/crypt/lvm
	RM          bool        `json:"rm"`          // removable flag
	HotPlug     bool        `json:"hotplug"`     // hotplug flag
	Children    []lsblkNode `json:"children"`
}

func lsblkAll() ([]Drive, error) {
	// No -b: SIZE comes back as a human-readable string like "3.6T".
	out, err := cmdOutput(
		"lsblk", "-J", "-p", "-o",
		"NAME,PATH,SIZE,FSTYPE,LABEL,UUID,MOUNTPOINT,MOUNTPOINTS,MODEL,TRAN,TYPE,RM,HOTPLUG",
	)
	if err != nil {
		// Older lsblk doesn't know MOUNTPOINTS — retry without it.
		out, err = cmdOutput(
			"lsblk", "-J", "-p", "-o",
			"NAME,PATH,SIZE,FSTYPE,LABEL,UUID,MOUNTPOINT,MODEL,TRAN,TYPE,RM,HOTPLUG",
		)
		if err != nil {
			return nil, err
		}
	}
	var root struct {
		BlockDevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		return nil, err
	}
	var flat []Drive
	for _, n := range root.BlockDevices {
		flat = append(flat, flatten(n, "")...)
	}
	return flat, nil
}

func flatten(n lsblkNode, _ string) []Drive {
	d := Drive{
		Path:         n.Path,
		Name:         strings.TrimPrefix(n.Path, "/dev/"),
		Size:         n.Size,
		FSType:       n.FSType,
		Label:        n.Label,
		UUID:         n.UUID,
		Model:        n.Model,
		Transport:    n.Tran,
		Type:         n.Type,
		Hotpluggable: n.RM || n.HotPlug,
	}
	d.MountPoint = n.Mountpoint
	if d.MountPoint == "" && len(n.Mountpoints) > 0 {
		for _, mp := range n.Mountpoints {
			if mp != "" {
				d.MountPoint = mp
				break
			}
		}
	}
	out := []Drive{d}
	for _, c := range n.Children {
		out = append(out, flatten(c, n.Name)...)
	}
	return out
}

// physicalDiskOf walks a device down to its underlying physical disk.
// Returns the disk's short name (e.g. "sda", "nvme0n1") or "" if unresolved.
func physicalDiskOf(devPath string) string {
	out, err := cmdOutput("lsblk", "-s", "-n", "-l", "-o", "NAME,TYPE", devPath)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[len(fields)-1] == "disk" {
			return fields[0]
		}
	}
	return ""
}

// systemDisks returns the set of physical disks that host any system mount
// or active swap. Names are short (sda, nvme0n1).
func systemDisks() (map[string]bool, error) {
	out := map[string]bool{}
	mounts := []string{
		"/", "/boot", "/boot/efi", "/efi", "/home", "/usr",
		"/var", "/var/lib", "/var/log", "/tmp", "/opt", "/srv", "/nix",
	}
	for _, mp := range mounts {
		src := findmntSource(mp)
		if src == "" {
			continue
		}
		if disk := physicalDiskOf(src); disk != "" {
			out[disk] = true
		}
	}
	// Swap from /proc/swaps
	if f, err := os.Open("/proc/swaps"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		first := true
		for sc.Scan() {
			if first { // header row
				first = false
				continue
			}
			fields := strings.Fields(sc.Text())
			if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			dev := fields[0]
			if !strings.HasPrefix(dev, "/dev/") {
				continue
			}
			if _, err := os.Stat(dev); err != nil {
				continue
			}
			if disk := physicalDiskOf(dev); disk != "" {
				out[disk] = true
			}
		}
	}
	return out, nil
}

func findmntSource(mp string) string {
	out, err := cmdOutput("findmnt", "-no", "SOURCE", mp)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// managedMounts returns the set of mountpoints under /media/<user>/ that this
// installer has previously added via fstab. Best-effort: we just enumerate
// the directories under /media/<user>/ that are currently mounted.
func managedMounts(user string) map[string]bool {
	out := map[string]bool{}
	if user == "" {
		return out
	}
	root := filepath.Join("/media", user)
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mp := filepath.Join(root, e.Name())
		// Treat as managed if something is mounted there.
		if cmdRunOK("mountpoint", "-q", mp) {
			out[mp] = true
		}
	}
	return out
}
