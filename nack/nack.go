package nack

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/lightwebinc/bitcoin-shard-listener/metrics"
)

// TrackerConfig holds tuning parameters for the gap tracker.
type TrackerConfig struct {
	JitterMax  time.Duration // Max random hold-off before first NACK (NORM suppression window)
	BackoffMax time.Duration // Cap on exponential backoff between retries
	MaxRetries int           // Max NACK attempts before declaring unrecoverable
	GapTTL     time.Duration // Max lifetime of a gap entry (~Bitcoin block interval)
}

// senderGroupKey is the composite state key per (senderID, groupIdx, sequenceID).
type senderGroupKey struct {
	senderID   uint32
	groupIdx   uint32
	sequenceID uint32
}

// gapEntry holds retry state for a single missing sequence number.
type gapEntry struct {
	txid        [32]byte
	seq         uint32
	senderID    uint32
	groupIdx    uint32
	sequenceID  uint32
	retries     int
	nextAttempt time.Time
	deadline    time.Time // absolute eviction deadline (= detectedAt + GapTTL)
	endpointIdx int       // round-robin index into retry endpoints
}

// senderState holds per-(senderID, groupIdx) tracking state.
type senderState struct {
	highestConsec uint32               // highest consecutive seq without gaps below
	pending       map[uint32]*gapEntry // missing seqs awaiting NACK or fill
}

// Tracker is the gap state machine. Construct with [New] and call [Start] to
// begin background GC and NACK dispatch.
type Tracker struct {
	cfg            TrackerConfig
	retryEndpoints []string
	iface          *net.Interface
	rec            *metrics.Recorder
	log            *slog.Logger

	mu     sync.Mutex
	states map[senderGroupKey]*senderState

	// nackQueue receives gap entries ready for NACK dispatch.
	nackQueue chan *gapEntry
}

// New constructs a Tracker. retryEndpoints is the list of host:port retry nodes.
// iface is used for potential future multicast NACK send (currently unicast UDP).
func New(cfg TrackerConfig, retryEndpoints []string, iface *net.Interface, rec *metrics.Recorder) *Tracker {
	return &Tracker{
		cfg:            cfg,
		retryEndpoints: retryEndpoints,
		iface:          iface,
		rec:            rec,
		log:            slog.Default().With("component", "nack"),
		states:         make(map[senderGroupKey]*senderState),
		nackQueue:      make(chan *gapEntry, 4096),
	}
}

// Observe is called by the listener worker when a BRC-123 frame with non-zero
// SenderID and non-zero SeqNum arrives. It detects gaps and schedules NACKs.
func (t *Tracker) Observe(senderID uint32, groupIdx uint32, seq uint32, sequenceID uint32, txid [32]byte) {
	if seq == 0 {
		return
	}
	if senderID == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	key := senderGroupKey{senderID: senderID, groupIdx: groupIdx, sequenceID: sequenceID}
	st, ok := t.states[key]
	if !ok {
		st = &senderState{
			highestConsec: seq,
			pending:       make(map[uint32]*gapEntry),
		}
		t.states[key] = st
		return
	}

	// If this seq fills a pending gap, cancel it.
	if e, found := st.pending[seq]; found {
		delete(st.pending, seq)
		_ = e
		if t.rec != nil {
			t.rec.GapSuppressed()
		}
	}

	// Advance consecutive high-water mark.
	if seq == st.highestConsec+1 {
		st.highestConsec = seq
	} else if seq > st.highestConsec+1 {
		// Gap: register all missing seqs between highestConsec+1 and seq-1.
		now := time.Now()
		for missing := st.highestConsec + 1; missing < seq; missing++ {
			if _, already := st.pending[missing]; already {
				continue
			}
			jitter := time.Duration(rand.Int64N(int64(t.cfg.JitterMax) + 1))
			e := &gapEntry{
				txid:        txid, // best effort — actual TxID of missing frame unknown
				seq:         missing,
				senderID:    senderID,
				groupIdx:    groupIdx,
				sequenceID:  sequenceID,
				nextAttempt: now.Add(jitter),
				deadline:    now.Add(t.cfg.GapTTL),
			}
			st.pending[missing] = e
			if t.rec != nil {
				t.rec.GapDetected()
			}
		}
		st.highestConsec = seq
	}
}

// Fill is called when a previously-missing frame arrives on the multicast group
// (repair received). It cancels any pending NACK for that (senderID, groupIdx, sequenceID, seq).
func (t *Tracker) Fill(senderID uint32, groupIdx uint32, sequenceID uint32, seq uint32) {
	if seq == 0 {
		return
	}
	t.mu.Lock()
	key := senderGroupKey{senderID: senderID, groupIdx: groupIdx, sequenceID: sequenceID}
	st, ok := t.states[key]
	if ok {
		if _, found := st.pending[seq]; found {
			delete(st.pending, seq)
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

	for key, st := range t.states {
		for seq, e := range st.pending {
			if now.After(e.deadline) {
				delete(st.pending, seq)
				if t.rec != nil {
					t.rec.GapUnrecovered()
				}
				t.log.Debug("gap evicted (TTL)",
					"sender", e.senderID,
					"group", key.groupIdx,
					"seq", seq,
				)
				continue
			}
			if e.retries >= t.cfg.MaxRetries {
				delete(st.pending, seq)
				if t.rec != nil {
					t.rec.GapUnrecovered()
				}
				t.log.Debug("gap evicted (retries)",
					"sender", e.senderID,
					"group", key.groupIdx,
					"seq", seq,
				)
				continue
			}
			if now.After(e.nextAttempt) {
				// Make a shallow copy to avoid races.
				entry := *e
				select {
				case t.nackQueue <- &entry:
				default:
					// Queue full — skip this cycle; will retry next tick.
				}
			}
		}
		if len(st.pending) == 0 {
			delete(t.states, key)
		}
	}
}

// dispatchLoop reads from nackQueue and sends NACK datagrams to retry endpoints.
func (t *Tracker) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-t.nackQueue:
			t.sendNACK(e)
		}
	}
}

func (t *Tracker) sendNACK(e *gapEntry) {
	if len(t.retryEndpoints) == 0 {
		return
	}

	endpoint := t.retryEndpoints[e.endpointIdx%len(t.retryEndpoints)]

	var buf [NACKSize]byte
	n := &NACK{
		MsgType:     MsgTypeNACK,
		TxID:        e.txid,
		ShardSeqNum: e.seq,
		SenderID:    e.senderID,
		SequenceID:  e.sequenceID,
	}
	Encode(n, buf[:])

	addr, err := net.ResolveUDPAddr("udp", endpoint)
	if err != nil {
		t.log.Warn("NACK: cannot resolve retry endpoint", "endpoint", endpoint, "err", err)
	} else {
		conn, err := net.DialUDP("udp", nil, addr)
		if err == nil {
			_, _ = conn.Write(buf[:])
			_ = conn.Close()
			if t.rec != nil {
				t.rec.NACKDispatched()
			}
			t.log.Debug("NACK dispatched",
				"endpoint", endpoint,
				"seq", e.seq,
				"retry", e.retries+1,
			)
		}
	}

	// Update retry state under lock.
	t.mu.Lock()
	key := senderGroupKey{senderID: e.senderID, groupIdx: e.groupIdx, sequenceID: e.sequenceID}
	if st, ok := t.states[key]; ok {
		if entry, ok := st.pending[e.seq]; ok {
			entry.retries++
			entry.endpointIdx++
			backoff := time.Duration(1<<uint(entry.retries)) * 500 * time.Millisecond
			if backoff > t.cfg.BackoffMax {
				backoff = t.cfg.BackoffMax
			}
			entry.nextAttempt = time.Now().Add(backoff)
		}
	}
	t.mu.Unlock()
}
