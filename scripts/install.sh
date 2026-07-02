#!/bin/sh
# Install akashi, the keyless MCP server health checker.
#
# Usage:
#   curl -fsSL https://roninforge.org/akashi/install.sh | sh
#
# What it does:
#   1. Detects your OS and architecture.
#   2. Downloads the matching release binary from GitHub Releases.
#   3. Verifies the archive's SHA-256 against the published checksums file.
#   4. Installs to $PREFIX/bin (default: /usr/local), or ~/.local/bin if
#      the default is not writable without sudo.
#
# Environment:
#   AKASHI_VERSION   pin a specific version (default: latest)
#   PREFIX           install prefix (default: /usr/local or ~/.local)
#   BIN_DIR          binary destination (default: $PREFIX/bin)
#   AKASHI_NO_COLOR  set to disable colored output
#
# This script runs under dash and bash. It does not rely on `bash`-only
# features. Exit codes are non-zero on any failure.

set -eu

REPO="RoninForge/akashi"
BIN="akashi"

# --- Logging --------------------------------------------------------------

if [ -t 1 ] && [ -z "${AKASHI_NO_COLOR:-}" ]; then
  c_reset=$(printf '\033[0m')
  c_bold=$(printf '\033[1m')
  c_red=$(printf '\033[31m')
  c_green=$(printf '\033[32m')
  c_yellow=$(printf '\033[33m')
else
  c_reset=""; c_bold=""; c_red=""; c_green=""; c_yellow=""
fi

info()  { printf '%s%s%s\n' "$c_bold" "$*" "$c_reset"; }
warn()  { printf '%swarning: %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
error() { printf '%serror: %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }
ok()    { printf '%s%s%s\n' "$c_green" "$*" "$c_reset"; }

# --- Platform detection ---------------------------------------------------

detect_os() {
  uname_s=$(uname -s)
  case "$uname_s" in
    Darwin) echo "darwin" ;;
    Linux)  echo "linux" ;;
    *) error "unsupported OS: $uname_s (darwin and linux are supported; windows users should download from GitHub Releases)" ;;
  esac
}

detect_arch() {
  uname_m=$(uname -m)
  case "$uname_m" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) error "unsupported architecture: $uname_m" ;;
  esac
}

# --- Dependency checks ----------------------------------------------------

need() {
  command -v "$1" >/dev/null 2>&1 || error "$1 is required but not installed"
}

need uname
need tar
need mkdir
need rm
need mv

if command -v curl >/dev/null 2>&1; then
  DL="curl --fail --silent --show-error --location"
elif command -v wget >/dev/null 2>&1; then
  DL="wget -q -O -"
else
  error "neither curl nor wget is installed"
fi

if command -v sha256sum >/dev/null 2>&1; then
  SHA="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA="shasum -a 256"
else
  warn "no sha256sum or shasum available; skipping checksum verification"
  SHA=""
fi

# --- Version resolution ---------------------------------------------------

resolve_version() {
  if [ -n "${AKASHI_VERSION:-}" ]; then
    # Allow either "v0.1.0" or "0.1.0".
    case "$AKASHI_VERSION" in
      v*) echo "$AKASHI_VERSION" ;;
      *)  echo "v$AKASHI_VERSION" ;;
    esac
    return
  fi
  # Default: GitHub redirects /releases/latest to the actual tag URL.
  # We follow the redirect and extract the tag from the Location header.
  if command -v curl >/dev/null 2>&1; then
    url=$(curl -sIL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
  else
    url=$(wget --max-redirect=0 -S -O /dev/null "https://github.com/$REPO/releases/latest" 2>&1 | grep -i 'Location:' | tail -n1 | awk '{print $2}')
  fi
  tag=${url##*/tag/}
  case "$tag" in
    v*) echo "$tag" ;;
    *)  error "could not resolve latest version from $url" ;;
  esac
}

# --- Install destination --------------------------------------------------

choose_bin_dir() {
  if [ -n "${BIN_DIR:-}" ]; then
    echo "$BIN_DIR"
    return
  fi
  if [ -n "${PREFIX:-}" ]; then
    echo "$PREFIX/bin"
    return
  fi
  if [ -w /usr/local/bin ] 2>/dev/null; then
    echo /usr/local/bin
  else
    mkdir -p "$HOME/.local/bin"
    echo "$HOME/.local/bin"
  fi
}

# --- Main -----------------------------------------------------------------

OS=$(detect_os)
ARCH=$(detect_arch)
VERSION=$(resolve_version)
BIN_DIR_PATH=$(choose_bin_dir)

STRIPPED_VERSION=${VERSION#v}
ARCHIVE="${BIN}_${STRIPPED_VERSION}_${OS}_${ARCH}.tar.gz"
ARCHIVE_URL="https://github.com/$REPO/releases/download/$VERSION/$ARCHIVE"
CHECKSUMS_URL="https://github.com/$REPO/releases/download/$VERSION/checksums.txt"

info "akashi $VERSION for $OS/$ARCH -> $BIN_DIR_PATH/$BIN"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

cd "$TMPDIR"

info "downloading $ARCHIVE_URL"
$DL "$ARCHIVE_URL" > "$ARCHIVE" || error "download failed: $ARCHIVE_URL"

if [ -n "$SHA" ]; then
  info "verifying checksum"
  $DL "$CHECKSUMS_URL" > "checksums.txt" || error "could not download checksums.txt"
  # Exact-field match on the filename column so the archive's `.sbom.json`
  # sibling cannot produce a second, substring-matched hash.
  expected=$(awk -v f="$ARCHIVE" '$2 == f {print $1}' checksums.txt)
  [ -n "$expected" ] || error "no checksum entry for $ARCHIVE"
  case "$expected" in
  *"
"*)
    error "multiple checksum matches for $ARCHIVE - checksums.txt is malformed"
    ;;
  esac
  actual=$($SHA "$ARCHIVE" | awk '{print $1}')
  if [ "$expected" != "$actual" ]; then
    error "checksum mismatch: expected $expected, got $actual"
  fi
fi

info "extracting"
tar -xzf "$ARCHIVE"

[ -f "$BIN" ] || error "extracted archive did not contain $BIN"

chmod +x "$BIN"

if [ ! -d "$BIN_DIR_PATH" ]; then
  mkdir -p "$BIN_DIR_PATH"
fi

if [ -w "$BIN_DIR_PATH" ]; then
  mv "$BIN" "$BIN_DIR_PATH/$BIN"
else
  warn "$BIN_DIR_PATH not writable, trying sudo"
  sudo mv "$BIN" "$BIN_DIR_PATH/$BIN"
fi

ok "installed $BIN_DIR_PATH/$BIN"

case ":$PATH:" in
  *:"$BIN_DIR_PATH":*) ;;
  *) warn "$BIN_DIR_PATH is not on your \$PATH. Add it to your shell profile:
    export PATH=\"$BIN_DIR_PATH:\$PATH\"" ;;
esac

info "next step: run '$BIN version' to confirm"
