package ledger

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
)

// GlobalAccountKey uniquely identifies a global (non-tenant) account.
type GlobalAccountKey struct {
	Resource    domain.ResourceType
	AccountType domain.AccountType
}

// CreateGlobalAccounts creates the global operator and sink accounts for all
// PoC resource types. These are created once at server startup (idempotent —
// TigerBeetle returns AccountExistsWithDifferentFlags only on true conflicts).
//
// Returns a map from (resource, accountType) → TigerBeetle account ID.
func CreateGlobalAccounts(c *Client, idMap map[GlobalAccountKey]types.Uint128) (map[GlobalAccountKey]types.Uint128, error) {
	result := make(map[GlobalAccountKey]types.Uint128)

	var accounts []types.Account
	var keys []GlobalAccountKey

	for _, r := range domain.AllResources {
		for _, at := range []domain.AccountType{domain.AccountTypeOperator, domain.AccountTypeSink} {
			key := GlobalAccountKey{Resource: r, AccountType: at}

			// Reuse existing ID if provided; otherwise generate a new random one.
			id, ok := idMap[key]
			if !ok {
				id = RandomID()
			}
			result[key] = id

			accounts = append(accounts, types.Account{
				ID:     id,
				Ledger: r.LedgerID(),
				Code:   uint16(domain.AccountCodeFromType(at)),
				// Operator and sink have no flags (debit balance grows unbounded).
			})
			keys = append(keys, key)
		}
	}

	results, err := c.tb.CreateAccounts(accounts)
	if err != nil {
		return nil, fmt.Errorf("CreateAccounts (global): %w", err)
	}

	for _, r := range results {
		if r.Result != types.AccountOK && r.Result != types.AccountExists {
			key := keys[r.Index]
			return nil, fmt.Errorf("global account %v/%v: %v", key.Resource, key.AccountType, r.Result)
		}
	}

	return result, nil
}

// CreateTenantQuotaAccounts creates the tenant_quota account for each PoC
// resource type. Returns a map from resource type → TigerBeetle account ID.
//
// Flags: DebitsMustNotExceedCredits | History
// — hard limit enforcement + balance history for reporting.
func CreateTenantQuotaAccounts(
	c *Client,
	tenantUUID [16]byte,
	existingIDs map[domain.ResourceType]types.Uint128,
) (map[domain.ResourceType]types.Uint128, error) {
	result := make(map[domain.ResourceType]types.Uint128)

	var accounts []types.Account
	var resources []domain.ResourceType

	tenantData := UUIDToUint128(tenantUUID)

	for _, r := range domain.AllResources {
		id, ok := existingIDs[r]
		if !ok {
			id = RandomID()
		}
		result[r] = id

		flags := types.AccountFlags{
			DebitsMustNotExceedCredits: true,
			History:                    true,
		}
		accounts = append(accounts, types.Account{
			ID:          id,
			Ledger:      r.LedgerID(),
			Code:        uint16(domain.AccountCodeTenantQuota),
			Flags:       flags.ToUint16(),
			UserData128: tenantData,
		})
		resources = append(resources, r)
	}

	results, err := c.tb.CreateAccounts(accounts)
	if err != nil {
		return nil, fmt.Errorf("CreateAccounts (tenant %x): %w", tenantUUID, err)
	}

	for _, res := range results {
		if res.Result != types.AccountOK && res.Result != types.AccountExists {
			return nil, fmt.Errorf("tenant account %v: %v", resources[res.Index], res.Result)
		}
	}

	return result, nil
}

// BalanceSnapshot holds a point-in-time balance for a single TB account.
type BalanceSnapshot struct {
	CreditsPosted uint64
	DebitsPosted  uint64
	DebitsPending uint64
}

// LookupBalances fetches current balances for accounts identified by their 16-byte IDs.
// The input map has resource_type string keys; the output preserves those keys.
// Accounts not found in TB are omitted from the result.
func LookupBalances(c *Client, accountIDs map[string][16]byte) (map[string]BalanceSnapshot, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}

	// Build parallel slices to correlate results back to resource keys.
	keys := make([]string, 0, len(accountIDs))
	ids := make([]types.Uint128, 0, len(accountIDs))
	for k, b := range accountIDs {
		keys = append(keys, k)
		ids = append(ids, UUIDToUint128(b))
	}

	accounts, err := c.tb.LookupAccounts(ids)
	if err != nil {
		return nil, fmt.Errorf("LookupAccounts: %w", err)
	}

	result := make(map[string]BalanceSnapshot, len(accounts))
	for i, wantID := range ids {
		for _, acc := range accounts {
			if acc.ID == wantID {
				result[keys[i]] = BalanceSnapshot{
					CreditsPosted: uint128Lo(acc.CreditsPosted),
					DebitsPosted:  uint128Lo(acc.DebitsPosted),
					DebitsPending: uint128Lo(acc.DebitsPending),
				}
				break
			}
		}
	}
	return result, nil
}

// uint128Lo extracts the lower 64 bits of a TigerBeetle Uint128 value.
// TigerBeetle stores integers in little-endian byte order; bytes 0–7 are the low word.
// All PoC amounts fit in uint64, so this conversion is safe.
func uint128Lo(v types.Uint128) uint64 {
	return binary.LittleEndian.Uint64(v[:8])
}

// LookupAccounts fetches current balances for the given account IDs.
func LookupAccounts(c *Client, ids []types.Uint128) ([]types.Account, error) {
	accounts, err := c.tb.LookupAccounts(ids)
	if err != nil {
		return nil, fmt.Errorf("LookupAccounts: %w", err)
	}
	return accounts, nil
}

// LookupAccount fetches a single account by ID. Returns nil if not found.
func LookupAccount(c *Client, id types.Uint128) (*types.Account, error) {
	_ = context.Background() // keep import; actual call is sync
	accounts, err := c.tb.LookupAccounts([]types.Uint128{id})
	if err != nil {
		return nil, fmt.Errorf("LookupAccount: %w", err)
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	a := accounts[0]
	return &a, nil
}
