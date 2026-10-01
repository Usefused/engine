# One Linux Engine container for Windows/WSL2

Run this directory from a WSL2 shell with Docker Desktop configured for Linux containers. It starts one Engine container; each active Unified App family gets a persistent worker process inside it. MCP remains on its existing implementation. It uses your existing Engine configuration and external database; it does not create, clear, or migrate a database.

```sh
export FUSED_ENGINE_IMAGE='your-pinned-fused-linux-image'
export FUSED_ENGINE_CONFIG='/absolute/path/to/your/engine.yaml'
# Optional capacity overrides: FUSED_ENGINE_CPUS, FUSED_ENGINE_MEMORY, FUSED_ENGINE_PIDS.
docker compose config --quiet
docker compose up -d
docker compose ps
curl --fail http://localhost:8081/health
```

The health response must include `unified_app_worker_ready: true`. The profile requires readiness, so failed isolation returns HTTP 503. `127.0.0.1` inside the container is not the Windows host: existing database/Registry endpoints must be reachable from the container, for example through your normal service DNS or Docker Desktop's `host.docker.internal`. Retain the deployment's existing encryption key, license, database credentials, and TLS settings in its private configuration. Do not use the repository's development configuration as a production template.

Run Compose from this directory so `./seccomp.json` resolves correctly. The image must contain the `fused` user, `/sbin/tini`, `/app/fused-engine`, `/app/fused-execution-worker`, and the packaged compiler. The default image entrypoint needs root to chown mounted data; this configuration starts directly as the existing unprivileged `fused` user. A new Docker named volume copies the image's `/app/data` directory and ownership. Migrating an older volume requires checking its ownership separately.

The container has a read-only root, a writable runtime-data volume, bounded temporary storage, no Linux capabilities, and no-new-privileges. No Docker socket is mounted. There is no privileged mode or unconfined seccomp/AppArmor fallback. Limits default to 2 CPUs, 2 GiB memory without additional swap, and 1,024 processes/threads; these are deployment bounds, not a throughput promise. The 64-app/256-client load test substantially saturated two CPUs.

## Seccomp provenance and scope

`seccomp.json` is Moby's default profile from commit `2ceae35d351c156cb5a8efc0fdc4a08cf94569d8`, plus two explicit amd64/arm64 rules:

- Permit `clone` only when namespace bits equal `CLONE_NEWUSER | CLONE_NEWNET | CLONE_NEWNS | CLONE_NEWPID | CLONE_NEWIPC | CLONE_NEWUTS` (mask `0x7e020000`, value `0x7c020000`). Existing default thread/process creation rules remain unchanged.
- Permit `chroot`; the worker needs it inside its new user namespace, while the parent has no host capabilities.

Both rules apply to the Engine container's processes because seccomp cannot select a pathname. The namespace combination is narrowly matched, but it is still an intentional relaxation of Docker's default profile. Host AppArmor/kernel policy may additionally deny user namespaces. Such failures must leave worker readiness false; do not bypass the security boundary.

Upstream source: https://github.com/moby/profiles/blob/2ceae35d351c156cb5a8efc0fdc4a08cf94569d8/seccomp/default.json

License: `LICENSE.moby` (Apache-2.0). The historical ARM64-only benchmark profile is separate; deployment uses this multi-architecture profile. Validation covers the local Linux ARM64 Docker runtime; Windows/WSL2 and x86-64 native runtime validation still need their actual hosts/CI.
