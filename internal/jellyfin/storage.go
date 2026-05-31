package jellyfin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"tuistream/internal/step"
)

// Default Arch locations for Jellyfin's growing state. The library DB,
// downloaded metadata and artwork live under the data dir; the cache dir holds
// the (regenerable) image cache and transcode scratch space.
//
// On Arch these paths are effectively pinned — Jellyfin ignores the
// JELLYFIN_DATA_DIR environment variable and always uses /var/lib/jellyfin.
// So rather than repoint Jellyfin, we relocate the *storage* underneath it
// with a bind mount: Jellyfin keeps using the default paths, but they
// physically live on the media drive.
const (
	defaultDataDir  = "/var/lib/jellyfin"
	defaultCacheDir = "/var/cache/jellyfin"
)

// MoveOptions configures relocating Jellyfin's data + cache onto a media drive.
type MoveOptions struct {
	MountPoint  string // managed media-drive mount, e.g. /media/q/MediaPool
	ServiceUnit string // e.g. "jellyfin.service"
}

func unitOrDefault(unit string) string {
	if unit == "" {
		return "jellyfin.service"
	}
	return unit
}

// Data/cache subpaths on the media drive — siblings of JellyfinMedia/ so a
// library scan never indexes them.
func dataDirFor(mp string) string  { return filepath.Join(mp, "JellyfinData", "data") }
func cacheDirFor(mp string) string { return filepath.Join(mp, "JellyfinData", "cache") }

// StorageState describes where Jellyfin currently keeps its data.
type StorageState struct {
	Moved    bool   // a bind mount relocating /var/lib/jellyfin is in fstab
	DataDir  string // bind source if moved, else the default
	CacheDir string
}

// LoadStorageState reports whether Jellyfin's storage has already been
// relocated, by looking for the bind mount in /etc/fstab. Cheap to call.
func LoadStorageState() StorageState {
	st := StorageState{DataDir: defaultDataDir, CacheDir: defaultCacheDir}
	b, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return st
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.Contains(line, "bind") {
			continue
		}
		switch fields[1] {
		case defaultDataDir:
			st.Moved = true
			st.DataDir = fields[0]
		case defaultCacheDir:
			st.CacheDir = fields[0]
		}
	}
	return st
}

// MovePlan builds the ordered steps to relocate Jellyfin's data + cache onto
// the media drive via bind mounts, then restart from the (unchanged) default
// paths and reclaim the OS-drive space.
//
// Why bind mounts: Jellyfin on Arch always uses /var/lib/jellyfin regardless of
// JELLYFIN_DATA_DIR, so we make that path *be* the media drive. The unit's
// existing WorkingDirectory=/var/lib/jellyfin makes systemd auto-wait for the
// mount, and x-systemd.requires-mounts-for chains it behind the media drive —
// so ordering is handled with no service edits.
//
// The original data is emptied only after a verified non-empty copy exists on
// the media drive, so a failure mid-run never loses the library.
func MovePlan(opts MoveOptions) []step.Step {
	unit := unitOrDefault(opts.ServiceUnit)
	mp := opts.MountPoint
	data := dataDirFor(mp)
	cache := cacheDirFor(mp)

	return []step.Step{
		{
			Title: "Stop " + unit,
			Cmd:   exec.Command("systemctl", "stop", unit),
		},
		{
			Title: "Create JellyfinData/{data,cache} on the media drive",
			Cmd: bashAsRoot(fmt.Sprintf(
				"install -d -o jellyfin -g jellyfin -m 0750 %s %s %s",
				shellQuote(filepath.Join(mp, "JellyfinData")),
				shellQuote(data), shellQuote(cache))),
		},
		{
			Title: "Copy library DB, metadata & artwork → media drive",
			Cmd: bashAsRoot(fmt.Sprintf(`
if command -v rsync >/dev/null 2>&1; then
  rsync -aHAX %[1]s/ %[2]s/
else
  cp -a %[1]s/. %[2]s/
fi
chown -R jellyfin:jellyfin %[2]s`,
				shellQuote(defaultDataDir), shellQuote(data))),
		},
		{
			Title: "Reclaim OS-drive space (only if the copy looks complete)",
			Cmd: bashAsRoot(fmt.Sprintf(`
if [ -n "$(ls -A %[1]s 2>/dev/null)" ]; then
  find %[2]s -mindepth 1 -delete 2>/dev/null || true
fi
find %[3]s -mindepth 1 -delete 2>/dev/null || true
install -d %[2]s %[3]s`,
				shellQuote(data), shellQuote(defaultDataDir), shellQuote(defaultCacheDir))),
		},
		{
			Title: "Add bind mounts to /etc/fstab",
			Cmd: bashAsRoot(fmt.Sprintf(`
grep -qsF ' %[2]s ' /etc/fstab || printf '%%s %%s none bind,x-systemd.requires-mounts-for=%[5]s 0 0\n' %[1]s %[2]s >> /etc/fstab
grep -qsF ' %[4]s ' /etc/fstab || printf '%%s %%s none bind,x-systemd.requires-mounts-for=%[5]s 0 0\n' %[3]s %[4]s >> /etc/fstab`,
				shellQuote(data), defaultDataDir,
				shellQuote(cache), defaultCacheDir,
				mp)),
		},
		{
			Title: "Mount the relocated storage",
			Cmd: bashAsRoot(fmt.Sprintf(`
systemctl daemon-reload
mountpoint -q %[1]s || mount %[1]s
mountpoint -q %[2]s || mount %[2]s
chown -R jellyfin:jellyfin %[1]s %[2]s`,
				defaultDataDir, defaultCacheDir)),
		},
		{
			Title: "Start " + unit + " from the relocated storage",
			Cmd: bashAsRoot(fmt.Sprintf(
				"systemctl reset-failed %[1]s 2>/dev/null || true\nsystemctl start %[1]s", unit)),
		},
	}
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
