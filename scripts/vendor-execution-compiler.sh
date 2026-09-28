#!/usr/bin/env bash
set -euo pipefail

# The Registry authoring package is the source of truth; Engine carries a
# pinned, buildable snapshot so image builds have no cross-repository fetch.
mode="${1:-}"
registry_root="${2:-}"
engine_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendor_root="$engine_root/runtime/execution"

# An explicit source checkout prevents silently comparing against the wrong repo.
if [[ "$mode" != "--sync" && "$mode" != "--verify" ]] || [[ -z "$registry_root" ]]; then
  echo "Usage: $0 --sync|--verify /path/to/registry-checkout" >&2
  exit 2
fi

source_root="$(cd "$registry_root" && pwd)/execution"
# Only a clean source snapshot can be attributed to its immutable Git revision.
if [[ -n "$(git -C "$registry_root" status --porcelain -- execution/package.json execution/package-lock.json execution/tsconfig.json execution/src execution/test execution/scripts execution/examples)" ]]; then
  echo "Registry execution compiler files have uncommitted changes" >&2
  exit 1
fi

# Unrelated Registry commits do not change the pinned compiler snapshot.
source_revision="$(git -C "$registry_root" log -1 --format=%H -- execution/package.json execution/package-lock.json execution/tsconfig.json execution/src execution/test execution/scripts execution/examples)"
# Sync copies the one authoring package rather than maintaining a second implementation.
if [[ "$mode" == "--sync" ]]; then
  mkdir -p "$vendor_root"
  cp "$source_root/package.json" "$source_root/package-lock.json" "$source_root/tsconfig.json" "$vendor_root/"
  rm -rf "$vendor_root/src" "$vendor_root/test" "$vendor_root/scripts" "$vendor_root/examples"
  cp -R "$source_root/src" "$source_root/test" "$source_root/scripts" "$source_root/examples" "$vendor_root/"
  printf '%s\n' "$source_revision" > "$vendor_root/SOURCE_REVISION"
fi

# Verify both the pinned commit and every build/test input; extra generated
# dist files and node_modules are intentionally outside this comparison.
if [[ "$(cat "$vendor_root/SOURCE_REVISION")" != "$source_revision" ]]; then
  echo "Execution compiler source revision differs from pinned snapshot" >&2
  exit 1
fi
for file in package.json package-lock.json tsconfig.json; do
  cmp "$source_root/$file" "$vendor_root/$file"
done
for directory in src test scripts examples; do
  diff -qr "$source_root/$directory" "$vendor_root/$directory"
done
