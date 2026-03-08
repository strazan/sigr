#!/bin/sh
set -eu

REPO="siggelabor/sigr"

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux)  echo "linux" ;;
    *)      echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)             echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

echo "Detecting platform: ${OS}/${ARCH}"

TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed 's/.*"tag_name": *"//;s/".*//')"
VERSION="${TAG#v}"

echo "Latest version: ${VERSION}"

TARBALL="sigr_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${TAG}/${TARBALL}"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/checksums.txt"

echo "Downloading ${URL}..."
curl -fsSL "${URL}" -o "${TMPDIR}/${TARBALL}"
curl -fsSL "${CHECKSUMS_URL}" -o "${TMPDIR}/checksums.txt"

echo "Verifying checksum..."
EXPECTED="$(grep "${TARBALL}" "${TMPDIR}/checksums.txt" | awk '{print $1}')"
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMPDIR}/${TARBALL}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMPDIR}/${TARBALL}" | awk '{print $1}')"
else
  echo "Warning: no sha256sum or shasum found, skipping verification" >&2
  ACTUAL="${EXPECTED}"
fi
if [ "${ACTUAL}" != "${EXPECTED}" ]; then
  echo "Checksum verification failed!" >&2
  exit 1
fi
echo "Checksum verified."

tar -xzf "${TMPDIR}/${TARBALL}" -C "${TMPDIR}"

INSTALL_DIR="/usr/local/bin"
if [ ! -w "${INSTALL_DIR}" ] 2>/dev/null; then
  INSTALL_DIR="${HOME}/.local/bin"
  mkdir -p "${INSTALL_DIR}"
fi

install -m 755 "${TMPDIR}/sigr" "${INSTALL_DIR}/sigr"
echo "Installed sigr to ${INSTALL_DIR}/sigr"
