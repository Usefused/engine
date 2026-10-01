# Unified App workers on Windows

The restored runtime uses one long-lived, OS-confined Goja worker process per app family. It does not create a Docker container for each app or execution. MCP keeps its existing runtime.

## Current deployment path

Run the Linux Engine inside WSL2, or run one Linux Engine container in Docker Desktop's WSL2 backend. The workers then use Linux isolation inside that Engine environment. Docker documents this backend at [Docker Desktop WSL2](https://docs.docker.com/desktop/features/wsl/). This deployment path is an architectural recommendation; the current live load tests ran on Linux ARM64 in Docker Desktop on macOS, not on a Windows machine.

Install the packaged Linux Engine and sibling `fused-execution-worker` together. Use the existing Engine configuration for database, license, and authentication. Keep secrets in the Engine; worker environments contain no inherited credentials. For container deployments, give the Engine aggregate memory/CPU/PID limits and retain nonroot execution and `no-new-privileges`. Do not use `--privileged`, disable seccomp, or globally disable AppArmor to make a failing worker start.

The Linux worker requires user/network/mount/PID/IPC/UTS namespaces and an empty chroot. Default Docker seccomp commonly rejects the required namespace clone. Use a deployment-reviewed, architecture-correct seccomp profile allowing the exact worker clone mask and chroot while retaining the default restrictions. The ARM64 profile used in the load-test artifacts is **not an x86 Windows deployment profile**. AppArmor on some WSL distributions can also restrict user namespaces; any exception must be scoped to the Engine executable rather than a host-wide sysctl change.

Set `FUSED_UNIFIED_APP_WORKER_REQUIRED=true` and check `/health`: the actual isolated-worker probe must report `unified_app_worker_ready: true`. A false result means Unified Apps are unavailable on that installation; it must never enable unconfined execution as a fallback. Existing SDK/provider functionality can remain available when the worker is optional.

## Native Windows implementation still required

A native Engine `.exe` currently builds, but its Unified App worker boundary is deliberately unsupported. A goroutine, Goja instance, or Windows Job Object alone does not supply the filesystem/network isolation provided by the Linux worker.

A native backend should retain the current bounded JSON-over-pipes protocol and family lifecycle, and replace only the OS-specific launcher and confinement/resource-limit code:

1. Launch the child in an **AppContainer**, preferably LPAC where compatible, with no network capabilities and tightly scoped access to its executable and required system libraries. Do not grant access to the workspace, credential files, or arbitrary user directories. Microsoft documents [AppContainer isolation](https://learn.microsoft.com/en-us/windows/win32/secauthz/appcontainer-isolation) and [launching an AppContainer](https://learn.microsoft.com/en-us/windows/win32/secauthz/implementing-an-appcontainer).
2. Apply a **Job Object** before authored input is accepted: kill the child when the parent/job handle closes, restrict child-process creation, and impose process/job memory and CPU bounds. Job Objects provide resource/lifetime controls alongside AppContainer, not instead of it. See [Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects).
3. Inherit only the required pipe handles; sanitize the environment and working directory. Avoid a start-then-restrict race by installing the security attributes at creation and assigning job restrictions before resuming the worker. Validate that confinement is active before acknowledging the `load` frame.
4. Add `.exe` worker resolution and release packaging only with native runtime coverage. CI must actually launch the confined child and verify filesystem/registry/network denial, limits, request cancellation, parent-death cleanup, handle leaks, concurrent requests, version draining, and failure/recovery on Windows x64 and ARM64.

This backend needs a Windows test host. Cross-compilation verifies symbols and packaging compatibility, not sandbox security. No native AppContainer implementation was introduced as part of restoring the existing runtime.

## Checked-in container configuration

The selected Windows path is packaged in [`deploy/unified-app-workers`](../deploy/unified-app-workers/README.md): one Linux Engine container with a persistent data volume, nonroot execution, resource bounds, required worker readiness, and a pinned Moby-based seccomp profile with the exact worker namespace exceptions. Its profile includes amd64 and arm64 rules. The load-test artifact's older ARM64-only profile remains historical evidence.
