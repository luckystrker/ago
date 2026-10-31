#!/bin/sh
# install.sh — install the AGo CLI binary (ago) from GitHub Releases.
#
# POSIX sh, no bashisms. Safe to run through curl | sh: nothing in this
# script reads stdin.
#
#   curl -fsSL https://raw.githubusercontent.com/luckystrker/ago/main/install.sh | sh
#
# Release assets are published by .github/workflows/release.yml:
#   ago_<tag>_<os>_<arch>.tar.gz   (unix)
#   ago_<tag>_<os>_<arch>.zip      (windows)
#   checksums.txt                  (sha256 of every archive)
#
# The latest release tag is resolved by following the redirect of
# .../releases/latest — no GitHub API call, no token needed.
# AGO-DEVIATION: the brief sketched /releases/latest/download/<asset>, but
# that shortcut only works for assets whose name does not carry the version;
# ours do (see docs/DECISIONS.md D-054), so the tag is resolved from the
# redirect instead — still without the API.
set -eu

# AGO_INSTALL_REPO_URL points the installer at a different release base URL;
# normal use always hits GitHub, the override exists for testing the script
# against a local server that mimics the releases layout.
REPO_URL="${AGO_INSTALL_REPO_URL:-https://github.com/luckystrker/ago}"

DRY_RUN="${DRY_RUN:-}"
# AGO_SKIP_CHECKSUM=1 is the only way to install when checksums.txt cannot
# be fetched: the archive is then unpacked with no integrity check at all,
# so the opt-in is explicit and loudly warned. A present-but-mismatching
# digest is always fatal and cannot be skipped.
AGO_SKIP_CHECKSUM="${AGO_SKIP_CHECKSUM:-}"
REQUESTED_VERSION=""
REQUESTED_DIR="${AGO_INSTALL_DIR:-}"
VERSION=""
OS=""
ARCH=""
BIN_NAME="ago"
ARCHIVE_EXT="tar.gz"
INSTALL_DIR=""
ARCHIVE=""
TMP=""

usage() {
  cat <<'EOF'
install.sh — install the AGo CLI binary (ago) from GitHub Releases.

Usage:
  sh install.sh [options]
  curl -fsSL https://raw.githubusercontent.com/luckystrker/ago/main/install.sh | sh

Options:
  -h, --help        Show this help and exit.
  --version=TAG     Install a specific release tag, e.g. --version=v0.5.0
                    (default: the latest published release).
  --dir=DIR         Install directory (default: $AGO_INSTALL_DIR if set,
                    otherwise $HOME/.local/bin).

Environment:
  AGO_INSTALL_DIR   Same as --dir=DIR.
  DRY_RUN=1         Print the steps without downloading or installing.
  AGO_SKIP_CHECKSUM=1
                    Install even when checksums.txt cannot be downloaded.
                    NO sha256 verification happens — an explicit, unsafe
                    opt-in; a mismatching checksum still aborts the install.

What it does:
  1. Detects OS and architecture (linux/darwin/windows x amd64/arm64).
  2. Picks the release: --version=<tag>, or the latest tag resolved via the
     redirect of https://github.com/luckystrker/ago/releases/latest
     (no GitHub API involved).
  3. Downloads ago_<tag>_<os>_<arch>.tar.gz (.zip on windows) and
     checksums.txt from the release and verifies the archive sha256.
  4. Unpacks ago (ago.exe on windows) into the install directory and prints
     a PATH hint when needed.

Fallback: when neither curl nor wget is available, or the download fails,
the script clones the repository and builds locally:
  git clone --depth 1 https://github.com/luckystrker/ago.git
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=<tag>" ./cmd/ago
(git and a Go >= 1.24 toolchain must be in PATH.)
EOF
}

info() { printf 'ago-install: %s\n' "$*"; }
warn() { printf 'ago-install: warning: %s\n' "$*" >&2; }
die()  { printf 'ago-install: error: %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }
dry()  { [ "${DRY_RUN}" = "1" ]; }

cleanup() {
  if [ -n "${TMP}" ]; then
    rm -rf "${TMP}"
  fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

sha256_of() {
  # $1 = file; print the digest, or return 1 when no tool is available.
  if have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif have openssl; then
    openssl dgst -sha256 -r "$1" | awk '{print $1}'
  else
    return 1
  fi
}

download_to() {
  # $1 = url, $2 = destination file. curl is preferred, wget is the fallback.
  if have curl; then
    curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1"
  else
    wget -q -t 3 -O "$2" "$1"
  fi
}

resolve_latest_tag() {
  # GitHub redirects .../releases/latest to .../releases/tag/<tag>; reading
  # the final URL needs no API and no token. Prints the tag or returns 1.
  _rlt_url=""
  if have curl; then
    _rlt_url=$(curl -fsLI -o /dev/null -w '%{url_effective}' "${REPO_URL}/releases/latest" 2>/dev/null) || return 1
  elif have wget; then
    _rlt_url=$(wget -S --spider --max-redirect=10 "${REPO_URL}/releases/latest" 2>&1 | sed -n 's/^[[:space:]]*[Ll]ocation: *//p' | tail -n 1) || return 1
  else
    return 1
  fi
  case "${_rlt_url}" in
    */releases/tag/*) printf '%s\n' "${_rlt_url##*/}" ;;
    *) return 1 ;;
  esac
}

verify_checksum() {
  # $1 = downloaded archive, $2 = checksums.txt. The filename field may carry
  # a sha256sum binary-mode marker ("hash *name" on some builds) — strip it.
  _vc_archive=${1##*/}
  _vc_expected=$(awk -v f="${_vc_archive}" '{ n = $NF; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' "$2")
  if [ -z "${_vc_expected}" ]; then
    die "checksums.txt does not list ${_vc_archive}"
  fi
  _vc_actual=$(sha256_of "$1") || die "cannot compute the sha256: none of sha256sum, shasum, openssl is available"
  if [ "${_vc_actual}" != "${_vc_expected}" ]; then
    die "sha256 mismatch for ${_vc_archive}: expected ${_vc_expected}, got ${_vc_actual}"
  fi
  info "checksum OK: ${_vc_archive}"
}

install_binary() {
  # $1 = path to the extracted binary; moves it into the install directory.
  mkdir -p "${INSTALL_DIR}"
  mv -f "$1" "${INSTALL_DIR}/${BIN_NAME}"
  chmod 0755 "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || \
    warn "could not set the executable bit on ${INSTALL_DIR}/${BIN_NAME}"
}

build_from_source() {
  have git || die "the source fallback needs git in PATH"
  have go || die "the source fallback needs a Go toolchain (>= 1.24) in PATH"
  _bfs_ref=""
  case "${VERSION}" in
    "" | latest) ;;
    *) _bfs_ref="${VERSION}" ;;
  esac
  _bfs_ldflags="-s -w -X main.Version=${VERSION:-dev}"
  if dry; then
    if [ -n "${_bfs_ref}" ]; then
      info "DRY_RUN: git clone --depth 1 --branch ${_bfs_ref} ${REPO_URL}.git"
    else
      info "DRY_RUN: git clone --depth 1 ${REPO_URL}.git"
    fi
    info "DRY_RUN: cd <src> && CGO_ENABLED=0 go build -trimpath -ldflags \"${_bfs_ldflags}\" -o ${INSTALL_DIR}/${BIN_NAME} ./cmd/ago"
    return 0
  fi
  info "building from source (git clone + go build)..."
  _bfs_src="${TMP}/src"
  if [ -n "${_bfs_ref}" ]; then
    git clone --depth 1 --branch "${_bfs_ref}" "${REPO_URL}.git" "${_bfs_src}"
  else
    git clone --depth 1 "${REPO_URL}.git" "${_bfs_src}"
  fi
  mkdir -p "${INSTALL_DIR}"
  (
    cd "${_bfs_src}" &&
      CGO_ENABLED=0 go build -trimpath -ldflags "${_bfs_ldflags}" \
        -o "${INSTALL_DIR}/${BIN_NAME}" ./cmd/ago
  )
}

print_path_hint() {
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) return 0 ;;
  esac
  warn "${INSTALL_DIR} is not on PATH"
  info "add it to your PATH, e.g. in ~/.profile or ~/.zshrc:"
  info "  export PATH=\"${INSTALL_DIR}:\$PATH\""
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      -h|--help)
        usage
        exit 0
        ;;
      --version=*)
        REQUESTED_VERSION="${1#--version=}"
        [ -n "${REQUESTED_VERSION}" ] || die "--version needs a non-empty tag, e.g. --version=v0.5.0"
        ;;
      --version)
        [ $# -ge 2 ] || die "--version needs a value, e.g. --version=v0.5.0"
        shift
        REQUESTED_VERSION="$1"
        ;;
      --dir=*)
        REQUESTED_DIR="${1#--dir=}"
        [ -n "${REQUESTED_DIR}" ] || die "--dir needs a non-empty directory"
        ;;
      --dir)
        [ $# -ge 2 ] || die "--dir needs a value, e.g. --dir=/usr/local/bin"
        shift
        REQUESTED_DIR="$1"
        ;;
      *)
        die "unknown argument: $1 (see --help)"
        ;;
    esac
    shift
  done

  _us=$(uname -s 2>/dev/null) || die "cannot detect the operating system (uname failed)"
  case "${_us}" in
    Linux) OS="linux" ;;
    Darwin) OS="darwin" ;;
    MINGW*|MSYS*|CYGWIN*|Windows_NT) OS="windows" ;;
    *) die "unsupported operating system: ${_us} (supported: linux, darwin, windows)" ;;
  esac
  _um=$(uname -m 2>/dev/null) || die "cannot detect the architecture (uname failed)"
  case "${_um}" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "unsupported architecture: ${_um} (supported: amd64, arm64)" ;;
  esac
  if [ "${OS}" = "windows" ]; then
    BIN_NAME="ago.exe"
    ARCHIVE_EXT="zip"
  fi

  if [ -n "${REQUESTED_DIR}" ]; then
    INSTALL_DIR="${REQUESTED_DIR}"
  else
    [ -n "${HOME:-}" ] || die "HOME is not set and --dir was not given; pass --dir=DIR"
    INSTALL_DIR="${HOME}/.local/bin"
  fi

  if [ -n "${REQUESTED_VERSION}" ]; then
    VERSION="${REQUESTED_VERSION}"
  elif dry; then
    VERSION="latest"
    info "DRY_RUN: not resolving the latest tag (dry run does no network); using the placeholder 'latest'"
  else
    if ! VERSION=$(resolve_latest_tag) || [ -z "${VERSION}" ]; then
      die "cannot resolve the latest release tag — offline, or no release is published yet.
ago-install: pass --version=<tag> (e.g. --version=v0.5.0), or build from source:
ago-install:   git clone ${REPO_URL}.git && go build -o ago ./cmd/ago"
    fi
    info "latest release: ${VERSION}"
  fi

  ARCHIVE="ago_${VERSION}_${OS}_${ARCH}.${ARCHIVE_EXT}"
  ASSET_URL="${REPO_URL}/releases/download/${VERSION}/${ARCHIVE}"
  CHECKSUM_URL="${REPO_URL}/releases/download/${VERSION}/checksums.txt"

  info "platform: ${OS}/${ARCH}"
  info "install dir: ${INSTALL_DIR}"

  if ! dry; then
    TMP=$(mktemp -d "${TMPDIR:-/tmp}/ago-install.XXXXXX")
  fi

  if ! have curl && ! have wget; then
    warn "neither curl nor wget is available; falling back to the source build"
    build_from_source
    print_path_hint
    if dry; then
      info "DRY_RUN: complete — nothing was downloaded or modified"
    else
      info "ago ${VERSION:-dev} installed at ${INSTALL_DIR}/${BIN_NAME}"
    fi
    return 0
  fi

  if dry; then
    info "DRY_RUN: download ${ASSET_URL}"
    info "DRY_RUN: download ${CHECKSUM_URL}"
    info "DRY_RUN: verify the archive sha256 against checksums.txt"
    info "DRY_RUN: extract ${BIN_NAME} from ${ARCHIVE}"
    info "DRY_RUN: install -> ${INSTALL_DIR}/${BIN_NAME}"
    print_path_hint
    info "DRY_RUN: complete — nothing was downloaded or modified"
    return 0
  fi

  info "downloading ${ASSET_URL}"
  if ! download_to "${ASSET_URL}" "${TMP}/${ARCHIVE}"; then
    warn "download failed; falling back to the source build"
    build_from_source
    print_path_hint
    info "ago ${VERSION:-dev} installed at ${INSTALL_DIR}/${BIN_NAME}"
    return 0
  fi

  if download_to "${CHECKSUM_URL}" "${TMP}/checksums.txt"; then
    verify_checksum "${TMP}/${ARCHIVE}" "${TMP}/checksums.txt"
  elif [ "${AGO_SKIP_CHECKSUM}" = "1" ]; then
    warn "AGO_SKIP_CHECKSUM=1: installing ${ARCHIVE} WITHOUT sha256 verification"
    warn "the downloaded archive is untrusted; verify it yourself or unset AGO_SKIP_CHECKSUM"
  else
    die "checksums.txt is unavailable for ${VERSION}; refusing to install an unverified archive
ago-install: set AGO_SKIP_CHECKSUM=1 to install anyway (no verification), or build from source:
ago-install:   git clone ${REPO_URL}.git && go build -o ago ./cmd/ago"
  fi

  info "extracting ${ARCHIVE}"
  _out="${TMP}/out"
  mkdir -p "${_out}"
  if [ "${ARCHIVE_EXT}" = "zip" ]; then
    have unzip || die "unzip is required to unpack the windows archive (.zip)"
    unzip -oq "${TMP}/${ARCHIVE}" -d "${_out}"
  else
    tar -xzf "${TMP}/${ARCHIVE}" -C "${_out}"
  fi
  [ -f "${_out}/${BIN_NAME}" ] || die "the archive does not contain ${BIN_NAME}"

  install_binary "${_out}/${BIN_NAME}"
  print_path_hint
  info "ago ${VERSION} installed at ${INSTALL_DIR}/${BIN_NAME}"
}

main "$@"
