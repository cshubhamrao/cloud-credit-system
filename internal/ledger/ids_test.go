package ledger_test

import (
	"testing"

	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// TestDeriveTransferID_Deterministic verifies I-2 (layer 3): the same heartbeat
// inputs always produce the same TigerBeetle transfer ID, enabling TB-level idempotency.
func TestDeriveTransferID_Deterministic(t *testing.T) {
	clusterID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	seq := uint64(42)
	ledgerID := uint32(1)

	id1 := ledger.DeriveTransferID(clusterID, seq, ledgerID)
	id2 := ledger.DeriveTransferID(clusterID, seq, ledgerID)

	if id1 != id2 {
		t.Errorf("DeriveTransferID not deterministic: got %x and %x", id1, id2)
	}
}

// TestDeriveTransferID_DifferentSeq verifies different sequence numbers produce different IDs.
func TestDeriveTransferID_DifferentSeq(t *testing.T) {
	clusterID := [16]byte{1}
	ledgerID := uint32(1)

	id1 := ledger.DeriveTransferID(clusterID, 1, ledgerID)
	id2 := ledger.DeriveTransferID(clusterID, 2, ledgerID)

	if id1 == id2 {
		t.Error("DeriveTransferID: different seqs produced the same ID")
	}
}

// TestDeriveTransferID_DifferentCluster verifies different cluster IDs produce different IDs.
func TestDeriveTransferID_DifferentCluster(t *testing.T) {
	clusterA := [16]byte{1}
	clusterB := [16]byte{2}
	seq := uint64(1)
	ledgerID := uint32(1)

	id1 := ledger.DeriveTransferID(clusterA, seq, ledgerID)
	id2 := ledger.DeriveTransferID(clusterB, seq, ledgerID)

	if id1 == id2 {
		t.Error("DeriveTransferID: different cluster IDs produced the same ID")
	}
}

// TestDeriveTransferID_DifferentLedger verifies I-2: each resource gets its own
// transfer ID for the same heartbeat — no cross-resource collisions.
func TestDeriveTransferID_DifferentLedger(t *testing.T) {
	clusterID := [16]byte{1, 2, 3, 4}
	seq := uint64(7)

	cpu := ledger.DeriveTransferID(clusterID, seq, 1) // cpu_hours
	mem := ledger.DeriveTransferID(clusterID, seq, 2) // memory_gb_hours
	nod := ledger.DeriveTransferID(clusterID, seq, 3) // active_nodes

	if cpu == mem || cpu == nod || mem == nod {
		t.Error("DeriveTransferID: same cluster+seq produced colliding IDs for different ledgers")
	}
}

// TestUUIDToUint128_RoundTrip verifies byte-level identity.
func TestUUIDToUint128_RoundTrip(t *testing.T) {
	original := [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	u128 := ledger.UUIDToUint128(original)
	back := ledger.Uint128ToBytes(u128)

	if back != original {
		t.Errorf("round-trip mismatch: original %x, got %x", original, back)
	}
}

// TestRandomID_Unique verifies two consecutive calls return different IDs.
func TestRandomID_Unique(t *testing.T) {
	id1 := ledger.RandomID()
	id2 := ledger.RandomID()

	if id1 == id2 {
		t.Error("RandomID: two calls returned identical IDs")
	}
}
