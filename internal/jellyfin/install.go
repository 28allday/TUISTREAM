package jellyfin

import (
	"os/exec"
	"strings"

	"tuistream/internal/step"
)

// InstallPlan builds the ordered list of commands needed to install Jellyfin
// from the official Arch extra repo. We don't support the AUR jellyfin-bin
// path anymore — see reference_aur_jellyfin_bin_stale memory for the why.
//
// This is a *plan*, not an execution. The TUI runs it step by step via
// tea.ExecProcess so pacman can prompt interactively if needed.
func InstallPlan() []step.Step {
	return []step.Step{
		{
			Title: "Refresh package databases",
			Cmd:   exec.Command("pacman", "-Sy", "--noconfirm"),
		},
		{
			Title: "Install jellyfin-server + jellyfin-web + jellyfin-ffmpeg",
			Cmd: exec.Command("pacman", "-S", "--needed", "--noconfirm",
				"jellyfin-server", "jellyfin-web", "jellyfin-ffmpeg"),
		},
		{
			Title: "Enable and start jellyfin.service",
			Cmd:   exec.Command("systemctl", "enable", "--now", "jellyfin.service"),
		},
	}
}

// UninstallPlan reverses an install. Pass `purgeData` to also delete
// /var/lib/jellyfin (library DB + settings).
func UninstallPlan(packages PackageSet, purgeData bool) []step.Step {
	var steps []step.Step

	steps = append(steps, step.Step{
		Title: "Stop and disable jellyfin.service",
		Cmd:   exec.Command("systemctl", "disable", "--now", "jellyfin.service"),
	})

	if names := packages.Installed(); len(names) > 0 {
		args := []string{"-Rns", "--noconfirm"}
		args = append(args, names...)
		steps = append(steps, step.Step{
			Title: "Remove " + sprintList(names),
			Cmd:   exec.Command("pacman", args...),
		})
	}

	if purgeData {
		steps = append(steps, step.Step{
			Title: "Delete /var/lib/jellyfin (library DB + settings)",
			Cmd:   exec.Command("rm", "-rf", "/var/lib/jellyfin"),
		})
		steps = append(steps, step.Step{
			Title: "Delete /etc/jellyfin and caches",
			Cmd:   bashAsRoot(`rm -rf /etc/jellyfin /var/cache/jellyfin /var/log/jellyfin`),
		})
	}

	return steps
}

// ---- helpers ----

func bashAsRoot(script string) *exec.Cmd {
	return exec.Command("bash", "-lc", script)
}

func sprintList(s []string) string {
	return strings.Join(s, ", ")
}
