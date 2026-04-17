package filter_test

import (
	"testing"

	"github.com/lightwebinc/bitcoin-shard-common/frame"
	"github.com/lightwebinc/bitcoin-shard-listener/filter"
)

func makeFrame(subtree [32]byte) *frame.Frame {
	return &frame.Frame{
		Version:   frame.FrameVerV2,
		SubtreeID: subtree,
	}
}

func subtreeID(b byte) [32]byte {
	var id [32]byte
	id[0] = b
	return id
}

func TestAllowAll(t *testing.T) {
	f := filter.New(nil, nil, nil)
	if !f.Allow(0, makeFrame([32]byte{})) {
		t.Error("empty filter should allow everything")
	}
	if !f.Allow(1000, makeFrame(subtreeID(0xAB))) {
		t.Error("empty filter should allow any shard/subtree")
	}
}

func TestShardInclude(t *testing.T) {
	f := filter.New([]uint32{5, 7}, nil, nil)
	if !f.Allow(5, makeFrame([32]byte{})) {
		t.Error("shard 5 should be allowed")
	}
	if !f.Allow(7, makeFrame([32]byte{})) {
		t.Error("shard 7 should be allowed")
	}
	if f.Allow(3, makeFrame([32]byte{})) {
		t.Error("shard 3 should be denied")
	}
	if f.Allow(0, makeFrame([32]byte{})) {
		t.Error("shard 0 should be denied")
	}
}

func TestSubtreeInclude(t *testing.T) {
	allowed := subtreeID(0x01)
	f := filter.New(nil, [][32]byte{allowed}, nil)
	if !f.Allow(0, makeFrame(allowed)) {
		t.Error("included subtree should be allowed")
	}
	if f.Allow(0, makeFrame(subtreeID(0x02))) {
		t.Error("non-included subtree should be denied")
	}
}

func TestSubtreeExclude(t *testing.T) {
	excluded := subtreeID(0xFF)
	f := filter.New(nil, nil, [][32]byte{excluded})
	if f.Allow(0, makeFrame(excluded)) {
		t.Error("excluded subtree should be denied")
	}
	if !f.Allow(0, makeFrame(subtreeID(0x01))) {
		t.Error("non-excluded subtree should be allowed")
	}
}

func TestExcludeOverridesInclude(t *testing.T) {
	id := subtreeID(0xAA)
	f := filter.New(nil, [][32]byte{id}, [][32]byte{id})
	if f.Allow(0, makeFrame(id)) {
		t.Error("exclude should win over include")
	}
}

func TestV1ZeroSubtreeNotInInclude(t *testing.T) {
	allowed := subtreeID(0x01)
	f := filter.New(nil, [][32]byte{allowed}, nil)
	v1 := &frame.Frame{Version: frame.FrameVerV1, SubtreeID: [32]byte{}}
	if f.Allow(0, v1) {
		t.Error("v1 frame with zero SubtreeID should be denied when subtree-include is non-empty")
	}
}

func TestV1ZeroSubtreeInInclude(t *testing.T) {
	f := filter.New(nil, [][32]byte{{}}, nil)
	v1 := &frame.Frame{Version: frame.FrameVerV1, SubtreeID: [32]byte{}}
	if !f.Allow(0, v1) {
		t.Error("v1 frame allowed when zero SubtreeID is explicitly included")
	}
}
