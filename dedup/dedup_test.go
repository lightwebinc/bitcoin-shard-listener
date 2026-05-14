package dedup

import (
	"testing"
	"time"
)

func key(g uint32, sub byte, seq uint64) Key {
	var s [32]byte
	s[0] = sub
	return Key{GroupIdx: g, SubtreeID: s, CurSeq: seq}
}

func TestSeenAndAdd_FirstInsertIsMiss(t *testing.T) {
	s := New(8, time.Second)
	if s.SeenAndAdd(key(0, 0, 1)) {
		t.Fatal("first insert should report miss")
	}
}

func TestSeenAndAdd_SecondInsertIsHit(t *testing.T) {
	s := New(8, time.Second)
	k := key(0, 0, 1)
	_ = s.SeenAndAdd(k)
	if !s.SeenAndAdd(k) {
		t.Fatal("second insert should report hit")
	}
}

func TestSeenAndAdd_DistinctKeysAreMisses(t *testing.T) {
	s := New(8, time.Second)
	if s.SeenAndAdd(key(0, 0, 1)) {
		t.Fatal("k1 first insert hit?")
	}
	if s.SeenAndAdd(key(1, 0, 1)) {
		t.Fatal("different group should not collide")
	}
	if s.SeenAndAdd(key(0, 1, 1)) {
		t.Fatal("different subtree should not collide")
	}
	if s.SeenAndAdd(key(0, 0, 2)) {
		t.Fatal("different curSeq should not collide")
	}
}

func TestSeenAndAdd_CapacityEviction(t *testing.T) {
	s := New(2, time.Hour)
	_ = s.SeenAndAdd(key(0, 0, 1))
	_ = s.SeenAndAdd(key(0, 0, 2))
	_ = s.SeenAndAdd(key(0, 0, 3)) // evicts key 1
	if s.SeenAndAdd(key(0, 0, 1)) {
		t.Fatal("evicted key should be a miss again")
	}
	if !s.SeenAndAdd(key(0, 0, 3)) {
		t.Fatal("recently inserted key should be a hit")
	}
}

func TestSeenAndAdd_TTLExpiry(t *testing.T) {
	s := New(8, 10*time.Millisecond)
	k := key(0, 0, 1)
	if s.SeenAndAdd(k) {
		t.Fatal("first insert hit?")
	}
	time.Sleep(20 * time.Millisecond)
	if s.SeenAndAdd(k) {
		t.Fatal("expired key must be reported as miss")
	}
}

func TestSeenAndAdd_DisabledSet(t *testing.T) {
	s := New(0, time.Second)
	for i := uint64(0); i < 5; i++ {
		if s.SeenAndAdd(key(0, 0, i)) {
			t.Fatalf("disabled set must always miss (i=%d)", i)
		}
		if s.SeenAndAdd(key(0, 0, i)) {
			t.Fatalf("disabled set must always miss on dup (i=%d)", i)
		}
	}
}
