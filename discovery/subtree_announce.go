package discovery

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"time"

	"github.com/lightwebinc/bitcoin-shard-common/frame"
	"github.com/lightwebinc/bitcoin-shard-listener/subtreegroup"
)

// SubtreeAnnounceListener joins the BRC-127 subtree announcement multicast
// group on one or more configured scopes and populates a [subtreegroup.Registry]
// with received SubtreeAnnounce datagrams. Call Start to begin listening;
// cancel the context to stop.
type SubtreeAnnounceListener struct {
	Registry      *subtreegroup.Registry
	Groups        []*net.UDPAddr // control group addresses to join
	Iface         *net.Interface // multicast interface
	DefaultTTL    time.Duration  // applied when announcement TTL == 0
	SenderInclude []*net.IPNet   // nil/empty = accept all non-excluded sources
	SenderExclude []*net.IPNet   // checked before include
	Debug         bool
}

// Start listens for SubtreeAnnounce datagrams on all configured groups.
// It also starts a background eviction goroutine (1 s tick).
// Blocks until ctx is cancelled.
func (sl *SubtreeAnnounceListener) Start(ctx context.Context) error {
	go sl.evictLoop(ctx)

	errCh := make(chan error, len(sl.Groups))
	for _, grp := range sl.Groups {
		grp := grp
		go func() {
			errCh <- sl.listenGroup(ctx, grp)
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (sl *SubtreeAnnounceListener) listenGroup(ctx context.Context, grp *net.UDPAddr) error {
	conn, err := net.ListenMulticastUDP("udp6", sl.Iface, grp)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadBuffer(1 << 16)

	buf := make([]byte, frame.SubtreeAnnounceSize+64)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			default:
				slog.Warn("subtree_announce: read error", "group", grp.IP, "err", err)
				continue
			}
		}

		if n < frame.SubtreeAnnounceSize {
			if sl.Debug {
				slog.Debug("subtree_announce: datagram too short", "n", n)
			}
			continue
		}

		if !sl.senderAllowed(src) {
			if sl.Debug {
				slog.Debug("subtree_announce: sender rejected by filter", "src", src.IP)
			}
			continue
		}

		ann, err := frame.DecodeSubtreeAnnounce(buf[:n])
		if err != nil {
			if sl.Debug {
				slog.Debug("subtree_announce: decode error", "err", err)
			}
			continue
		}

		ttl := sl.DefaultTTL
		if ann.TTL > 0 {
			ttl = time.Duration(ann.TTL) * time.Second
		}
		sl.Registry.Add(ann.GroupID, ann.SubtreeID, ttl)

		if sl.Debug {
			slog.Debug("subtree_announce: added entry",
				"group", hex.EncodeToString(ann.GroupID[:]),
				"subtree", hex.EncodeToString(ann.SubtreeID[:]),
				"ttl", ttl,
				"src", src.IP,
			)
		}
	}
}

// senderAllowed applies exclude → include filtering on the UDP source.
// Returns true if the announcement should be processed.
func (sl *SubtreeAnnounceListener) senderAllowed(src *net.UDPAddr) bool {
	ip := src.IP
	for _, cidr := range sl.SenderExclude {
		if cidr.Contains(ip) {
			return false
		}
	}
	if len(sl.SenderInclude) == 0 {
		return true
	}
	for _, cidr := range sl.SenderInclude {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (sl *SubtreeAnnounceListener) evictLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sl.Registry.Evict()
		}
	}
}
