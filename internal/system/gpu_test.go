package system

import (
	"os"
	"path/filepath"
	"testing"
)

// fakePCIDev writes a minimal sysfs PCI device dir with class + vendor.
func fakePCIDev(t *testing.T, root, addr, class, vendor string) {
	t.Helper()
	dir := filepath.Join(root, addr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, val := range map[string]string{"class": class, "vendor": vendor} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectGPUs(t *testing.T) {
	cases := []struct {
		name string
		devs [][3]string // addr, class, vendor
		want []GPUVendor
	}{
		{
			name: "intel igpu only (zee)",
			devs: [][3]string{
				{"0000:00:02.0", "0x030000", "0x8086"},
				{"0000:00:1f.3", "0x040300", "0x8086"}, // audio, same vendor — ignored
			},
			want: []GPUVendor{VendorIntel},
		},
		{
			name: "amd discrete",
			devs: [][3]string{{"0000:03:00.0", "0x030000", "0x1002"}},
			want: []GPUVendor{VendorAMD},
		},
		{
			name: "nvidia 3d controller class",
			devs: [][3]string{{"0000:01:00.0", "0x030200", "0x10de"}},
			want: []GPUVendor{VendorNVIDIA},
		},
		{
			name: "hybrid intel+nvidia, stable order",
			devs: [][3]string{
				{"0000:01:00.0", "0x030000", "0x10de"},
				{"0000:00:02.0", "0x030000", "0x8086"},
			},
			want: []GPUVendor{VendorIntel, VendorNVIDIA},
		},
		{
			name: "no display devices",
			devs: [][3]string{{"0000:00:14.0", "0x0c0330", "0x8086"}},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range tc.devs {
				fakePCIDev(t, root, d[0], d[1], d[2])
			}
			got := detectGPUs(root)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestDetectGPUsMissingRoot(t *testing.T) {
	if got := detectGPUs("/nonexistent/sysfs"); got != nil {
		t.Fatalf("expected nil for missing root, got %v", got)
	}
}

// TestDetectGPUsRealHost never fails — it just logs what the real host
// reports so `go test -v` doubles as a quick manual check.
func TestDetectGPUsRealHost(t *testing.T) {
	t.Logf("real host GPUs: %v (nvidia driver loaded: %v)", DetectGPUs(), NvidiaDriverLoaded())
}
