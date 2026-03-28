# Configuration

gvisord reads a single JSON config file. All fields have defaults; you only need to specify templates and a runsc path.

See [examples/basic/config.json](../examples/basic/config.json) for a minimal config and [deploy/config.example.json](../deploy/config.example.json) for a complete one.

## daemon

| Field | Default | Description |
|-------|---------|-------------|
| socket | /run/gvisord/gvisord.sock | Unix socket path |
| socket_mode | 0600 | Socket file permissions (octal string) |
| socket_group | (empty) | Group ownership for socket |
| pid_file | /run/gvisord/gvisord.pid | PID file path |
| workdir | /run/gvisord/sentries | Sentry bundle working directory |
| harness_path | /var/gvisord/harness/gvisord-exec | Path to harness binary on host |
| cache_dir | /var/cache/gvisord | Dependency cache, bind-mounted as /cache |
| lease_timeout | 5m | Max lease duration before auto-recycle |
| checkpoint_dirs | [] | Allowed checkpoint path prefixes (empty = allow all) |

## runsc

| Field | Default | Description |
|-------|---------|-------------|
| path | /usr/local/bin/runsc | Path to runsc binary |
| extra_flags | [] | Extra flags for every runsc invocation |

## network

| Field | Default | Description |
|-------|---------|-------------|
| mode | cni | "cni" for bridge networking, "none" to disable |
| plugin_dir | /opt/cni/bin | CNI plugin binaries |
| config_dir | /etc/cni/net.d | CNI config files |
| bridge | gvisord0 | Bridge interface name |
| subnet | 10.88.0.0/16 | IPAM subnet |

## cgroup

| Field | Default | Description |
|-------|---------|-------------|
| base_path | /sys/fs/cgroup | cgroup v2 mount point |
| prefix | gvisord | Slice name prefix |

## resource_classes

CPU and memory limits, referenced by pool configs. If omitted, three defaults are provided: `small` (500m CPU, 512MB), `medium` (1000m CPU, 1024MB), `large` (2000m CPU, 4096MB).

| Field | Description |
|-------|-------------|
| name | Class name |
| cpu_millis | CPU limit in millicores (1000 = 1 CPU) |
| memory_mb | Memory limit in MB |

## templates

Each template is a rootfs + checkpoint pair. A template can have multiple pools at different resource classes.

| Field | Description |
|-------|-------------|
| rootfs | Path to unpacked container filesystem |
| checkpoint | Path to checkpoint directory |
| pools | Array of pool configs |

### pools

| Field | Default | Description |
|-------|---------|-------------|
| resource_class | (required) | Which resource class |
| pool_size | (required) | Number of warm sentries |
| max_pending | pool_size * 2 | Max queued callers |
| pre_restore | true | Restore at startup vs on-demand |

## limits

Health checks and eviction thresholds, applied after each recycle.

| Field | Default | Description |
|-------|---------|-------------|
| max_restores_per_sentry | 100 | Replace after N restores |
| max_sentry_age | 10m | Replace after this age |
| max_rss_growth_kb | 204800 | Replace if RSS grows by this much |
| max_open_fds | 512 | Replace if FD count exceeds this |
| idle_timeout | 5m | Evict idle sentries (0 = never) |
