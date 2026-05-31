package drives

import (
	"fmt"
	"os/exec"

	"tuistream/internal/step"
)

// FormatChoice names one of the filesystems the user can put on a media
// drive. Empty string = "don't format, keep what's there".
type FormatChoice string

const (
	FormatKeep  FormatChoice = ""      // use the existing filesystem as-is
	FormatExt4  FormatChoice = "ext4"  // widely compatible, solid default
	FormatBtrfs FormatChoice = "btrfs" // snapshots, multi-device growth, scrub
	FormatXFS   FormatChoice = "xfs"   // great for very large files
)

// FormatStep returns a single step that wipes the device and lays down a
// fresh filesystem of `fs`. `label` is applied as the filesystem label so
// `blkid` shows it and the inventory's "Label" column populates.
func FormatStep(devicePath, label string, fs FormatChoice) (step.Step, bool) {
	if fs == FormatKeep {
		return step.Step{}, false
	}
	var args []string
	var bin string
	switch fs {
	case FormatExt4:
		bin = "mkfs.ext4"
		args = []string{"-F"}
		if label != "" {
			args = append(args, "-L", label)
		}
	case FormatBtrfs:
		bin = "mkfs.btrfs"
		args = []string{"-f"}
		if label != "" {
			args = append(args, "-L", label)
		}
	case FormatXFS:
		bin = "mkfs.xfs"
		args = []string{"-f"}
		if label != "" {
			args = append(args, "-L", label)
		}
	default:
		return step.Step{}, false
	}
	args = append(args, devicePath)
	return step.Step{
		Title: fmt.Sprintf("Format %s as %s (label=%q) — ERASES existing data", devicePath, fs, label),
		Cmd:   exec.Command(bin, args...),
	}, true
}
