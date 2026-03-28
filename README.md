# gvisord

Sentry pool daemon for [gVisor](https://gvisor.dev). It keeps pre-restored sandboxes ready so you can run untrusted code with fresh kernel isolation and get results back in under 50ms.

gvisord listens on a Unix socket, and the interface is one JSON request per connection. Send a script, get stdout/stderr back.

```bash
gvisord run python/small '{"script":"print(1+1)"}'
{
  "type": "success",
  "exit_code": 0,
  "stdout": "2\n",
  "stderr": "",
  "elapsed_ms": 34,
  "sentry_id": "python-small-1",
  "acquire_ms": 0.01
}
```

Each sandbox gets its own gVisor kernel, loaded from a checkpoint. After each use the kernel is torn down and a fresh one is restored, so nothing survives between executions: no processes, no file descriptors, no mounts, no network state.

## How it works

gvisord manages a pool of `runsc` containers (sentries), each restored from a checkpoint where the language runtime and an HTTP harness (`gvisord-exec`) are already running. When you send a `run` request, the daemon picks a ready sentry, forwards the script to the harness inside it, collects the result, then destroys the sandbox and restores a fresh one from the checkpoint. Pool sizing, networking, and cgroup limits are all internal to the daemon.

## Getting started

### Prerequisites

- Linux with cgroup v2
- [runsc](https://gvisor.dev/docs/user_guide/install/)
- [CNI plugins](https://github.com/containernetworking/plugins) at `/opt/cni/bin` (optional, for bridge networking)
- Go 1.23+ (building from source)

### Build

```bash
go build -o gvisord ./cmd/gvisord
CGO_ENABLED=0 GOOS=linux go build -o gvisord-exec ./cmd/gvisord-exec
```

`gvisord` is the host daemon, and `gvisord-exec` is a static binary that runs inside each sandbox as the execution harness (bind-mounted into the container automatically).

### Prepare a rootfs and checkpoint

gvisord needs a rootfs (an unpacked container image) and a checkpoint (the process state captured after the runtime boots). The checkpoint is what makes restore fast, since the language runtime is already initialized when the sentry comes up.

<details>
<summary>Example: Python rootfs + checkpoint</summary>

```bash
mkdir -p /var/gvisord/images/python/rootfs
docker export $(docker create python:3.12-slim) \
  | tar -xf - -C /var/gvisord/images/python/rootfs

mkdir -p /tmp/ckpt-bundle
cat > /tmp/ckpt-bundle/config.json << 'EOF'
{
  "ociVersion": "1.0.0",
  "process": {
    "args": ["python3", "-c", "import time; time.sleep(3600)"],
    "cwd": "/", "env": ["PATH=/usr/local/bin:/usr/bin:/bin"],
    "user": {"uid": 0, "gid": 0}
  },
  "root": {"path": "/var/gvisord/images/python/rootfs", "readonly": true},
  "mounts": [{"destination": "/proc", "type": "proc", "source": "proc"},
             {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs"}],
  "linux": {"namespaces": [{"type":"pid"},{"type":"mount"},{"type":"ipc"}]}
}
EOF

runsc --root=/tmp/ckpt-root create --bundle=/tmp/ckpt-bundle ckpt-py
runsc --root=/tmp/ckpt-root start ckpt-py
sleep 2

mkdir -p /var/gvisord/images/python/checkpoint
runsc --root=/tmp/ckpt-root checkpoint \
  --image-path=/var/gvisord/images/python/checkpoint ckpt-py
```

</details>

### Configure

<details>
<summary>config.json</summary>

```json
{
  "runsc": {
    "path": "/usr/local/bin/runsc",
    "extra_flags": []
  },
  "resource_classes": [
    { "name": "small", "cpu_millis": 500, "memory_mb": 512 }
  ],
  "templates": {
    "python": {
      "rootfs": "/var/gvisord/images/python/rootfs",
      "checkpoint": "/var/gvisord/images/python/checkpoint",
      "pools": [
        { "resource_class": "small", "pool_size": 3, "pre_restore": true }
      ]
    }
  }
}
```

This creates a workload called `python/small` with 3 warm sentries, each capped at 0.5 CPU and 512MB. The [configuration docs](docs/configuration.md) and [examples/](examples/) cover the full set of options.

</details>

### Start

```bash
sudo ./gvisord --config config.json
```

Optional flags: `--log-level debug|info|warn|error` (default `info`).

The daemon pre-warms the pool and starts listening on `/run/gvisord/gvisord.sock`. It logs when sentries are ready.

### Use

The API is JSON over the Unix socket, one request per connection.

---

<details>
<summary>From your application</summary>

```python
import socket, json

def gvisord_call(method, params=None):
    sock = socket.socket(socket.AF_UNIX)
    sock.connect("/run/gvisord/gvisord.sock")
    req = {"method": method}
    if params:
        req["params"] = params
    sock.sendall(json.dumps(req).encode() + b"\n")
    resp = json.loads(sock.recv(65536))
    sock.close()
    return resp

# check readiness
print(gvisord_call("health"))
# {"result": {"healthy": true}}

# run a script
print(gvisord_call("run", {
    "workload": "python/small",
    "script": "print(1+1)",
}))
# {"result": {"type": "success", "stdout": "2\n", "exit_code": 0, ...}}
```

This works from any language that can open a Unix socket. Connect, send one JSON object, read one back, close.

</details>

---

<details>
<summary>From the CLI</summary>

```bash
gvisord health
# {"healthy": true}

gvisord run python/small '{"script":"print(1+1)"}'
# {"type": "success", "stdout": "2\n", "exit_code": 0, ...}

gvisord run python/small '{"script":"print(event)","event":{"name":"test"},"timeout":10}'

gvisord status
gvisord drain
```

The CLI talks to the same socket. You can override the path with `GVISORD_SOCKET`.

</details>

---

If CNI networking is configured, the daemon talks to the harness over HTTP internally. Without CNI it falls back to `runsc exec`. The caller doesn't need to know which path is used.

There's also an `execute`/`complete` lease-based API for cases where you want to hold a sentry across multiple requests or use a custom protocol. That's documented in the [API docs](docs/api.md).

## Pool sizing

Each sentry uses about 50MB of RSS. Recycling takes ~65ms with warm sentry mode (WIP) or ~150ms with stock runsc. If you need N concurrent executions, size the pool at roughly 2N so half can serve while the other half recycle.

| Metric                 | Warm sentry | Stock runsc |
| ---------------------- | ----------- | ----------- |
| Acquire (sentry ready) | ~0ms        | ~0ms        |
| End-to-end             | ~35ms       | ~35ms       |
| Recycle                | ~65ms       | ~150ms      |
| Memory per sentry      | ~50MB       | ~50MB       |

## Docs

- [API](docs/api.md)
- [Configuration](docs/configuration.md)
- [Deployment](docs/deployment.md)
- [Security](docs/security.md)
- [Design](docs/design.md)

## Testing

```bash
go test ./...
go test -race ./...

# Integration tests (Linux, runsc, root):
sudo GVISORD_INTEGRATION=1 go test -v ./e2e/ -run TestIntegration
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
