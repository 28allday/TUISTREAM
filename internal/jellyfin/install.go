package jellyfin

import (
	"os/exec"
	"strings"

	"tuistream/internal/step"
	"tuistream/internal/system"
)

// InstallPlan builds the ordered list of commands needed to install Jellyfin
// from the official Arch extra repo. We don't support the AUR jellyfin-bin
// path anymore — see reference_aur_jellyfin_bin_stale memory for the why.
//
// This is a *plan*, not an execution. The TUI runs it step by step via
// tea.ExecProcess so pacman can prompt interactively if needed.
func InstallPlan() []step.Step {
	steps := []step.Step{
		{
			Title: "Refresh package databases",
			Cmd:   exec.Command("pacman", "-Sy", "--noconfirm"),
		},
		{
			Title: "Install jellyfin-server + jellyfin-web + jellyfin-ffmpeg",
			Cmd: exec.Command("pacman", "-S", "--needed", "--noconfirm",
				"jellyfin-server", "jellyfin-web", "jellyfin-ffmpeg"),
		},
	}
	steps = append(steps, gpuEncodingSteps()...)
	return append(steps, step.Step{
		Title: "Enable and start jellyfin.service",
		Cmd:   exec.Command("systemctl", "enable", "--now", "jellyfin.service"),
	})
}

// gpuEncodingSteps maps the host's GPU vendor(s) to the userspace packages
// jellyfin-ffmpeg needs before hardware transcoding works on that vendor.
// Without them Jellyfin installs and direct-plays fine, but the moment a
// client needs a transcode every attempt dies at hw-init with the opaque
// "FFmpeg exited with code 251" / fatal-playback-error combo.
//
// NVIDIA is deliberately conservative: NVENC needs only the driver's own
// userspace (nvidia-utils), so with a loaded driver there is nothing to
// add, and without one we won't auto-install a kernel driver from here —
// picking nvidia vs nvidia-open vs -dkms per kernel flavour is a job for
// the distro/user, and getting it wrong can break the box's boot.
func gpuEncodingSteps() []step.Step {
	var steps []step.Step
	for _, v := range system.DetectGPUs() {
		switch v {
		case system.VendorIntel:
			steps = append(steps, step.Step{
				Title: "Install Intel QSV/VA-API encoding packages",
				Cmd: exec.Command("pacman", "-S", "--needed", "--noconfirm",
					"intel-media-driver", "vpl-gpu-rt", "intel-compute-runtime"),
			})
		case system.VendorAMD:
			steps = append(steps, step.Step{
				Title: "Install AMD VA-API encoding packages",
				Cmd: exec.Command("pacman", "-S", "--needed", "--noconfirm",
					"mesa", "libva-mesa-driver"),
			})
		case system.VendorNVIDIA:
			if system.NvidiaDriverLoaded() {
				continue
			}
			steps = append(steps, step.Step{
				Title: "NVIDIA GPU found but no driver loaded — skipping NVENC setup",
				Cmd: exec.Command("bash", "-lc",
					`echo "Install the NVIDIA driver for your kernel (nvidia / nvidia-open / nvidia-lts) and reboot to enable NVENC transcoding."`),
			})
		}
	}
	return steps
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
