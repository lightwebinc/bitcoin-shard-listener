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
	tr.Observe(0, [32]byte{}, 0, 1000, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("first frame: PendingGaps = %d, want 0", g)
	}
}

func TestObserveContiguous_NoGap(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("contiguous chain: PendingGaps = %d, want 0", g)
	}
}

func TestObserveCurSeqZero_Ignored(t *testing.T) {
	tr := newTestTracker()
	// CurSeq == 0 means the proxy has not stamped the frame; must be ignored.
	tr.Observe(0, [32]byte{}, 0, 0, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("zero CurSeq: PendingGaps = %d, want 0", g)
	}
	// Chain must still initialise correctly on the first non-zero frame.
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after zero-CurSeq: PendingGaps = %d, want 0", g)
	}
}

func TestObservePrevSeqZero_NewChain_NoGap(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	// PrevSeq == 0 signals a new chain start; must not detect a gap against
	// the previous lastCurSeq.
	tr.Observe(0, [32]byte{}, 0, 500, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("new chain (PrevSeq=0): PendingGaps = %d, want 0", g)
	}
}

func TestObserveGap_Detected(t *testing.T) {
	tr := newTestTracker()
	// Frame A: curSeq=100 — establishes chain.
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	// Frame C: prevSeq=200 (expected 100) — frame B is missing.
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("gap detected: PendingGaps = %d, want 1", g)
	}
}

func TestObserveDuplicateGap_NotDuplicated(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	// Both frames reveal the same missing range (prevSeq=200 != lastCurSeq=100).
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 400, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("duplicate gap: PendingGaps = %d, want 1", g)
	}
}

func TestObserveMultipleGroups_IndependentChains(t *testing.T) {
	tr := newTestTracker()
	// Group 0: gap between A and C.
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	// Group 1: clean contiguous delivery.
	tr.Observe(1, [32]byte{}, 0, 500, [32]byte{})
	tr.Observe(1, [32]byte{}, 500, 600, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("multi-group: PendingGaps = %d, want 1 (only group 0 has gap)", g)
	}
}

func TestObserveGap_AutoClosed_WhenMatchingCurSeqArrives(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{}) // gap, pending key=200 created
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("before auto-close: PendingGaps = %d, want 1", g)
	}

	// A frame arrives whose CurSeq == 200 (the missing frame's identity,
	// arriving as an out-of-order retransmit: prevSeq=100 < lastCurSeq=300).
	// Observe auto-closes pending[200] via the fill check; because prevSeq < lastCurSeq
	// no new phantom gap is created — PendingGaps drops to 0.
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after retransmit fill: PendingGaps = %d, want 0 (no phantom gap)", g)
	}

	// Confirm Fill is now a no-op (gap already removed).
	beforeFill := tr.PendingGaps()
	tr.Fill(0, [32]byte{}, 200)
	afterFill := tr.PendingGaps()
	if afterFill != beforeFill {
		t.Errorf("Fill(200) changed PendingGaps %d→%d: original gap should already be closed by auto-close",
			beforeFill, afterFill)
	}
}

func TestObserveOutOfOrder_NoPhantomGap(t *testing.T) {
	tr := newTestTracker()
	// Establish chain up to curSeq=300 with a gap (curSeq=200 missing).
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{}) // gap key=200
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("setup: PendingGaps = %d, want 1", g)
	}

	// Out-of-order retransmit of the missing frame arrives (prevSeq=100 < lastCurSeq=300).
	// Must close the pending gap AND must NOT create a new phantom gap.
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after retransmit: PendingGaps = %d, want 0 (phantom gap created)", g)
	}
}

func TestObserveOutOfOrder_LastCurSeqNotRegressed(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	// A frame from an unrecognised predecessor (prevSeq=20) arrives — in the
	// multi-chain design this creates an orphan gap at key=20 (a second chain
	// may genuinely start here). The main chain is unaffected.
	tr.Observe(0, [32]byte{}, 20, 50, [32]byte{})
	// A subsequent in-order frame for the MAIN chain must NOT create a gap.
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	// PendingGaps == 1: the orphan gap at key=20 (will expire via GapTTL).
	// The main chain (0→100→200→300) has no gaps.
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("after orphan + in-order: PendingGaps = %d, want 1 (orphan gap at key=20)", g)
	}
}

// ── Multi-chain / multi-sender tests ─────────────────────────────────────────

func TestObserveMultiSender_NoFalseGap(t *testing.T) {
	tr := newTestTracker()
	// Sender A: chain starting at curSeq=1000.
	tr.Observe(0, [32]byte{}, 0, 1000, [32]byte{})
	// Sender B: independent chain starting at curSeq=2000.
	tr.Observe(0, [32]byte{}, 0, 2000, [32]byte{})
	// Sender A continues.
	tr.Observe(0, [32]byte{}, 1000, 1100, [32]byte{})
	// Sender B continues — must NOT create a gap against A's tail.
	tr.Observe(0, [32]byte{}, 2000, 2100, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("multi-sender interleaving: PendingGaps = %d, want 0", g)
	}
}

func TestObserveMultiSender_GapInOneChain_OtherUnaffected(t *testing.T) {
	tr := newTestTracker()
	// Chain A: 0→100→200.
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	// Chain B: 0→500 (independent start).
	tr.Observe(0, [32]byte{}, 0, 500, [32]byte{})
	// Gap in chain A: frame with curSeq=300 is missing; chain A jumps to 400.
	tr.Observe(0, [32]byte{}, 300, 400, [32]byte{}) // gap key=300
	// Chain B continues cleanly.
	tr.Observe(0, [32]byte{}, 500, 600, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("gap in one chain: PendingGaps = %d, want 1", g)
	}
}

func TestObserveChainStart_RegistersTail(t *testing.T) {
	tr := newTestTracker()
	// Two chains start in the same group.
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 0, 200, [32]byte{})
	// Both continue without gaps.
	tr.Observe(0, [32]byte{}, 100, 150, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 250, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("two chain starts: PendingGaps = %d, want 0", g)
	}
}

func TestObserveDuplicate_Suppressed(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	// Exact duplicate of the last frame.
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("duplicate frame: PendingGaps = %d, want 0", g)
	}
}

func TestObserveGap_LeftBoundarySet_SingleSender(t *testing.T) {
	tr := newTestTracker()
	// Single sender: when a gap is detected, leftBoundary should equal the
	// tail's lastCurSeq (= last good frame's CurSeq).
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{}) // tail at 200
	tr.Observe(0, [32]byte{}, 300, 400, [32]byte{}) // gap key=300; single tail → leftBoundary=200
	// Gap should exist; test via GapLeftBoundary accessor.
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("setup: PendingGaps = %d, want 1", g)
	}
	lb := tr.GapLeftBoundary(0, [32]byte{}, 300)
	if lb != 200 {
		t.Errorf("leftBoundary = %d, want 200", lb)
	}
}

func TestObserveGap_LeftBoundaryZero_MultiSender(t *testing.T) {
	tr := newTestTracker()
	// Two tails in group → leftBoundary must be 0 (ambiguous chain).
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})   // chain A tail
	tr.Observe(0, [32]byte{}, 0, 500, [32]byte{})   // chain B tail
	tr.Observe(0, [32]byte{}, 300, 400, [32]byte{}) // gap key=300; two tails → leftBoundary=0
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("setup: PendingGaps = %d, want 1", g)
	}
	lb := tr.GapLeftBoundary(0, [32]byte{}, 300)
	if lb != 0 {
		t.Errorf("leftBoundary = %d, want 0 (multi-sender, ambiguous)", lb)
	}
}

func TestObserveChainReconnect_OrphanMerged(t *testing.T) {
	tr := newTestTracker()
	// Chain A: 0→100. Then gap at curSeq=200 (frame arriving with prevSeq=200).
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{}) // gap key=200; orphan tail at 300
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("setup: PendingGaps = %d, want 1", g)
	}
	// Retransmit of the missing frame: prev=100, cur=200.
	// This fills the gap AND extends chain A → cascadeMerge should attribute orphan.
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after retransmit: PendingGaps = %d, want 0", g)
	}
	// Chain A should now extend through 300 without gap.
	tr.Observe(0, [32]byte{}, 300, 400, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after continuation: PendingGaps = %d, want 0", g)
	}
}

func TestObserveBurstGap_CascadeMerge(t *testing.T) {
	tr := newTestTracker()
	// Chain A: 0→100. Burst: frames 200 and 300 missing. Frame 400 arrives.
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 300, 400, [32]byte{}) // gap key=300; orphan at 400
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("after first gap: PendingGaps = %d, want 1", g)
	}
	// Retransmit of frame 300 (prev=200, cur=300):
	// Fills gap-300, but creates gap-200 (prev=200 still not in tails).
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("after retx-300: PendingGaps = %d, want 1 (gap-200 opened)", g)
	}
	// Retransmit of frame 200 (prev=100, cur=200):
	// Fills gap-200, extends chain A to 200, cascade merges orphans at 300 and 400.
	tr.Observe(0, [32]byte{}, 100, 200, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after retx-200: PendingGaps = %d, want 0", g)
	}
	// Chain continues cleanly.
	tr.Observe(0, [32]byte{}, 400, 500, [32]byte{})
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after continuation: PendingGaps = %d, want 0", g)
	}
}

func TestSweepOnce_StaleTailEvicted(t *testing.T) {
	cfg := nack.TrackerConfig{
		JitterMax:  0,
		BackoffMax: 5 * time.Second,
		MaxRetries: 3,
		GapTTL:     10 * time.Second,
		TailTTL:    50 * time.Millisecond, // very short for testing
	}
	tr := nack.New(cfg, nil, nil, nil, nil)
	// Register a tail.
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	if tr.ActiveTails() == 0 {
		t.Fatal("tail should be registered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Start(ctx)
	// Wait for TailTTL + sweep interval to pass.
	time.Sleep(300 * time.Millisecond)
	if n := tr.ActiveTails(); n != 0 {
		t.Errorf("stale tail: ActiveTails = %d, want 0", n)
	}
}

// ── Fill ─────────────────────────────────────────────────────────────────────

func TestFill_ClosesGap(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{}) // gap, key=200
	if g := tr.PendingGaps(); g != 1 {
		t.Fatalf("before Fill: PendingGaps = %d, want 1", g)
	}
	tr.Fill(0, [32]byte{}, 200)
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("after Fill: PendingGaps = %d, want 0", g)
	}
}

func TestFill_Nonexistent_NoPanic(t *testing.T) {
	tr := newTestTracker()
	// Fill on an entry that does not exist must be a no-op.
	tr.Fill(0, [32]byte{}, 9999)
	if g := tr.PendingGaps(); g != 0 {
		t.Errorf("Fill nonexistent: PendingGaps = %d, want 0", g)
	}
}

func TestFill_ZeroCurSeq_Ignored(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
	// Fill with curSeq=0 must be ignored (0 is the "unset" sentinel).
	tr.Fill(0, [32]byte{}, 0)
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("Fill(0): PendingGaps = %d, want 1 (gap not removed)", g)
	}
}

func TestFill_MultipleGroups_OnlyClosesCorrectGroup(t *testing.T) {
	tr := newTestTracker()
	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{}) // gap in group 0, key=200
	tr.Observe(1, [32]byte{}, 0, 500, [32]byte{})
	tr.Observe(1, [32]byte{}, 700, 800, [32]byte{}) // gap in group 1, key=700
	if g := tr.PendingGaps(); g != 2 {
		t.Fatalf("before fill: PendingGaps = %d, want 2", g)
	}
	tr.Fill(0, [32]byte{}, 200) // close only group 0 gap
	if g := tr.PendingGaps(); g != 1 {
		t.Errorf("after Fill(group=0): PendingGaps = %d, want 1", g)
	}
	tr.Fill(1, [32]byte{}, 700) // close group 1 gap
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

	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{}) // gap, key=200
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

	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{}) // gap
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

	tr.Observe(0, [32]byte{}, 0, 100, [32]byte{})
	tr.Observe(0, [32]byte{}, 200, 300, [32]byte{})
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

// ── Subtree isolation ─────────────────────────────────────────────────────────

func TestObserve_SubtreeIsolation(t *testing.T) {
tr := newTestTracker()
var subA, subB [32]byte
subA[0] = 0xAA
subB[0] = 0xBB

// Subtree A frames 100 → 200 → 300 (contiguous chain).
tr.Observe(0, subA, 0, 100, [32]byte{})
tr.Observe(0, subA, 100, 200, [32]byte{})
tr.Observe(0, subA, 200, 300, [32]byte{})

// Subtree B frames with completely different sequence values, ALSO contiguous.
tr.Observe(0, subB, 0, 7000, [32]byte{})
tr.Observe(0, subB, 7000, 8000, [32]byte{})

// Neither subtree has gaps. The interleaved arrival of B frames must not
// produce false gaps in A's chain (they live in different namespaces).
if g := tr.PendingGaps(); g != 0 {
t.Errorf("interleaved subtrees produced %d gaps, want 0", g)
}
}

func TestObserve_SubtreeGapDoesNotAffectOtherSubtree(t *testing.T) {
tr := newTestTracker()
var subA, subB [32]byte
subA[0] = 0xAA
subB[0] = 0xBB

// Subtree A: introduce a gap (PrevSeq=999 has no matching tail).
tr.Observe(0, subA, 999, 1000, [32]byte{})
// Subtree B: clean chain.
tr.Observe(0, subB, 0, 5000, [32]byte{})
tr.Observe(0, subB, 5000, 6000, [32]byte{})

// Exactly one gap: in subtree A only.
if g := tr.PendingGaps(); g != 1 {
t.Errorf("PendingGaps = %d, want 1 (only in subtree A)", g)
}
}
