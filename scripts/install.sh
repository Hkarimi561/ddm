#!/usr/bin/env bash
# ddm installer for Linux and macOS.
#
#   curl -fsSL https://<owner>.github.io/<repo>/install.sh | bash
#
# Environment variables:
#   DDM_VERSION      release tag to install, e.g. v0.1.0 (default: latest)
#   DDM_INSTALL_DIR  where to put the binary (default: /usr/local/bin as root,
#                    ~/.local/bin otherwise)
#   DDM_REPO         GitHub repository to download from (owner/name)
#   DDM_BASE_URL     download archives from here instead of GitHub Releases
#                    (any URL curl understands, including file://)
#   DDM_OS, DDM_ARCH override platform detection
#   DDM_NO_SERVICE   set to 1 to skip starting the web UI service
#
# Run it again at any time to update; `ddm update` does the same from ddm itself.
#
# Everything lives inside main(), so a download cut off half-way through
# never runs a partial script.

set -euo pipefail

main() {
  local repo="${DDM_REPO:-__DDM_REPO__}"
  case "$repo" in __DDM_*) repo="Hkarimi561/ddm" ;; esac
  local version="${DDM_VERSION:-latest}"

  need tar
  local os arch
  os="${DDM_OS:-$(detect_os "$repo")}"
  arch="${DDM_ARCH:-$(detect_arch "$os")}"

  local base="${DDM_BASE_URL:-}"
  if [ -z "$base" ]; then
    if [ "$version" = "latest" ]; then
      base="https://github.com/$repo/releases/latest/download"
    else
      case "$version" in v*) ;; *) version="v$version" ;; esac
      base="https://github.com/$repo/releases/download/$version"
    fi
  fi
  base="${base%/}"

  local asset="ddm_${os}_${arch}.tar.gz"
  local tmp
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064 # expand $tmp now, while it is in scope
  trap "rm -rf '$tmp'" EXIT

  say "Downloading $asset ($version) from $base"
  fetch "$base/$asset" "$tmp/$asset"
  fetch "$base/checksums.txt" "$tmp/checksums.txt"

  local want got
  want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
  [ -n "$want" ] || die "checksums.txt has no entry for $asset"
  got="$(sha256 "$tmp/$asset")"
  [ "$want" = "$got" ] || die "checksum mismatch for $asset (expected $want, got $got)"

  tar -xzf "$tmp/$asset" -C "$tmp" ddm

  local dir="${DDM_INSTALL_DIR:-}"
  if [ -z "$dir" ]; then
    if [ "$(id -u)" = "0" ]; then dir="/usr/local/bin"; else dir="$HOME/.local/bin"; fi
  fi
  mkdir -p "$dir"
  local previous=""
  [ -x "$dir/ddm" ] && previous="$("$dir/ddm" --version 2>/dev/null || true)"
  # Write next to the target and rename, so an already running ddm is not
  # disturbed and a failed copy never leaves a broken binary behind.
  cp "$tmp/ddm" "$dir/.ddm.new"
  chmod 755 "$dir/.ddm.new"
  mv -f "$dir/.ddm.new" "$dir/ddm"

  local installed
  installed="$("$dir/ddm" --version 2>/dev/null || echo ddm)"
  if [ -n "$previous" ] && [ "$previous" != "$installed" ]; then
    say "Updated $previous -> $installed ($dir/ddm)"
  else
    say "Installed $installed to $dir/ddm"
  fi

  case ":$PATH:" in
    *":$dir:"*) ;;
    *)
      say ""
      say "$dir is not on your PATH. Add it with:"
      case "${SHELL:-}" in
        */zsh) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc && source ~/.zshrc" ;;
        */fish) say "  fish_add_path $dir" ;;
        *) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.bashrc && source ~/.bashrc" ;;
      esac
      ;;
  esac

  if ! command -v docker >/dev/null 2>&1; then
    say ""
    say "Note: ddm needs Docker to load images: https://docs.docker.com/get-docker/"
  fi
  if [ "${DDM_NO_SERVICE:-}" != "1" ]; then
    say ""
    # Starts the settings web UI now and at every login, and prints its
    # address and password.
    "$dir/ddm" service install || say "The web UI could not be set up; start it with: ddm serve"
  fi
  say ""
  say "Get started:  ddm pull nginx:latest"
}

detect_os() {
  case "$(uname -s)" in
    Linux) echo linux ;;
    Darwin) echo darwin ;;
    MINGW* | MSYS* | CYGWIN*)
      die "on Windows, run: powershell -c \"irm https://${1%%/*}.github.io/${1#*/}/install.ps1 | iex\"" ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
}

detect_arch() {
  local m
  m="$(uname -m)"
  # A shell running under Rosetta reports x86_64 on Apple silicon; prefer
  # the native binary.
  if [ "$1" = "darwin" ] && [ "$m" = "x86_64" ] &&
    [ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = "1" ]; then
    m="arm64"
  fi
  case "$m" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) die "unsupported CPU architecture: $m (ddm ships amd64 and arm64 builds)" ;;
  esac
}

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 5 --retry-delay 2 -o "$2" "$1" || die "download failed: $1"
  elif command -v wget >/dev/null 2>&1; then
    case "$1" in
      file://*) cp "${1#file://}" "$2" || die "copy failed: $1" ;;
      *) wget -q --tries=5 -O "$2" "$1" || die "download failed: $1" ;;
    esac
  else
    die "curl or wget is required"
  fi
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    die "sha256sum or shasum is required to verify the download"
  fi
}

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
say() { printf '%s\n' "$*"; }
die() {
  printf 'ddm install: %s\n' "$*" >&2
  exit 1
}

main "$@"
