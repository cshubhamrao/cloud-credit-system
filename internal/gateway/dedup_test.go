package gateway_test

import (
	"testing"
	"time"

	"github.com/cshubhamrao/cloud-credit-system/internal/gateway"
)

// TestMemDedup_TTLPruning verifies that the pruneLoop evicts entries older than the TTL.
// After eviction, the same seq is no longer considered a duplicate (fresh start).
func TestMemDedup_TTLPruning(t *testing.T) {
	ttl := 50 * time.Millisecond
	d := gateway.NewMemDedup(ttl)

	// First observation — not a duplicate.
	if d.IsDuplicate("cluster-a", 7) {
		t.Fatal("seq 7 should not be a duplicate on first observation")
	}
	// Immediately, same seq is a duplicate.
	if !d.IsDuplicate("cluster-a", 7) {
		t.Fatal("seq 7 should be a duplicate immediately after first observation")
	}

	// Wait long enough for pruneLoop to evict the entry (3× TTL is safe margin).
	time.Sleep(3 * ttl)

	// After eviction the entry is gone — seq 7 looks new again.
	if d.IsDuplicate("cluster-a", 7) {
		t.Error("seq 7 should not be a duplicate after TTL eviction")
	}
}

func newTestDedup() *gateway.DedupCache {
	// Use a large TTL so entries are never pruned during tests.
	return gateway.NewDedupCache(1 * time.Hour)
}

// TestDedupCache_FirstSeen verifies I-2 layer 1: an unseen sequence is not a duplicate.
func TestDedupCache_FirstSeen(t *testing.T) {
	c := newTestDedup()
	if c.IsDuplicate("cluster-a", 1) {
		t.Error("first-seen seq 1 should not be a duplicate")
	}
}

// TestDedupCache_SameSeq verifies I-2 layer 1: the same sequence is a duplicate.
func TestDedupCache_SameSeq(t *testing.T) {
	c := newTestDedup()
	c.IsDuplicate("cluster-a", 5) // first observation
	if !c.IsDuplicate("cluster-a", 5) {
		t.Error("same seq 5 should be a duplicate")
	}
}

// TestDedupCache_LowerSeq verifies that a lower sequence is also considered a duplicate
// (sequences are monotonically increasing per cluster).
func TestDedupCache_LowerSeq(t *testing.T) {
	c := newTestDedup()
	c.IsDuplicate("cluster-a", 10)
	if !c.IsDuplicate("cluster-a", 5) {
		t.Error("seq 5 after seeing seq 10 should be a duplicate (out-of-order / replay)")
	}
}

// TestDedupCache_HigherSeq verifies that a higher sequence advances the tracked value.
func TestDedupCache_HigherSeq(t *testing.T) {
	c := newTestDedup()
	c.IsDuplicate("cluster-a", 1)
	c.IsDuplicate("cluster-a", 2)
	if c.IsDuplicate("cluster-a", 3) {
		t.Error("seq 3 should not be a duplicate after seeing 1 and 2")
	}
	// Now seq 2 should be a duplicate.
	if !c.IsDuplicate("cluster-a", 2) {
		t.Error("seq 2 should now be a duplicate")
	}
}

// TestDedupCache_DifferentClusters verifies I-2: each cluster has an independent
// sequence counter — the same seq number is valid across different clusters.
func TestDedupCache_DifferentClusters(t *testing.T) {
	c := newTestDedup()
	c.IsDuplicate("cluster-a", 7)

	// Same seq on a different cluster is not a duplicate.
	if c.IsDuplicate("cluster-b", 7) {
		t.Error("seq 7 on cluster-b should not be a duplicate of cluster-a's seq 7")
	}
}

// TestDedupCache_MultipleUpdates verifies that updates to one cluster don't affect another.
func TestDedupCache_MultipleUpdates(t *testing.T) {
	c := newTestDedup()

	c.IsDuplicate("cluster-a", 100)
	c.IsDuplicate("cluster-b", 50)

	if !c.IsDuplicate("cluster-a", 99) {
		t.Error("seq 99 < 100 should be a duplicate for cluster-a")
	}
	if c.IsDuplicate("cluster-b", 51) {
		t.Error("seq 51 > 50 should not be a duplicate for cluster-b")
	}
}
