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

// TestDeriveAllocationTransferID_Deterministic verifies I-5: the same inputs always
// produce the same allocation transfer ID so Temporal activity retries are idempotent.
func TestDeriveAllocationTransferID_Deterministic(t *testing.T) {
	tenantUUID := [16]byte{10, 20, 30, 40}
	seqNo := uint64(7)
	ledgerID := uint32(1)

	id1 := ledger.DeriveAllocationTransferID(tenantUUID, seqNo, ledgerID)
	id2 := ledger.DeriveAllocationTransferID(tenantUUID, seqNo, ledgerID)

	if id1 != id2 {
		t.Errorf("DeriveAllocationTransferID not deterministic: %x vs %x", id1, id2)
	}
}

// TestDeriveAllocationTransferID_DifferentSeq verifies different seqNos produce different IDs.
func TestDeriveAllocationTransferID_DifferentSeq(t *testing.T) {
	tenantUUID := [16]byte{1}
	ledgerID := uint32(1)

	id1 := ledger.DeriveAllocationTransferID(tenantUUID, 1, ledgerID)
	id2 := ledger.DeriveAllocationTransferID(tenantUUID, 2, ledgerID)

	if id1 == id2 {
		t.Error("DeriveAllocationTransferID: different seqNos produced the same ID")
	}
}

// TestDeriveAllocationTransferID_NoCollisionWithUsage verifies I-5: an allocation ID
// does not collide with a usage transfer ID for the same (seqNo, ledgerID) inputs,
// ensuring domain separation between transfer types.
func TestDeriveAllocationTransferID_NoCollisionWithUsage(t *testing.T) {
	// DeriveTransferID uses (clusterID, seq, ledgerID); DeriveAllocationTransferID
	// uses (tenantUUID, seqNo, ledgerID). Use byte-identical inputs to test separation.
	shared := [16]byte{1, 2, 3, 4}
	seq := uint64(42)
	ledgerID := uint32(1)

	usageID := ledger.DeriveTransferID(shared, seq, ledgerID)
	allocID := ledger.DeriveAllocationTransferID(shared, seq, ledgerID)

	if usageID == allocID {
		t.Error("DeriveAllocationTransferID collides with DeriveTransferID for identical inputs — domain separator missing")
	}
}

// TestDeriveGaugePendingID_Deterministic verifies I-5: same inputs → same pending ID.
func TestDeriveGaugePendingID_Deterministic(t *testing.T) {
	tenantUUID := [16]byte{5}
	clusterUUID := [16]byte{9}
	seqNo := uint64(3)
	ledgerID := uint32(3)

	id1 := ledger.DeriveGaugePendingID(tenantUUID, seqNo, clusterUUID, ledgerID)
	id2 := ledger.DeriveGaugePendingID(tenantUUID, seqNo, clusterUUID, ledgerID)

	if id1 != id2 {
		t.Errorf("DeriveGaugePendingID not deterministic: %x vs %x", id1, id2)
	}
}

// TestDeriveGaugeVoidID_Deterministic verifies I-5: same inputs → same void ID.
func TestDeriveGaugeVoidID_Deterministic(t *testing.T) {
	tenantUUID := [16]byte{5}
	clusterUUID := [16]byte{9}
	seqNo := uint64(3)
	ledgerID := uint32(3)

	id1 := ledger.DeriveGaugeVoidID(tenantUUID, seqNo, clusterUUID, ledgerID)
	id2 := ledger.DeriveGaugeVoidID(tenantUUID, seqNo, clusterUUID, ledgerID)

	if id1 != id2 {
		t.Errorf("DeriveGaugeVoidID not deterministic: %x vs %x", id1, id2)
	}
}

// TestDeriveGauge_PendingVoidNoCollision verifies that pending and void IDs differ
// for identical inputs — the domain separator distinguishes them.
func TestDeriveGauge_PendingVoidNoCollision(t *testing.T) {
	tenantUUID := [16]byte{1}
	clusterUUID := [16]byte{2}
	seqNo := uint64(1)
	ledgerID := uint32(3)

	pendingID := ledger.DeriveGaugePendingID(tenantUUID, seqNo, clusterUUID, ledgerID)
	voidID := ledger.DeriveGaugeVoidID(tenantUUID, seqNo, clusterUUID, ledgerID)

	if pendingID == voidID {
		t.Error("DeriveGaugePendingID and DeriveGaugeVoidID returned the same ID — domain separator missing")
	}
}

// TestDeriveGauge_NoCollisionAcrossClusters verifies different clusterUUIDs produce
// different gauge IDs (so cluster A's reservation can't accidentally void cluster B's).
func TestDeriveGauge_NoCollisionAcrossClusters(t *testing.T) {
	tenantUUID := [16]byte{1}
	seqNo := uint64(5)
	ledgerID := uint32(3)
	clusterA := [16]byte{1}
	clusterB := [16]byte{2}

	idA := ledger.DeriveGaugePendingID(tenantUUID, seqNo, clusterA, ledgerID)
	idB := ledger.DeriveGaugePendingID(tenantUUID, seqNo, clusterB, ledgerID)

	if idA == idB {
		t.Error("DeriveGaugePendingID: different clusters produced the same pending ID")
	}
}
