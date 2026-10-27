package main

import (
	"strings"
	"testing"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
)

func TestConfigSetGetAndList(t *testing.T) {
	h := newFakeHarness(t, testkit.NewFakeProvider("fake"))
	decodeOK(t, h.run("login", "--provider", "fake", "--name", "work", "--json"), nil)

	decodeOK(t, h.run("config", "set", "outlook.client_id", "11111111-2222-3333-4444-555555555555", "--json"), nil)
	decodeOK(t, h.run("config", "set", "policy.drafts_only", "true", "--json"), nil)
	decodeOK(t, h.run("config", "set", "policy.allow_recipients", "@Example.com, boss@other.example", "--account", "work", "--json"), nil)

	reg, err := maildoconfig.Load(h.configPath, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := reg.Find("work")
	if reg.Outlook == nil || reg.Outlook.ClientID != "11111111-2222-3333-4444-555555555555" || !acct.Policy.DraftsOnly ||
		strings.Join(acct.Policy.AllowRecipients, ",") != "@example.com,boss@other.example" {
		t.Fatalf("saved config = %+v / %+v", reg.Outlook, acct.Policy)
	}

	var got configSetting
	decodeOK(t, h.run("config", "get", "policy.drafts_only", "--json"), &got)
	if got.Value != "true" || got.Account != "work" {
		t.Fatalf("get = %+v", got)
	}
	var list configListResult
	decodeOK(t, h.run("config", "list", "--json"), &list)
	if len(list.Settings) < 5 {
		t.Fatalf("list = %+v", list)
	}

	assertFrameworkUsage(t, h.run("config", "set", "policy.drafts_only", "maybe", "--json"))
	assertFrameworkUsage(t, h.run("config", "set", "gmail.client_id", "not-a-google-id", "--json"))
	assertFrameworkUsage(t, h.run("config", "set", "no.such.setting", "x", "--json"))
	assertFrameworkUsage(t, h.run("config", "set", "outlook.client_id", "x", "--account", "work", "--json"))
}

// An agent without a terminal must not be able to widen its own limits.
func TestConfigLooseningAPolicyNeedsATerminal(t *testing.T) {
	h := newFakeHarness(t, testkit.NewFakeProvider("fake"))
	decodeOK(t, h.run("login", "--provider", "fake", "--name", "work", "--json"), nil)
	decodeOK(t, h.run("config", "set", "policy.read_only", "true"), nil)
	for _, args := range [][]string{
		{"config", "set", "policy.read_only", "false"},
		{"config", "set", "policy.unattended_send", "true"},
	} {
		res := h.run(args...)
		if res.Exit == 0 || !strings.Contains(res.Stderr, "needs a person at a terminal") {
			t.Fatalf("%v: exit %d %s", args, res.Exit, res.Stderr)
		}
	}
	res := h.runWith(testutil.Options{Stdin: strings.NewReader("1\n"), Terminal: true, TerminalSet: true}, "config", "set", "policy.read_only", "false")
	if res.Exit != 0 {
		t.Fatalf("a person at a terminal could not loosen the policy: %s", res.Stderr)
	}
}
