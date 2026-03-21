package domain

// AccountCode is the 16-bit code field on a TigerBeetle account.
// Codes identify the account's role in the double-entry structure.
type AccountCode uint16

const (
	// AccountCodeOperator (1): global virtual debit source per ledger.
	// One operator account per resource type, shared across all tenants.
	// Flags: 0 (debit balance grows indefinitely — expected behavior).
	AccountCodeOperator AccountCode = 1

	// AccountCodeTenantQuota (2): tenant's prepaid wallet per resource.
	// One per (tenant, resource_type) pair.
	// Flags: DebitsMustNotExceedCredits | History
	AccountCodeTenantQuota AccountCode = 2

	// AccountCodeSink (3): global archived-usage collector per ledger.
	// One sink account per resource type, shared across all tenants.
	// Flags: 0
	AccountCodeSink AccountCode = 3
)

// AccountType is the string representation used in PostgreSQL tb_account_ids.
type AccountType string

const (
	AccountTypeOperator    AccountType = "operator"
	AccountTypeTenantQuota AccountType = "tenant_quota"
	AccountTypeSink        AccountType = "sink"
)

// AccountCodeFromType converts an AccountType string to its numeric code.
func AccountCodeFromType(t AccountType) AccountCode {
	switch t {
	case AccountTypeOperator:
		return AccountCodeOperator
	case AccountTypeTenantQuota:
		return AccountCodeTenantQuota
	case AccountTypeSink:
		return AccountCodeSink
	default:
		panic("unknown account type: " + string(t))
	}
}
