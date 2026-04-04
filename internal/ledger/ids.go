package ledger

import (
	"encoding/binary"

	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
	"github.com/zeebo/blake3"
)

// DeriveTransferID produces a deterministic 128-bit TigerBeetle transfer ID
// from (clusterID, sequenceNumber, ledgerID). The same inputs always produce
// the same ID, which is the idempotency guarantee for heartbeat usage records.
//
// Uses blake3 — a fast, non-cryptographic hash suitable for deduplication keys.
// The IDs carry no secret, so cryptographic strength is unnecessary.
//
// Layout: blake3(clusterID_bytes || seq_le64 || ledger_le32), first 16 bytes.
func DeriveTransferID(clusterID [16]byte, seq uint64, ledgerID uint32) types.Uint128 {
	h := blake3.New()
	h.Write(clusterID[:])
	var seqBuf [8]byte
	binary.LittleEndian.PutUint64(seqBuf[:], seq)
	h.Write(seqBuf[:])
	var ledBuf [4]byte
	binary.LittleEndian.PutUint32(ledBuf[:], ledgerID)
	h.Write(ledBuf[:])
	var sum [32]byte
	h.Sum(sum[:0])
	return bytesToUint128(sum[:16])
}

// RandomID generates a random-looking but time-ordered 128-bit ID.
// Only use for transfers where a deterministic derive function is not applicable.
func RandomID() types.Uint128 {
	// TigerBeetle recommends time-based IDs for good LSM performance.
	// Use the tigerbeetle-go helper.
	return types.ID()
}

// DeriveAllocationTransferID produces a deterministic 128-bit ID for a quota
// allocation or adjustment transfer. Using a deterministic ID means Temporal
// activity retries are idempotent — TigerBeetle returns TransferExists on the
// second attempt instead of creating a duplicate credit (invariant I-5).
//
// Inputs must uniquely identify one allocation event:
//   - tenantUUID: the tenant being credited
//   - seqNo: a monotonically increasing counter (FlushSeqNo in the accounting
//     workflow; PeriodStartNs cast to uint64 in the provisioning workflow)
//   - ledgerID: the resource ledger (one per resource type)
//
// Layout: blake3("alloc" || tenantUUID || seqNo_le64 || ledger_le32), first 16 bytes.
func DeriveAllocationTransferID(tenantUUID [16]byte, seqNo uint64, ledgerID uint32) types.Uint128 {
	h := blake3.New()
	h.Write([]byte("alloc"))
	h.Write(tenantUUID[:])
	var seqBuf [8]byte
	binary.LittleEndian.PutUint64(seqBuf[:], seqNo)
	h.Write(seqBuf[:])
	var ledBuf [4]byte
	binary.LittleEndian.PutUint32(ledBuf[:], ledgerID)
	h.Write(ledBuf[:])
	var sum [32]byte
	h.Sum(sum[:0])
	return bytesToUint128(sum[:16])
}

// DeriveGaugePendingID produces a deterministic 128-bit ID for a new pending
// gauge reservation transfer. Combined with DeriveGaugeVoidID, this makes the
// void+create pair idempotent on Temporal activity retry (invariant I-5).
//
// Layout: blake3("gauge-pending" || tenantUUID || seqNo_le64 || clusterUUID || ledger_le32), first 16 bytes.
func DeriveGaugePendingID(tenantUUID [16]byte, seqNo uint64, clusterUUID [16]byte, ledgerID uint32) types.Uint128 {
	h := blake3.New()
	h.Write([]byte("gauge-pending"))
	h.Write(tenantUUID[:])
	var seqBuf [8]byte
	binary.LittleEndian.PutUint64(seqBuf[:], seqNo)
	h.Write(seqBuf[:])
	h.Write(clusterUUID[:])
	var ledBuf [4]byte
	binary.LittleEndian.PutUint32(ledBuf[:], ledgerID)
	h.Write(ledBuf[:])
	var sum [32]byte
	h.Sum(sum[:0])
	return bytesToUint128(sum[:16])
}

// DeriveGaugeVoidID produces a deterministic 128-bit ID for the void transfer
// that releases an old pending gauge reservation. Using the same domain separator
// as DeriveGaugePendingID but with "gauge-void" ensures no collision between the
// void and the new pending transfer within the same flush.
//
// Layout: blake3("gauge-void" || tenantUUID || seqNo_le64 || clusterUUID || ledger_le32), first 16 bytes.
func DeriveGaugeVoidID(tenantUUID [16]byte, seqNo uint64, clusterUUID [16]byte, ledgerID uint32) types.Uint128 {
	h := blake3.New()
	h.Write([]byte("gauge-void"))
	h.Write(tenantUUID[:])
	var seqBuf [8]byte
	binary.LittleEndian.PutUint64(seqBuf[:], seqNo)
	h.Write(seqBuf[:])
	h.Write(clusterUUID[:])
	var ledBuf [4]byte
	binary.LittleEndian.PutUint32(ledBuf[:], ledgerID)
	h.Write(ledBuf[:])
	var sum [32]byte
	h.Sum(sum[:0])
	return bytesToUint128(sum[:16])
}

// UUIDToUint128 converts a 16-byte UUID to a TigerBeetle Uint128.
func UUIDToUint128(uuid [16]byte) types.Uint128 {
	return bytesToUint128(uuid[:])
}

// Uint128ToBytes extracts the 16-byte representation of a TigerBeetle Uint128.
// types.Uint128 is [16]byte, so this is a direct copy.
func Uint128ToBytes(id types.Uint128) [16]byte {
	return id
}

func bytesToUint128(b []byte) types.Uint128 {
	var id types.Uint128
	copy(id[:], b[:16])
	return id
}
