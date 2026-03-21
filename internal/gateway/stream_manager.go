package gateway

import (
	"sync"
	"time"
)

// StreamEntry tracks a live bidi stream connection for a cluster.
type StreamEntry struct {
	ClusterID   string
	ConnectedAt time.Time
	LastSeen    time.Time
}

// StreamManager tracks active bidi stream connections per cluster.
// One entry per cluster_id — a reconnecting cluster replaces the old entry.
type StreamManager struct {
	mu      sync.RWMutex
	streams map[string]*StreamEntry
}

func NewStreamManager() *StreamManager {
	return &StreamManager{streams: make(map[string]*StreamEntry)}
}

// Register records a new or replacement stream for a cluster.
func (m *StreamManager) Register(clusterID string) *StreamEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := &StreamEntry{
		ClusterID:   clusterID,
		ConnectedAt: time.Now(),
		LastSeen:    time.Now(),
	}
	m.streams[clusterID] = e
	return e
}

// Deregister removes a cluster's stream entry on disconnect.
func (m *StreamManager) Deregister(clusterID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.streams, clusterID)
}

// Touch updates the last-seen time for a cluster.
func (m *StreamManager) Touch(clusterID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.streams[clusterID]; ok {
		e.LastSeen = time.Now()
	}
}

// ActiveClusters returns a snapshot of all connected cluster IDs.
func (m *StreamManager) ActiveClusters() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.streams))
	for id := range m.streams {
		ids = append(ids, id)
	}
	return ids
}
