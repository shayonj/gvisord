# Design

Architecture and implementation notes for contributors.

## Two binaries

`gvisord` (host daemon) and `gvisord-exec` (in-sandbox harness) are separate for three reasons:

- The harness knows nothing about the daemon, host paths, or pool management. If user code compromises it, there's no path to the control plane.
- The harness is a static binary that works in any rootfs. The daemon needs root on the host.
- With CNI they communicate over HTTP on the bridge. Without CNI the daemon bypasses the harness and uses `runsc exec` directly. No shared state either way.

## Internal flow

```
Caller --> api.Server --> pool.Manager --> pool.Pool --> runsc.Client
                |              |                              |
           unix socket    lease map                      exec runsc CLI
                          (lease_id -> sentry)
```

Each pool has a slice of sentries, a condition variable for queuing, and a pending counter. The spawn sequence per sentry:

1. PrepareBundle (symlink rootfs, copy config.json)
2. InjectBundleMounts (harness + cache bind mounts)
3. CleanFilestores (remove stale .gvisor files from rootfs)
4. CNI CreateNetNS (if mode=cni)
5. InjectNetNS (set netns in OCI spec)
6. runsc create (warm sentry only: creates the persistent sentry process)
7. runsc restore (if pre_restore=true or stock runsc)
8. runsc state (get PID)
9. cgroup PlacePID

## Sentry states

Code defines three states: `ready`, `running`, `draining`.

```
    spawn --> [ready]  <-----------------------------------+
                |                                          |
       acquire (lease) --> [running]                       |
                |              |                           |
                |       caller uses sandbox                |
                |              |                           |
       complete (lease) --> [draining]                     |
                               |                           |
                       SIGTERM --> wait (3s) --> SIGKILL   |
                               |                           |
                   +-----------+-----------+               |
                   |                       |               |
             warm sentry              stock runsc          |
                   |                       |               |
             runsc reset             delete + spawn -------+
                   |                                       |
             health check                                  |
                   |                                       |
             restore from checkpoint ----------------------+

    If anything fails during draining, the sentry is
    destroyed and a replacement is spawned.
```

## Concurrency

Acquire does a linear scan under the pool mutex. Pool size is typically 1-10, so this is fine. When all sentries are busy, callers wait on a sync.Cond up to max_pending. Recycle runs in a detached goroutine and doesn't block the Complete caller. A background reaper ticks every 30s and evicts sentries idle beyond idle_timeout. If the pool is completely empty (all evicted), concurrent callers may each spawn a sentry synchronously; extras become additional pool members.

## OCI bundle construction

Three functions cooperate on the bundle's config.json:

1. PrepareBundle creates the directory, symlinks rootfs, copies config.json from checkpoint.
2. InjectBundleMounts appends bind mounts for /harness/gvisord-exec (ro) and /cache (ro).
3. InjectNetNS adds or updates the network namespace entry.

All three treat config.json as a generic map -- no OCI spec structs, no dependencies.

## Cgroup

Pure filesystem writes against cgroup v2. EnsureSlices creates slice directories and writes cpu.max / memory.max. SentrySlicePath creates a sub-cgroup per sentry. PlacePID writes the sentry PID to cgroup.procs. Requires cgroup v2, detected by checking for cgroup.controllers at startup. CleanupStaleSlices removes leftovers from previous crashes.

## CNI

Shells out to standard CNI plugin binaries. Creates netns with `ip netns add`, invokes bridge and loopback plugins with ADD/DEL on stdin. Plugin existence is validated at startup.

## Leases

Leases live in a map on the Manager, protected by a mutex. Each lease gets a timer that auto-completes after lease_timeout. Complete stops the timer, removes the lease, and kicks off recycle in a goroutine.
