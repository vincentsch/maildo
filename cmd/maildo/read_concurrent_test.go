package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad/testutil"
)

func TestReadThreadConcurrentSubprocessReadOnlyOutputs(t *testing.T) {
	h := seededReadHarness(t, "work")
	before := hashTree(t, h.root)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	outDir := t.TempDir()
	modes := []string{"read-id", "read-query", "thread-id", "thread-query"}

	var cmds []*exec.Cmd
	for i := 0; i < 12; i++ {
		mode := modes[i%len(modes)]
		outPath := filepath.Join(outDir, fmt.Sprintf("%s-%02d.json", mode, i))
		cmd := exec.Command(exe, "-test.run=^TestReadThreadConcurrentHelperProcess$", "--", h.configPath, mode, outPath)
		cmd.Env = append(os.Environ(), "MAILDO_READ_HELPER_PROCESS=1")
		cmds = append(cmds, cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start subprocess %d: %v", i, err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("subprocess %d failed: %v", i, err)
		}
	}
	for i := 0; i < 12; i++ {
		mode := modes[i%len(modes)]
		outPath := filepath.Join(outDir, fmt.Sprintf("%s-%02d.json", mode, i))
		data, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("read subprocess output %d: %v", i, err)
		}
		switch {
		case mode == "read-id" || mode == "read-query":
			var got readResult
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decode read subprocess output %d: %v\n%s", i, err, data)
			}
			if got.Message.ID != "work-new" || got.Message.Account != "work" || got.Message.Provider != "fake" {
				t.Fatalf("read subprocess %d result = %#v", i, got.Message)
			}
		default:
			var got threadResult
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decode thread subprocess output %d: %v\n%s", i, err, data)
			}
			if got.Thread.ThreadID != "thread-work-new" || len(got.Thread.Messages) == 0 {
				t.Fatalf("thread subprocess %d result = %#v", i, got.Thread)
			}
		}
	}

	after := hashTree(t, h.root)
	if !sameStrings(before, after) {
		t.Fatalf("concurrent subprocess read/thread mutated config root\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestReadThreadConcurrentHelperProcess(t *testing.T) {
	if os.Getenv("MAILDO_READ_HELPER_PROCESS") != "1" {
		return
	}
	sep := -1
	for i, arg := range os.Args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || len(os.Args) < sep+4 {
		fmt.Fprintln(os.Stderr, "missing helper args")
		os.Exit(2)
	}
	configPath := os.Args[sep+1]
	mode := os.Args[sep+2]
	outPath := os.Args[sep+3]
	fake := testkit.NewFakeReadProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages(), testkit.SampleThreads())
	registry := testkit.NewProviderRegistry(fake)
	app := newApp(Deps{Providers: registry, DirectSendPreparers: testkit.NewDirectSendPreparerRegistry(providerLifecycles(registry)...), SetupParsers: defaultSetupParsers(registry.Names())})
	args := []string{"--config", configPath}
	switch mode {
	case "read-id":
		args = append(args, "read", "--id", "work-new", "--json")
	case "read-query":
		args = append(args, "read", "--query", "anything", "--latest", "--all-accounts", "--json")
	case "thread-id":
		args = append(args, "thread", "--id", "work-new", "--json")
	case "thread-query":
		args = append(args, "thread", "--query", "anything", "--first", "--account", "work", "--account", "personal", "--json")
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	res := testutil.Run(app, args...)
	if res.Exit != 0 {
		fmt.Fprintf(os.Stderr, "exit=%d\nstdout=%s\nstderr=%s\n", res.Exit, res.Stdout, res.Stderr)
		os.Exit(res.Exit)
	}
	if err := os.WriteFile(outPath, []byte(res.Stdout), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}
