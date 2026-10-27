package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

func TestLoadMissingSaveRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maildo", "config.yaml")
	reg, err := Load(path, []string{"outlook"})
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if reg.Version != 1 || len(reg.Accounts) != 0 {
		t.Fatalf("missing registry = %#v", reg)
	}
	reg.Outlook = &OutlookConfig{ClientID: "client-id"}
	reg.ActiveAccount = "work"
	reg.Accounts = []Account{{
		Name:              "work",
		Provider:          "outlook",
		Email:             "work@example.com",
		DisplayName:       "Work",
		ProviderAccountID: "provider-id",
		DefaultFrom:       "work@example.com",
	}}
	if err := Save(path, reg, []string{"outlook"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 0600", got)
	}
	dirSt, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirSt.Mode().Perm(); got != 0o700 {
		t.Fatalf("config dir mode = %o, want 0700", got)
	}
	loaded, err := Load(path, []string{"outlook"})
	if err != nil {
		t.Fatalf("Load saved: %v", err)
	}
	if loaded.Outlook == nil || loaded.Outlook.ClientID != "client-id" {
		t.Fatalf("outlook client id not preserved: %#v", loaded.Outlook)
	}
	got, ok := loaded.Find("work")
	if !ok {
		t.Fatal("saved account not found")
	}
	if got.AuthRef != "outlook/work" || got.ProviderAccountID != "provider-id" || got.DefaultFrom != "work@example.com" {
		t.Fatalf("loaded account = %#v", got)
	}
	if migrated, err := Migrate(loaded); err != nil || migrated != loaded {
		t.Fatalf("Migrate v1 = (%#v, %v), want identity nil", migrated, err)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "empty", body: "", code: errs.CodeInvalidConfig},
		{name: "versionless", body: "accounts: []\n", code: errs.CodeInvalidConfig},
		{name: "malformed", body: "version: 1\naccounts:\n  - [", code: errs.CodeInvalidConfig},
		{name: "future", body: "version: 2\n", code: errs.CodeUnsupportedConfigVersion},
		{name: "duplicate", body: "version: 1\naccounts:\n  - name: work\n    provider: outlook\n  - name: work\n    provider: outlook\n", code: errs.CodeInvalidConfig},
		{name: "invalid stored name", body: "version: 1\naccounts:\n  - name: Work\n    provider: outlook\n", code: errs.CodeInvalidConfig},
		{name: "bad active", body: "version: 1\nactive_account: missing\naccounts:\n  - name: work\n    provider: outlook\n", code: errs.CodeInvalidConfig},
		{name: "bad auth ref", body: "version: 1\naccounts:\n  - name: work\n    provider: outlook\n    auth_ref: outlook/other\n", code: errs.CodeInvalidConfig},
		{name: "unknown provider", body: "version: 1\naccounts:\n  - name: work\n    provider: fake\n", code: errs.CodeInvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path, []string{"outlook"})
			assertErrCode(t, err, tt.code)
		})
	}
}

func TestLoadReadErrorIsPathFreeProductError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir config path: %v", err)
	}
	_, err := Load(path, []string{"outlook"})
	assertErrCode(t, err, errs.CodeInvalidConfig)
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), filepath.Dir(path)) {
		t.Fatalf("error leaked path: %v", err)
	}
}

func TestFakeProviderValidityDependsOnRecognizedProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 1\naccounts:\n  - name: work\n    provider: fake\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, []string{"outlook"}); err == nil {
		t.Fatal("Load with production providers succeeded, want invalid_config")
	}
	if _, err := Load(path, []string{"fake", "outlook"}); err != nil {
		t.Fatalf("Load with fake provider recognized: %v", err)
	}
	loaded, err := Load(path, []string{"fake", "outlook"})
	if err != nil {
		t.Fatalf("Load for auth_ref check: %v", err)
	}
	account, ok := loaded.Find("work")
	if !ok || account.AuthRef != "fake/work" {
		t.Fatalf("derived auth_ref = (%#v, %t), want fake/work true", account, ok)
	}
}

func TestProviderSetupRejectsUnknownProvider(t *testing.T) {
	if err := (ProviderSetup{}).ValidateFor("fake"); err == nil {
		t.Fatal("ValidateFor accepted unknown provider with empty setup")
	}
	reg := &Registry{Version: 1}
	account := Account{Name: "work", Provider: "fake"}
	if _, err := SetupFor(reg, account); err == nil {
		t.Fatal("SetupFor accepted unknown provider")
	}
	if err := ApplySetup(reg, &account, ProviderSetup{}); err == nil {
		t.Fatal("ApplySetup accepted unknown provider")
	}
}

func TestMailServerCanonicalSetup(t *testing.T) {
	reg := &Registry{Version: 1, ActiveAccount: "hosted", Accounts: []Account{{
		Name:        "hosted",
		Provider:    "imap",
		Email:       "Me@xn--exmple-cua.com",
		DefaultFrom: "Me@xn--exmple-cua.com",
		Username:    "login@example.com",
		AuthRef:     "imap/hosted",
		MailServer: &MailServerConfig{
			IMAP: ServerEndpoint{Host: "imap.example.com", Port: 993, TLS: TLSImplicit},
			SMTP: ServerEndpoint{Host: "2001:db8::1", Port: 465, TLS: TLSStartTLS},
		},
	}}}
	data, err := Encode(reg, []string{"outlook", "gmail", "imap", "fastmail"})
	if err != nil {
		t.Fatalf("Encode canonical IMAP: %v", err)
	}
	loaded, err := Decode(data, []string{"outlook", "gmail", "imap", "fastmail"})
	if err != nil {
		t.Fatalf("Decode canonical IMAP: %v", err)
	}
	account, ok := loaded.Find("hosted")
	if !ok || account.MailServer == nil || account.MailServer.SMTP.Host != "2001:db8::1" {
		t.Fatalf("loaded account = %#v", account)
	}

	bad := strings.Replace(string(data), "imap.example.com", "IMAP.Example.COM", 1)
	if _, err := Decode([]byte(bad), []string{"outlook", "gmail", "imap", "fastmail"}); err == nil {
		t.Fatal("Decode accepted noncanonical mail server host")
	}
	bad = strings.Replace(string(data), "Me@xn--exmple-cua.com", "Me@Exämple.COM", 1)
	if _, err := Decode([]byte(bad), []string{"outlook", "gmail", "imap", "fastmail"}); err == nil {
		t.Fatal("Decode accepted noncanonical configured email")
	}
}

func TestFastmailStoredProfileIsFixed(t *testing.T) {
	reg := &Registry{Version: 1, Accounts: []Account{{
		Name:        "personal",
		Provider:    "fastmail",
		Email:       "me@fastmail.com",
		DefaultFrom: "me@fastmail.com",
		Username:    "me@fastmail.com",
		AuthRef:     "fastmail/personal",
		MailServer:  &MailServerConfig{IMAP: FastmailIMAPEndpoint(), SMTP: FastmailSMTPEndpoint()},
	}}}
	if _, err := Encode(reg, []string{"outlook", "gmail", "imap", "fastmail"}); err != nil {
		t.Fatalf("canonical Fastmail rejected: %v", err)
	}
	reg.Accounts[0].Username = "alias@fastmail.com"
	if _, err := Encode(reg, []string{"outlook", "gmail", "imap", "fastmail"}); err == nil {
		t.Fatal("Fastmail alias username accepted")
	}
	reg.Accounts[0].Username = reg.Accounts[0].Email
	reg.Accounts[0].MailServer.SMTP.Port = 587
	if _, err := Encode(reg, []string{"outlook", "gmail", "imap", "fastmail"}); err == nil {
		t.Fatal("Fastmail endpoint override accepted")
	}
}

func TestSaveRequiresExplicitRecognizedProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	reg := &Registry{Version: 1, Accounts: []Account{{Name: "work", Provider: "fake"}}}
	if err := Save(path, reg, nil); err == nil {
		t.Fatal("Save inferred providers from registry")
	}
	if err := Save(path, reg, []string{"fake"}); err != nil {
		t.Fatalf("Save with explicit provider: %v", err)
	}
}

func TestPortableAccountAndProviderNamesRejectDeviceBasenames(t *testing.T) {
	for _, name := range []string{"nul", "con", "com1", "lpt9", "com¹", "lpt³"} {
		if IsValidAccountName(name) {
			t.Fatalf("account name %q accepted", name)
		}
		if IsValidProviderName(name) {
			t.Fatalf("provider name %q accepted", name)
		}
	}
	if !IsValidAccountName("work-01") {
		t.Fatal("valid account rejected")
	}
	if !IsValidProviderName("provider_01") {
		t.Fatal("valid provider rejected")
	}
}

func TestRegistryLimits(t *testing.T) {
	reg := &Registry{Version: 1}
	for i := 0; i < MaxRegistryAccounts+1; i++ {
		reg.Accounts = append(reg.Accounts, Account{
			Name:     fmt.Sprintf("acct%04d", i),
			Provider: "outlook",
			AuthRef:  DeriveAuthRef("outlook", fmt.Sprintf("acct%04d", i)),
		})
	}
	if _, err := Encode(reg, []string{"outlook"}); err == nil {
		t.Fatal("over-account-limit registry encoded")
	}

	providers := make([]string, MaxRecognizedProviders+1)
	for i := range providers {
		providers[i] = fmt.Sprintf("p%03d", i)
	}
	if _, err := Encode(&Registry{Version: 1}, providers); err == nil {
		t.Fatal("over-provider-limit set accepted")
	}

	tooLarge := append([]byte("version: 1\n# "), bytes.Repeat([]byte("x"), MaxRegistryBytes)...)
	if _, err := Decode(tooLarge, []string{"outlook"}); err == nil {
		t.Fatal("over-byte-limit registry decoded")
	}
}

func TestAccountNameValidation(t *testing.T) {
	valid := []string{"a", "work", "work-1", "work_1", "a123"}
	for _, name := range valid {
		if err := ValidateAccountName(name); err != nil {
			t.Fatalf("ValidateAccountName(%q): %v", name, err)
		}
	}
	invalid := []string{
		"", "Work", "1work", ".", "..", "../work", "work/name", `work\name`,
		"work name", "work\nname", "http://work", "all", "active", "current",
		"default", "none", "null", "a123456789012345678901234567890123456789012345678901234567890123",
	}
	for _, name := range invalid {
		err := ValidateAccountName(name)
		assertErrCode(t, err, errs.CodeInvalidAccountName)
	}
}

func assertErrCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want *errs.Error", err)
	}
	if e.Code() != code {
		t.Fatalf("code = %q, want %q (err=%v)", e.Code(), code, err)
	}
	if e.ExitCode() != 1 {
		t.Fatalf("exit = %d, want 1", e.ExitCode())
	}
}
