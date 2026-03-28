# API

gvisord exposes a JSON API over a Unix domain socket (`/run/gvisord/gvisord.sock` by default). Each connection handles one request-response pair.

## Protocol

Send a JSON object with `method` and optional `params`. Get back `result` or `error` + `code`. An unknown method returns `code: "unknown_method"`.

```
-> {"method": "run", "params": {"workload": "python/small", "script": "print(1)"}}
<- {"result": {"type": "success", "stdout": "1\n", "exit_code": 0, ...}}
```

## run

Run a script in a sandbox. The daemon acquires a sentry, sends the request to the in-sandbox harness, collects the result, and recycles the sentry. One call, one response.

```json
{
  "method": "run",
  "params": {
    "workload": "python/small",
    "script": "print(event['name'])",
    "interpreter": "python3",
    "deps": ["requests"],
    "event": {"name": "test"},
    "env": {"API_KEY": "xxx"},
    "timeout": 30
  }
}
```

`workload` is required. Either `script` or `command` is required. All other fields are optional.

Response:

```json
{
  "result": {
    "type": "success",
    "exit_code": 0,
    "stdout": "test\n",
    "stderr": "",
    "elapsed_ms": 22.4,
    "sentry_id": "python-3",
    "acquire_ms": 0.01
  }
}
```

`type` is `success`, `error`, or `timeout`. Error codes: `bad_request`, `run_failed`, `capacity_exhausted`.

## execute

Acquire a sandbox and get a lease. For advanced use cases where you want direct access to the sentry (multiple requests, streaming, custom harness protocols).

```json
{"method": "execute", "params": {"workload": "python/small"}}
```

Optional `"checkpoint": "/path/to/override"` to use a different checkpoint (must be under a configured `checkpoint_dirs` path). Checkpoint overrides are not supported when the pool has `pre_restore: true` since the sentry is already restored at startup.

Response:

```json
{
  "result": {
    "workload": "python/small",
    "sentry_id": "python-small-1",
    "lease_id": "a1b2c3d4e5f6...",
    "pid": 12345,
    "ip": "10.88.0.5",
    "checkpoint": "/var/gvisord/images/python/checkpoint",
    "acquire_ms": 0.012,
    "restore_ms": 0.0,
    "restore_num": 1
  }
}
```

Error codes: `bad_request`, `execute_failed`, `capacity_exhausted`

## complete

Release a leased sandbox. Triggers sentry recycling in the background (kill, then either reset+restore or destroy+spawn depending on the runtime).

```json
{"method": "complete", "params": {"lease_id": "a1b2c3d4e5f6..."}}
```

Response: `{"result": {"ok": true}}`

Error codes: `bad_request`, `complete_failed`

## status

Pool status, sentry details, execution metrics.

```json
{"method": "status"}
```

Response:

```json
{
  "result": {
    "pools": [
      {
        "workload": "python/small",
        "pool_size": 2,
        "max_pending": 4,
        "ready": 1,
        "running": 1,
        "pending": 0,
        "checkpoint": "/var/gvisord/images/python/checkpoint",
        "rootfs": "/var/gvisord/images/python/rootfs",
        "pre_restore": true,
        "total_executions": 42,
        "avg_acquire_ms": 0.015,
        "avg_restore_ms": 0.0,
        "sentries": [
          {
            "id": "python-small-1",
            "pid": 12345,
            "state": "ready",
            "restores": 5,
            "age_s": 120.5,
            "idle_s": 30.2,
            "rss_kb": 52480,
            "fds": 64,
            "ip": "10.88.0.5"
          }
        ]
      }
    ],
    "active_leases": 1
  }
}
```

## health

Whether any pool has a ready sentry.

```json
{"method": "health"}
```

Response: `{"result": {"healthy": true}}`

## drain

Gracefully shut down all pools and stop the API server.

```json
{"method": "drain"}
```

Response: `{"result": {"ok": true}}`

## CLI

The `gvisord` binary doubles as a CLI client:

```bash
gvisord run <workload> '<json_params>'
gvisord execute <workload> [checkpoint]
gvisord complete <lease_id>
gvisord status
gvisord health
gvisord drain
```

Set `GVISORD_SOCKET` to override the default socket path.

## Harness (gvisord-exec)

The harness runs inside each sandbox on port 8080.

`GET /health` returns `{"status": "ok"}`.

`POST /run` executes code and returns results. Handles one request then exits.

```json
{
  "script": "print('hello')",
  "interpreter": "python3",
  "deps": ["numpy"],
  "event": {"key": "value"},
  "env": {"MY_VAR": "123"},
  "timeout": 30
}
```

Fields: `script` or `command` (provide one), `interpreter` (default python3), `deps` (cache subdirs added to PYTHONPATH/NODE_PATH), `event` (injected as variable), `env` (extra env vars), `timeout` (seconds, default 30). The daemon's `run` method validates that `script` or `command` is present before forwarding to the harness.

Response:

```json
{
  "type": "success",
  "exit_code": 0,
  "stdout": "hello\n",
  "stderr": "",
  "elapsed_ms": 12.5
}
```

`type` is `success`, `error`, or `timeout`.
