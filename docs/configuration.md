# bitcoin-shard-listener — Configuration Reference

All parameters are accepted as CLI flags. Environment variables serve as
fallbacks; hard-coded defaults apply when neither is present.

## Network

### `-iface` / `MULTICAST_IF` (default: `eth0`)

Network interface for multicast group joins and NACK send. Must be the same
interface the multicast fabric is reachable on.

### `-listen-port` / `LISTEN_PORT` (default: `9001`)

UDP port to receive multicast frames on. Must match the proxy's egress port.

### `-scope` / `MC_SCOPE` (default: `site`)

Multicast scope nibble. Must match the proxy's `-scope`.

| Value | Prefix | Reach |
|---|---|---|
| `link` | `FF02` | Same L2 segment only |
| `site` | `FF05` | Site-local; crosses routers within a site (default) |
| `org` | `FF08` | Organisation-wide |
| `global` | `FF0E` | Internet-wide |

### `-mc-base-addr` / `MC_BASE_ADDR`

Base IPv6 address for the assigned multicast address space (bytes 2–12). Must
match the proxy's `-mc-base-addr`. Leave empty to use the default all-zeros
middle bytes.

---

## Sharding

### `-shard-bits` / `SHARD_BITS` (default: `2`)

Txid prefix bit width used as the shard key. Must exactly match the proxy's
`-shard-bits`. Determines how many multicast groups exist (2ᴺ).

| Bits | Groups |
|---|---|
| 1 | 2 |
| 2 | 4 |
| 8 | 256 |
| 16 | 65 536 |
| 24 | 16 777 216 |

### `-shard-include` / `SHARD_INCLUDE`

Comma-separated list of shard indices to subscribe to and forward. Empty (the
default) means subscribe to all groups. Example: `0,1,3`.

### `-subtree-include` / `SUBTREE_INCLUDE`

Comma-separated list of 32-byte hex SubtreeIDs to allow (V2 frames only).
Empty means accept all subtrees.

### `-subtree-exclude` / `SUBTREE_EXCLUDE`

Comma-separated list of 32-byte hex SubtreeIDs to drop. Applied after include.
Empty means exclude nothing.

---

## Egress (unicast downstream)

### `-egress-addr` / `EGRESS_ADDR` (default: `127.0.0.1:9100`)

Downstream unicast `host:port`. Frames passing the filter are forwarded here.

### `-egress-proto` / `EGRESS_PROTO` (default: `udp`)

Egress protocol: `udp` or `tcp`.

- **UDP** — one datagram per frame; no connection state.
- **TCP** — persistent connection; reconnects automatically on error.

### `-strip-header` / `STRIP_HEADER` (default: `false`)

When `true`, only the raw BSV transaction payload is forwarded (no frame
header). When `false`, the complete 100-byte V2 frame is forwarded verbatim.

---

## NACK / Gap Recovery

Gap tracking is performed for V2 frames where both `SenderID` (bytes 80–95)
and `ShardSeqNum` (bytes 40–47) are non-zero.

IPv4-mapped SenderIDs (`::ffff:a.b.c.d`) are fully valid and participate in
gap tracking. A zero SenderID means the proxy has not yet stamped the field and
gap tracking is skipped for that frame.

### `-retry-endpoints` / `RETRY_ENDPOINTS`

Comma-separated `host:port` list of multicast retry caching nodes to send NACK
datagrams to. Empty disables NACK dispatch (gaps are still detected and
counted). Example: `10.0.0.1:9002,10.0.0.2:9002`.

### `-nack-jitter-max` / `NACK_JITTER_MAX` (default: `200ms`)

Maximum random hold-off before the first NACK is dispatched (NORM suppression
window). Prevents NACK implosion when many listeners detect the same gap.

### `-nack-backoff-max` / `NACK_BACKOFF_MAX` (default: `5s`)

Cap on exponential backoff between successive NACK retries for the same gap.

### `-nack-max-retries` / `NACK_MAX_RETRIES` (default: `5`)

Maximum NACK attempts per gap. After this is exceeded the gap is declared
unrecoverable and evicted (`bsl_gaps_unrecovered_total` incremented).

### `-nack-gap-ttl` / `NACK_GAP_TTL` (default: `10m`)

Maximum lifetime of a gap entry before it is evicted regardless of retry
count. Set to approximately one Bitcoin block interval to avoid accumulating
stale state across block boundaries.

---

## Runtime

### `-workers` / `NUM_WORKERS` (default: `runtime.NumCPU()`)

Number of SO_REUSEPORT receive worker goroutines. The kernel distributes
datagrams across all workers; the same source consistently lands on the same
worker for CPU-local per-sender gap state.

### `-debug` / `DEBUG` (default: `false`)

Enable per-frame debug logging (decode errors, forwarded frames, gap events).

### `-drain-timeout` / `DRAIN_TIMEOUT` (default: `0`)

Pre-shutdown drain window. When non-zero, `/readyz` returns 503 immediately
on signal receipt while workers continue forwarding for this duration. Useful
for rolling restarts behind a load balancer.

---

## Observability

### `-metrics-addr` / `METRICS_ADDR` (default: `:9200`)

HTTP bind address for:
- `GET /metrics` — Prometheus scrape endpoint
- `GET /healthz` — always `200 OK` while the process is running
- `GET /readyz` — `200` when all workers are ready; `503` while starting or draining

### `-instance` / `INSTANCE_ID` (default: hostname)

OTel `service.instance.id` resource attribute. Useful in federated deployments
to identify individual listener instances.

### `-otlp-endpoint` / `OTLP_ENDPOINT`

gRPC endpoint for OTLP metric push (e.g. `otel-collector:4317`). Empty
disables push export; Prometheus scraping always works regardless.

---

## Example: minimal

```
bitcoin-shard-listener \
  -iface eth0 \
  -shard-bits 2 \
  -egress-addr 127.0.0.1:9100
```

## Example: shard filter + NACK

```
bitcoin-shard-listener \
  -iface eth0 \
  -shard-bits 8 \
  -shard-include 0,1,2,3 \
  -egress-addr consumer.local:9100 \
  -egress-proto tcp \
  -retry-endpoints retry1.local:9002,retry2.local:9002 \
  -nack-jitter-max 100ms \
  -nack-max-retries 3 \
  -metrics-addr :9200
```
