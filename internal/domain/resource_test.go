package domain_test

import (
	"testing"

	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
)

func TestAllResources_Count(t *testing.T) {
	if len(domain.AllResources) != 3 {
		t.Errorf("expected 3 resources, got %d", len(domain.AllResources))
	}
}

func TestAllResources_Order(t *testing.T) {
	want := []domain.ResourceType{
		domain.ResourceCPUHours,
		domain.ResourceMemoryGBHours,
		domain.ResourceActiveNodes,
	}
	for i, r := range domain.AllResources {
		if r != want[i] {
			t.Errorf("AllResources[%d] = %q, want %q", i, r, want[i])
		}
	}
}

func TestLedgerID(t *testing.T) {
	cases := []struct {
		resource domain.ResourceType
		wantID   uint32
	}{
		{domain.ResourceCPUHours, 1},
		{domain.ResourceMemoryGBHours, 2},
		{domain.ResourceActiveNodes, 3},
	}
	for _, tc := range cases {
		if got := tc.resource.LedgerID(); got != tc.wantID {
			t.Errorf("%s.LedgerID() = %d, want %d", tc.resource, got, tc.wantID)
		}
	}
}

func TestResourceTypeFromLedger_RoundTrip(t *testing.T) {
	for _, r := range domain.AllResources {
		got, ok := domain.ResourceTypeFromLedger(r.LedgerID())
		if !ok {
			t.Errorf("ResourceTypeFromLedger(%d): not found", r.LedgerID())
		}
		if got != r {
			t.Errorf("ResourceTypeFromLedger(%d) = %q, want %q", r.LedgerID(), got, r)
		}
	}
}

func TestResourceTypeFromLedger_Unknown(t *testing.T) {
	got, ok := domain.ResourceTypeFromLedger(999)
	if ok {
		t.Errorf("ResourceTypeFromLedger(999) returned ok=true with value %q", got)
	}
}

func TestIsGauge(t *testing.T) {
	cases := []struct {
		resource domain.ResourceType
		wantGauge bool
	}{
		{domain.ResourceCPUHours, false},
		{domain.ResourceMemoryGBHours, false},
		{domain.ResourceActiveNodes, true},
	}
	for _, tc := range cases {
		if got := tc.resource.IsGauge(); got != tc.wantGauge {
			t.Errorf("%s.IsGauge() = %v, want %v", tc.resource, got, tc.wantGauge)
		}
	}
}

func TestLedgerIDs_Unique(t *testing.T) {
	seen := make(map[uint32]domain.ResourceType)
	for _, r := range domain.AllResources {
		id := r.LedgerID()
		if prev, dup := seen[id]; dup {
			t.Errorf("ledger ID %d shared by %q and %q — IDs must be unique", id, prev, r)
		}
		seen[id] = r
	}
}
