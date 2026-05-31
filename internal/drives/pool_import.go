package drives

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"tuistream/internal/step"
)

// DetachedPool is a multi-device btrfs filesystem whose member devices are all
// present but none is currently mounted — a pool left over from a previous
// setup that can be re-attached WITHOUT reformatting.
//
// The classifier marks these devices RolePoolMember and (rightly) refuses to
// offer them for the destructive Add-drive flow, because wiping any one member
// destroys the whole pool. Import is the safe counterpart: it mounts the pool
// as-is. Single-device btrfs drives are NOT pools — they're handled by the
// normal Add-drive "Keep existing filesystem" path.
type DetachedPool struct {
	UUID    string  // shared across every member device
	Label   string  // btrfs label, "" if unlabelled
	Members []Drive // every device carrying this UUID, in inventory order
}

// FirstDevice returns the device path to mount the pool via; btrfs resolves the
// remaining members from the kernel's device scan. "" if the pool has no
// members (shouldn't happen for a value returned by DetachedPools).
func (p DetachedPool) FirstDevice() string {
	if len(p.Members) == 0 {
		return ""
	}
	return p.Members[0].Path
}

// DisplayLabel returns the label or a placeholder for the UI.
func (p DetachedPool) DisplayLabel() string {
	if p.Label != "" {
		return p.Label
	}
	return "(unlabelled)"
}

// DetachedPools groups the inventory's btrfs devices by UUID and returns those
// that look like an importable detached pool: 2+ member devices, none mounted,
// and none living on a system disk. These are exactly the devices the role
// classifier flags as "btrfs pool … (multi-device, detached)".
func (inv *Inventory) DetachedPools() []DetachedPool {
	type group struct {
		label   string
		members []Drive
		mounted bool
		onSys   bool
	}
	var order []string
	groups := map[string]*group{}
	for _, d := range inv.All {
		if d.FSType != "btrfs" || d.UUID == "" {
			continue
		}
		g := groups[d.UUID]
		if g == nil {
			g = &group{}
			groups[d.UUID] = g
			order = append(order, d.UUID)
		}
		g.members = append(g.members, d)
		if d.Label != "" {
			g.label = d.Label
		}
		if d.MountPoint != "" {
			g.mounted = true
		}
		if inv.SystemDisks[d.Name] || inv.SystemDisks[d.ParentDisk] {
			g.onSys = true
		}
	}
	var out []DetachedPool
	for _, u := range order {
		g := groups[u]
		// 2+ members → genuinely a multi-device pool (single-device btrfs is the
		// Add-drive Keep path). All unmounted → "detached". Not on a system disk.
		if len(g.members) < 2 || g.mounted || g.onSys {
			continue
		}
		out = append(out, DetachedPool{UUID: u, Label: g.label, Members: g.members})
	}
	return out
}

// ImportPoolOptions captures an import-existing-pool run.
type ImportPoolOptions struct {
	Pool  DetachedPool
	Label string // friendly directory name under /media/<user>/
	User  string

	// SeedStarterFolders creates the Jellyfin library folders under
	// <mount>/JellyfinMedia/. Left off when the pool already holds content so
	// we don't clutter an existing library layout.
	SeedStarterFolders bool

	// RecursiveACL grants jellyfin read access over the whole existing tree
	// (setfacl -R), not just the mount root. Set when the pool already holds
	// media so pre-existing files copied with tight permissions stay readable.
	RecursiveACL bool
}

// ImportPoolPlan builds the ordered, NON-DESTRUCTIVE commands to re-attach an
// existing btrfs pool at /media/<user>/<label>:
//
//  0. Ensure btrfs-progs + acl are present
//  1. mkdir the mount point
//  2. Back up /etc/fstab
//  3. Append a UUID-based fstab line (btrfs assembles all members from the UUID)
//  4. daemon-reload + btrfs device scan + mount -a
//  5. ACL: grant the jellyfin user read (if the user exists)
//  6. Optionally seed the Jellyfin starter folders
//
// Nothing here formats, wipes, or partitions — the pool's data is untouched.
func ImportPoolPlan(opts ImportPoolOptions) ([]step.Step, error) {
	dev := opts.Pool.FirstDevice()
	if dev == "" {
		return nil, fmt.Errorf("ImportPoolPlan: pool has no member devices")
	}
	if opts.Label == "" {
		return nil, fmt.Errorf("ImportPoolPlan: empty label")
	}
	if opts.User == "" {
		return nil, fmt.Errorf("ImportPoolPlan: no target user (SUDO_USER)")
	}

	mountPoint := filepath.Join("/media", opts.User, opts.Label)
	var steps []step.Step

	if prereq, ok := PrereqStepImportPool(); ok {
		steps = append(steps, prereq)
	}

	steps = append(steps, step.Step{
		Title: "Create mount point " + mountPoint,
		Cmd:   exec.Command("install", "-d", "-o", opts.User, "-g", opts.User, "-m", "0755", mountPoint),
	})

	steps = append(steps, step.Step{
		Title: "Back up /etc/fstab",
		Cmd:   bashAsRoot(`cp -a /etc/fstab "/etc/fstab.bak.$(date +%s)"`),
	})

	steps = append(steps, step.Step{
		Title: "Add UUID-based /etc/fstab entry for the pool",
		Cmd: bashAsRoot(fmt.Sprintf(`
set -e
DEV=%s
MP=%s
LABEL=%s

# Make sure the kernel knows every member device before we read the UUID,
# otherwise a cold pool can read back as not-yet-assembled.
btrfs device scan >/dev/null 2>&1 || true

UUID="$(blkid -s UUID -o value "$DEV")"
if [ -z "$UUID" ]; then
  echo "Couldn't read UUID for $DEV"
  exit 1
fi

# btrfs mounts via any constituent device. nofail so a missing drive at boot
# doesn't drop the box into emergency mode.
OPTS="defaults,nofail,x-gvfs-show,x-gvfs-name=$LABEL"
LINE="UUID=${UUID}  ${MP}  btrfs  ${OPTS}  0  0"

grep -qF -- "$LINE" /etc/fstab || printf '%%s\n' "$LINE" >> /etc/fstab
echo "fstab: $LINE"
`,
			shellQuote(dev),
			shellQuote(mountPoint),
			shellQuote(opts.Label),
		)),
	})

	steps = append(steps, step.Step{
		Title: "Reload systemd & mount the pool",
		Cmd:   bashAsRoot("systemctl daemon-reload; btrfs device scan >/dev/null 2>&1 || true; mount -a"),
	})

	steps = append(steps, aclGrantStep(mountPoint, opts.RecursiveACL))

	if opts.SeedStarterFolders {
		steps = append(steps, starterFoldersStep(opts.User, mountPoint))
	}

	return steps, nil
}
