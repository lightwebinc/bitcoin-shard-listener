package nack

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/lightwebinc/bitcoin-shard-listener/discovery"
	"github.com/lightwebinc/bitcoin-shard-listener/metrics"
)

// TrackerConfig holds tuning parameters for the gap tracker.
type TrackerConfig struct {
	JitterMax  time.Duration // Max random hold-off before first NACK (NORM suppression window)
	BackoffMax time.Duration // Cap on exponential backoff between retries
	MaxRetries int           // Max NACK attempts before declaring unrecoverable
	GapTTL     time.Duration // Max lifetime of a gap entry (~Bitcoin block interval)
}

// gapEntry holds retry state for a single gap in the hash chain.
//
// A gap is the absence of one or more frames between the last successfully
// received frame (whose CurSeq == prevSeq) and the frame that revealed the
// gap (whose PrevSeq == curSeq).
type gapEntry struct {
	prevSeq     uint64 // CurSeq of last good frame — used for forward NACK (LookupByPrevSeq)
	curSeq      uint64 // PrevSeq of next received frame — used for backward NACK (LookupByCurSeq)
	groupIdx    uint32
	retries     int
	nextAttempt time.Time
	deadline    time.Time // absolute eviction deadline
	endpointIdx int       // round-robin index into registry snapshot
}

// groupState holds per-group tracking state.
type groupState struct {
	lastCurSeq uint64               // CurSeq of the most recently observed frame
	pending    map[uint64]*gapEntry // keyed by gapEntry.curSeq
}

// Tracker is the gap state machine. Construct with [New] and call [Start] to
// begin background GC and NACK dispatch.
type Tracker struct {
	cfg           TrackerConfig
	iface         *net.Interface
	rec           *metrics.Recorder
	log           *slog.Logger
	registry      *discovery.Registry
	respTimeout   time.Duration // deadline for ACK/MISS response (default 300ms)
	maxConcurrent int           // semaphore bound for concurrent sendNACK goroutines

	mu     sync.Mutex
	states map[uint32]*groupState // keyed by groupIdx

	// nackQueue receives gap entries ready for NACK dispatch.
	nackQueue chan *gapEntry

	// sem bounds concurrent sendNACK goroutines.
	sem chan struct{}
}

// New constructs a Tracker. retryEndpoints is the static seed list.
// registry is the dynamic endpoint registry from beacon discovery (may be nil
// to use only static seeds). iface is reserved for future multicast NACK send.
func New(cfg TrackerConfig, retryEndpoints []string, iface *net.Interface, rec *metrics.Recorder, registry *discovery.Registry) *Tracker {
	const defaultMaxConcurrent = 64
	const defaultRespTimeout = 300 * time.Millisecond

	if registry == nil {
		registry = discovery.NewRegistry()
	}
	if len(retryEndpoints) > 0 {
		registry.Seed(retryEndpoints)
	}

	return &Tracker{
		cfg:           cfg,
		iface:         iface,
		rec:           rec,
		log:           slog.Default().With("component", "nack"),
		registry:      registry,
		respTimeout:   defaultRespTimeout,
		maxConcurrent: defaultMaxConcurrent,
		states:        make(map[uint32]*groupState),
		nackQueue:     make(chan *gapEntry, 4096),
		sem:           make(chan struct{}, defaultMaxConcurrent),
	}
}

// Observe is called by the listener worker on every BRC-124 v2 frame.
// It detects chain breaks (PrevSeq mismatch) and schedules parallel NACKs.
// curSeq == 0 means the proxy has not yet stamped the frame; it is ignored.
func (t *Tracker) Observe(groupIdx uint32, prevSeq, curSeq uint64, txid [32]byte) {
	if curSeq == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	st, ok := t.states[groupIdx]
	if !ok {
		// First frame seen for this group — initialise chain.
		t.states[groupIdx] = &groupState{
			lastCurSeq: curSeq,
			pending:    make(map[uint64]*gapEntry),
		}
		return
	}

	// Fill: if this frame's CurSeq matches a pending gap key, close it.
	if _, found := st.pending[curSeq]; found {
		delete(st.pending, curSeq)
		if t.rec != nil {
			t.rec.GapSuppressed()
		}
	}

	// Gap detection: prevSeq should equal lastCurSeq for a contiguous chain.
	// prevSeq == 0 signals the start of a new chain from the same group.
	if prevSeq != 0 && prevSeq != st.lastCurSeq {
		// One or more frames are missing. The gap is bounded by:
		//   prevSeq  = CurSeq of last good frame    (forward lookup key)
		//   curSeq of gap = incoming f.PrevSeq       (backward lookup key)
		gapKey := prevSeq // key by CurSeq of the last-in-gap frame
		if _, already := st.pending[gapKey]; !already {
			now := time.Now()
			jitter := time.Duration(rand.Int64N(int64(t.cfg.JitterMax) + 1))
			e := &gapEntry{
				prevSeq:     st.lastCurSeq,
				curSeq:      prevSeq,
				groupIdx:    groupIdx,
				nextAttempt: now.Add(jitter),
				deadline:    now.Add(t.cfg.GapTTL),
			}
			st.pending[gapKey] = e
			if t.rec != nil {
				t.rec.GapDetected()
			}
		}
	}

	st.lastCurSeq = curSeq
}

// Fill cancels a pending gap when a retransmitted frame arrives via multicast
// with the given (groupIdx, curSeq).
func (t *Tracker) Fill(groupIdx uint32, curSeq uint64) {
	if curSeq == 0 {
		return
	}
	t.mu.Lock()
	if st, ok := t.states[groupIdx]; ok {
		if _, found := st.pending[curSeq]; found {
			delete(st.pending, curSeq)
			if t.rec != nil {
				t.rec.GapSuppressed()
			}
		}
	}
	t.mu.Unlock()
}

// Start launches the background NACK dispatch loop and GC sweeper.
// It returns when ctx is cancelled.
func (t *Tracker) Start(ctx context.Context) {
	go t.dispatchLoop(ctx)
	go t.gcLoop(ctx)
}

// gcLoop scans pending gaps on a regular interval, evicts expired entries,
// and enqueues entries whose nextAttempt has passed.
func (t *Tracker) gcLoop(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			t.sweepOnce(now)
		}
	}
}

func (t *Tracker) sweepOnce(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for groupIdx, st := range t.states {
		for key, e := range st.pending {
			if now.After(e.deadline) {
				delete(st.pending, key)
				if t.rec != nil {
					t.rec.GapUnrecovered()
				}
				t.log.Debug("gap evicted (TTL)",
					"group", groupIdx,
					"cur_seq", e.curSeq,
				)
				continue
			}
			if e.retries >= t.cfg.MaxRetries {
				delete(st.pending, key)
				if t.rec != nil {
					t.rec.GapUnrecovered()
				}
				t.log.Debug("gap evicted (retries)",
					"group", groupIdx,
					"cur_seq", e.curSeq,
				)
				continue
			}
			if now.After(e.nextAttempt) {
				entry := *e // shallow copy to avoid races
				select {
				case t.nackQueue <- &entry:
				default:
					// Queue full — skip this cycle; will retry next tick.
				}
			}
		}
		if len(st.pending) == 0 {
			delete(t.states, groupIdx)
		}
	}
}

// dispatchLoop reads from nackQueue and launches bounded sendNACK goroutines.
func (t *Tracker) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-t.nackQueue:
			select {
			case t.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-t.sem }()
				t.sendNACK(e)
			}()
		}
	}
}

// sendNACK dispatches two NACKs in parallel (forward by PrevSeq, backward by
// CurSeq) to the same retry endpoint, then waits for the first ACK/MISS
// response. On ACK the gap is cancelled; on MISS or timeout, retry state
// advances with exponential backoff.
func (t *Tracker) sendNACK(e *gapEntry) {
	snap := t.registry.Snapshot()
	if len(snap) == 0 {
		return
	}

	idx := e.endpointIdx % len(snap)
	endpoint := snap[idx]

	addr, err := net.ResolveUDPAddr("udp", endpoint.Addr)
	if err != nil {
		t.log.Warn("NACK: cannot resolve retry endpoint", "endpoint", endpoint.Addr, "err", err)
		t.advanceEndpoint(e, false)
		return
	}

	// Ephemeral unconnected socket: accept ACK/MISS from any source address.
	// (Connected sockets filter by exact source; SLAAC addresses on the retry
	// endpoint would cause silent discard of ACK responses.)
	conn, err := net.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.log.Warn("NACK: listen failed", "endpoint", endpoint.Addr, "err", err)
		t.advanceEndpoint(e, false)
		return
	}
	defer func() { _ = conn.Close() }()

	// Forward NACK: find frame whose PrevSeq == e.prevSeq.
	var fwdBuf [NACKSize]byte
	Encode(&NACK{MsgType: MsgTypeNACK, LookupType: LookupByPrevSeq, LookupSeq: e.prevSeq}, fwdBuf[:])
	_, _ = conn.WriteTo(fwdBuf[:], addr)

	// Backward NACK: find frame whose CurSeq == e.curSeq.
	var bwdBuf [NACKSize]byte
	Encode(&NACK{MsgType: MsgTypeNACK, LookupType: LookupByCurSeq, LookupSeq: e.curSeq}, bwdBuf[:])
	_, _ = conn.WriteTo(bwdBuf[:], addr)

	if t.rec != nil {
		t.rec.NACKDispatched()
	}
	t.log.Debug("NACK dispatched",
		"endpoint", endpoint.Addr,
		"tier", endpoint.Tier,
		"prev_seq", e.prevSeq,
		"cur_seq", e.curSeq,
		"retry", e.retries+1,
	)

	// Wait for any response (ACK or MISS). Both NACKs share the same socket,
	// so the first response received determines the action.
	_ = conn.SetReadDeadline(time.Now().Add(t.respTimeout))
	var respBuf [ResponseSize + 16]byte
	nr, _, err := conn.ReadFrom(respBuf[:])
	if err != nil {
		t.advanceEndpoint(e, false)
		return
	}

	resp, err := DecodeResponse(respBuf[:nr])
	if err != nil {
		t.log.Debug("NACK: invalid response", "endpoint", endpoint.Addr, "err", err)
		t.advanceEndpoint(e, false)
		return
	}

	switch resp.MsgType {
	case MsgTypeACK:
		t.cancelGap(e)
		t.log.Debug("NACK: ACK received, gap cancelled",
			"endpoint", endpoint.Addr,
			"cur_seq", e.curSeq,
			"flags", resp.Flags,
		)
	case MsgTypeMISS:
		t.log.Debug("NACK: MISS received, advancing endpoint",
			"endpoint", endpoint.Addr,
			"cur_seq", e.curSeq,
		)
		t.advanceEndpoint(e, true)
	}
}

// advanceEndpoint updates retry state after a NACK attempt.
// immediate=true (MISS): retry now with next endpoint.
// immediate=false (timeout/error): exponential backoff.
func (t *Tracker) advanceEndpoint(e *gapEntry, immediate bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, ok := t.states[e.groupIdx]
	if !ok {
		return
	}
	entry, ok := st.pending[e.curSeq]
	if !ok {
		return
	}

	entry.retries++
	entry.endpointIdx++

	if immediate {
		entry.nextAttempt = time.Now()
	} else {
		backoff := time.Duration(1<<uint(entry.retries)) * 500 * time.Millisecond
		if backoff > t.cfg.BackoffMax {
			backoff = t.cfg.BackoffMax
		}
		entry.nextAttempt = time.Now().Add(backoff)
	}
}

// PendingGaps returns the total number of unresolved gap entries across all
// groups. Useful for testing and diagnostics.
func (t *Tracker) PendingGaps() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for _, st := range t.states {
		total += len(st.pending)
	}
	return total
}

// cancelGap removes a gap entry after receiving an ACK.
func (t *Tracker) cancelGap(e *gapEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if st, ok := t.states[e.groupIdx]; ok {
		if _, found := st.pending[e.curSeq]; found {
			delete(st.pending, e.curSeq)
			if t.rec != nil {
				t.rec.GapSuppressed()
			}
		}
	}
}
