# TUISTREAM (`tuistream`)

A **terminal UI** for setting up and running a **headless [Jellyfin](https://jellyfin.org)
media server** on Arch / [Omarchy](https://omarchy.org) — over SSH, with no
desktop environment. It installs Jellyfin, attaches and mounts media drives
(single disks or btrfs RAID pools), opens the firewall, and shows a live health
monitor, all without dropping you to a shell or switching terminals mid-task.

> TUISTREAM is an independent setup/management tool for Jellyfin. It is not
> affiliated with the Jellyfin project; "Jellyfin" is used here descriptively.

## Features

- **Setup** — install / uninstall Jellyfin, open or close the firewall ports,
  copy the server's web address to your clipboard (works over SSH + tmux), and
  move Jellyfin's library storage onto a media drive.
- **Hardware transcoding ready out of the box** — the installer detects the
  machine's GPU (Intel / AMD / NVIDIA) and installs the matching encoding
  packages (Intel QSV/VA-API, AMD VA-API) so clients that need a transcode
  don't hit "fatal playback error". NVIDIA NVENC needs only the driver you
  already have; if no NVIDIA driver is loaded the installer says so rather
  than guessing which kernel driver to install.
- **Drive spin-down by default** — spinning media drives are automatically put
  to sleep after 3 idle minutes (cooler, quieter) by a tiny background watcher
  that survives reboots — and works even on NAS drives that ignore their own
  firmware idle timer (looking at you, WD Red). System drives are never
  touched; `s` opts out if you want 24/7 spinning.
- **Add media drive** — attach a spare disk or partition: keep its existing
  filesystem or format it (btrfs / ext4 / xfs), or combine 2+ disks into a
  **btrfs RAID pool** (1 / 0 / 5 / 10). The boot drive is never offered — the
  classifier follows every candidate down through LUKS / LVM / RAID to its
  physical disk and refuses anything hosting `/`, `/boot`, or swap.
- **Import an existing btrfs pool** — re-attach a multi-device pool from a
  previous setup **non-destructively** (mounts it as-is; never formats).
- **Safe with existing media** — keeping a filesystem previews what's on it,
  skips creating starter folders when it already has content, and grants Jellyfin
  read access recursively so existing media is actually visible to the scanner.
- **Manage** — copy media in from an external drive (with a file picker),
  mount / eject drives, and rename or delete files, each behind a confirmation.
- **Monitor** — per-core CPU, load / memory / uptime, per-mount capacity, btrfs
  pool error counters, and per-disk SMART health — for a box you only reach over
  SSH.
- **Single static binary**; missing tools (btrfs-progs, xfsprogs, acl, …) are
  installed on first launch.

## Install

One line, nothing to clone:

```sh
curl -fsSL https://raw.githubusercontent.com/28allday/TUISTREAM/main/install.sh | bash
```

This installs the right binary for your architecture into `/usr/local/bin`
(using `sudo` if needed) so both `tuistream` and `sudo tuistream` resolve. Pin a
version with `TUISTREAM_VERSION=v0.1.0`, or change the prefix with `PREFIX=/opt`.

TUISTREAM installs to a **system** path on purpose: it's a root-by-design tool
(installs Jellyfin, edits `/etc/fstab`, mounts drives), so it must be on the sudo
`secure_path` — `~/.local/bin` is not.

## Usage

```sh
sudo tuistream                 # full TUI
tuistream --read-only          # inventory views only, no root, no actions
```

Run with `sudo`, it acts on behalf of the real user behind the sudo invocation
(`$SUDO_USER`): media drives mount under `/media/<user>/` and its Omarchy theme
is used. `tab` / `shift+tab` switch tabs, `r` refreshes, `q` quits; each tab
shows its key bar at the bottom.

## Getting started

A first-run, from an empty box to a working server — all from the **Setup** tab
(`sudo tuistream`):

1. **Install Jellyfin** — press `i` and confirm. TUISTREAM pulls Jellyfin from
   the official Arch repo and starts the service.
2. **Attach storage for your media**:
   - A spare disk → press `a` (**add drive**), pick it, choose *Keep existing
     filesystem* or format it, and name it. It mounts at
     `/media/<user>/<name>`.
   - A multi-disk btrfs pool from a previous setup → press `p` (**import pool**)
     to re-attach it untouched. (`p` only shows when such a pool is present.)
   - On a fresh drive, TUISTREAM seeds library folders for you under
     `<mount>/JellyfinMedia/`: `Movies`, `Shows`, `Music`, `Books`,
     `Home Videos`, `Music Videos`. Copy your media into these (the **Manage**
     tab's `c` can copy in from an external drive).
3. **Open the firewall** — press `f` so other devices on your LAN can reach the
   server (ports `8096/tcp` web UI and `7359/udp` discovery).
4. **Open the web UI** — press `y` to copy the server's address
   (`http://<host>:8096`) to your clipboard, even over SSH, then open it in a
   browser on any LAN device.
5. **Create your libraries in Jellyfin** — in the web UI's first-run wizard, add
   a library for each type and point it at the matching folder under
   `/media/<user>/<name>/JellyfinMedia/` (e.g. *Movies* → `…/JellyfinMedia/Movies`).
   The folder names already match Jellyfin's content types.

Optional: press `j` to move Jellyfin's own library database and metadata off the
OS drive onto a media drive (handy on a small boot SSD).

Note on **drive spin-down**: when TUISTREAM sees spinning media drives it
automatically installs a small watcher service that spins them down after 3
idle minutes, so they don't run hot 24/7. (It watches actual disk I/O rather
than trusting the drive's own idle timer, which many NAS drives silently
ignore.) The first play after a sleep takes a few seconds while the drives
wake. Press `s` to opt out — TUISTREAM remembers and won't re-apply the
default.

## Build from source

Needs Go 1.26+.

```sh
git clone https://github.com/28allday/TUISTREAM
cd TUISTREAM
./install.sh    # builds, then installs to /usr/local/bin
```

## License

MIT — see [LICENSE](LICENSE). TUISTREAM runs Jellyfin (GPL-2.0) as a separate
process; it does not bundle or link it. The terminal UI is built on the
[Charm](https://github.com/charmbracelet) libraries (MIT).
