// Package domain contains the core constants, types, and invariants for the
// cloud credit system. Nothing in this package imports from generated code.
package domain

// ResourceType identifies a billable resource. Each resource maps to a
// dedicated TigerBeetle ledger (ledger ID = uint32).
type ResourceType string

const (
	ResourceCPUHours       ResourceType = "cpu_hours"
	ResourceMemoryGBHours  ResourceType = "memory_gb_hours"
	ResourceActiveNodes    ResourceType = "active_nodes"
)

// PoC resources in order; extend for production.
var AllResources = []ResourceType{
	ResourceCPUHours,
	ResourceMemoryGBHours,
	ResourceActiveNodes,
}

// LedgerID returns the TigerBeetle ledger ID for a resource type.
// Ledger IDs are stable and must never change after accounts are created.
func (r ResourceType) LedgerID() uint32 {
	switch r {
	case ResourceCPUHours:
		return 1
	case ResourceMemoryGBHours:
		return 2
	case ResourceActiveNodes:
		return 3
	default:
		panic("unknown resource type: " + string(r))
	}
}

// IsGauge returns true for point-in-time resources (active_nodes).
// Gauge resources are tracked with pending transfers that expire on missed heartbeats.
func (r ResourceType) IsGauge() bool {
	return r == ResourceActiveNodes
}

// ResourceTypeFromLedger returns the ResourceType for a given TigerBeetle ledger ID.
func ResourceTypeFromLedger(ledgerID uint32) (ResourceType, bool) {
	for _, r := range AllResources {
		if r.LedgerID() == ledgerID {
			return r, true
		}
	}
	return "", false
}
