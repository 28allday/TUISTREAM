package system

import (
	"os"
	"path/filepath"
	"strings"
)

// GPU vendor detection for hardware-accelerated transcoding. We walk
// /sys/bus/pci/devices directly instead of shelling out to lspci so the
// pre-flight needs no extra package and works before any GPU driver is
// loaded (an unbound card still has its PCI class/vendor files).

type GPUVendor string

const (
	VendorIntel  GPUVendor = "intel"
	VendorAMD    GPUVendor = "amd"
	VendorNVIDIA GPUVendor = "nvidia"
)

// DetectGPUs returns the distinct GPU vendors present on the host, in a
// stable intel/amd/nvidia order. Vendors jellyfin-ffmpeg has no encoder
// for are ignored.
func DetectGPUs() []GPUVendor {
	return detectGPUs("/sys/bus/pci/devices")
}

func detectGPUs(sysfsRoot string) []GPUVendor {
	entries, err := os.ReadDir(sysfsRoot)
	if err != nil {
		return nil
	}
	seen := map[GPUVendor]bool{}
	for _, e := range entries {
		dir := filepath.Join(sysfsRoot, e.Name())
		// PCI class 0x03xxxx = display controller (VGA, 3D, other).
		class := readSysfsHex(filepath.Join(dir, "class"))
		if !strings.HasPrefix(class, "0x03") {
			continue
		}
		switch readSysfsHex(filepath.Join(dir, "vendor")) {
		case "0x8086":
			seen[VendorIntel] = true
		case "0x1002":
			seen[VendorAMD] = true
		case "0x10de":
			seen[VendorNVIDIA] = true
		}
	}
	var out []GPUVendor
	for _, v := range []GPUVendor{VendorIntel, VendorAMD, VendorNVIDIA} {
		if seen[v] {
			out = append(out, v)
		}
	}
	return out
}

// NvidiaDriverLoaded reports whether the proprietary/open NVIDIA kernel
// driver is active. When it is, its userspace (nvidia-utils) is already
// installed as a dependency, which is all NVENC needs — no extra packages.
func NvidiaDriverLoaded() bool {
	_, err := os.Stat("/proc/driver/nvidia")
	return err == nil
}

func readSysfsHex(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}
