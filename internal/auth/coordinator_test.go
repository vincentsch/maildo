package auth

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
)

func TestMain(m *testing.M) {
	if os.Getenv("MAILDO_AUTH_COORDINATOR_CHILD") == "1" {
		phase := TransactionPhase(os.Getenv("MAILDO_AUTH_PHASE"))
		if err := writeRecoverableJournal(os.Getenv("MAILDO_AUTH_ROOT"), os.Getenv("MAILDO_AUTH_CONFIG"), phase); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(97)
	}
	if os.Getenv("MAILDO_AUTH_COORDINATOR_CHILD") == "lock" {
		coord, err := NewCoordinator(os.Getenv("MAILDO_AUTH_CONFIG"), []string{"outlook"})
		if err != nil {
			os.Exit(2)
		}
		lock, err := coord.lock(context.Background())
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, "locked")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_ = lock.unlock()
		os.Exit(0)
	}
	if os.Getenv("MAILDO_AUTH_COORDINATOR_CHILD") == "crash_transaction" {
		if err := crashRenameTransaction(os.Getenv("MAILDO_AUTH_ROOT"), os.Getenv("MAILDO_AUTH_CONFIG"), TransactionPhase(os.Getenv("MAILDO_AUTH_PHASE"))); err != nil {
			os.Exit(2)
		}
		os.Exit(3) // The phase hook must exit before the transaction returns.
	}
	os.Exit(m.Run())
}

func TestNewCoordinatorValidationAndProviderSnapshotClone(t *testing.T) {
	root := t.TempDir()
	providers := []string{"outlook"}
	coord, err := NewCoordinator(filepath.Join(root, "config.yaml"), providers)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	providers[0] = "bad"
	if _, err := maildoconfig.Decode([]byte("version: 1\n"), coord.recognizedProviders); err != nil {
		t.Fatalf("provider snapshot was not cloned: %v", err)
	}

	for _, tt := range []struct {
		name      string
		config    string
		providers []string
	}{
		{name: "empty providers", config: "config.yaml", providers: nil},
		{name: "duplicate providers", config: "config.yaml", providers: []string{"outlook", "outlook"}},
		{name: "device provider", config: "config.yaml", providers: []string{"nul"}},
		{name: "auth basename", config: "AUTH", providers: []string{"outlook"}},
		{name: "device config", config: "nul.yaml", providers: []string{"outlook"}},
		{name: "forbidden config char", config: "bad:name.yaml", providers: []string{"outlook"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCoordinator(filepath.Join(root, tt.config), tt.providers); err == nil {
				t.Fatal("NewCoordinator succeeded")
			}
		})
	}
}

func TestCoordinatorDryRunVirginTreeCreatesNoLock(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	coord, err := NewCoordinator(configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	reg, err := coord.LoadRegistry(context.Background(), true)
	if err != nil {
		t.Fatalf("LoadRegistry dry-run: %v", err)
	}
	if reg.Version != 1 {
		t.Fatalf("version = %d", reg.Version)
	}
	if _, err := os.Stat(filepath.Join(root, "auth", ".transaction.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock stat err = %v, want not exist", err)
	}
}

func TestCoordinatorRevisionConflict(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	account := maildoconfig.Account{Name: "work", Provider: "outlook", AuthRef: "outlook/work"}
	reg := &maildoconfig.Registry{Version: 1, ActiveAccount: "work", Accounts: []maildoconfig.Account{account}}
	if err := maildoconfig.Save(configPath, reg, []string{"outlook"}); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	coord, err := NewCoordinator(configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	_, rev, _, _, err := coord.Snapshot(context.Background(), "outlook", "work")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	reg.ActiveAccount = ""
	if err := maildoconfig.Save(configPath, reg, []string{"outlook"}); err != nil {
		t.Fatalf("save changed registry: %v", err)
	}
	err = coord.SaveCredentialBytes(context.Background(), rev, "outlook", "work", []byte("{}"), errs.AuthStateRefresh)
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeProviderConflict {
		t.Fatalf("SaveCredentialBytes err = %T %v, want provider_conflict", err, err)
	}
}

func TestCoordinatorRecoversTransactionByPhase(t *testing.T) {
	for _, tt := range []struct {
		name      string
		phase     TransactionPhase
		wantName  string
		wantCache string
	}{
		{name: "prepared rolls back", phase: TransactionPrepared, wantName: "work", wantCache: "old"},
		{name: "cache installed rolls back", phase: TransactionCacheInstalled, wantName: "work", wantCache: "old"},
		{name: "registry installed rolls forward", phase: TransactionRegistryInstalled, wantName: "renamed", wantCache: "new"},
		{name: "committed rolls forward", phase: TransactionCommitted, wantName: "renamed", wantCache: "new"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "config.yaml")
			oldAccount := maildoconfig.Account{Name: "work", Provider: "outlook", AuthRef: "outlook/work"}
			newAccount := maildoconfig.Account{Name: "renamed", Provider: "outlook", AuthRef: "outlook/renamed"}
			oldReg := &maildoconfig.Registry{Version: 1, ActiveAccount: "work", Accounts: []maildoconfig.Account{oldAccount}}
			newReg := &maildoconfig.Registry{Version: 1, ActiveAccount: "renamed", Accounts: []maildoconfig.Account{newAccount}}
			oldRegBytes, _ := maildoconfig.Encode(oldReg, []string{"outlook"})
			newRegBytes, _ := maildoconfig.Encode(newReg, []string{"outlook"})
			if err := maildoconfig.Save(configPath, oldReg, []string{"outlook"}); err != nil {
				t.Fatalf("save old registry: %v", err)
			}
			if err := NewStore(root).SaveBytes("outlook", "work", []byte("old")); err != nil {
				t.Fatalf("save old cache: %v", err)
			}
			txid := strings.Repeat("1", 32)
			txDir := filepath.Join(root, "auth", ".transactions", txid)
			if err := os.MkdirAll(txDir, 0o700); err != nil {
				t.Fatalf("mkdir tx: %v", err)
			}
			mustWriteMaterial(t, txDir, "registry.prior", oldRegBytes)
			mustWriteMaterial(t, txDir, "registry.desired", newRegBytes)
			mustWriteMaterial(t, txDir, "cache.prior", []byte("old"))
			mustWriteMaterial(t, txDir, "cache.desired", []byte("new"))
			journal := Journal{
				Version:      1,
				TxID:         txid,
				Operation:    errs.AuthTransactionRename,
				Phase:        tt.phase,
				RegistryName: filepath.Base(configPath),
				Registry: RegistryImages{
					Prior:   presentImage("registry.prior", oldRegBytes),
					Desired: presentImage("registry.desired", newRegBytes),
				},
				Cache: CacheImages{
					OldPath: "outlook/work.json",
					NewPath: "outlook/renamed.json",
					Prior:   presentImage("cache.prior", []byte("old")),
					Desired: presentImage("cache.desired", []byte("new")),
				},
				OldBinding: CredentialBinding{Provider: "outlook", Account: "work"},
				NewBinding: CredentialBinding{Provider: "outlook", Account: "renamed"},
			}
			if tt.phase == TransactionRegistryInstalled || tt.phase == TransactionCommitted {
				if err := maildoconfig.Save(configPath, newReg, []string{"outlook"}); err != nil {
					t.Fatalf("install desired registry: %v", err)
				}
				if err := NewStore(root).SaveBytes("outlook", "renamed", []byte("new")); err != nil {
					t.Fatalf("install desired cache: %v", err)
				}
			}
			if err := writeJournal(txDir, journal); err != nil {
				t.Fatalf("write journal: %v", err)
			}
			coord, err := NewCoordinator(configPath, []string{"outlook"})
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			reg, err := coord.LoadRegistry(context.Background(), false)
			if err != nil {
				t.Fatalf("LoadRegistry recovery: %v", err)
			}
			if _, ok := reg.Find(tt.wantName); !ok {
				t.Fatalf("recovered registry = %#v, want account %q", reg.Accounts, tt.wantName)
			}
			cacheName := tt.wantName
			data, present, err := NewStore(root).LoadBytes("outlook", cacheName)
			if err != nil || !present || string(data) != tt.wantCache {
				t.Fatalf("cache %s = %q present=%v err=%v", cacheName, string(data), present, err)
			}
			zero(data)
			if _, err := os.Stat(txDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tx dir stat err = %v, want removed", err)
			}
		})
	}
}

func TestCoordinatorRecoversJournalLeftByCrashedProcess(t *testing.T) {
	for _, tt := range []struct {
		phase          TransactionPhase
		account, cache string
	}{
		{TransactionPrepared, "work", "old"},
		{TransactionCacheInstalled, "work", "old"},
		{TransactionRegistryInstalled, "renamed", "new"},
		{TransactionObsoleteCacheRemoved, "renamed", "new"},
		{TransactionCommitted, "renamed", "new"},
	} {
		t.Run(string(tt.phase), func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "config.yaml")
			cmd := exec.Command(os.Args[0], "-test.run=TestCoordinatorRecoversJournalLeftByCrashedProcess")
			cmd.Env = append(os.Environ(),
				"MAILDO_AUTH_COORDINATOR_CHILD=1",
				"MAILDO_AUTH_ROOT="+root,
				"MAILDO_AUTH_CONFIG="+configPath,
				"MAILDO_AUTH_PHASE="+string(tt.phase),
			)
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 97 {
				t.Fatalf("crash fixture exit = %v, want 97", err)
			}
			coord, err := NewCoordinator(configPath, []string{"outlook"})
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			reg, err := coord.LoadRegistry(context.Background(), false)
			if err != nil {
				t.Fatalf("LoadRegistry recovery: %v", err)
			}
			if _, ok := reg.Find(tt.account); !ok || reg.ActiveAccount != tt.account {
				t.Fatalf("recovered registry = %#v, want %s account", reg, tt.account)
			}
			data, present, err := NewStore(root).LoadBytes("outlook", tt.account)
			if err != nil || !present || string(data) != tt.cache {
				t.Fatalf("recovered cache = %q present=%v err=%v", string(data), present, err)
			}
			zero(data)
			other := "work"
			if tt.account == "work" {
				other = "renamed"
			}
			if _, present, err := NewStore(root).LoadBytes("outlook", other); err != nil || present {
				t.Fatalf("obsolete cache survived recovery: present=%t err=%v", present, err)
			}
			if _, err := os.Stat(filepath.Join(root, "auth", ".transactions", strings.Repeat("2", 32))); !os.IsNotExist(err) {
				t.Fatalf("recovered journal retained: %v", err)
			}
		})
	}
}

func TestCoordinatorRecoversInterruptedRenameTransaction(t *testing.T) {
	for _, tt := range []struct {
		phase          TransactionPhase
		account, cache string
	}{
		// Hooks run after installation but before that phase's journal write.
		{TransactionCacheInstalled, "work", "old"},
		{TransactionRegistryInstalled, "work", "old"},
		{TransactionObsoleteCacheRemoved, "renamed", "new"},
		{TransactionCommitted, "renamed", "new"},
	} {
		t.Run(string(tt.phase), func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "config.yaml")
			cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinatorRecoversInterruptedRenameTransaction$")
			cmd.Env = append(os.Environ(), "MAILDO_AUTH_COORDINATOR_CHILD=crash_transaction", "MAILDO_AUTH_ROOT="+root, "MAILDO_AUTH_CONFIG="+configPath, "MAILDO_AUTH_PHASE="+string(tt.phase))
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 97 {
				t.Fatalf("phase crash exit=%v", err)
			}
			coord, err := NewCoordinator(configPath, []string{"outlook"})
			if err != nil {
				t.Fatal(err)
			}
			reg, err := coord.LoadRegistry(context.Background(), false)
			if err != nil || reg.ActiveAccount != tt.account {
				t.Fatalf("recovered registry=%#v err=%v", reg, err)
			}
			data, present, err := NewStore(root).LoadBytes("outlook", tt.account)
			if err != nil || !present || string(data) != tt.cache {
				t.Fatalf("recovered cache present=%t err=%v", present, err)
			}
			zero(data)
		})
	}
}

func crashRenameTransaction(root, configPath string, phase TransactionPhase) error {
	oldAccount := maildoconfig.Account{Name: "work", Provider: "outlook", AuthRef: "outlook/work"}
	newAccount := maildoconfig.Account{Name: "renamed", Provider: "outlook", AuthRef: "outlook/renamed"}
	prior := &maildoconfig.Registry{Version: 1, ActiveAccount: "work", Accounts: []maildoconfig.Account{oldAccount}}
	if err := maildoconfig.Save(configPath, prior, []string{"outlook"}); err != nil {
		return err
	}
	if err := NewStore(root).SaveBytes("outlook", "work", []byte("old")); err != nil {
		return err
	}
	coord, err := NewCoordinator(configPath, []string{"outlook"})
	if err != nil {
		return err
	}
	_, rev, _, _, err := coord.Snapshot(context.Background(), "outlook", "work")
	if err != nil {
		return err
	}
	transactionPhaseHook = func(current TransactionPhase) error {
		if current == phase {
			os.Exit(97)
		}
		return nil
	}
	return coord.CommitTransaction(context.Background(), TransactionRequest{
		Operation: errs.AuthTransactionRename, Revision: rev,
		Registry:   &maildoconfig.Registry{Version: 1, ActiveAccount: "renamed", Accounts: []maildoconfig.Account{newAccount}},
		OldBinding: CredentialBinding{Provider: "outlook", Account: "work"}, NewBinding: CredentialBinding{Provider: "outlook", Account: "renamed"},
		DesiredCache: []byte("new"), DesiredCacheSet: true,
	}, errs.AuthStateRefresh)
}

func TestCoordinatorLockContentionAndProcessDeath(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	coord, err := NewCoordinator(configPath, []string{"outlook"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinatorLockContentionAndProcessDeath$")
	cmd.Env = append(os.Environ(), "MAILDO_AUTH_COORDINATOR_CHILD=lock", "MAILDO_AUTH_CONFIG="+configPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child lock receipt %q: %v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := coord.LoadRegistry(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending coordinator err=%v, want deadline exceeded", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := coord.LoadRegistry(ctx2, false); err != nil {
		t.Fatalf("process death left lock unusable: %v", err)
	}
}

func TestMalformedJournalEvidenceDoesNotRemoveRegistry(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	reg := &maildoconfig.Registry{Version: 1, Accounts: []maildoconfig.Account{{Name: "work", Provider: "outlook", AuthRef: "outlook/work"}}}
	if err := maildoconfig.Save(configPath, reg, []string{"outlook"}); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	txid := strings.Repeat("3", 32)
	txDir := filepath.Join(root, "auth", ".transactions", txid)
	if err := os.MkdirAll(txDir, 0o700); err != nil {
		t.Fatalf("mkdir tx: %v", err)
	}
	journal := Journal{
		Version:      1,
		TxID:         txid,
		Operation:    errs.AuthTransactionRename,
		Phase:        TransactionPrepared,
		RegistryName: filepath.Base(configPath),
		OldBinding:   CredentialBinding{Provider: "outlook", Account: "work"},
		NewBinding:   CredentialBinding{Provider: "outlook", Account: "renamed"},
		Cache: CacheImages{
			OldPath: "outlook/work.json",
			NewPath: "outlook/renamed.json",
		},
	}
	data, err := json.Marshal(journal)
	if err != nil {
		t.Fatalf("marshal journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(txDir, "journal.json"), data, 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	coord, err := NewCoordinator(configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	err = coord.CleanupStale(context.Background())
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeInvalidConfig {
		t.Fatalf("CleanupStale err = %T %v, want invalid_config", err, err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("registry was removed after malformed evidence: %v", err)
	}
	if _, err := os.Stat(txDir); err != nil {
		t.Fatalf("malformed evidence was not preserved: %v", err)
	}
}

func TestTransactionFailureRecoversPublishedCache(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	oldAccount := maildoconfig.Account{Name: "work", Provider: "outlook", AuthRef: "outlook/work"}
	newAccount := maildoconfig.Account{Name: "renamed", Provider: "outlook", AuthRef: "outlook/renamed"}
	oldReg := &maildoconfig.Registry{Version: 1, ActiveAccount: "work", Accounts: []maildoconfig.Account{oldAccount}}
	newReg := &maildoconfig.Registry{Version: 1, ActiveAccount: "renamed", Accounts: []maildoconfig.Account{newAccount}}
	if err := maildoconfig.Save(configPath, oldReg, []string{"outlook"}); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if err := NewStore(root).SaveBytes("outlook", "work", []byte("old")); err != nil {
		t.Fatalf("save old cache: %v", err)
	}
	coord, err := NewCoordinator(configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	_, rev, _, _, err := coord.Snapshot(context.Background(), "outlook", "work")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	transactionPhaseHook = func(phase TransactionPhase) error {
		if phase == TransactionCacheInstalled {
			return errs.New(errs.CodeInvalidConfig, "forced phase failure")
		}
		return nil
	}
	defer func() { transactionPhaseHook = nil }()
	err = coord.CommitTransaction(context.Background(), TransactionRequest{
		Operation:       errs.AuthTransactionRename,
		Revision:        rev,
		Registry:        newReg,
		OldBinding:      CredentialBinding{Provider: "outlook", Account: "work"},
		NewBinding:      CredentialBinding{Provider: "outlook", Account: "renamed"},
		DesiredCache:    []byte("new"),
		DesiredCacheSet: true,
	}, errs.AuthStateRefresh)
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeInvalidConfig {
		t.Fatalf("CommitTransaction err = %T %v, want invalid_config", err, err)
	}
	reg, err := maildoconfig.Load(configPath, []string{"outlook"})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if _, ok := reg.Find("work"); !ok {
		t.Fatalf("registry was not rolled back: %#v", reg.Accounts)
	}
	oldData, present, err := NewStore(root).LoadBytes("outlook", "work")
	if err != nil || !present || string(oldData) != "old" {
		t.Fatalf("old cache = %q present=%v err=%v", string(oldData), present, err)
	}
	zero(oldData)
	if data, present, err := NewStore(root).LoadBytes("outlook", "renamed"); err != nil || present {
		zero(data)
		t.Fatalf("desired cache survived rollback: present=%v err=%v", present, err)
	}
	if _, err := os.Stat(filepath.Join(root, "auth", ".transactions")); err == nil {
		entries, readErr := os.ReadDir(filepath.Join(root, "auth", ".transactions"))
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("transaction evidence after successful rollback entries=%d err=%v", len(entries), readErr)
		}
	}
}

func TestLockRejectsSymlinkedAuthDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on some Windows runners")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "auth")); err != nil {
		t.Fatalf("symlink auth: %v", err)
	}
	coord, err := NewCoordinator(filepath.Join(root, "config.yaml"), []string{"outlook"})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	err = coord.CleanupStale(context.Background())
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeInvalidConfig {
		t.Fatalf("CleanupStale err = %T %v, want invalid_config", err, err)
	}
	if _, err := os.Stat(filepath.Join(outside, ".transaction.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock escaped through symlink: %v", err)
	}
}

func mustWriteMaterial(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := writeMaterial(dir, name, data); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func writeRecoverableJournal(root, configPath string, phase TransactionPhase) error {
	if root == "" || configPath == "" {
		return fmt.Errorf("missing recovery fixture paths")
	}
	oldAccount := maildoconfig.Account{Name: "work", Provider: "outlook", AuthRef: "outlook/work"}
	newAccount := maildoconfig.Account{Name: "renamed", Provider: "outlook", AuthRef: "outlook/renamed"}
	oldReg := &maildoconfig.Registry{Version: 1, ActiveAccount: "work", Accounts: []maildoconfig.Account{oldAccount}}
	newReg := &maildoconfig.Registry{Version: 1, ActiveAccount: "renamed", Accounts: []maildoconfig.Account{newAccount}}
	oldRegBytes, err := maildoconfig.Encode(oldReg, []string{"outlook"})
	if err != nil {
		return err
	}
	newRegBytes, err := maildoconfig.Encode(newReg, []string{"outlook"})
	if err != nil {
		return err
	}
	if err := maildoconfig.Save(configPath, oldReg, []string{"outlook"}); err != nil {
		return err
	}
	if err := NewStore(root).SaveBytes("outlook", "work", []byte("old")); err != nil {
		return err
	}
	txid := strings.Repeat("2", 32)
	txDir := filepath.Join(root, "auth", ".transactions", txid)
	if err := os.MkdirAll(txDir, 0o700); err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"registry.prior":   oldRegBytes,
		"registry.desired": newRegBytes,
		"cache.prior":      []byte("old"),
		"cache.desired":    []byte("new"),
	} {
		if err := writeMaterial(txDir, name, data); err != nil {
			return err
		}
	}
	journal := Journal{
		Version:      1,
		TxID:         txid,
		Operation:    errs.AuthTransactionRename,
		Phase:        phase,
		RegistryName: filepath.Base(configPath),
		Registry: RegistryImages{
			Prior:   presentImage("registry.prior", oldRegBytes),
			Desired: presentImage("registry.desired", newRegBytes),
		},
		Cache: CacheImages{
			OldPath: "outlook/work.json",
			NewPath: "outlook/renamed.json",
			Prior:   presentImage("cache.prior", []byte("old")),
			Desired: presentImage("cache.desired", []byte("new")),
		},
		OldBinding: CredentialBinding{Provider: "outlook", Account: "work"},
		NewBinding: CredentialBinding{Provider: "outlook", Account: "renamed"},
	}
	if phase == TransactionCacheInstalled || phase == TransactionRegistryInstalled || phase == TransactionObsoleteCacheRemoved || phase == TransactionCommitted {
		if err := NewStore(root).SaveBytes("outlook", "renamed", []byte("new")); err != nil {
			return err
		}
	}
	if phase == TransactionRegistryInstalled || phase == TransactionObsoleteCacheRemoved || phase == TransactionCommitted {
		if err := maildoconfig.Save(configPath, newReg, []string{"outlook"}); err != nil {
			return err
		}
	}
	return writeJournal(txDir, journal)
}
