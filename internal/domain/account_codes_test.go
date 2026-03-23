package domain_test

import (
	"testing"

	"github.com/cshubhamrao/cloud-credit-system/internal/domain"
)

func TestAccountCodeFromType_AllValid(t *testing.T) {
	cases := []struct {
		typ  domain.AccountType
		want domain.AccountCode
	}{
		{domain.AccountTypeOperator, domain.AccountCodeOperator},
		{domain.AccountTypeTenantQuota, domain.AccountCodeTenantQuota},
		{domain.AccountTypeSink, domain.AccountCodeSink},
	}
	for _, tc := range cases {
		got := domain.AccountCodeFromType(tc.typ)
		if got != tc.want {
			t.Errorf("AccountCodeFromType(%q) = %d, want %d", tc.typ, got, tc.want)
		}
	}
}

func TestAccountCodeFromType_Values(t *testing.T) {
	if domain.AccountCodeFromType(domain.AccountTypeOperator) != 1 {
		t.Error("operator code must be 1")
	}
	if domain.AccountCodeFromType(domain.AccountTypeTenantQuota) != 2 {
		t.Error("tenant_quota code must be 2")
	}
	if domain.AccountCodeFromType(domain.AccountTypeSink) != 3 {
		t.Error("sink code must be 3")
	}
}

func TestAccountCodeFromType_UnknownPanics(t *testing.T) {
	cases := []domain.AccountType{"invalid", "", "OPERATOR", "Operator", "tenant quota"}
	for _, typ := range cases {
		typ := typ
		t.Run(string(typ), func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("AccountCodeFromType(%q) should have panicked", typ)
				}
			}()
			domain.AccountCodeFromType(typ)
		})
	}
}

func TestAccountCodes_Unique(t *testing.T) {
	seen := make(map[domain.AccountCode]domain.AccountType)
	for _, typ := range []domain.AccountType{
		domain.AccountTypeOperator,
		domain.AccountTypeTenantQuota,
		domain.AccountTypeSink,
	} {
		code := domain.AccountCodeFromType(typ)
		if prev, dup := seen[code]; dup {
			t.Errorf("account code %d shared by %q and %q — codes must be unique", code, prev, typ)
		}
		seen[code] = typ
	}
}
