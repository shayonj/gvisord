# Security

## Trust boundaries

```
Host (root) --- gvisord daemon --- runsc
                     |
              Unix socket (0600)
                     |
              Caller (unprivileged)
                     |
              HTTP over CNI bridge
                     |
              gVisor sandbox
                |-- gvisord-exec (harness)
                '-- user code (untrusted)
```

The daemon runs as root. It manages cgroups, network namespaces, and talks to runsc. The Unix socket defaults to 0600, configurable via `socket_mode` and `socket_group`. The harness runs inside gVisor with no host access. User code runs as a subprocess of the harness, inside the gVisor kernel.

## Kernel isolation

Each execution gets a dedicated gVisor kernel. After each use the kernel is torn down and a fresh one is restored from the checkpoint, so all processes, file descriptors, mount tables, network state, and filesystem caches are gone. Nothing survives between executions. With stock runsc the entire container is destroyed and re-created; with warm sentry mode the sentry process persists but the kernel is reset, providing the same isolation guarantee.

## Socket permissions

```json
{
  "daemon": {
    "socket_mode": "0660",
    "socket_group": "gvisord"
  }
}
```

Only processes with matching UID/GID can connect. No further authentication beyond OS-level access control.

## Checkpoint path restriction

The `execute` API accepts an optional checkpoint override. To prevent callers from loading arbitrary host files as checkpoints, set an allowlist:

```json
{
  "daemon": {
    "checkpoint_dirs": ["/var/gvisord/checkpoints"]
  }
}
```

Paths outside these prefixes are rejected. An empty list allows all paths.

## Resource isolation

Each resource class maps to a cgroup v2 slice with hard limits. CPU uses `cpu.max` (bandwidth throttling). Memory uses `memory.max` (OOM kills only that sentry). Each sentry gets its own sub-cgroup.

## Read-only mounts

The harness and cache are bind-mounted read-only into sandboxes by gvisord:

- `/harness/gvisord-exec` -- harness binary (ro)
- `/cache` -- dependency cache (ro)

The rootfs is symlinked from the checkpoint bundle. Whether it is read-only depends on the `root.readonly` field in the checkpoint's OCI config.json (typically set to `true`).

## Lease expiry

Leases auto-expire after `lease_timeout` (default 5m). If a caller crashes without calling `complete`, the sentry is recycled automatically.

## Out of scope

- gvisord does not handle TLS between host and sandbox (traffic is local, same host).
- Egress filtering is a separate concern (iptables + proxy).
- Seccomp policy comes from the checkpoint/OCI spec.
- Secrets should be passed via the harness HTTP request, not baked into checkpoint environment variables.
