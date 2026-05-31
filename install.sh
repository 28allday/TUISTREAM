#!/usr/bin/env bash
#
# install.sh — install TUISTREAM, a TUI for a headless Jellyfin server on
# Omarchy / Arch.
#
# Quick install (nothing to clone — the Once way):
#
#   curl -fsSL https://raw.githubusercontent.com/28allday/TUISTREAM/main/install.sh | bash
#
# When run from a git clone it builds from source instead (if Go is present),
# otherwise it downloads the latest released binary for your architecture.
#
#   ./install.sh
#
# Environment overrides:
#   PREFIX=/somewhere          install prefix (default /usr/local → /usr/local/bin)
#   TUISTREAM_VERSION=v0.1.0    pin a release (default: latest)
#
# Why /usr/local/bin (not ~/.local/bin): TUISTREAM is a root-by-design tool —
# it installs Jellyfin, edits /etc/fstab and mounts drives, so it's run as
# `sudo tuistream`. /usr/local/bin is on the default Arch sudo secure_path;
# ~/.local/bin is NOT, so a user-local copy wouldn't be found under sudo.

set -euo pipefail

REPO="28allday/TUISTREAM"
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
NAME="tuistream"
BIN="$BIN_DIR/$NAME"
VERSION="${TUISTREAM_VERSION:-latest}"

log()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# If the script lives next to the source tree, we're in a clone.
SCRIPT_DIR=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi

# Writing to a system prefix needs root; fall back to sudo when we're not it.
SUDO=""
if [ ! -w "$BIN_DIR" ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
  else
    die "cannot write $BIN_DIR and sudo not available — re-run as root."
  fi
fi
$SUDO mkdir -p "$BIN_DIR"

# ---- obtain the binary ---------------------------------------------------
TMPBIN=""
if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/go.mod" ] && command -v go >/dev/null 2>&1; then
  log "Building $NAME from source..."
  TMPBIN="$(mktemp)"
  trap 'rm -f "$TMPBIN"' EXIT
  ( cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
      -o "$TMPBIN" "./cmd/$NAME" )
elif [ -n "$SCRIPT_DIR" ] && [ -x "$SCRIPT_DIR/dist/${NAME}-linux-amd64" ]; then
  log "Using prebuilt binary from dist/"
  TMPBIN="$SCRIPT_DIR/dist/${NAME}-linux-amd64"
else
  # Download the released binary for this OS/arch (curl-style install).
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  [ "$os" = "linux" ] || die "TUISTREAM ships Linux binaries only (detected: $os). Clone the repo and build with Go."
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  asset="${NAME}-${os}-${arch}"
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/$asset"
  else
    url="https://github.com/$REPO/releases/download/$VERSION/$asset"
  fi
  log "Downloading $asset ($VERSION)..."
  TMPBIN="$(mktemp)"
  trap 'rm -f "$TMPBIN"' EXIT
  if command -v curl >/dev/null 2>&1; then
    curl -fSL --proto '=https' --tlsv1.2 -o "$TMPBIN" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$TMPBIN" "$url"
  else
    die "need curl or wget to download the binary."
  fi
fi

# ---- install -------------------------------------------------------------
log "Installing to $BIN"
$SUDO install -Dm755 "$TMPBIN" "$BIN"

# Remove any stale ~/.local/bin copy that would shadow the system one under a
# non-sudo PATH (older installs landed there).
OLD="$HOME/.local/bin/$NAME"
if [ -e "$OLD" ]; then
  log "Removing stale user-local copy: $OLD"
  rm -f "$OLD"
fi

echo
log "Done. Run it with:  sudo $NAME"
