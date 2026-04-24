# bitcoin-shard-listener — Architecture

## Overview

`bitcoin-shard-listener` sits downstream of `bitcoin-shard-proxy` in the BSV
transaction distribution pipeline. The proxy multicasts BRC-122 frames onto an
IPv6 multicast fabric; the listener joins the relevant groups, filters frames
by shard index and/or subtree ID, forwards matching frames to a configurable
unicast downstream over UDP or TCP, and performs NORM-inspired NACK-based gap
recovery.

```
BSV senders
   │ (TCP or UDP ingress)
   ▼
bitcoin-shard-proxy
   │ BRC-122 frames, SenderID stamped in-place at bytes 40–43 (CRC32c)
   │ IPv6 multicast  FF05::<group-index>
   ▼
Multicast fabric (site-scoped FF05::/16)
   │
   ├── direct subscribers (miners, exchanges, …)
   │
   └── bitcoin-shard-listener
          │ filter → egress (unicast UDP or TCP)
          ▼
       downstream consumers
```

## Receive workers

Each worker:
1. Opens a UDP socket with `SO_REUSEPORT` on the configured listen port.
2. Joins all configured multicast groups on the configured interface.
3. Calls `frame.Decode`, `shard.Engine.GroupIndex`, `filter.Allow`, and
   `egress.Send` in the hot path for every received datagram.
4. Calls `nack.Tracker.Observe` for BRC-122 frames with non-zero `SenderID` and
   `ShardSeqNum`.

**SO_REUSEPORT and multicast:** Linux does **not** load-balance multicast
datagrams across SO_REUSEPORT sockets — every socket that has joined the group
receives a full copy of each datagram. Running more than one worker therefore
causes every frame to be processed and forwarded multiple times.
**`NUM_WORKERS` must be set to `1` for multicast-receive deployments.**

SO_REUSEPORT load balancing applies to unicast UDP only. The E2E test suite
exploits this property by injecting frames as unicast to `[::]:listen-port`,
allowing multiple worker sockets to be tested in isolation.

## BRC-122 frame format (92 bytes)

```text
Offset  Size  Field
------  ----  -----
     0     4  Network magic         0xE3E1F3E8
     4     2  Protocol ver          0x02BF
     6     1  Frame version         0x02 (BRC-122)
     7     1  Reserved              0x00
     8    32  Transaction ID        raw 256-bit txid (internal byte order)
    40     4  Sender ID             CRC32c of IPv6; 0 = unset
    44     4  Sequence ID           uint32 BE; random flow identifier; 0 = unset
    48     4  Shard Sequence Number uint32 BE; monotonic counter; 0 = unset
    52     4  Reserved              padding; must be 0x00000000
    56    32  Subtree ID            32-byte batch identifier; zeros = unset
    88     4  Payload length        uint32 BE; max 10 MiB
    92     *  BSV tx payload
```

`SenderID` is stamped in-place by the proxy as the CRC32c (Castagnoli) of the
TCP/UDP source IPv6 address. Collision risk is minimal on realistic BSV
networks (~1,000 mining nodes, ~12-20 core transaction processors). Gap tracking
is skipped when `SenderID` is zero (field unset) or `ShardSeqNum` is zero.

## Gap tracking (NACK / NORM-inspired)

State key: `(SenderID, groupIdx, SequenceID)`. Per-key state:
- `highestConsec`: highest sequence number without a gap below it.
- `pending`: map of missing sequence numbers to their `gapEntry`.

When seq arrives out-of-order (seq > highestConsec+1):
1. Register all missing seqs between highestConsec+1 and seq-1 as `pending`.
2. Advance `highestConsec` to seq.

When a pending seq arrives (fill from multicast repair):
1. Delete from `pending` — gap suppressed.
2. `bsl_gaps_suppressed_total` incremented.

A background sweeper fires every 100 ms:
- Entries past `deadline` (= detected + `nack-gap-ttl`) are evicted as
  `bsl_gaps_unrecovered_total`.
- Entries past `nextAttempt` with `retries < nack-max-retries` are dispatched
  to the `nackQueue`.
- `nackQueue` consumers send 56-byte NACK datagrams to retry endpoints over
  unicast UDP. Retry intervals follow exponential backoff capped at
  `nack-backoff-max`.

## Filter

Filtering is pure (no I/O) and allocation-free on the hot path:

| Config | Behaviour |
|---|---|
| `shard-include` empty | all shard indices accepted |
| `shard-include` non-empty | only listed indices accepted |
| `subtree-include` empty | all SubtreeIDs accepted |
| `subtree-include` non-empty | only listed IDs accepted |
| `subtree-exclude` | listed IDs dropped; overrides include |

## V1 frame support

`frame.Decode` accepts both v1 (44-byte header) and BRC-122 (92-byte header) frames.
v1 frames are decoded with zero-valued `ShardSeqNum`, `SubtreeID`,
`SenderID`, and `SequenceID`. Shard filtering applies to v1 frames normally;
subtree filtering has no effect (zero `SubtreeID` passes all include/exclude checks).
Gap tracking is skipped for v1 frames because `SenderID` is zero.

## Egress

A single `egress.Sender` per worker delivers frames to `egress-addr`:

| `egress-proto` | Behaviour |
|---|---|
| `udp` | `net.DialUDP` on startup; `Write` per frame |
| `tcp` | lazy connect on first frame; reconnect on write error |

`strip-header=true` sends only the raw BSV transaction bytes (frame payload);
`strip-header=false` (default) sends the complete 92-byte BRC-122 frame verbatim.

## Testing

Worker sockets bind to `[::]:listen-port`, which accepts **both multicast and
unicast** datagrams. The E2E test suite (`test/run-e2e.sh`) exploits this: it
injects frames as plain unicast UDP (`[::1]:listen-port`) using
`send-test-frames` from the proxy repo, bypassing the proxy and the multicast
fabric entirely. This makes E2E tests self-contained and reliable on any Linux
host without requiring kernel multicast loopback support on the loopback
interface.

In production the socket receives multicast frames exclusively; the unicast
receive path is an implementation property of the `[::]` bind address, not an
intended ingress path.
