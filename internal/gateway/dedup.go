package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	glide "github.com/valkey-io/valkey-glide/go/v2"
	"github.com/valkey-io/valkey-glide/go/v2/options"
)

// Deduplicator is the interface for gateway-level heartbeat dedup.
// It is the first-line optimisation before a signal reaches Temporal;
// authoritative dedup (Invariant I-2) is enforced by processedSeqs in the workflow.
type Deduplicator interface {
	IsDuplicate(clusterID string, seq uint64) bool
}

// ── In-memory (single pod / dev) ─────────────────────────────────────────────

type dedupEntry struct {
	seq    uint64
	seenAt time.Time
}

// MemDedup is a single-pod in-memory deduplicator. Suitable for local dev.
type MemDedup struct {
	mu      sync.Mutex
	entries map[string]*dedupEntry
	ttl     time.Duration
}

func NewMemDedup(ttl time.Duration) *MemDedup {
	d := &MemDedup{entries: make(map[string]*dedupEntry), ttl: ttl}
	go d.pruneLoop()
	return d
}

func (d *MemDedup) IsDuplicate(clusterID string, seq uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[clusterID]
	if !ok || seq > e.seq {
		d.entries[clusterID] = &dedupEntry{seq: seq, seenAt: time.Now()}
		return false
	}
	return true
}

func (d *MemDedup) pruneLoop() {
	ticker := time.NewTicker(d.ttl)
	defer ticker.Stop()
	for range ticker.C {
		d.mu.Lock()
		cutoff := time.Now().Add(-d.ttl)
		for k, e := range d.entries {
			if e.seenAt.Before(cutoff) {
				delete(d.entries, k)
			}
		}
		d.mu.Unlock()
	}
}

// ── Valkey/Redis (multi-pod / production) ────────────────────────────────────

// redisDedup is a distributed deduplicator backed by Valkey/Redis.
//
// Uses two native commands per check:
//
//	ZADD dedup:{clusterID} GT CH {seq} s
//	EXPIRE dedup:{clusterID} {ttl}
//
// GT  — only updates the score if the new value is greater (advances the high-water mark).
// CH  — return value counts changed members (1 = advanced, 0 = GT failed / duplicate).
// EXPIRE — refresh TTL on every call so idle clusters eventually evict.
//
// Both commands are idempotent and safe across any number of pods.
//
// Key schema: dedup:{clusterID}   sorted set with single member "s", score = max seen seq
type redisDedup struct {
	client *glide.Client
	ttl    time.Duration
}

func NewRedisDedup(client *glide.Client, ttl time.Duration) Deduplicator {
	return &redisDedup{client: client, ttl: ttl}
}

func (d *redisDedup) IsDuplicate(clusterID string, seq uint64) bool {
	key := fmt.Sprintf("dedup:%s", clusterID)
	ctx := context.Background()

	opts := options.NewZAddOptions().
		SetUpdateOptions(options.ScoreGreaterThanCurrent)
	opts, _ = opts.SetChanged(true)

	// ZADD key GT CH seq s
	// Returns 1 if seq advanced (not duplicate), 0 if GT failed (duplicate).
	changed, err := d.client.ZAddWithOptions(ctx, key, map[string]float64{"s": float64(seq)}, *opts)
	if err != nil {
		// Valkey unavailable — fail open, let Temporal's processedSeqs handle it.
		return false
	}

	// Refresh TTL regardless of whether this was a duplicate — keeps active
	// cluster keys alive without resetting the score.
	_, _ = d.client.Expire(ctx, key, d.ttl)

	return changed == 0 // 0 = GT failed = seq <= current max = duplicate
}

// ── DedupCache kept for backward compat with existing tests ──────────────────

// DedupCache aliases MemDedup so existing test code compiles unchanged.
type DedupCache = MemDedup

func NewDedupCache(ttl time.Duration) *DedupCache { return NewMemDedup(ttl) }
