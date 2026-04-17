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
//  1. ReadFrom (per-worker 10 MiB + header receive buffer)
//  2. frame.Decode — extract TxID, Version, ShardSeqNum, SenderID
//  3. shard.Engine.GroupIndex — derive groupIdx from TxID
//  4. filter.Filter.Allow — shard/subtree gating
//  5. egress.Sender.Send — unicast forward to downstream
//  6. nack.Tracker.Observe — gap detection (V2 only, non-zero SenderID+SeqNum)
package listener

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"syscall"

	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

	"github.com/lightwebinc/bitcoin-shard-proxy/frame"
	"github.com/lightwebinc/bitcoin-shard-proxy/shard"

	"github.com/lightwebinc/bitcoin-shard-listener/egress"
	"github.com/lightwebinc/bitcoin-shard-listener/filter"
	"github.com/lightwebinc/bitcoin-shard-listener/metrics"
	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

const recvBufSize = frame.HeaderSize + frame.MaxPayload

// Worker is a single multicast receive goroutine.
type Worker struct {
	id      int
	iface   *net.Interface
	port    int
	groups  []*net.UDPAddr // multicast groups to join
	engine  *shard.Engine
	filt    *filter.Filter
	egr     *egress.Sender
	tracker *nack.Tracker
	rec     *metrics.Recorder
	debug   bool
	log     *slog.Logger
}

// New constructs a Worker.
func New(
	id int,
	iface *net.Interface,
	port int,
	groups []*net.UDPAddr,
	engine *shard.Engine,
	filt *filter.Filter,
	egr *egress.Sender,
	tracker *nack.Tracker,
	rec *metrics.Recorder,
	debug bool,
) *Worker {
	return &Worker{
		id:      id,
		iface:   iface,
		port:    port,
		groups:  groups,
		engine:  engine,
		filt:    filt,
		egr:     egr,
		tracker: tracker,
		rec:     rec,
		debug:   debug,
		log:     slog.Default().With("component", "listener", "worker", id),
	}
}

// Run opens a SO_REUSEPORT socket, joins all multicast groups, and processes
// frames until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	conn, err := openReusePortSocket(w.port)
	if err != nil {
		return fmt.Errorf("worker %d: open socket: %w", w.id, err)
	}
	defer func() { _ = conn.Close() }()

	pc := ipv6.NewPacketConn(conn)

	for _, grp := range w.groups {
		if err := pc.JoinGroup(w.iface, &net.UDPAddr{IP: grp.IP}); err != nil {
			return fmt.Errorf("worker %d: join group %s: %w", w.id, grp.IP, err)
		}
	}

	if w.rec != nil {
		w.rec.WorkerReady()
		defer w.rec.WorkerDone()
	}

	w.log.Info("listener worker ready", "iface", w.iface.Name, "port", w.port, "groups", len(w.groups))

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	buf := make([]byte, recvBufSize)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if isClosedErr(err) {
				return nil
			}
			w.log.Debug("ReadFromUDP error", "err", err)
			continue
		}
		w.processFrame(buf[:n])
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
		ver := "v1"
		if f.Version == frame.FrameVerV2 {
			ver = "v2"
		}
		w.rec.FrameReceived(w.id, w.iface.Name, ver)
	}

	groupIdx := w.engine.GroupIndex(&f.TxID)

	if !w.filt.Allow(groupIdx, f) {
		if w.rec != nil {
			w.rec.FrameDropped(w.id, "filtered")
		}
		return
	}

	if err := w.egr.Send(raw, f); err != nil {
		if w.rec != nil {
			w.rec.EgressError(w.id)
		}
		w.log.Warn("egress send error", "err", err)
	} else {
		if w.rec != nil {
			w.rec.FrameForwarded(w.id, w.egr.Proto())
		}
	}

	// Gap tracking: V2 only, both SenderID and SeqNum must be non-zero.
	var zero [16]byte
	if w.tracker != nil &&
		f.Version == frame.FrameVerV2 &&
		f.ShardSeqNum != 0 &&
		f.SenderID != zero {
		w.tracker.Observe(f.SenderID, groupIdx, f.ShardSeqNum, f.TxID)
	}

	if w.debug {
		w.log.Debug("frame forwarded",
			"version", f.Version,
			"group", groupIdx,
			"seq", f.ShardSeqNum,
		)
	}
}

// openReusePortSocket opens a UDP6 socket with SO_REUSEPORT bound to 0.0.0.0:port.
func openReusePortSocket(port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var setsockoptErr error
			if err := c.Control(func(fd uintptr) {
				setsockoptErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return setsockoptErr
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp6", fmt.Sprintf("[::]:%d", port))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return isErrno(err, syscall.EBADF) || isErrno(err, syscall.EINVAL) ||
		containsString(err.Error(), "use of closed network connection")
}

func isErrno(err error, target syscall.Errno) bool {
	for err != nil {
		if e, ok := err.(syscall.Errno); ok {
			return e == target
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
		} else {
			break
		}
	}
	return false
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && searchString(s, sub))
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
