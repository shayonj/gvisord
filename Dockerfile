# Stage 1: Build both binaries
FROM golang:1.23-bookworm AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .

RUN go build -o /out/gvisord ./cmd/gvisord
RUN CGO_ENABLED=0 go build -o /out/gvisord-exec ./cmd/gvisord-exec

# Stage 2: Runtime image
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    iproute2 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/gvisord /usr/local/bin/gvisord
COPY --from=builder /out/gvisord-exec /var/gvisord/harness/gvisord-exec

RUN mkdir -p /run/gvisord /var/gvisord/images /var/cache/gvisord /etc/gvisord

VOLUME ["/run/gvisord", "/var/gvisord/images", "/var/cache/gvisord"]

ENTRYPOINT ["gvisord"]
CMD ["--config", "/etc/gvisord/config.json"]
