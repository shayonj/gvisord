# Deployment

## systemd

```ini
[Unit]
Description=gvisord sentry pool daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/gvisord --config /etc/gvisord/config.json
RuntimeDirectory=gvisord
Restart=on-failure
NoNewPrivileges=no

[Install]
WantedBy=multi-user.target
```

See [deploy/gvisord.service](../deploy/gvisord.service).

## Docker

```bash
docker build -t gvisord .
docker run --privileged \
  -v /var/gvisord/images:/var/gvisord/images \
  -v /run/gvisord:/run/gvisord \
  -v /etc/gvisord:/etc/gvisord \
  gvisord
```

`--privileged` is needed for cgroup and namespace management. For production, use specific capabilities:

```bash
docker run --cap-add SYS_ADMIN --cap-add NET_ADMIN \
  --cap-add SYS_PTRACE --cap-add SYS_CHROOT ...
```

## Kubernetes

Deploy as a DaemonSet. Worker pods mount the socket directory and stay unprivileged.

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: gvisord
spec:
  selector:
    matchLabels:
      app: gvisord
  template:
    metadata:
      labels:
        app: gvisord
    spec:
      hostPID: true
      containers:
      - name: gvisord
        image: your-registry/gvisord:latest
        securityContext:
          capabilities:
            add: [SYS_ADMIN, NET_ADMIN, SYS_PTRACE, SYS_CHROOT]
        volumeMounts:
        - name: socket
          mountPath: /run/gvisord
        - name: images
          mountPath: /var/gvisord/images
        - name: config
          mountPath: /etc/gvisord
      volumes:
      - name: socket
        hostPath: {path: /run/gvisord, type: DirectoryOrCreate}
      - name: images
        hostPath: {path: /var/gvisord/images}
      - name: config
        configMap: {name: gvisord-config}
```
