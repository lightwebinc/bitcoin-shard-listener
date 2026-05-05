package nack_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

func newTestTracker() *nack.Tracker {
	cfg := nack.TrackerConfig{
		JitterMax:  0,
		BackoffMax: 5 * time.Second,
		MaxRetries: 3,
		GapTTL:     10 * time.Second,
	}
	return nack.New(cfg, nil, nil, nil, nil)
}

// ── Observe ───────────────────────────────────────────────────────────────────

func TestObserveFirstFrame_NoGap(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 1000, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("first frame: PendingGaps = %d, want 0", g)
	}
}

func TestObserveContiguous_NoGap(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 100, 200, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("contiguous chain: PendingGaps = %d, want 0", g)
	}
}

func TestObserveCurSeqZero_Ignored(t *testing.T) {
	tr := newTestTracker()
	// CurSeq == 0 means the proxy has not stamped the frame; must be ignored.
	tr.Observe(0, 0, 0, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("zero CurSeq: PendingGaps = %d, want 0", g)
	}
	// Chain must still initialise correctly on the first non-zero frame.
	tr.Observe(0, 0, 100, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after zero-CurSeq: PendingGaps = %d, want 0", g)
	}
}

func TestObservePrevSeqZero_NewChain_NoGap(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 100, 200, [32]byte{})
	// PrevSeq == 0 signals a new chain start; must not detect a gap against
	// the previous lastCurSeq.
	tr.Observe(0, 0, 500, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("new chain (PrevSeq=0): PendingGaps = %d, want 0", g)
	}
}

func TestObserveGap_Detected(t *testing.T) {
	tr := newTestTracker()
	// Frame A: curSeq=100 — establishes chain.
	tr.Observe(0, 0, 100, [32]byte{})
	// Frame C: prevSeq=200 (expected 100) — frame B is missing.
	tr.Observe(0, 200, 300, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("gap detected: PendingGaps = %d, want 1", g)
	}
}

func TestObserveDuplicateGap_NotDuplicated(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 100, [32]byte{})
	// Both frames reveal the same missing range (prevSeq=200 != lastCurSeq=100).
	tr.Observe(0, 200, 300, [32]byte{})
	tr.Observe(0, 200, 400, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("duplicate gap: PendingGaps = %d, want 1", g)
	}
}

func TestObserveMultipleGroups_IndependentChains(t *testing.T) {
	tr := newTestTracker()
	// Group 0: gap between A and C.
	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{})
	// Group 1: clean contiguous delivery.
	tr.Observe(1, 0, 500, [32]byte{})
	tr.Observe(1, 500, 600, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("multi-group: PendingGaps = %d, want 1 (only group 0 has gap)", g)
	}
}

func TestObserveGap_AutoClosed_WhenMatchingCurSeqArrives(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{}) // gap, pending key=200 created
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("before auto-close: PendingGaps = %d, want 1", g)
	}

	// A frame arrives whose CurSeq == 200 (the missing frame's identity).
	// Observe auto-closes pending[200] before gap-detection runs.
	// However, the out-of-order arrival (prevSeq=100 != lastCurSeq=300) also
	// creates a new spurious entry, so total PendingGaps remains 1.
	tr.Observe(0, 100, 200, [32]byte{})

	// Verify the original gap (key=200) was actually removed from pending by
	// confirming that a subsequent Fill(0, 200) is a no-op.
	beforeFill := tr.PendingGaps()
	tr.Fill(0, 200)
	afterFill := tr.PendingGaps()
	if afterFill != beforeFill {
		t.Errorf("Fill(200) changed PendingGaps %d→%d: original gap should already be closed by auto-close",
			beforeFill, afterFill)
	}
}

// ── Fill ─────────────────────────────────────────────────────────────────────

func TestFill_ClosesGap(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{}) // gap, key=200
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("before Fill: PendingGaps = %d, want 1", g)
	}
	tr.Fill(0, 200)
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after Fill: PendingGaps = %d, want 0", g)
	}
}

func TestFill_Nonexistent_NoPanic(t *testing.T) {
	tr := newTestTracker()
	// Fill on an entry that does not exist must be a no-op.
	tr.Fill(0, 9999)
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("Fill nonexistent: PendingGaps = %d, want 0", g)
	}
}

func TestFill_ZeroCurSeq_Ignored(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{})
	// Fill with curSeq=0 must be ignored (0 is the "unset" sentinel).
	tr.Fill(0, 0)
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("Fill(0): PendingGaps = %d, want 1 (gap not removed)", g)
	}
}

func TestFill_MultipleGroups_OnlyClosesCorrectGroup(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{}) // gap in group 0, key=200
	tr.Observe(1, 0, 500, [32]byte{})
	tr.Observe(1, 700, 800, [32]byte{}) // gap in group 1, key=700
	if g := tr.PendingGaps(); g != 2 {
		t.Fatalf("before fill: PendingGaps = %d, want 2", g)
	}
	tr.Fill(0, 200) // close only group 0 gap
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("after Fill(group=0): PendingGaps = %d, want 1", g)
	}
	tr.Fill(1, 700) // close group 1 gap
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after Fill(group=1): PendingGaps = %d, want 0", g)
	}
}

// ── sendNACK integration tests ──────────────────────────────────────────────
//
// These tests start the full Tracker (gcLoop + dispatchLoop) with a mock UDP
// endpoint and verify that ACK/MISS/timeout are handled correctly.

// pollGaps waits up to timeout for tr.PendingGaps() to equal want.
func pollGaps(tr *nack.Tracker, want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if tr.PendingGaps() == want {
			return want
		}
		time.Sleep(25 * time.Millisecond)
	}
	return tr.PendingGaps()
}

func TestSendNACK_ACK_CancelsGap(t *testing.T) {
	// Start a mock UDP endpoint that responds with ACK to any NACK.
	mockConn, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Skipf("UDP loopback unavailable: %v", err)
	}
	defer func() { _ = mockConn.Close() }()

	go func() {
		buf := make([]byte, 256)
		for {
			_, src, err := mockConn.ReadFrom(buf)
			if err != nil {
				return
			}
			var resp [nack.ResponseSize]byte
			nack.EncodeResponse(&nack.Response{
				MsgType: nack.MsgTypeACK,
				Flags:   0x01,
				CurSeq:  200,
			}, resp[:])
			_, _ = mockConn.WriteTo(resp[:], src)
		}
	}()

	cfg := nack.TrackerConfig{
		JitterMax:  0,
		BackoffMax: 1 * time.Second,
		MaxRetries: 5,
		GapTTL:     10 * time.Second,
	}
	tr := nack.New(cfg, []string{mockConn.LocalAddr().String()}, nil, nil, nil)

	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{}) // gap, key=200
	if tr.PendingGaps() != 1 {
		t.Fatalf("setup: PendingGaps = %d, want 1", tr.PendingGaps())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Start(ctx)

	// Poll for the gap to be cancelled by ACK.
	got := pollGaps(tr, 0, 3*time.Second)
	if got != 0 {
		t.Errorf("after ACK: PendingGaps = %d, want 0", got)
	}
}

func TestSendNACK_MISS_AdvancesRetry(t *testing.T) {
	// Mock endpoint that always responds with MISS.
	mockConn, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Skipf("UDP loopback unavailable: %v", err)
	}
	defer func() { _ = mockConn.Close() }()

	go func() {
		buf := make([]byte, 256)
		for {
			_, src, err := mockConn.ReadFrom(buf)
			if err != nil {
				return
			}
			var resp [nack.ResponseSize]byte
			nack.EncodeResponse(&nack.Response{
				MsgType: nack.MsgTypeMISS,
				CurSeq:  0,
			}, resp[:])
			_, _ = mockConn.WriteTo(resp[:], src)
		}
	}()

	cfg := nack.TrackerConfig{
		JitterMax:  0,
		BackoffMax: 1 * time.Second,
		MaxRetries: 2,
		GapTTL:     10 * time.Second,
	}
	tr := nack.New(cfg, []string{mockConn.LocalAddr().String()}, nil, nil, nil)

	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{}) // gap
	if tr.PendingGaps() != 1 {
		t.Fatalf("setup: PendingGaps = %d, want 1", tr.PendingGaps())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Start(ctx)

	// With MaxRetries=2 the gap should be evicted after retries are exhausted.
	got := pollGaps(tr, 0, 5*time.Second)
	if got != 0 {
		t.Errorf("after MISS exhaustion: PendingGaps = %d, want 0", got)
	}
}

func TestSendNACK_Timeout_BacksOff(t *testing.T) {
	// Mock endpoint that never responds — sendNACK will hit respTimeout.
	mockConn, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Skipf("UDP loopback unavailable: %v", err)
	}
	// Don't read from mockConn — let NACKs timeout.
	defer func() { _ = mockConn.Close() }()

	cfg := nack.TrackerConfig{
		JitterMax:  0,
		BackoffMax: 500 * time.Millisecond,
		MaxRetries: 2,
		GapTTL:     10 * time.Second,
	}
	tr := nack.New(cfg, []string{mockConn.LocalAddr().String()}, nil, nil, nil)

	tr.Observe(0, 0, 100, [32]byte{})
	tr.Observe(0, 200, 300, [32]byte{})
	if tr.PendingGaps() != 1 {
		t.Fatalf("setup: PendingGaps = %d, want 1", tr.PendingGaps())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Start(ctx)

	// Gap survives the first cycle (timeout → backoff) but is eventually
	// evicted when MaxRetries is exceeded.
	got := pollGaps(tr, 0, 8*time.Second)
	if got != 0 {
		t.Errorf("after timeout exhaustion: PendingGaps = %d, want 0", got)
	}
}
