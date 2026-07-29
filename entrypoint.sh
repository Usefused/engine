#!/bin/bash
set -e

# The Engine image runs as an unprivileged user, so keep mounted runtime data
# writable before handing off to the process.
if id fused >/dev/null 2>&1; then
    runtime_user=fused
else
    runtime_user=fused
fi

# Ensure the sandboxes directory exists
mkdir -p /app/data/sandboxes

# Fix permissions on the mounted volume
chown -R "$runtime_user:$runtime_user" /app/data

exec su-exec "$runtime_user" "$@"
