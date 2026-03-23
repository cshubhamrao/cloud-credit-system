// Package activities_test contains integration tests for TBActivities and PGActivities.
// These tests require live TigerBeetle and PostgreSQL — run with `make test-integration`.
package activities_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/cshubhamrao/cloud-credit-system/internal/accounting"
	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
	"github.com/cshubhamrao/cloud-credit-system/internal/ledger"
)

// getTBClient opens a TigerBeetle client for integration tests.
func getTBClient(t *testing.T) *ledger.Client {
	t.Helper()
	addr := os.Getenv("TIGERBEETLE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:3000"
	}
	c, err := ledger.NewClient(0, addr)
	require.NoError(t, err, "failed to connect to TigerBeetle — is docker-compose up?")
	t.Cleanup(func() { c.Close() })
	return c
}

// getPool opens a pgx connection pool for integration tests.
func getPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/creditdb?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err, "failed to connect to PostgreSQL — is docker-compose up?")
	t.Cleanup(func() { pool.Close() })
	return pool
}

// tbTestFixture holds a pre-provisioned TB state for a single test tenant.
type tbTestFixture struct {
	acts      *accounting.TBActivities
	globalIDs map[ledger.GlobalAccountKey]types.Uint128
	// Per-tenant state
	tenantUUID [16]byte
	quotaIDs   map[string][16]byte // resource_type → tenant quota account ID bytes
	opIDs      map[string][16]byte // resource_type → global operator ID bytes
	sinkIDs    map[string][16]byte // resource_type → global sink ID bytes
}

// newTBFixture creates global + tenant accounts and returns a ready-to-use fixture.
func newTBFixture(t *testing.T) *tbTestFixture {
	t.Helper()
	c := getTBClient(t)
	acts := accounting.NewTBActivities(c)

	globalIDs, err := ledger.CreateGlobalAccounts(c, nil)
	require.NoError(t, err)

	tenantUUID := ledger.Uint128ToBytes(ledger.RandomID())
	res, err := acts.CreateTenantTBAccounts(context.Background(), accounting.CreateTenantTBAccountsInput{
		TenantUUID: tenantUUID,
	})
	require.NoError(t, err)

	fx := &tbTestFixture{
		acts:       acts,
		globalIDs:  globalIDs,
		tenantUUID: tenantUUID,
		quotaIDs:   res.AccountIDs,
		opIDs:      make(map[string][16]byte),
		sinkIDs:    make(map[string][16]byte),
	}
	for _, r := range domain.AllResources {
		opKey := ledger.GlobalAccountKey{Resource: r, AccountType: domain.AccountTypeOperator}
		sinkKey := ledger.GlobalAccountKey{Resource: r, AccountType: domain.AccountTypeSink}
		if id, ok := globalIDs[opKey]; ok {
			fx.opIDs[string(r)] = ledger.Uint128ToBytes(id)
		}
		if id, ok := globalIDs[sinkKey]; ok {
			fx.sinkIDs[string(r)] = ledger.Uint128ToBytes(id)
		}
	}
	return fx
}

// allocate credits via SubmitAllocationTransfers.
func (fx *tbTestFixture) allocate(t *testing.T, credits map[string]int64) {
	t.Helper()
	err := fx.acts.SubmitAllocationTransfers(context.Background(), accounting.SubmitAllocationInput{
		TenantUUID:        fx.tenantUUID,
		TenantQuotaIDs:    fx.quotaIDs,
		GlobalOperatorIDs: fx.opIDs,
		Credits:           credits,
	})
	require.NoError(t, err)
}
