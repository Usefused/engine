#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

cd "$tmp_dir"
git init -q
git config user.email "test@example.com"
git config user.name "Release Notes Test"

mkdir -p internal/controlplane
echo "package controlplane" >internal/controlplane/controlplane.go
git add .
git commit -qm "control-plane: bootstrap"
git tag v0.1.0

echo "// control-plane only" >>internal/controlplane/controlplane.go
git commit -qam "control-plane: only change"

mkdir -p internal/engine
echo "package engine" >internal/engine/engine.go
git add .
git commit -qm "engine: add runtime fix"
git tag v0.2.0

GITHUB_REF_NAME=v0.2.0 "$repo_root/scripts/generate-engine-release-notes.sh" notes.md
grep -q "engine: add runtime fix" notes.md
if grep -q "control-plane: only change" notes.md; then
  echo "control-plane-only commits leaked into engine release notes" >&2
  exit 1
fi

echo "// control-plane only again" >>internal/controlplane/controlplane.go
git commit -qam "control-plane: follow-up only"
git tag v0.3.0

GITHUB_REF_NAME=v0.3.0 "$repo_root/scripts/generate-engine-release-notes.sh" empty-notes.md
grep -q "No engine-facing changes since v0.2.0." empty-notes.md
