#!/bin/bash
set -e

# Fused Engine & CLI Installation Script
# This script detects the OS and Architecture, downloads the latest Engine release from GitHub,
# installs the `fused-engine` binary, and then automatically installs the `fused-cli`.

REPO="Usefused/engine"
BINARY="fused-engine"
INSTALL_DIR="/usr/local/bin"

# Detect OS
OS="$(uname -s)"
case "${OS}" in
    Linux*)     OS="Linux";;
    Darwin*)    OS="Darwin";;
    MINGW*|MSYS*|CYGWIN*)
        echo "Windows detected. Please use WSL or Linux/macOS for the Fused Engine."
        exit 1;;
    *)          echo "Unsupported operating system: ${OS}"; exit 1;;
esac

# Detect Architecture
ARCH="$(uname -m)"
case "${ARCH}" in
    x86_64)     ARCH="x86_64";;
    arm64|aarch64) ARCH="arm64";;
    *)          echo "Unsupported architecture: ${ARCH}"; exit 1;;
esac

echo "=> Detected ${OS} ${ARCH} for Fused Engine"

# Determine version to install
if [ -n "$VERSION" ]; then
    TARGET_VERSION="$VERSION"
    echo "=> Using specified Engine version ${TARGET_VERSION}"
else
    echo "=> Fetching latest Engine release version..."
    TARGET_VERSION=$(curl -s "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
fi

if [ -z "$TARGET_VERSION" ]; then
    echo "Error: Could not determine release version."
    exit 1
fi

echo "=> Installing Fused Engine version ${TARGET_VERSION}"

# Construct the download URL based on GoReleaser naming convention
# Example: fused-engine_Linux_arm64.tar.gz
TAR_NAME="${BINARY}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TARGET_VERSION}/${TAR_NAME}"

# Create a temporary directory
TMP_DIR=$(mktemp -d)
cd "$TMP_DIR"

echo "=> Downloading ${DOWNLOAD_URL}..."
curl -sL -o "${TAR_NAME}" "${DOWNLOAD_URL}"

# Extract the archive
echo "=> Extracting archive..."
tar -xzf "${TAR_NAME}"

# Move the binary to the install directory
echo "=> Installing to ${INSTALL_DIR}/${BINARY} (requires sudo)..."
sudo mv "${BINARY}" "${INSTALL_DIR}/${BINARY}"
sudo chmod +x "${INSTALL_DIR}/${BINARY}"

# Clean up
cd - > /dev/null
rm -rf "$TMP_DIR"

echo "=> Fused Engine installed successfully!"

# Now install the CLI
echo "=> Proceeding to install the Fused CLI..."
curl -sSL https://raw.githubusercontent.com/Usefused/cli/main/install.sh | bash

echo "=> Full installation complete!"
echo "=> Run 'fused-engine start' or 'fused-cli --help' to get started."
