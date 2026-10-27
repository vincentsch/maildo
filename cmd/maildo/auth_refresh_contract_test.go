package main

import (
	"context"
	"path/filepath"
	"testing"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
)

func TestAuthRefreshCheckedIsOwnedByOrchestrator(t *testing.T) {
	base := testkit.NewFakeProvider("fake")
	root := t.TempDir()
	h := &cliHarness{
		t:          t,
		root:       root,
		configPath: filepath.Join(root, "config.yaml"),
		providers:  testkit.NewProviderRegistry(uncheckedRefreshProvider{FakeProvider: base}),
	}
	account := workAccount()
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{account})
	if err := testkit.SeedCredential(h.root, account, nil); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var result refreshResult
	decodeOK(t, h.run("auth", "refresh", "work", "--json"), &result)
	if !result.RefreshChecked || result.CredentialChanged || result.RotatedRefreshToken {
		t.Fatalf("refresh result = %#v", result)
	}
}

type uncheckedRefreshProvider struct {
	*testkit.FakeProvider
}

func (p uncheckedRefreshProvider) ForcedRefresh(ctx context.Context, ac provider.StoredAuthContext) (provider.RefreshResult, error) {
	result, err := p.FakeProvider.ForcedRefresh(ctx, ac)
	result.RefreshChecked = false
	return result, err
}
