#!/bin/sh
set -e

REPO="mpm/dworm"
INSTALL_DIR="${DWORM_INSTALL_DIR:-$HOME/.local/bin}"

# Detect OS
OS="$(uname -s)"
case "$OS" in
    Linux*)  OS=linux ;;
    Darwin*) OS=darwin ;;
    *)       echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
    arm64)   ARCH=arm64 ;;
    *)       echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Get version (from argument or latest release)
if [ -n "$1" ]; then
    VERSION="$1"
else
    echo "Fetching latest version..."
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
    if [ -z "$VERSION" ]; then
        echo "Failed to fetch latest version"
        exit 1
    fi
fi

echo "Installing dworm ${VERSION} for ${OS}/${ARCH}..."

# Download
ARCHIVE="dworm_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading ${URL}..."
curl -fsSL "$URL" -o "${TMPDIR}/${ARCHIVE}"

# Verify checksum
echo "Verifying checksum..."
CHECKSUM_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
curl -fsSL "$CHECKSUM_URL" -o "${TMPDIR}/checksums.txt"
cd "$TMPDIR"
if command -v sha256sum >/dev/null 2>&1; then
    grep "$ARCHIVE" checksums.txt | sha256sum -c -
elif command -v shasum >/dev/null 2>&1; then
    grep "$ARCHIVE" checksums.txt | shasum -a 256 -c -
else
    echo "Warning: No sha256sum or shasum found, skipping checksum verification"
fi

# Extract and install
echo "Installing to ${INSTALL_DIR}..."
mkdir -p "$INSTALL_DIR"
tar -xzf "$ARCHIVE"
cp "dworm_${VERSION}_${OS}_${ARCH}/dworm" "$INSTALL_DIR/"
cp "dworm_${VERSION}_${OS}_${ARCH}/dworm_endpoint" "$INSTALL_DIR/"
chmod +x "$INSTALL_DIR/dworm" "$INSTALL_DIR/dworm_endpoint"

echo ""
echo "Successfully installed dworm to ${INSTALL_DIR}"

# Check if in PATH
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "Note: Add ${INSTALL_DIR} to your PATH if not already done" ;;
esac
