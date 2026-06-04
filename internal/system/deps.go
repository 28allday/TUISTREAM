// Package system handles host-level concerns that don't fit drives /
// jellyfin / firewall — most notably the one-time install of every Arch
// package tuistream's flows actually invoke.
//
// The TUI's individual flows used to install their own missing tools on
// the fly, which interrupted the user mid-action and broke the "one
// confirm screen, then runs to completion" promise. Doing it once at
// launch keeps the flows themselves dependency-free.
package system

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Dep is one Arch package + the binary tuistream invokes from it. The
// binary check lets us decide whether the package is missing without
// shelling out to `pacman -Qi`, which is much slower.
type Dep struct {
	Pkg    string
	Binary string
}

// Required is the closed set of userspace tools tuistream needs. Anything
// in Arch's `base` group (util-linux, e2fsprogs, coreutils) is assumed
// present and intentionally NOT listed.
var Required = []Dep{
	{Pkg: "rsync", Binary: "rsync"},            // Manage > Copy
	{Pkg: "acl", Binary: "setfacl"},            // Setup > add-drive ACL grant, Copy ACL re-grant
	{Pkg: "btrfs-progs", Binary: "mkfs.btrfs"}, // Setup > format btrfs / pool
	{Pkg: "xfsprogs", Binary: "mkfs.xfs"},      // Setup > format xfs
	{Pkg: "gptfdisk", Binary: "sgdisk"},        // Setup > wipe-whole-disk
	{Pkg: "parted", Binary: "partprobe"},       // Setup > partprobe after wipe
	{Pkg: "smartmontools", Binary: "smartctl"}, // Monitor > SMART health per disk
	{Pkg: "hdparm", Binary: "hdparm"},          // Setup > drive spin-down timer
}

// Missing returns the subset of Required whose binary isn't on $PATH.
func Missing() []Dep {
	var out []Dep
	for _, d := range Required {
		if _, err := exec.LookPath(d.Binary); err != nil {
			out = append(out, d)
		}
	}
	return out
}

// EnsureInstalled checks for missing deps and runs `pacman -S --needed
// --noconfirm` for them. Output is streamed to stderr so the user can see
// what's happening before the TUI takes over the terminal.
//
// Returns nil if everything's already present or the install succeeded.
// On pacman failure, returns the error but doesn't exit the process —
// the caller decides whether to abort or warn-and-continue.
func EnsureInstalled() error {
	miss := Missing()
	if len(miss) == 0 {
		return nil
	}
	pkgs := make([]string, len(miss))
	for i, d := range miss {
		pkgs[i] = d.Pkg
	}
	fmt.Fprintf(os.Stderr,
		"tuistream: installing missing tools: %s\n", strings.Join(pkgs, " "))
	args := append([]string{"-S", "--needed", "--noconfirm"}, pkgs...)
	cmd := exec.Command("pacman", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
