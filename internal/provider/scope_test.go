package provider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestResolveAccountsDirectInvalidScopeBranches(t *testing.T) {
	account := maildoconfig.Account{
		Name:     "fake-acct",
		Provider: "fake",
		AuthRef:  "fake/fake-acct",
	}
	registry := &maildoconfig.Registry{Version: 1, ActiveAccount: "fake-acct", Accounts: []maildoconfig.Account{account}}
	loader := func(context.Context) (*maildoconfig.Registry, error) { return registry.Clone(), nil }
	reg := testkit.NewProviderRegistry(testkit.NewFakeProvider("fake"))

	t.Run("account and all accounts", func(t *testing.T) {
		cmd := &cobra.Command{Use: "x"}
		req := provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true}
		provider.ConfigureAccountFlags(cmd, req)
		if err := cmd.ParseFlags([]string{"--account", "fake-acct", "--all-accounts"}); err != nil {
			t.Fatalf("ParseFlags: %v", err)
		}
		_, err := resolveAccountsForTest(t, cmd, reg, loader, testAccountBinder, req)
		assertProviderErrorCode(t, err, errs.CodeInvalidAccountScope)
	})

	t.Run("bulk empty targets", func(t *testing.T) {
		cmd := &cobra.Command{Use: "x"}
		req := provider.ScopeRequest{Mode: provider.ModeBulkQualified}
		provider.ConfigureAccountFlags(cmd, req)
		_, err := resolveAccountsForTest(t, cmd, reg, loader, testAccountBinder, req)
		assertProviderErrorCode(t, err, errs.CodeInvalidAccountScope)
	})

	t.Run("bulk unqualified target", func(t *testing.T) {
		cmd := &cobra.Command{Use: "x"}
		req := provider.ScopeRequest{
			Mode:                    provider.ModeBulkQualified,
			AccountQualifiedTargets: []provider.AccountQualifiedTarget{{ID: "plain-id"}},
		}
		provider.ConfigureAccountFlags(cmd, req)
		_, err := resolveAccountsForTest(t, cmd, reg, loader, testAccountBinder, req)
		assertProviderErrorCode(t, err, errs.CodeInvalidAccountScope)
	})
}

func TestResolveAccountsRejectsExplicitEmptyFlagBeforeConfigLoad(t *testing.T) {
	reg := testkit.NewProviderRegistry(testkit.NewFakeProvider("fake"))
	loader := func(context.Context) (*maildoconfig.Registry, error) {
		t.Fatal("loader called before explicit empty account rejection")
		return nil, nil
	}

	for _, tt := range []struct {
		name string
		req  provider.ScopeRequest
	}{
		{name: "single", req: provider.ScopeRequest{Mode: provider.ModeSingle}},
		{name: "read many", req: provider.ScopeRequest{Mode: provider.ModeMulti}},
		{name: "all accounts", req: provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true}},
		{name: "account qualified", req: provider.ScopeRequest{Mode: provider.ModeBulkQualified, AccountQualifiedTargets: []provider.AccountQualifiedTarget{{Account: "fake-acct", ID: "message-id"}}}},
	} {
		for _, args := range [][]string{{"--account", ""}, {"--account="}} {
			t.Run(tt.name+" "+args[0], func(t *testing.T) {
				cmd := &cobra.Command{Use: "x"}
				provider.ConfigureAccountFlags(cmd, tt.req)
				if err := cmd.ParseFlags(args); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}
				_, err := resolveAccountsForTest(t, cmd, reg, loader, testAccountBinder, tt.req)
				assertProviderErrorCode(t, err, errs.CodeInvalidAccountScope)
			})
		}
	}
}

func testAccountBinder(req provider.AccountClientRequest) (provider.AccountClientOpenFunc, error) {
	return func(context.Context) (provider.AccountClient, error) {
		return testkit.FakeAccountClient{Account: req.Binding.Account, Provider: req.Binding.Provider}, nil
	}, nil
}

func resolveAccountsForTest(t *testing.T, cmd *cobra.Command, reg *provider.Registry, loader provider.RegistrySnapshotLoader, binder provider.AccountClientBinder, req provider.ScopeRequest) (provider.AccountSelection, error) {
	t.Helper()
	var out provider.AccountSelection
	err := provider.WithAccountClientScope(context.Background(), func(scope provider.AccountClientScope) error {
		var err error
		out, err = provider.ResolveAccounts(scope, cmd, reg, loader, binder, req)
		return err
	})
	return out, err
}

func assertProviderErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("error = %T %v, want product error %q", err, err, code)
	}
	if product.Code() != code {
		t.Fatalf("code = %q, want %q", product.Code(), code)
	}
}
