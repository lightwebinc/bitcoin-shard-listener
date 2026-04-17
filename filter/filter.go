// Package filter implements allocation-free shard and subtree filtering for
// bitcoin-shard-listener. All logic is pure; no I/O is performed.
//
// A frame passes the filter if and only if:
//  1. Its group index is in the shard-include set (or the set is empty = all).
//  2. Its SubtreeID is in the subtree-include set (or the set is empty = all).
//  3. Its SubtreeID is NOT in the subtree-exclude set.
//
// V1 frames have a zero SubtreeID. If subtree-include is non-empty a V1 frame
// will pass only if [32]byte{} is in the include set.
package filter

import (
	"github.com/lightwebinc/bitcoin-shard-proxy/frame"
)

// Filter holds the compiled include/exclude sets. Construct with [New].
type Filter struct {
	shardInclude   map[uint32]struct{}  // nil = all shards accepted
	subtreeInclude map[[32]byte]struct{} // nil = all subtrees accepted
	subtreeExclude map[[32]byte]struct{} // nil = no subtrees excluded
}

// New constructs a Filter from the parsed config lists.
//   - shardInclude: nil or empty means accept all shard indices.
//   - subtreeInclude: nil or empty means accept all subtree IDs.
//   - subtreeExclude: nil or empty means exclude nothing.
func New(shardInclude []uint32, subtreeInclude, subtreeExclude [][32]byte) *Filter {
	f := &Filter{}
	if len(shardInclude) > 0 {
		f.shardInclude = make(map[uint32]struct{}, len(shardInclude))
		for _, idx := range shardInclude {
			f.shardInclude[idx] = struct{}{}
		}
	}
	if len(subtreeInclude) > 0 {
		f.subtreeInclude = make(map[[32]byte]struct{}, len(subtreeInclude))
		for _, id := range subtreeInclude {
			f.subtreeInclude[id] = struct{}{}
		}
	}
	if len(subtreeExclude) > 0 {
		f.subtreeExclude = make(map[[32]byte]struct{}, len(subtreeExclude))
		for _, id := range subtreeExclude {
			f.subtreeExclude[id] = struct{}{}
		}
	}
	return f
}

// Allow returns true if the frame should be forwarded to egress.
// groupIdx is derived from the frame's TxID by the caller.
func (f *Filter) Allow(groupIdx uint32, fr *frame.Frame) bool {
	if f.shardInclude != nil {
		if _, ok := f.shardInclude[groupIdx]; !ok {
			return false
		}
	}
	if f.subtreeExclude != nil {
		if _, ok := f.subtreeExclude[fr.SubtreeID]; ok {
			return false
		}
	}
	if f.subtreeInclude != nil {
		if _, ok := f.subtreeInclude[fr.SubtreeID]; !ok {
			return false
		}
	}
	return true
}
