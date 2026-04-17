#!/bin/sh

SHARD_BITS=${SHARD_BITS:-2}
UDP_LISTEN_PORT=${UDP_LISTEN_PORT:-9000}
MC_EGRESS_PORT=${MC_EGRESS_PORT:-9001}
RECV_TIMEOUT=${RECV_TIMEOUT:-15s}

LOOPBACK="lo"
NUM_GROUPS=$(( 1 << SHARD_BITS ))

echo "=== E2E test suite: shard_bits=$SHARD_BITS iface=$LOOPBACK num_groups=$NUM_GROUPS ==="

ip link set "$LOOPBACK" multicast on 2>/dev/null || true
ip -6 route add ff00::/8 dev "$LOOPBACK" table local 2>/dev/null || true

PASSED=0
FAILED=0

# ── Test 1: basic delivery ────────────────────────────────────────────────────
# Send one frame per shard group through the full pipeline:
#   send-test-frames → proxy (multicast) → listener → sink
# Expect the sink to receive exactly NUM_GROUPS frames.
echo ""
echo "--- Test 1: basic delivery (expect $NUM_GROUPS frames end-to-end) ---"

bitcoin-shard-proxy \
    -iface "$LOOPBACK" -scope link -shard-bits "$SHARD_BITS" \
    -udp-listen-port "$UDP_LISTEN_PORT" -egress-port "$MC_EGRESS_PORT" \
    -metrics-addr ":9100" -debug &
P1=$!

bitcoin-shard-listener \
    -iface "$LOOPBACK" -scope link -shard-bits "$SHARD_BITS" \
    -listen-port "$MC_EGRESS_PORT" \
    -egress-addr "127.0.0.1:9102" -egress-proto udp \
    -metrics-addr ":9200" -workers 1 -debug &
L1=$!

sink-test-frames -port 9102 -count "$NUM_GROUPS" -timeout "$RECV_TIMEOUT" &
S1=$!

sleep 1

send-test-frames \
    -addr "[::1]:$UDP_LISTEN_PORT" \
    -shard-bits "$SHARD_BITS" -spread -count 1 -interval 50

wait "$S1" && S1_EXIT=0 || S1_EXIT=$?

sleep 1
FORWARDED1=$(curl -sf "http://127.0.0.1:9200/metrics" \
    | grep '^bsl_frames_forwarded_total{' \
    | awk '{sum += $2} END {print int(sum)}' 2>/dev/null) || FORWARDED1=0

kill "$P1" "$L1" 2>/dev/null || true
wait "$P1" "$L1" 2>/dev/null || true

if [ "$S1_EXIT" -eq 0 ] && [ "${FORWARDED1:-0}" -ge "$NUM_GROUPS" ]; then
    echo "=== PASS: basic delivery (forwarded=${FORWARDED1:-0}) ==="
    PASSED=$(( PASSED + 1 ))
else
    echo "=== FAIL: basic delivery (sink_exit=$S1_EXIT forwarded=${FORWARDED1:-0}) ==="
    FAILED=$(( FAILED + 1 ))
fi

# ── Test 2: shard filter ──────────────────────────────────────────────────────
# Listener subscribes only to group 0. Send one frame per group; only the
# group-0 frame should reach the sink (expect 1 frame).
echo ""
echo "--- Test 2: shard filter (shard-include=0, expect 1 frame) ---"

bitcoin-shard-proxy \
    -iface "$LOOPBACK" -scope link -shard-bits "$SHARD_BITS" \
    -udp-listen-port "$UDP_LISTEN_PORT" -egress-port "$MC_EGRESS_PORT" \
    -metrics-addr ":9101" -debug &
P2=$!

bitcoin-shard-listener \
    -iface "$LOOPBACK" -scope link -shard-bits "$SHARD_BITS" \
    -listen-port "$MC_EGRESS_PORT" \
    -shard-include 0 \
    -egress-addr "127.0.0.1:9103" -egress-proto udp \
    -metrics-addr ":9201" -workers 1 -debug &
L2=$!

sink-test-frames -port 9103 -count 1 -timeout "$RECV_TIMEOUT" &
S2=$!

sleep 1

send-test-frames \
    -addr "[::1]:$UDP_LISTEN_PORT" \
    -shard-bits "$SHARD_BITS" -spread -count 1 -interval 50

wait "$S2" && S2_EXIT=0 || S2_EXIT=$?

kill "$P2" "$L2" 2>/dev/null || true
wait "$P2" "$L2" 2>/dev/null || true

if [ "$S2_EXIT" -eq 0 ]; then
    echo "=== PASS: shard filter ==="
    PASSED=$(( PASSED + 1 ))
else
    echo "=== FAIL: shard filter (sink_exit=$S2_EXIT) ==="
    FAILED=$(( FAILED + 1 ))
fi

# ── Test 3: strip-header ──────────────────────────────────────────────────────
# Listener configured with -strip-header; the sink receives raw BSV payload
# bytes (no 100-byte frame header). Use -raw mode in the sink to count
# datagrams without attempting frame decode.
echo ""
echo "--- Test 3: strip-header (expect $NUM_GROUPS raw datagrams) ---"

bitcoin-shard-proxy \
    -iface "$LOOPBACK" -scope link -shard-bits "$SHARD_BITS" \
    -udp-listen-port "$UDP_LISTEN_PORT" -egress-port "$MC_EGRESS_PORT" \
    -metrics-addr ":9102" -debug &
P3=$!

bitcoin-shard-listener \
    -iface "$LOOPBACK" -scope link -shard-bits "$SHARD_BITS" \
    -listen-port "$MC_EGRESS_PORT" \
    -strip-header \
    -egress-addr "127.0.0.1:9104" -egress-proto udp \
    -metrics-addr ":9202" -workers 1 -debug &
L3=$!

sink-test-frames -port 9104 -count "$NUM_GROUPS" -raw -timeout "$RECV_TIMEOUT" &
S3=$!

sleep 1

send-test-frames \
    -addr "[::1]:$UDP_LISTEN_PORT" \
    -shard-bits "$SHARD_BITS" -spread -count 1 -interval 50

wait "$S3" && S3_EXIT=0 || S3_EXIT=$?

kill "$P3" "$L3" 2>/dev/null || true
wait "$P3" "$L3" 2>/dev/null || true

if [ "$S3_EXIT" -eq 0 ]; then
    echo "=== PASS: strip-header ==="
    PASSED=$(( PASSED + 1 ))
else
    echo "=== FAIL: strip-header (sink_exit=$S3_EXIT) ==="
    FAILED=$(( FAILED + 1 ))
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "=== E2E results: $PASSED passed, $FAILED failed ==="
[ "$FAILED" -eq 0 ]
