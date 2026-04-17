# bitcoin-shard-listener

Multicast subscriber and unicast forwarder for the BSV transaction sharding
pipeline. Receives V2 frames from the `bitcoin-shard-proxy` multicast fabric,
applies shard and subtree filters, forwards matching frames to a configurable
downstream unicast consumer over UDP or TCP, and performs NORM-inspired
NACK-based gap recovery keyed on `(SenderID, groupIndex)`.

## Features

- **SO_REUSEPORT** multi-worker receive with kernel-level source affinity
- **Shard filter** — subscribe to a subset of shard groups (empty = all)
- **Subtree filter** — include/exclude by 32-byte SubtreeID (V2 frames)
- **Gap tracking** — per `(SenderID, groupIndex)` sequence gap detection
- **IPv4-mapped SenderID support** — `::ffff:a.b.c.d` senders fully tracked
- **NACK dispatch** — 64-byte NACK datagrams to configurable retry endpoints
- **Egress UDP or TCP** with optional strip-header mode (payload-only)
- **Prometheus + OTLP metrics**, `/healthz`, `/readyz`
- **Graceful shutdown** with configurable drain window

## Quick start

```sh
# Subscribe to all groups; forward to localhost:9100 over UDP
bitcoin-shard-listener \
  -iface eth0 \
  -shard-bits 2 \
  -egress-addr 127.0.0.1:9100
```

## Build

```sh
make build       # -> build/bitcoin-shard-listener
make test        # run all tests
make docker      # build Docker image
```

## Documentation

- [Architecture](docs/architecture.md)
- [Configuration reference](docs/configuration.md)
- [Protocol specification](../bitcoin-shard-proxy/docs/protocol.md)

## Dependencies

- `github.com/lightwebinc/bitcoin-shard-proxy` — `frame`, `shard` packages
  (local `replace` directive; both repos must be checked out side-by-side)
- Prometheus client + OpenTelemetry SDK (same versions as proxy)
- `golang.org/x/net/ipv6` — multicast group join
- `golang.org/x/sys/unix` — `SO_REUSEPORT`

## License

See [LICENSE](LICENSE).
