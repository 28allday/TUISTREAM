// Package jellyfin detects what Jellyfin packages are installed on the host
// and whether the service is running. The Status type drives the Setup tab's
// "Install vs Reinstall" decision.
package jellyfin

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// WebPort is Jellyfin's default HTTP port. The firewall plan opens 8096/tcp.
const WebPort = 8096

// WebURLs returns the browseable addresses for the Jellyfin web UI on this
// host: http://<hostname>.local:<port> (mDNS) plus http://<lan-ip>:<port> for
// each real non-loopback IPv4. A headless server is reached by one of these
// from a browser on the same network (or over Tailscale). Virtual/bridge
// interfaces (docker, veth, bridges) are skipped — their IPs aren't reachable.
func WebURLs() []string {
	var urls []string
	if h, err := os.Hostname(); err == nil && h != "" {
		host := h
		if !strings.Contains(host, ".") {
			host += ".local"
		}
		urls = append(urls, fmt.Sprintf("http://%s:%d", host, WebPort))
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return urls
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		n := ifc.Name
		if strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") ||
			strings.HasPrefix(n, "veth") || strings.HasPrefix(n, "virbr") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue // skip IPv6 and loopback for the simple display
			}
			urls = append(urls, fmt.Sprintf("http://%s:%d", ip4.String(), WebPort))
		}
	}
	return urls
}

// PackageSet is the union of all Jellyfin-related Arch packages we know about.
type PackageSet struct {
	JellyfinBin    bool // jellyfin-bin (AUR — official Microsoft-compiled binary)
	Jellyfin       bool // jellyfin (meta-package in official repos)
	JellyfinServer bool // jellyfin-server (extra)
	JellyfinWeb    bool // jellyfin-web (extra)
	JellyfinFFmpeg bool // jellyfin-ffmpeg
}

// Any reports whether ANY Jellyfin package is installed.
func (p PackageSet) Any() bool {
	return p.JellyfinBin || p.Jellyfin || p.JellyfinServer || p.JellyfinWeb || p.JellyfinFFmpeg
}

// Installed returns the list of installed package names (in display order).
func (p PackageSet) Installed() []string {
	var out []string
	if p.JellyfinBin {
		out = append(out, "jellyfin-bin")
	}
	if p.Jellyfin {
		out = append(out, "jellyfin")
	}
	if p.JellyfinServer {
		out = append(out, "jellyfin-server")
	}
	if p.JellyfinWeb {
		out = append(out, "jellyfin-web")
	}
	if p.JellyfinFFmpeg {
		out = append(out, "jellyfin-ffmpeg")
	}
	return out
}

// Status is a snapshot of how Jellyfin sits on this host.
type Status struct {
	Packages      PackageSet
	ServiceUnit   string // "jellyfin.service" if found, "" otherwise
	ServiceActive bool   // is jellyfin.service currently running?
	UserExists    bool   // does the 'jellyfin' system user exist?
}

// LoadStatus inspects the host. Cheap to call repeatedly.
func LoadStatus() (Status, error) {
	var s Status
	s.Packages = detectPackages()

	if unit, ok := detectServiceUnit(); ok {
		s.ServiceUnit = unit
		s.ServiceActive = isActive(unit)
	}

	if _, err := exec.Command("id", "-u", "jellyfin").Output(); err == nil {
		s.UserExists = true
	}
	return s, nil
}

// IsInstalled is shorthand — anywhere we just want a boolean to drive UI.
func (s Status) IsInstalled() bool {
	return s.Packages.Any() || s.ServiceUnit != ""
}

// ---- internals ----

func detectPackages() PackageSet {
	return PackageSet{
		JellyfinBin:    pacmanInstalled("jellyfin-bin"),
		Jellyfin:       pacmanInstalled("jellyfin"),
		JellyfinServer: pacmanInstalled("jellyfin-server"),
		JellyfinWeb:    pacmanInstalled("jellyfin-web"),
		JellyfinFFmpeg: pacmanInstalled("jellyfin-ffmpeg"),
	}
}

func pacmanInstalled(name string) bool {
	cmd := exec.Command("pacman", "-Qi", name)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func detectServiceUnit() (string, bool) {
	for _, unit := range []string{"jellyfin.service", "jellyfin-server.service"} {
		out, err := exec.Command("systemctl", "list-unit-files", "--no-legend", unit).Output()
		if err != nil {
			continue
		}
		if strings.Contains(string(out), unit) {
			return unit, true
		}
	}
	return "", false
}

func isActive(unit string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", unit)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false
		}
		return false
	}
	return true
}
