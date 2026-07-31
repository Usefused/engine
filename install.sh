#!/bin/bash
set -e

# Fused Engine Installation Script
# This script detects the OS and Architecture, downloads the latest Engine release from GitHub,
# and installs the `fused-engine` binary.

REPO="Usefused/engine"
BINARY="fused-engine"
INSTALL_DIR="${HOME}/.local/bin"

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

# Construct the download URL based on GoReleaser naming convention.
# Example: fused_Linux_arm64.tar.gz
TAR_NAME="fused_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TARGET_VERSION}/${TAR_NAME}"

# Create a temporary directory
TMP_DIR=$(mktemp -d)
cd "$TMP_DIR"

echo "=> Downloading ${DOWNLOAD_URL}..."
curl -sL -o "${TAR_NAME}" "${DOWNLOAD_URL}"

# Extract the archive
echo "=> Extracting archive..."
tar -xzf "${TAR_NAME}"

# Ensure the install directory exists
mkdir -p "${INSTALL_DIR}"

# Move the binary to the install directory
echo "=> Installing to ${INSTALL_DIR}/${BINARY}..."
mv "${BINARY}" "${INSTALL_DIR}/${BINARY}"
chmod +x "${INSTALL_DIR}/${BINARY}"

echo "=> Note: Installed to ${INSTALL_DIR} to avoid requiring sudo."
echo "=> If you prefer a system-wide installation, you can move it to /usr/local/bin using sudo."

# Clean up
cd - > /dev/null
rm -rf "$TMP_DIR"

echo "=> Fused Engine installed successfully!"
echo "=> Install fused-cli separately from https://github.com/Usefused/cli/releases when you need the CLI."
echo "=> Run 'fused-engine start' to get started."

# Verify the install directory is on PATH
if ! echo ":${PATH}:" | grep -q ":${INSTALL_DIR}:"; then
    echo ""
    echo "WARNING: ${INSTALL_DIR} is not in your PATH."
    echo "Add the following line to your ~/.bashrc or ~/.zshrc and restart your terminal:"
    echo "  export PATH=\"\$PATH:${INSTALL_DIR}\""
fi
