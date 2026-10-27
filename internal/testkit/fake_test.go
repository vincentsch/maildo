package testkit

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestFakeArchivePolicyClientNormalizesWarningsWithoutFolderReads(t *testing.T) {
	client := FakeArchivePolicyClient{
		Account:  "work",
		Provider: "outlook",
		Report:   mail.ArchivePolicyReport{},
	}
	var accountClient provider.AccountClient = client
	if _, ok := accountClient.(provider.FolderReader); ok {
		t.Fatal("archive policy client unexpectedly implements FolderReader")
	}

	report, err := client.ArchivePolicyReport(context.Background(), mail.ArchivePolicyReportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Warnings == nil {
		t.Fatal("archive policy warnings were not normalized to a non-nil slice")
	}
}

func TestFakeProviderAccountClientAccountsClone(t *testing.T) {
	fake := NewFakeProvider("fake")
	fake.AccountClientErr = errors.New("stop before cache")
	_, _ = fake.NewAccountClient(context.Background(), provider.StoredAuthContext{Account: "work", Provider: "fake"})

	first := fake.AccountClientAccounts()
	first[0] = "mutated"
	second := fake.AccountClientAccounts()
	if !reflect.DeepEqual(second, []string{"work"}) {
		t.Fatalf("account snapshot = %#v, want cloned work entry", second)
	}
}

func TestFakeProviderAccountClientAccountsConcurrent(t *testing.T) {
	fake := NewFakeProvider("fake")
	fake.AccountClientErr = errors.New("stop before cache")
	accounts := []string{"work", "personal", "archive", "work", "personal", "ops"}
	var wg sync.WaitGroup
	for _, account := range accounts {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fake.NewAccountClient(context.Background(), provider.StoredAuthContext{Account: account, Provider: "fake"})
		}()
	}
	for i := 0; i < len(accounts)*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = fake.AccountClientAccounts()
		}()
	}
	wg.Wait()

	got := fake.AccountClientAccounts()
	sort.Strings(got)
	want := append([]string(nil), accounts...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("account snapshot = %#v, want %#v", got, want)
	}
}

func TestFakeProviderClientsRejectOperationsAfterDestroy(t *testing.T) {
	fake := NewFakeFolderProvider("fake", SampleFolders())
	client, err := fake.NewAccountClient(context.Background(), provider.StoredAuthContext{
		Provider:   "fake",
		Account:    "work",
		Credential: staticCredentialSession{current: fake.cache("work")},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := client.(provider.FolderReader)
	if !ok {
		t.Fatalf("client = %T, want FolderReader", client)
	}
	client.Destroy()
	_, err = reader.ListFolders(context.Background(), mail.FolderListRequest{})
	assertFakeProductCode(t, err, errs.CodeProviderClientClosed)
}

func TestFakeRuleCreatePersistsProviderFieldPresence(t *testing.T) {
	state := NewRuleState(map[string][]mail.Rule{"work": {}})
	client := FakeRuleMutationClient{
		FakeRuleClient: FakeRuleClient{FakeFolderClient: FakeFolderClient{Account: "work", Provider: "fake"}},
		state:          state,
	}
	want := mail.RuleProviderFieldPresence{
		ConditionsHasAttachments:                 true,
		ActionsMarkAsRead:                        true,
		ActionsStopProcessingRules:               true,
		ActionsStopProcessingRulesValueAvailable: true,
	}
	receipt, err := client.CreateRule(context.Background(), mail.RuleCreateRequest{Rule: mail.Rule{
		DisplayName:           "Presence",
		ProviderFieldPresence: want,
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.GetRule(context.Background(), mail.RuleGetRequest{RuleID: receipt.RuleID})
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderFieldPresence != want {
		t.Fatalf("presence = %#v, want %#v", got.ProviderFieldPresence, want)
	}
}

type staticCredentialSession struct {
	current provider.Credential
}

func (s staticCredentialSession) WithCurrent(ctx context.Context, fn func(provider.CredentialLease) error) error {
	_ = ctx
	return fn(staticCredentialLease{current: s.current})
}

type staticCredentialLease struct {
	current provider.Credential
}

func (l staticCredentialLease) Current() provider.Credential {
	return l.current
}

func (l staticCredentialLease) Replace(provider.Credential) error {
	return nil
}

func assertFakeProductCode(t *testing.T, err error, code string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != code {
		t.Fatalf("error = %#v, want product code %q", err, code)
	}
}

func TestFakeRuleStateClonesNestedRuleData(t *testing.T) {
	state := NewRuleState(map[string][]mail.Rule{"work": {{
		ID:             "rule-1",
		Conditions:     mail.RulePredicates{SubjectContains: []string{"kept"}},
		ProviderMeta:   map[string]any{"nested": map[string]any{"value": "kept"}},
		ProviderState:  map[string]any{"actions": map[string]any{"nested": []any{"kept"}}},
		HealthFindings: []mail.RuleHealthFinding{{Code: "fixture", Details: map[string]any{"nested": []any{"kept"}}}},
	}}})

	first, err := state.get("work", "rule-1")
	if err != nil {
		t.Fatal(err)
	}
	first.Conditions.SubjectContains[0] = "changed"
	first.ProviderMeta["nested"].(map[string]any)["value"] = "changed"
	first.ProviderState["actions"].(map[string]any)["nested"].([]any)[0] = "changed"
	first.HealthFindings[0].Details["nested"].([]any)[0] = "changed"

	got, err := state.get("work", "rule-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Conditions.SubjectContains[0] != "kept" ||
		got.ProviderMeta["nested"].(map[string]any)["value"] != "kept" ||
		got.ProviderState["actions"].(map[string]any)["nested"].([]any)[0] != "kept" ||
		got.HealthFindings[0].Details["nested"].([]any)[0] != "kept" {
		t.Fatalf("fake state was aliased: %#v", got)
	}
}
