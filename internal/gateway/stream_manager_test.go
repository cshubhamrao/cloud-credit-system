package gateway_test

import (
	"sort"
	"testing"
	"time"

	"github.com/cshubhamrao/cloud-credit-system/internal/gateway"
)

func TestStreamManager_RegisterReturnsEntry(t *testing.T) {
	m := gateway.NewStreamManager()
	e := m.Register("cluster-a")
	if e == nil {
		t.Fatal("Register returned nil entry")
	}
	if e.ClusterID != "cluster-a" {
		t.Errorf("ClusterID = %q, want %q", e.ClusterID, "cluster-a")
	}
	if e.ConnectedAt.IsZero() {
		t.Error("ConnectedAt not set")
	}
	if e.LastSeen.IsZero() {
		t.Error("LastSeen not set")
	}
}

func TestStreamManager_RegisterAppearsInActiveClusters(t *testing.T) {
	m := gateway.NewStreamManager()
	m.Register("cluster-a")

	ids := m.ActiveClusters()
	if len(ids) != 1 || ids[0] != "cluster-a" {
		t.Errorf("ActiveClusters() = %v, want [cluster-a]", ids)
	}
}

func TestStreamManager_RegisterReplacement(t *testing.T) {
	m := gateway.NewStreamManager()
	e1 := m.Register("cluster-a")
	time.Sleep(time.Millisecond) // ensure ConnectedAt differs
	e2 := m.Register("cluster-a")

	if e1 == e2 {
		t.Error("replacement Register should return a new entry, not the old pointer")
	}
	if !e2.ConnectedAt.After(e1.ConnectedAt) {
		t.Error("replacement entry should have a later ConnectedAt")
	}
	// Still only one entry
	if len(m.ActiveClusters()) != 1 {
		t.Errorf("ActiveClusters() len = %d, want 1", len(m.ActiveClusters()))
	}
}

func TestStreamManager_DeregisterRemovesCluster(t *testing.T) {
	m := gateway.NewStreamManager()
	m.Register("cluster-a")
	m.Deregister("cluster-a")

	ids := m.ActiveClusters()
	if len(ids) != 0 {
		t.Errorf("expected empty after deregister, got %v", ids)
	}
}

func TestStreamManager_DeregisterNonExistentIsNoop(t *testing.T) {
	m := gateway.NewStreamManager()
	// Should not panic
	m.Deregister("does-not-exist")
	m.Deregister("does-not-exist") // double-delete also safe
}

func TestStreamManager_TouchUpdatesLastSeen(t *testing.T) {
	m := gateway.NewStreamManager()
	e := m.Register("cluster-a")
	before := e.LastSeen

	time.Sleep(time.Millisecond)
	m.Touch("cluster-a")

	if !e.LastSeen.After(before) {
		t.Error("Touch should advance LastSeen")
	}
}

func TestStreamManager_TouchNonExistentIsNoop(t *testing.T) {
	m := gateway.NewStreamManager()
	// Should not panic
	m.Touch("does-not-exist")
}

func TestStreamManager_TouchDoesNotAffectOtherClusters(t *testing.T) {
	m := gateway.NewStreamManager()
	ea := m.Register("cluster-a")
	eb := m.Register("cluster-b")
	beforeB := eb.LastSeen

	time.Sleep(time.Millisecond)
	m.Touch("cluster-a")

	if eb.LastSeen != beforeB {
		t.Error("touching cluster-a should not affect cluster-b's LastSeen")
	}
	_ = ea
}

func TestStreamManager_ActiveClusters_Empty(t *testing.T) {
	m := gateway.NewStreamManager()
	ids := m.ActiveClusters()
	if ids == nil {
		t.Error("ActiveClusters() should return non-nil slice for empty manager")
	}
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

func TestStreamManager_ActiveClusters_Multiple(t *testing.T) {
	m := gateway.NewStreamManager()
	m.Register("cluster-a")
	m.Register("cluster-b")
	m.Register("cluster-c")

	ids := m.ActiveClusters()
	sort.Strings(ids)
	want := []string{"cluster-a", "cluster-b", "cluster-c"}
	for i, id := range ids {
		if id != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, id, want[i])
		}
	}
}

func TestStreamManager_Lifecycle(t *testing.T) {
	m := gateway.NewStreamManager()

	// Register → Touch → Deregister → re-Register
	m.Register("cluster-a")
	m.Touch("cluster-a")
	m.Deregister("cluster-a")

	if len(m.ActiveClusters()) != 0 {
		t.Error("expected no active clusters after deregister")
	}

	// Re-register as a fresh connection
	m.Register("cluster-a")
	if len(m.ActiveClusters()) != 1 {
		t.Error("expected 1 active cluster after re-register")
	}
}
