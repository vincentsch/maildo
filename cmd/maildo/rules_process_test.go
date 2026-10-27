package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/maildo/internal/testkit"
)

const ruleInterruptionHelperEnv = "MAILDO_RULE_INTERRUPTION_HELPER"

const interruptedCreateDisplayName = "Interrupted create recovery"

func TestRuleMutationInterruptedProcessRequiresFreshRead(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules()), "work")

	for _, tt := range []struct {
		stage     string
		wantState string
		wantValue string
	}{
		{stage: "before_dispatch", wantState: "initial", wantValue: "Invoice"},
		{stage: "after_dispatch", wantState: "updated", wantValue: "interrupted-applied"},
		{stage: "preview", wantState: "initial", wantValue: "Invoice"},
	} {
		t.Run(tt.stage, func(t *testing.T) {
			root := t.TempDir()
			statePath := filepath.Join(root, "state")
			markerPath := filepath.Join(root, "ready")
			outPath := filepath.Join(root, "recovered.json")
			if err := os.WriteFile(statePath, []byte("initial"), 0o600); err != nil {
				t.Fatal(err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			cmd := ruleInterruptionHelperCommand(exe, h.configPath, statePath, markerPath, outPath, tt.stage)
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start helper: %v", err)
			}
			waitForRuleInterruptionMarker(t, markerPath, cmd)
			if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("kill helper: %v", err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("interrupted helper exited successfully")
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("interrupted helper emitted output stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			state, err := os.ReadFile(statePath)
			if err != nil || string(state) != tt.wantState {
				t.Fatalf("interrupted state=%q err=%v, want %q", state, err, tt.wantState)
			}

			recovery := ruleInterruptionHelperCommand(exe, h.configPath, statePath, markerPath, outPath, "recover")
			if output, err := recovery.CombinedOutput(); err != nil {
				t.Fatalf("fresh recovery process: %v\n%s", err, output)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			var view ruleViewResult
			if err := json.Unmarshal(data, &view); err != nil {
				t.Fatalf("decode recovery output: %v\n%s", err, data)
			}
			if view.Rule.ID != "rule-1" || !sameStrings(view.Rule.Conditions.SubjectContains, []string{tt.wantValue}) {
				t.Fatalf("fresh recovery view=%#v", view.Rule)
			}
			stateAfter, err := os.ReadFile(statePath)
			if err != nil || string(stateAfter) != tt.wantState {
				t.Fatalf("fresh read retried or changed state=%q err=%v", stateAfter, err)
			}
		})
	}

	if actionLogExists(t, h) {
		t.Fatal("interruption or fresh reads wrote action log")
	}
}

func TestRuleCreateAndDeleteInterruptedProcessesRecoverFromProviderState(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules()), "work")

	for _, tt := range []struct {
		name          string
		stage         string
		recoveryStage string
		initialState  string
	}{
		{name: "create accepted before id returned", stage: "create_after_dispatch", recoveryStage: "recover_create", initialState: "initial"},
		{name: "delete accepted before verification", stage: "delete_after_dispatch", recoveryStage: "recover_delete", initialState: "delete_present"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			statePath := filepath.Join(root, "state")
			markerPath := filepath.Join(root, "ready")
			outPath := filepath.Join(root, "recovered.json")
			if err := os.WriteFile(statePath, []byte(tt.initialState), 0o600); err != nil {
				t.Fatal(err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			cmd := ruleInterruptionHelperCommand(exe, h.configPath, statePath, markerPath, outPath, tt.stage)
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start helper: %v", err)
			}
			waitForRuleInterruptionMarker(t, markerPath, cmd)
			if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("kill helper: %v", err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("interrupted helper exited successfully")
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("interrupted helper emitted output stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			stateBeforeRecovery, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}

			recovery := ruleInterruptionHelperCommand(exe, h.configPath, statePath, markerPath, outPath, tt.recoveryStage)
			if output, err := recovery.CombinedOutput(); err != nil {
				t.Fatalf("fresh recovery process: %v\n%s", err, output)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			var list ruleListResult
			if err := json.Unmarshal(data, &list); err != nil {
				t.Fatalf("decode recovery output: %v\n%s", err, data)
			}
			switch tt.recoveryStage {
			case "recover_create":
				createdID := strings.TrimPrefix(string(stateBeforeRecovery), "created:")
				if createdID == string(stateBeforeRecovery) || createdID == "" {
					t.Fatalf("provider state did not retain created id: %q", stateBeforeRecovery)
				}
				matches := 0
				for _, rule := range list.Rules {
					if rule.DisplayName == interruptedCreateDisplayName {
						matches++
						if rule.ID != createdID {
							t.Fatalf("recovered rule id=%q, want provider id %q", rule.ID, createdID)
						}
					}
				}
				if matches != 1 {
					t.Fatalf("recovered created rule matches=%d, rules=%#v", matches, list.Rules)
				}
			case "recover_delete":
				if string(stateBeforeRecovery) != "deleted" {
					t.Fatalf("provider delete state = %q", stateBeforeRecovery)
				}
				for _, rule := range list.Rules {
					if rule.ID == "rule-1" {
						t.Fatalf("fresh read found deleted rule: %#v", rule)
					}
				}
			}

			stateAfterRecovery, err := os.ReadFile(statePath)
			if err != nil || !bytes.Equal(stateAfterRecovery, stateBeforeRecovery) {
				t.Fatalf("fresh read retried or changed provider state before=%q after=%q err=%v", stateBeforeRecovery, stateAfterRecovery, err)
			}
		})
	}

	if actionLogExists(t, h) {
		t.Fatal("interruption or fresh reads wrote action log")
	}
}

func TestRuleDeleteRealTerminalDoesNotPrompt(t *testing.T) {
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script utility is unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := seededRulesHarnessWithProvider(t, testkit.NewFakeRuleMutationProvider("fake", mutationUpdateRules()), "work")
	root := t.TempDir()
	args := []string{exe, "-test.run=^TestRuleMutationInterruptionHelperProcess$", "--", h.configPath, filepath.Join(root, "state"), filepath.Join(root, "ready"), filepath.Join(root, "out")}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuoteRuleHelper(arg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "-qfec", strings.Join(quoted, " "), "/dev/null")
	cmd.Env = append(os.Environ(), ruleInterruptionHelperEnv+"=pty_no_prompt")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("real-terminal delete did not exit on its own: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("real-terminal helper failed: %v\n%s", err, output)
	}
	if bytes.Contains(output, []byte("rule-1")) {
		t.Fatalf("real-terminal confirmation exposed target: %q", output)
	}
}

func TestRuleMutationInterruptionHelperProcess(t *testing.T) {
	stage := os.Getenv(ruleInterruptionHelperEnv)
	if stage == "" {
		return
	}
	sep := -1
	for i, arg := range os.Args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || len(os.Args) < sep+5 {
		fmt.Fprintln(os.Stderr, "missing helper arguments")
		os.Exit(2)
	}
	configPath := os.Args[sep+1]
	statePath := os.Args[sep+2]
	markerPath := os.Args[sep+3]
	outPath := os.Args[sep+4]

	rules := mutationUpdateRules()
	state, _ := os.ReadFile(statePath)
	if string(state) == "updated" {
		rules["work"][0].Conditions.SubjectContains = []string{"interrupted-applied"}
		rules["work"][0].ProviderState["conditions"].(map[string]any)["subjectContains"] = []any{"interrupted-applied"}
	} else if createdID := strings.TrimPrefix(string(state), "created:"); createdID != string(state) && createdID != "" {
		rules["work"] = append(rules["work"], mail.Rule{
			ID: createdID, DisplayName: interruptedCreateDisplayName, Sequence: 11, IsEnabled: true,
			Conditions: mail.RulePredicates{SubjectContains: []string{"interrupted-create"}},
			Actions:    mail.RuleActions{MarkAsRead: ruleProcessBool(true)},
		})
	} else if string(state) == "deleted" {
		remaining := rules["work"][:0]
		for _, rule := range rules["work"] {
			if rule.ID != "rule-1" {
				remaining = append(remaining, rule)
			}
		}
		rules["work"] = remaining
	}
	fake := testkit.NewFakeRuleMutationProvider("fake", rules)
	var registered provider.Provider = fake
	if stage != "recover" && !strings.HasPrefix(stage, "recover_") && stage != "pty_no_prompt" {
		registered = &interruptingRuleProvider{FakeProvider: fake, stage: stage, statePath: statePath, markerPath: markerPath}
	}
	registry := testkit.NewProviderRegistry(registered)
	app := newApp(Deps{Providers: registry, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(registry)...), SetupParsers: defaultSetupParsers(registry.Names())})

	switch stage {
	case "recover":
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		exit := app.RunIO([]string{"--config", configPath, "rules", "view", "rule-1", "--json"}, strings.NewReader(""), out, os.Stderr)
		_ = out.Close()
		os.Exit(exit)
	case "recover_create", "recover_delete":
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		exit := app.RunIO([]string{"--config", configPath, "rules", "list", "--json"}, strings.NewReader(""), out, os.Stderr)
		_ = out.Close()
		if exit != 0 || fake.RuleCreateCalls.Load() != 0 || fake.RuleUpdateCalls.Load() != 0 || fake.RuleDeleteCalls.Load() != 0 {
			os.Exit(2)
		}
		os.Exit(0)
	case "pty_no_prompt":
		exit := app.RunIO([]string{"--config", configPath, "rules", "delete", "rule-1", "--no-prompt", "--json"}, os.Stdin, os.Stdout, os.Stderr)
		if exit == 0 || fake.RuleDeleteCalls.Load() != 0 || fake.RuleGetCalls.Load() != 0 {
			os.Exit(2)
		}
		os.Exit(0)
	case "before_dispatch", "after_dispatch":
		exit := app.RunIO([]string{"--config", configPath, "rules", "update", "rule-1", "--subject-contains", "interrupted-applied", "--apply", "--json"}, os.Stdin, os.Stdout, os.Stderr)
		os.Exit(exit)
	case "create_after_dispatch":
		exit := app.RunIO([]string{"--config", configPath, "rules", "create", "--name", interruptedCreateDisplayName, "--subject-contains", "interrupted-create", "--mark-read", "--apply", "--json"}, os.Stdin, os.Stdout, os.Stderr)
		os.Exit(exit)
	case "delete_after_dispatch":
		exit := app.RunIO([]string{"--config", configPath, "rules", "delete", "rule-1", "--confirm", "rule-1", "--json"}, os.Stdin, os.Stdout, os.Stderr)
		os.Exit(exit)
	case "preview":
		exit := app.RunIO([]string{"--config", configPath, "rules", "update", "rule-1", "--subject-contains", "interrupted-preview", "--json"}, os.Stdin, os.Stdout, os.Stderr)
		os.Exit(exit)
	default:
		fmt.Fprintln(os.Stderr, "unsupported helper stage")
		os.Exit(2)
	}
}

type interruptingRuleProvider struct {
	*testkit.FakeProvider
	stage      string
	statePath  string
	markerPath string
}

func (p *interruptingRuleProvider) NewAccountClient(ctx context.Context, authContext provider.StoredAuthContext) (provider.AccountClient, error) {
	client, err := p.FakeProvider.NewAccountClient(ctx, authContext)
	if err != nil {
		return nil, err
	}
	return interruptingRuleClient{
		AccountClient: client,
		FolderReader:  client.(provider.FolderReader),
		RuleReader:    client.(provider.RuleReader),
		RuleManager:   client.(provider.RuleManager),
		stage:         p.stage,
		statePath:     p.statePath,
		markerPath:    p.markerPath,
	}, nil
}

type interruptingRuleClient struct {
	provider.AccountClient
	provider.FolderReader
	provider.RuleReader
	provider.RuleManager
	stage      string
	statePath  string
	markerPath string
}

func (c interruptingRuleClient) ListRules(ctx context.Context, req mail.RuleListRequest) (mail.RuleListResult, error) {
	result, err := c.RuleReader.ListRules(ctx, req)
	if err == nil && c.stage == "preview" {
		c.signalAndBlock()
	}
	return result, err
}

func (c interruptingRuleClient) UpdateRule(ctx context.Context, req mail.RuleUpdateRequest) (mail.RuleMutationResult, error) {
	if c.stage == "before_dispatch" {
		c.signalAndBlock()
	}
	result, err := c.RuleManager.UpdateRule(ctx, req)
	if err == nil && c.stage == "after_dispatch" {
		if err := os.WriteFile(c.statePath, []byte("updated"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		c.signalAndBlock()
	}
	return result, err
}

func (c interruptingRuleClient) CreateRule(ctx context.Context, req mail.RuleCreateRequest) (mail.RuleMutationResult, error) {
	result, err := c.RuleManager.CreateRule(ctx, req)
	if err == nil && c.stage == "create_after_dispatch" {
		if err := os.WriteFile(c.statePath, []byte("created:"+result.RuleID), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		c.signalAndBlock()
	}
	return result, err
}

func (c interruptingRuleClient) DeleteRule(ctx context.Context, req mail.RuleDeleteRequest) (mail.RuleDeleteResult, error) {
	result, err := c.RuleManager.DeleteRule(ctx, req)
	if err == nil && c.stage == "delete_after_dispatch" {
		if err := os.WriteFile(c.statePath, []byte("deleted"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		c.signalAndBlock()
	}
	return result, err
}

func (c interruptingRuleClient) signalAndBlock() {
	if err := os.WriteFile(c.markerPath, []byte("ready"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	time.Sleep(time.Hour)
}

func ruleInterruptionHelperCommand(exe, configPath, statePath, markerPath, outPath, stage string) *exec.Cmd {
	cmd := exec.Command(exe, "-test.run=^TestRuleMutationInterruptionHelperProcess$", "--", configPath, statePath, markerPath, outPath)
	cmd.Env = append(os.Environ(), ruleInterruptionHelperEnv+"="+stage)
	return cmd
}

func waitForRuleInterruptionMarker(t *testing.T, markerPath string, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatal("interruption helper did not reach synchronized stage")
		case <-ticker.C:
			if _, err := os.Stat(markerPath); err == nil {
				return
			}
		}
	}
}

func shellQuoteRuleHelper(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func ruleProcessBool(value bool) *bool {
	return &value
}
