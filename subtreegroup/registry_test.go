package subtreegroup

import (
	"testing"
	"time"
)

func gid(b byte) [16]byte {
	var g [16]byte
	g[0] = b
	return g
}

func sid(b byte) [32]byte {
	var s [32]byte
	s[0] = b
	return s
}

func TestRegistry_AddContains(t *testing.T) {
	r := New([][16]byte{gid(1)}, 900*time.Second)
	r.Add(gid(1), sid(0xAA), 10*time.Second)
	if !r.Contains(sid(0xAA)) {
		t.Error("expected Contains to return true after Add")
	}
}

func TestRegistry_ContainsFalseWhenAbsent(t *testing.T) {
	r := New([][16]byte{gid(1)}, 900*time.Second)
	if r.Contains(sid(0xBB)) {
		t.Error("expected Contains false for unknown subtree")
	}
}

func TestRegistry_UnsubscribedGroupIgnored(t *testing.T) {
	r := New([][16]byte{gid(1)}, 900*time.Second)
	r.Add(gid(2), sid(0xCC), 10*time.Second) // group 2 not subscribed
	if r.Contains(sid(0xCC)) {
		t.Error("unsubscribed group should not be stored")
	}
}

func TestRegistry_ExpiredEntryNotContained(t *testing.T) {
	r := New([][16]byte{gid(1)}, 900*time.Second)
	r.Add(gid(1), sid(0xDD), 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if r.Contains(sid(0xDD)) {
		t.Error("expired entry should not be contained")
	}
}

func TestRegistry_Evict(t *testing.T) {
	r := New([][16]byte{gid(1)}, 900*time.Second)
	r.Add(gid(1), sid(0xEE), 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if r.Len() != 1 {
		t.Fatalf("expected 1 entry before evict, got %d", r.Len())
	}
	r.Evict()
	if r.Len() != 0 {
		t.Errorf("expected 0 entries after evict, got %d", r.Len())
	}
}

func TestRegistry_Remove(t *testing.T) {
	r := New([][16]byte{gid(1)}, 900*time.Second)
	r.Add(gid(1), sid(0xFF), 10*time.Second)
	r.Remove(gid(1), sid(0xFF))
	if r.Contains(sid(0xFF)) {
		t.Error("expected Contains false after Remove")
	}
}

func TestRegistry_DefaultTTLOnZero(t *testing.T) {
	r := New([][16]byte{gid(1)}, 10*time.Millisecond)
	r.Add(gid(1), sid(0x11), 0) // 0 means use default (10ms)
	if !r.Contains(sid(0x11)) {
		t.Error("entry should be present immediately after Add with TTL=0")
	}
	time.Sleep(20 * time.Millisecond)
	if r.Contains(sid(0x11)) {
		t.Error("entry should have expired after defaultTTL elapsed")
	}
}

func TestRegistry_MultipleGroups(t *testing.T) {
	r := New([][16]byte{gid(1), gid(2)}, 900*time.Second)
	r.Add(gid(1), sid(0x11), 10*time.Second)
	r.Add(gid(2), sid(0x22), 10*time.Second)
	if !r.Contains(sid(0x11)) {
		t.Error("subtree in group 1 should be contained")
	}
	if !r.Contains(sid(0x22)) {
		t.Error("subtree in group 2 should be contained")
	}
	if r.Len() != 2 {
		t.Errorf("expected Len 2, got %d", r.Len())
	}
}

func TestRegistry_SameSubtreeDifferentGroups(t *testing.T) {
	r := New([][16]byte{gid(1), gid(2)}, 900*time.Second)
	r.Add(gid(1), sid(0xAB), 10*time.Second)
	r.Add(gid(2), sid(0xAB), 10*time.Second)
	if r.Len() != 2 {
		t.Errorf("expected Len 2 (same subtree in two groups), got %d", r.Len())
	}
	if !r.Contains(sid(0xAB)) {
		t.Error("subtree present in both groups should be contained")
	}
}
