package drives

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tuistream/internal/step"
)

// MountOptions captures the user's choices for an Add-drive run.
type MountOptions struct {
	Drive     Drive        // the chosen partition
	Label     string       // friendly name (becomes the directory under /media/<user>/)
	User      string       // target user (SUDO_USER); ACLs and ownership go to this user
	UserGroup string       // primary group of User; "" → resolve via getent
	FormatAs  FormatChoice // "" = keep existing fs; otherwise mkfs.<choice> with -F/-f

	// WipeWholeDisk: nuke the entire parent disk (zap GPT, wipefs every
	// signature, partprobe) before mkfs, and use the whole-disk path for
	// mkfs and the fstab UUID lookup. Only meaningful when FormatAs is set —
	// "keep existing" is a no-op for wipe. Destroys any OTHER partitions on
	// the parent disk; the confirm screen lists them so this is explicit.
	WipeWholeDisk bool

	// SeedStarterFolders creates the Jellyfin library starter folders
	// (StarterFolders) under <mount>/JellyfinMedia/. The TUI leaves this off
	// when keeping an existing filesystem that already holds the user's own
	// content, so we don't litter their layout with empty library dirs.
	SeedStarterFolders bool

	// RecursiveACL grants the jellyfin user read access over the ENTIRE existing
	// tree (setfacl -R), not just the mount root + future files. Set when we're
	// keeping a drive that already holds media: a non-recursive grant would
	// leave pre-existing files unreadable to Jellyfin if they were copied with
	// tight permissions. Harmless (read-only grant) but can take a moment on a
	// very large library, so it's only switched on when there's content to fix.
	RecursiveACL bool
}

// EffectiveFSType returns the filesystem the drive will have AFTER the plan
// runs — i.e. the formatted-to choice if we're formatting, else the
// existing filesystem.
func (o MountOptions) EffectiveFSType() string {
	if o.FormatAs != FormatKeep {
		return string(o.FormatAs)
	}
	return o.Drive.FSType
}

// EffectiveTargetDevice returns the block-device path mkfs/blkid/mount will
// actually operate on. If WipeWholeDisk is set AND a parent disk was
// resolved, that's the whole disk (/dev/sda); for a whole-disk Drive
// (Type=="disk") it's already the disk; otherwise it's the chosen partition.
func (o MountOptions) EffectiveTargetDevice() string {
	if o.WipeWholeDisk && o.Drive.ParentDisk != "" {
		return "/dev/" + o.Drive.ParentDisk
	}
	return o.Drive.Path
}

// WipeTarget returns the path of the whole-disk wipe step's victim, or "" if
// no wipe step should run. We wipe whenever the user chose a destructive
// format AND either explicitly opted into wipe-whole-disk or picked a
// whole-disk Drive (where wiping the entire device is the only sensible
// thing).
func (o MountOptions) WipeTarget() string {
	if o.FormatAs == FormatKeep {
		return ""
	}
	if o.Drive.Type == "disk" {
		// Whole-disk Drive — wipe self.
		return o.Drive.Path
	}
	if o.WipeWholeDisk && o.Drive.ParentDisk != "" {
		return "/dev/" + o.Drive.ParentDisk
	}
	return ""
}

// MountPlan builds the ordered list of commands needed to attach `opts.Drive`
// as a media drive at /media/<user>/<label>:
//
//  0. Optionally mkfs.<choice> the drive (destructive — guarded by the TUI's
//     final confirm modal)
//  1. mkdir the mount point
//  2. Back up /etc/fstab
//  3. Append a UUID-based line to /etc/fstab — UUID read at runtime so it
//     reflects the post-format UUID if we just reformatted
//  4. daemon-reload + mount -a
//  5. ACL: grant the `jellyfin` user read on the tree (if user exists)
//  6. Create the Jellyfin library starter folders (StarterFolders)
func MountPlan(opts MountOptions) ([]step.Step, error) {
	if opts.Drive.Path == "" {
		return nil, fmt.Errorf("MountPlan: no drive selected")
	}
	if opts.Label == "" {
		return nil, fmt.Errorf("MountPlan: empty label")
	}
	if opts.User == "" {
		return nil, fmt.Errorf("MountPlan: no target user (SUDO_USER)")
	}

	mountPoint := filepath.Join("/media", opts.User, opts.Label)
	effFS := opts.EffectiveFSType()
	target := opts.EffectiveTargetDevice()

	var steps []step.Step

	// -2. Make sure the userspace tools we're about to invoke actually exist.
	//     A minimal Arch install doesn't ship mkfs.btrfs / mkfs.xfs / sgdisk /
	//     partprobe / setfacl — installing them first turns a confusing
	//     "command not found" mid-flow into a clean pacman run.
	wipingThisRun := opts.WipeTarget() != ""
	if prereq, ok := PrereqStep(opts.FormatAs, wipingThisRun); ok {
		steps = append(steps, prereq)
	}

	// -1. Whole-disk wipe (optional, very destructive). Only meaningful when
	//     we're also formatting — keeping the existing FS doesn't pair with
	//     "erase the disk". The TUI's confirm screen lists every other
	//     partition that will be erased so this is informed consent.
	if parentDev := opts.WipeTarget(); parentDev != "" {
		steps = append(steps, step.Step{
			Title: "Wipe ENTIRE disk " + parentDev,
			Cmd: bashAsRoot(fmt.Sprintf(`
set -e
DEV=%s
echo "▸ Preparing $DEV for a whole-disk wipe …"
# Unmount any of its partitions that happen to still be mounted.
for p in $(lsblk -lnpo NAME "$DEV" | tail -n +2); do
  mp="$(findmnt -no TARGET "$p" 2>/dev/null || true)"
  if [ -n "$mp" ]; then
    echo "  umount $p (was at $mp)"
    umount -f "$p" || true
  fi
done
echo "  wipefs -a -f $DEV"
wipefs -a -f "$DEV"
if command -v sgdisk >/dev/null 2>&1; then
  echo "  sgdisk --zap-all $DEV"
  sgdisk --zap-all "$DEV" >/dev/null 2>&1 || true
fi
if command -v partprobe >/dev/null 2>&1; then
  partprobe "$DEV" >/dev/null 2>&1 || true
fi
blockdev --rereadpt "$DEV" >/dev/null 2>&1 || true
udevadm settle >/dev/null 2>&1 || true
`, shellQuote(parentDev))),
		})
	}

	// 0. Format step (optional, destructive). Uses the EFFECTIVE target —
	//    whole disk if we just wiped it, otherwise the picked partition.
	if fmtStep, ok := FormatStep(target, opts.Label, opts.FormatAs); ok {
		steps = append(steps, fmtStep)
	}

	// 1. Mount point.
	steps = append(steps, step.Step{
		Title: "Create mount point " + mountPoint,
		Cmd:   exec.Command("install", "-d", "-o", opts.User, "-g", opts.User, "-m", "0755", mountPoint),
	})

	// 2. fstab backup.
	steps = append(steps, step.Step{
		Title: "Back up /etc/fstab",
		Cmd:   bashAsRoot(`cp -a /etc/fstab "/etc/fstab.bak.$(date +%s)"`),
	})

	// 3. fstab entry — UUID read AT RUNTIME, from the effective target
	//    (whole disk or partition), so post-format UUID is captured.
	uid, gid := resolveUIDGID(opts.User)
	steps = append(steps, step.Step{
		Title: "Add UUID-based /etc/fstab entry",
		Cmd: bashAsRoot(fmt.Sprintf(`
set -e
DEV=%s
MP=%s
FS=%s
LABEL=%s
UID_N=%d
GID_N=%d

UUID="$(blkid -s UUID -o value "$DEV")"
if [ -z "$UUID" ]; then
  echo "Couldn't read UUID for $DEV"
  exit 1
fi

COMMON="defaults,nofail,x-gvfs-show,x-gvfs-name=$LABEL"
case "$FS" in
  exfat|ntfs|vfat) OPTS="uid=${UID_N},gid=${GID_N},umask=002,${COMMON}" ;;
  *)               OPTS="$COMMON" ;;
esac
LINE="UUID=${UUID}  ${MP}  ${FS}  ${OPTS}  0  2"

grep -qF -- "$LINE" /etc/fstab || printf '%%s\n' "$LINE" >> /etc/fstab
echo "fstab entry: $LINE"
`,
			shellQuote(target),
			shellQuote(mountPoint),
			shellQuote(effFS),
			shellQuote(opts.Label),
			uid, gid,
		)),
	})

	// 4. Reload + mount.
	steps = append(steps, step.Step{
		Title: "Reload systemd & mount the new entry",
		Cmd:   bashAsRoot("systemctl daemon-reload && mount -a"),
	})

	// ACL grant — only meaningful on filesystems that actually support POSIX
	// ACLs. On ntfs/exfat we already used uid=/gid= in the fstab options so
	// jellyfin (added to the user's primary group via supplementary group, if
	// configured) will see the right perms; skip setfacl there.
	switch effFS {
	case "ext4", "btrfs", "xfs":
		steps = append(steps, aclGrantStep(mountPoint, opts.RecursiveACL))
	}

	if opts.SeedStarterFolders {
		steps = append(steps, starterFoldersStep(opts.User, mountPoint))
	}

	return steps, nil
}

// aclGrantStep returns the step that grants the jellyfin service account read
// access to a mounted media tree. Shared by the Add-drive and pool-import flows
// so the ACL logic can't drift between them.
//
// With recursive=false it grants the mount root plus a default ACL (so files
// created LATER inherit access) — correct for a freshly-formatted/empty drive.
// With recursive=true it additionally walks the existing tree with `setfacl -R`
// so media already on a kept drive becomes readable even if it was copied with
// tight permissions; this is the only correct choice when content is present.
func aclGrantStep(mountPoint string, recursive bool) step.Step {
	title := "Grant the 'jellyfin' user read access (POSIX ACL)"
	recurse := ""
	if recursive {
		title = "Grant the 'jellyfin' user read access to existing files (recursive ACL)"
		// rX = read on files, traverse on dirs only — won't make plain files
		// executable. Run before the default-ACL line so a partial failure on
		// one odd file still leaves the bulk granted.
		recurse = "  echo \"  applying read access across existing files (may take a moment on a large library)…\"\n" +
			"  setfacl -R -m u:jellyfin:rX " + shellQuote(mountPoint) + " || true\n"
	}
	return step.Step{
		Title: title,
		Cmd: bashAsRoot(fmt.Sprintf(`
if id -u jellyfin >/dev/null 2>&1; then
%[2]s  setfacl -m u:jellyfin:rx %[1]s || true
  setfacl -d -m u:jellyfin:rx %[1]s || true
else
  echo "  (jellyfin user not present yet — re-run after installing Jellyfin)"
fi
`, shellQuote(mountPoint), recurse)),
	}
}

// StarterFolders are the library directories seeded on a new media drive when
// the user opts in. Names match Jellyfin's content types exactly, so each maps
// straight onto a library you add in the Jellyfin web UI (Movies → Movies
// library, Shows → Shows library, and so on).
var StarterFolders = []string{
	"Movies", "Shows", "Music", "Books", "Home Videos", "Music Videos",
}

// starterFoldersStep builds the step that creates StarterFolders under
// <mountPoint>/JellyfinMedia/, owned by `user` and readable by the jellyfin
// service account. Shared by single-drive (MountPlan) and pool (PoolPlan) so
// the two paths can never drift apart. Each folder name is shell-quoted so
// multi-word entries ("Home Videos") survive word-splitting in the for-loop.
func starterFoldersStep(user, mountPoint string) step.Step {
	quoted := make([]string, len(StarterFolders))
	for i, f := range StarterFolders {
		quoted[i] = shellQuote(f)
	}
	return step.Step{
		Title: "Create starter folders (" + strings.Join(StarterFolders, ", ") + ")",
		Cmd: bashAsRoot(fmt.Sprintf(`
for d in %[3]s; do
  install -d -o %[1]s -g %[1]s -m 0755 %[2]s/JellyfinMedia/"$d"
done
if id -u jellyfin >/dev/null 2>&1; then
  setfacl -R -m u:jellyfin:rx %[2]s/JellyfinMedia 2>/dev/null || true
  setfacl -R -d -m u:jellyfin:rx %[2]s/JellyfinMedia 2>/dev/null || true
fi
`, user, shellQuote(mountPoint), strings.Join(quoted, " "))),
	}
}

// InspectMount looks at what's already on `path` (assumed to be the freshly-
// mounted media drive) and returns a human-readable summary the confirm view
// can show before the user commits to anything.
func InspectMount(path string) (string, bool, error) {
	out, err := exec.Command("bash", "-c", fmt.Sprintf(`
shopt -s nullglob dotglob
dirs=()
files=()
total=0
for e in %s/*; do
  base="$(basename "$e")"
  [ "$base" = "lost+found" ] && continue
  case "$base" in .*) continue;; esac
  if [ -d "$e" ]; then dirs+=("$base"); else files+=("$base"); fi
  total=$((total+1))
done
echo "DIRS:${dirs[*]}"
echo "FILES:${files[*]}"
echo "TOTAL:$total"
echo "SIZE:$(df -h --output=used %s 2>/dev/null | tail -n1 | tr -d ' ')"
`, shellQuote(path), shellQuote(path))).Output()
	if err != nil {
		return "", false, err
	}
	lines := strings.Split(string(out), "\n")
	var dirs, files []string
	var size string
	total := 0
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "DIRS:"):
			dirs = splitSpaceFields(strings.TrimPrefix(l, "DIRS:"))
		case strings.HasPrefix(l, "FILES:"):
			files = splitSpaceFields(strings.TrimPrefix(l, "FILES:"))
		case strings.HasPrefix(l, "TOTAL:"):
			fmt.Sscanf(strings.TrimPrefix(l, "TOTAL:"), "%d", &total)
		case strings.HasPrefix(l, "SIZE:"):
			size = strings.TrimPrefix(l, "SIZE:")
		}
	}
	if total == 0 {
		return "Empty.", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Has files (≈%s of data).\n", size)
	if len(dirs) > 0 {
		max := 8
		if len(dirs) < max {
			max = len(dirs)
		}
		fmt.Fprintf(&b, "  Top-level folders: %s", strings.Join(dirs[:max], ", "))
		if len(dirs) > max {
			fmt.Fprintf(&b, " … +%d more", len(dirs)-max)
		}
		b.WriteString("\n")
	}
	if len(files) > 0 {
		max := 5
		if len(files) < max {
			max = len(files)
		}
		fmt.Fprintf(&b, "  Loose files: %s", strings.Join(files[:max], ", "))
		if len(files) > max {
			fmt.Fprintf(&b, " … +%d more", len(files)-max)
		}
		b.WriteString("\n")
	}
	b.WriteString("They'll be kept as-is.")
	return b.String(), true, nil
}

// InspectDevice temporarily mounts `dev` read-only, summarises its top-level
// contents via InspectMount, then unmounts and removes the scratch mountpoint.
//
// It exists because the Add-drive confirm screen needs to show what's already
// on a "Keep existing filesystem" drive, but at confirm time the drive is not
// mounted anywhere. A read-only mount means we can never damage the data we're
// reassuring the user about.
//
// Best-effort by design: any failure (unsupported FS, missing driver, busy
// device) returns a soft, human-readable message and hasFiles=false rather
// than an error, so the confirm flow is never blocked by inspection. A 20s
// context timeout guards against a wedged mount hanging the UI.
func InspectDevice(dev string) (summary string, hasFiles bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tmpOut, err := exec.CommandContext(ctx, "mktemp", "-d", "/tmp/tuistream-inspect.XXXXXX").Output()
	if err != nil {
		return "Couldn't create a scratch mountpoint to inspect — files will be kept untouched.", false
	}
	mp := strings.TrimSpace(string(tmpOut))
	defer exec.Command("rmdir", mp).Run()

	if err := exec.CommandContext(ctx, "mount", "-o", "ro", dev, mp).Run(); err != nil {
		return "Couldn't read this drive read-only to preview it — files will be kept untouched.", false
	}
	defer exec.Command("umount", mp).Run()

	s, has, err := InspectMount(mp)
	if err != nil {
		return "Couldn't list this drive's contents — files will be kept untouched.", false
	}
	return s, has
}

// ---- helpers ----

func bashAsRoot(script string) *exec.Cmd {
	return exec.Command("bash", "-lc", script)
}

func resolveUIDGID(user string) (int, int) {
	out, err := exec.Command("getent", "passwd", user).Output()
	if err != nil {
		return 1000, 1000
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) < 4 {
		return 1000, 1000
	}
	var uid, gid int
	fmt.Sscanf(fields[2], "%d", &uid)
	fmt.Sscanf(fields[3], "%d", &gid)
	return uid, gid
}

// shellQuote produces a POSIX-shell-safe single-quoted token.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' || r == '/' || r == ':' || r == '=' || r == '@' || r == '+' || r == ',') {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

func splitSpaceFields(s string) []string {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	sc.Split(bufio.ScanWords)
	var out []string
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}
