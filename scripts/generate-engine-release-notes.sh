#!/usr/bin/env bash
set -euo pipefail

output_file="${1:-release-notes.md}"
current_tag="${GITHUB_REF_NAME:-}"

if [[ -z "$current_tag" ]]; then
  current_tag="$(git describe --tags --exact-match 2>/dev/null || true)"
fi

current_ref="HEAD"
if [[ -n "$current_tag" ]] && git rev-parse -q --verify "refs/tags/${current_tag}" >/dev/null; then
  current_ref="$current_tag"
fi

current_commit="$(git rev-list -n 1 "$current_ref")"
previous_tag="$(git describe --tags --match 'v*' --abbrev=0 "${current_commit}^" 2>/dev/null || true)"
range="$current_commit"

if [[ -n "$previous_tag" ]]; then
  range="${previous_tag}..${current_commit}"
fi

# GoReleaser's OSS changelog is commit-message based, so we pre-filter by
# Engine-owned paths before handing notes to the release step.
engine_paths=(
  "cmd/engine/"
  "internal/engine/"
  "internal/shared/"
  "proto/engine/"
  "runtime/"
  "scripts/"
  "engine.yaml"
  "go.mod"
  "go.sum"
  "ui/"
  "Dockerfile"
  "Dockerfile.goreleaser"
  ".goreleaser.yaml"
  ".github/workflows/release.yml"
  "Makefile"
)

entries_file="$(mktemp)"
trap 'rm -f "$entries_file"' EXIT

git log --no-merges --format='- %s' "$range" -- "${engine_paths[@]}" |
  awk '!seen[$0]++' >"$entries_file"

{
  echo "## Changelog"
  echo

  if [[ ! -s "$entries_file" ]]; then
    if [[ -n "$previous_tag" ]]; then
      echo "- No engine-facing changes since ${previous_tag}."
    else
      echo "- No engine-facing changes in this release."
    fi
  else
    cat "$entries_file"
  fi
} >"$output_file"
