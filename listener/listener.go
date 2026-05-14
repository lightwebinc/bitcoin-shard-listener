// Package listener implements the multicast receive workers for
// bitcoin-shard-listener.
//
// # Worker model
//
// Each Worker binds one UDP socket with SO_REUSEPORT on the configured port
// and joins all configured multicast groups on the configured interface. The
// kernel distributes incoming datagrams across all SO_REUSEPORT workers; the
// same source will consistently land on the same worker, giving CPU-local
// per-sender gap tracking with no lock contention between workers.
//
// # Hot path per frame
//
//  1. ReadFrom (per-worker receive buffer)
//  2. frame.Decode — extract TxID, Version, PrevSeq, CurSeq
//  3. shard.Engine.GroupIndex — derive groupIdx from TxID
//  4. filter.Filter.Allow — shard/subtree gating
//  5. egress.Sender.Send — unicast forward to downstream
//  6. nack.Tracker.Observe — gap detection (BRC-124/BRC-128 only, non-zero CurSeq)
package listener

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lightwebinc/bitcoin-shard-common/frame"
	"github.com/lightwebinc/bitcoin-shard-common/shard"

	"github.com/lightwebinc/bitcoin-shard-listener/dedup"
	"github.com/lightwebinc/bitcoin-shard-listener/egress"
	"github.com/lightwebinc/bitcoin-shard-listener/filter"
	"github.com/lightwebinc/bitcoin-shard-listener/metrics"
	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

const (
	recvBufSize = 4 * 1024 * 1024 // per-worker UDP receive buffer

	// socketRecvBuf is the UDP receive buffer requested on each worker socket.
	socketRecvBuf = 64 * 1024 * 1024 // 64 MiB
)

// Worker is a single multicast receive goroutine.
type Worker struct {
	id                int
	iface             *net.Interface
	port              int
	groups            []*net.UDPAddr // multicast groups to join
	engine            *shard.Engine
	filt              *filter.Filter
	egr               *egress.Sender
	mcastEgr          *egress.MCastSender // nil when multicast egress is disabled
	tracker           *nack.Tracker
	rec               *metrics.Recorder
	debug             bool
	verifyPayloadHash bool
	dedupSet          *dedup.Set // nil = dedup disabled
	log               *slog.Logger
}

// SetEgressDedup attaches a duplicate-suppression set keyed on
// (groupIdx, subtreeID, CurSeq). When set, retransmits whose key was already
// forwarded recently are dropped before egress. nil disables dedup. Defaults
// to disabled.
func (w *Worker) SetEgressDedup(s *dedup.Set) {
	w.dedupSet = s
}

// New constructs a Worker. mcastEgr may be nil to disable multicast egress.
func New(
	id int,
	iface *net.Interface,
	port int,
	groups []*net.UDPAddr,
	engine *shard.Engine,
	filt *filter.Filter,
	egr *egress.Sender,
	mcastEgr *egress.MCastSender,
	tracker *nack.Tracker,
	rec *metrics.Recorder,
	debug bool,
) *Worker {
	return &Worker{
		id:       id,
		iface:    iface,
		port:     port,
		groups:   groups,
		engine:   engine,
		filt:     filt,
		egr:      egr,
		mcastEgr: mcastEgr,
		tracker:  tracker,
		rec:      rec,
		debug:    debug,
		log:      slog.Default().With("component", "listener", "worker", id),
	}
}

// SetVerifyPayloadHash toggles SHA256d(payload)==TxID verification on
// BRC-124/BRC-128 frames. When true, frames whose payload hash does not match
// their TxID are dropped before egress and gap tracking, and
// bsl_frames_invalid_payload_total is incremented. Defaults to false.
func (w *Worker) SetVerifyPayloadHash(v bool) {
	w.verifyPayloadHash = v
}

// Run opens a SO_REUSEPORT socket, joins all multicast groups, and processes
// frames until ctx is cancelled.
//
// The socket is created via raw syscalls so it is never registered with Go's
// internal edge-triggered epoll. Blocking Recvfrom is used so the OS thread
// parks in the kernel and wakes the moment a datagram arrives, with zero
// scheduler overhead between the wakeup and the read.
func (w *Worker) Run(ctx context.Context) error {
	fd, err := openRawSocket(w.port)
	if err != nil {
		return fmt.Errorf("worker %d: open socket: %w", w.id, err)
	}

	for _, grp := range w.groups {
		mreq := &unix.IPv6Mreq{Interface: uint32(w.iface.Index)}
		copy(mreq.Multiaddr[:], grp.IP.To16())
		if err := unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, mreq); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("worker %d: join group %s: %w", w.id, grp.IP, err)
		}
	}

	if w.rec != nil {
		w.rec.WorkerReady()
		defer w.rec.WorkerDone()
	}

	w.log.Info("listener worker ready", "iface", w.iface.Name, "port", w.port, "groups", len(w.groups))

	// SO_RCVTIMEO makes Recvfrom wake up periodically so we can check ctx.
	// This is the reliable shutdown mechanism: closing the fd from another
	// goroutine is POSIX-undefined and does not always unblock recvfrom on
	// all Linux kernel versions. Keep the fd-close goroutine as a fast path
	// for kernels that do support it.
	tv := unix.NsecToTimeval((200 * time.Millisecond).Nanoseconds())
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	go func() {
		<-ctx.Done()
		_ = unix.Close(fd)
	}()

	buf := make([]byte, recvBufSize)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				if ctx.Err() != nil {
					return nil
				}
				continue
			}
			if err == unix.EBADF || err == unix.EINVAL {
				return nil
			}
			if err == unix.EINTR {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			w.log.Error("recvfrom error", "err", err)
			continue
		}
		if n > 0 {
			w.processFrame(buf[:n])
		}
	}
}

func (w *Worker) processFrame(raw []byte) {
	f, err := frame.Decode(raw)
	if err != nil {
		if w.rec != nil {
			w.rec.FrameDropped(w.id, "decode_error")
		}
		if w.debug {
			w.log.Debug("decode error", "err", err, "len", len(raw))
		}
		return
	}

	if w.rec != nil {
		ver := "brc12"
		if f.Version == frame.FrameVerV2 {
			ver = "brc124"
		}
		w.rec.FrameReceived(w.id, w.iface.Name, ver)
	}

	// Optional payload-hash verification (GAP-2). Only meaningful for V2
	// frames (BRC-12 has no chain semantics; the TxID is still the BSV
	// double-SHA256 of the payload but legacy frames are forwarded verbatim
	// regardless). When disabled, this branch is skipped entirely.
	if w.verifyPayloadHash && f.Version == frame.FrameVerV2 {
		first := sha256.Sum256(f.Payload)
		second := sha256.Sum256(first[:])
		if second != f.TxID {
			if w.rec != nil {
				w.rec.FrameInvalidPayload(w.id)
			}
			if w.debug {
				w.log.Debug("payload hash mismatch",
					"txid_prefix", fmt.Sprintf("%x", f.TxID[:8]),
					"computed_prefix", fmt.Sprintf("%x", second[:8]),
					"payload_len", len(f.Payload),
				)
			}
			return
		}
	}

	groupIdx := w.engine.GroupIndex(&f.TxID)

	if allow, reason := w.filt.Allow(groupIdx, f); !allow {
		if w.rec != nil {
			w.rec.FrameDropped(w.id, reason)
		}
		return
	}

	// Egress duplicate suppression (GAP-3): when an inline frame and its
	// retransmit both reach the listener (common at 1+% loss with a warm
	// retry endpoint), forward only the first. Gap-state suppression in
	// nack.Tracker.Observe is independent and still runs below.
	if w.dedupSet != nil && f.Version == frame.FrameVerV2 && f.CurSeq != 0 {
		if w.dedupSet.SeenAndAdd(dedup.Key{GroupIdx: groupIdx, SubtreeID: f.SubtreeID, CurSeq: f.CurSeq}) {
			if w.rec != nil {
				w.rec.FrameDeduped(w.id)
			}
			// Skip egress, but still let the tracker observe the frame so
			// gap-fill / chain-tail bookkeeping stays accurate.
			if w.tracker != nil {
				w.tracker.Observe(groupIdx, f.SubtreeID, f.PrevSeq, f.CurSeq, f.TxID)
			}
			return
		}
	}

	if err := w.egr.Send(raw, f); err != nil {
		if w.rec != nil {
			w.rec.EgressError(w.id)
		}
		w.log.Debug("egress send error", "err", err)
	} else {
		if w.rec != nil {
			w.rec.FrameForwarded(w.id, w.egr.Proto())
		}
	}

	// Multicast egress fan-out: fires independently of unicast outcome.
	if w.mcastEgr != nil {
		if err := w.mcastEgr.Send(raw, f, groupIdx); err != nil {
			if w.rec != nil {
				w.rec.MCEgressError(w.id)
			}
			w.log.Debug("mc egress send error", "err", err)
		} else {
			if w.rec != nil {
				w.rec.FrameForwarded(w.id, w.mcastEgr.Proto())
			}
		}
	}

	// Gap tracking: BRC-124/BRC-128 only, CurSeq must be non-zero (proxy-stamped).
	if w.tracker != nil && f.Version == frame.FrameVerV2 && f.CurSeq != 0 {
		w.tracker.Observe(groupIdx, f.SubtreeID, f.PrevSeq, f.CurSeq, f.TxID)
	}

	if w.debug {
		w.log.Debug("frame forwarded",
			"version", f.Version,
			"group", groupIdx,
			"cur_seq", f.CurSeq,
		)
	}
}

// openRawSocket creates a UDP6 socket with SO_REUSEPORT bound to [::]:port
// using raw syscalls, bypassing Go's net package so the fd is never registered
// with Go's internal edge-triggered epoll.
func openRawSocket(port int) (int, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("SO_REUSEPORT: %w", err)
	}
	// Receive buffer: ignore error — kernel silently caps at rmem_max.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, socketRecvBuf)
	sa := &unix.SockaddrInet6{Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind [::]::%d: %w", port, err)
	}
	return fd, nil
}
