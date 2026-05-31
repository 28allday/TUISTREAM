package drives

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"tuistream/internal/step"
)

// RAIDLevel is the btrfs data-profile we pass to `mkfs.btrfs -d`. We always
// pair this with a safe metadata profile (raid1 for raid0/raid5, raid10 for
// raid10) because btrfs metadata corruption is much worse than data
// corruption — the filesystem itself becomes unreadable.
type RAIDLevel string

const (
	RAID0  RAIDLevel = "raid0"
	RAID1  RAIDLevel = "raid1"
	RAID5  RAIDLevel = "raid5"
	RAID10 RAIDLevel = "raid10"
)

// MinDrives reports the smallest sensible drive count for a given level.
// (mkfs.btrfs will let you create raid5 with 2 drives but it's degenerate.)
func (r RAIDLevel) MinDrives() int {
	switch r {
	case RAID0, RAID1:
		return 2
	case RAID5:
		return 3
	case RAID10:
		return 4
	}
	return 2
}

// MetadataProfile returns the safe metadata profile for this RAID level.
// btrfs metadata corruption breaks the whole filesystem, so we don't follow
// the data profile blindly — raid5 metadata is unsafe (write-hole), raid0
// metadata loses the filesystem on a single-disk failure.
func (r RAIDLevel) MetadataProfile() string {
	switch r {
	case RAID0, RAID5:
		return "raid1" // safe choice independent of data profile
	case RAID10:
		return "raid10"
	}
	return string(r)
}

// Tolerates returns "tolerates N drive failure(s)" or "no redundancy".
func (r RAIDLevel) Tolerates() string {
	switch r {
	case RAID0:
		return "no redundancy — one failed drive loses the whole pool"
	case RAID1:
		return "any single drive can fail"
	case RAID5:
		return "any single drive can fail"
	case RAID10:
		return "one drive in each mirror can fail"
	}
	return ""
}

// PoolOptions captures the user's choices for creating a multi-drive pool.
type PoolOptions struct {
	Drives []Drive
	Level  RAIDLevel
	Label  string // friendly directory name under /media/<user>/
	User   string

	// WipeWholeDisks: when true, every selected partition's PARENT DISK is
	// zapped (sgdisk --zap-all + wipefs -af) before mkfs, and mkfs.btrfs is
	// given the whole-disk path (/dev/sda) instead of the partition
	// (/dev/sda1). This is the right thing for RAID because it kills every
	// stray FS/MD/LVM signature that would otherwise confuse btrfs about
	// which device belongs to the pool. It also destroys any OTHER
	// partitions on those disks — the confirm screen must show this.
	WipeWholeDisks bool
}

// TargetDevices returns the actual block-device paths mkfs.btrfs will be
// called with, given WipeWholeDisks. With WipeWholeDisks=true these are the
// unique parent disks (each listed once); with =false they are the chosen
// partitions verbatim.
func (o PoolOptions) TargetDevices() []string {
	if !o.WipeWholeDisks {
		out := make([]string, len(o.Drives))
		for i, d := range o.Drives {
			out[i] = d.Path
		}
		return out
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range o.Drives {
		parent := d.ParentDisk
		if parent == "" {
			// Already a whole disk — use as-is.
			out = append(out, d.Path)
			continue
		}
		p := "/dev/" + parent
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// PoolPlan builds the ordered list of commands to wipe the selected drives
// and create a single btrfs filesystem spanning all of them at the chosen
// RAID level. Format is always destructive (RAID setup requires fresh disks).
func PoolPlan(opts PoolOptions) ([]step.Step, error) {
	if len(opts.Drives) < opts.Level.MinDrives() {
		return nil, fmt.Errorf("RAID %s needs at least %d drives (got %d)",
			opts.Level, opts.Level.MinDrives(), len(opts.Drives))
	}
	if opts.Label == "" {
		return nil, fmt.Errorf("PoolPlan: empty label")
	}
	if opts.User == "" {
		return nil, fmt.Errorf("PoolPlan: no target user")
	}

	mountPoint := filepath.Join("/media", opts.User, opts.Label)

	devs := opts.TargetDevices()
	devsQuoted := make([]string, len(devs))
	for i, d := range devs {
		devsQuoted[i] = shellQuote(d)
	}
	devList := strings.Join(devsQuoted, " ")

	var steps []step.Step

	// 0. Prereqs: btrfs-progs, gptfdisk, parted, acl. Minimal Arch boxes
	//    don't ship these.
	if prereq, ok := PrereqStepBtrfsPool(); ok {
		steps = append(steps, prereq)
	}

	// 1. Wipe existing signatures. For partition-level we just wipefs; for
	//    whole-disk we additionally sgdisk-zap and partprobe so btrfs can't
	//    later get confused by leftover GPT backups or per-partition magic.
	wipeTitle := fmt.Sprintf("Wipe signatures from %d partition(s)", len(devs))
	wipeBody := fmt.Sprintf(`
set -e
for d in %s; do
  # Unmount the partition if it's still mounted, else mkfs.btrfs refuses it.
  mp="$(findmnt -no TARGET "$d" 2>/dev/null | head -n1 || true)"
  if [ -n "$mp" ]; then
    echo "  umount $d (was at $mp)"
    umount -R -f "$d" 2>/dev/null || umount -l "$d" 2>/dev/null || true
  fi
  echo "▸ wipefs -a -f $d"
  wipefs -a -f "$d"
done
`, devList)
	if opts.WipeWholeDisks {
		wipeTitle = fmt.Sprintf("Wipe ENTIRE parent disk(s) — %d device(s)", len(devs))
		wipeBody = fmt.Sprintf(`
set -e
for d in %s; do
  echo "▸ Preparing $d for RAID …"
  # Unmount the disk itself (a whole-disk fs, e.g. a btrfs pool from a previous
  # run) AND any of its partitions that happen to still be mounted. The disk
  # line must be included — without it, re-running over an already-mounted
  # whole-disk pool fails mkfs.btrfs with "ERROR: $d is mounted".
  for p in $(lsblk -lnpo NAME "$d"); do
    mp="$(findmnt -no TARGET "$p" 2>/dev/null | head -n1 || true)"
    if [ -n "$mp" ]; then
      echo "  umount $p (was at $mp)"
      umount -R -f "$p" 2>/dev/null || umount -l "$p" 2>/dev/null || true
    fi
  done
  # Belt-and-braces wipe: util-linux wipefs nukes the signatures it knows,
  # sgdisk --zap-all (from gptfdisk, optional) nukes GPT + protective MBR
  # including the backup at the disk's tail, and partprobe forces the kernel
  # to drop its cached partition table.
  echo "  wipefs -a -f $d"
  wipefs -a -f "$d"
  if command -v sgdisk >/dev/null 2>&1; then
    echo "  sgdisk --zap-all $d"
    sgdisk --zap-all "$d" >/dev/null 2>&1 || true
  fi
  if command -v partprobe >/dev/null 2>&1; then
    partprobe "$d" >/dev/null 2>&1 || true
  fi
  blockdev --rereadpt "$d" >/dev/null 2>&1 || true
  udevadm settle >/dev/null 2>&1 || true
done
`, devList)
	}
	steps = append(steps, step.Step{
		Title: wipeTitle,
		Cmd:   bashAsRoot(wipeBody),
	})

	// 2. Create the btrfs pool. -f overwrites any remaining FS magic.
	mkfsArgs := []string{
		"-f",
		"-L", opts.Label,
		"-d", string(opts.Level),
		"-m", opts.Level.MetadataProfile(),
	}
	mkfsArgs = append(mkfsArgs, devs...)
	scope := "partition"
	if opts.WipeWholeDisks {
		scope = "whole-disk"
	}
	steps = append(steps, step.Step{
		Title: fmt.Sprintf("Create btrfs %s pool (label=%q, %d %s device(s))",
			opts.Level, opts.Label, len(devs), scope),
		Cmd: exec.Command("mkfs.btrfs", mkfsArgs...),
	})

	// 3. Mount point.
	steps = append(steps, step.Step{
		Title: "Create mount point " + mountPoint,
		Cmd:   exec.Command("install", "-d", "-o", opts.User, "-g", opts.User, "-m", "0755", mountPoint),
	})

	// 4. fstab backup.
	steps = append(steps, step.Step{
		Title: "Back up /etc/fstab",
		Cmd:   bashAsRoot(`cp -a /etc/fstab "/etc/fstab.bak.$(date +%s)"`),
	})

	// 5. fstab entry — btrfs lets us mount any constituent device and have
	//    it auto-resolve the rest. We use the first device's post-mkfs UUID
	//    (could be either /dev/sda or /dev/sda1 depending on WipeWholeDisks).
	steps = append(steps, step.Step{
		Title: "Add UUID-based /etc/fstab entry for the pool",
		Cmd: bashAsRoot(fmt.Sprintf(`
set -e
DEV=%s
MP=%s
LABEL=%s

UUID="$(blkid -s UUID -o value "$DEV")"
if [ -z "$UUID" ]; then
  echo "Couldn't read UUID for $DEV after mkfs.btrfs — aborting"
  exit 1
fi

# btrfs mounts via any constituent device. nofail so a missing drive at boot
# doesn't drop the box into emergency mode — we'd rather start without it.
OPTS="defaults,nofail,x-gvfs-show,x-gvfs-name=$LABEL"
LINE="UUID=${UUID}  ${MP}  btrfs  ${OPTS}  0  0"

grep -qF -- "$LINE" /etc/fstab || printf '%%s\n' "$LINE" >> /etc/fstab
echo "fstab: $LINE"
`,
			shellQuote(devs[0]),
			shellQuote(mountPoint),
			shellQuote(opts.Label),
		)),
	})

	// 6. Mount.
	steps = append(steps, step.Step{
		Title: "Reload systemd & mount the pool",
		Cmd:   bashAsRoot("systemctl daemon-reload && mount -a"),
	})

	// 7. ACL (btrfs supports POSIX ACLs unconditionally).
	steps = append(steps, step.Step{
		Title: "Grant the 'jellyfin' user read access (POSIX ACL)",
		Cmd: bashAsRoot(fmt.Sprintf(`
if id -u jellyfin >/dev/null 2>&1; then
  setfacl -m u:jellyfin:rx %[1]s || true
  setfacl -d -m u:jellyfin:rx %[1]s || true
else
  echo "  (jellyfin user not present yet — re-run after installing Jellyfin)"
fi
`, shellQuote(mountPoint))),
	})

	// 8. Starter folders.
	steps = append(steps, starterFoldersStep(opts.User, mountPoint))

	return steps, nil
}

// AvailableLevels returns the RAID levels valid for `n` drives, in the order
// we want to show them in the UI (RAID 1 first since it's the safest).
func AvailableLevels(n int) []RAIDLevel {
	all := []RAIDLevel{RAID1, RAID0, RAID5, RAID10}
	var out []RAIDLevel
	for _, l := range all {
		if l.MinDrives() <= n {
			out = append(out, l)
		}
	}
	return out
}
