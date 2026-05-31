package drives

import (
	"os/exec"
	"strings"

	"tuistream/internal/step"
)

// PrereqStep returns an idempotent `pacman -S --needed --noconfirm` step
// covering all the userspace tools the mount / format / wipe / ACL flow
// will actually invoke. Returns ok=false when nothing extra is needed.
//
// Arch's `base` group already ships:  util-linux (wipefs, lsblk, blkid,
// mount, findmnt), e2fsprogs (mkfs.ext4), coreutils. Things that are NOT
// in base, and that we may rely on:
//
//	btrfs-progs   →  mkfs.btrfs
//	xfsprogs      →  mkfs.xfs
//	gptfdisk      →  sgdisk (whole-disk wipe)
//	parted        →  partprobe  (whole-disk wipe)
//	acl           →  setfacl    (jellyfin read access)
//	dosfstools    →  mkfs.vfat / fsck.vfat — not used by us, skipped
func PrereqStep(formatAs FormatChoice, wipingWholeDisk bool) (step.Step, bool) {
	pkgs := requiredPackages(formatAs, wipingWholeDisk)
	if len(pkgs) == 0 {
		return step.Step{}, false
	}
	args := append([]string{"-S", "--needed", "--noconfirm"}, pkgs...)
	return step.Step{
		Title: "Install required tools: " + strings.Join(pkgs, ", "),
		Cmd:   exec.Command("pacman", args...),
	}, true
}

// PrereqStepBtrfsPool is the prereq step for a btrfs RAID pool: btrfs-progs
// is non-negotiable, gptfdisk + parted are highly desirable for whole-disk
// wipe, acl for jellyfin grant.
func PrereqStepBtrfsPool() (step.Step, bool) {
	return PrereqStep(FormatBtrfs, true)
}

// PrereqStepImportPool is the prereq step for importing an EXISTING btrfs pool:
// btrfs-progs (mount.btrfs + `btrfs device scan`) and acl (jellyfin grant).
// No mkfs/wipe tools — import is non-destructive — so we don't pull gptfdisk
// or parted. FormatBtrfs with wipingWholeDisk=false yields exactly acl +
// btrfs-progs.
func PrereqStepImportPool() (step.Step, bool) {
	return PrereqStep(FormatBtrfs, false)
}

func requiredPackages(formatAs FormatChoice, wipingWholeDisk bool) []string {
	seen := map[string]bool{}
	var pkgs []string
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		pkgs = append(pkgs, name)
	}

	// ACL is always needed — setfacl grants jellyfin read access on every
	// flow that ends with the drive mounted under /media/<user>/.
	add("acl")

	switch formatAs {
	case FormatBtrfs:
		add("btrfs-progs")
	case FormatXFS:
		add("xfsprogs")
	case FormatExt4:
		// e2fsprogs is in the `base` group on Arch, always present.
	}

	if wipingWholeDisk {
		add("gptfdisk") // sgdisk
		add("parted")   // partprobe
	}

	return pkgs
}
