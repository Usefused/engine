#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${1:?Pass the Engine image to validate}"
bundle_dir="$(mktemp -d)"
container_id=""

# A failed extraction must not leave behind temporary containers or deployment files.
cleanup() {
  # Container creation can fail before an ID is available for removal.
  if [ -n "$container_id" ]; then
    docker rm "$container_id" >/dev/null
  fi
  rm -rf "$bundle_dir"
}
trap cleanup EXIT

# Extraction must work before Engine startup, without a license or database configuration.
container_id="$(docker create "$image")"
docker cp "$container_id:/app/deploy/unified-app-workers/." "$bundle_dir/"
for file in seccomp.json LICENSE.moby compose.yaml README.md; do
  cmp "$repo_root/deploy/unified-app-workers/$file" "$bundle_dir/$file"
done

# Validate Docker's acceptance of the extracted profile and the image's Compose init/Node paths.
docker run --rm --network none --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --security-opt "seccomp=$bundle_dir/seccomp.json" \
  --entrypoint /usr/bin/tini "$image" -- node -e \
  "JSON.parse(require('node:fs').readFileSync('/app/deploy/unified-app-workers/seccomp.json', 'utf8'))"

echo "Worker deployment bundle verified: $image"
