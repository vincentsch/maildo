package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/audit"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/testutil"
)

func TestMessageMutationsFakeProviderHappyPathsAndDryRuns(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		operation string
	}{
		{name: "move", args: []string{"move", "--id", "work-new", "--to", "archive", "--json"}, operation: "move"},
		{name: "copy query", args: []string{"copy", "--query", "anything", "--latest", "--to", "inbox", "--json"}, operation: "copy"},
		{name: "archive", args: []string{"archive", "--id", "work-new", "--json"}, operation: "archive"},
		{name: "delete", args: []string{"delete", "--id", "work-new", "--json"}, operation: "delete"},
		{name: "restore", args: []string{"restore", "--id", "work-new", "--to", "inbox", "--json"}, operation: "restore"},
		{name: "junk", args: []string{"junk", "--id", "work-new", "--json"}, operation: "junk"},
		{name: "mark read", args: []string{"mark", "read", "--id", "work-new", "--json"}, operation: "mark_read"},
		{name: "mark unread", args: []string{"mark", "unread", "--id", "work-new", "--json"}, operation: "mark_unread"},
		{name: "flag set", args: []string{"flag", "set", "--id", "work-new", "--json"}, operation: "flag_set"},
		{name: "flag clear", args: []string{"flag", "clear", "--id", "work-new", "--json"}, operation: "flag_clear"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, fake := seededMutationHarness(t)
			var result mutationResult
			decodeOK(t, h.run(tt.args...), &result)
			if result.Operation != tt.operation || !result.Applied || result.MessageID == "" {
				t.Fatalf("mutation result = %#v", result)
			}
			if fake.MessageMutationCalls.Load() != 1 {
				t.Fatalf("mutation calls = %d, want 1", fake.MessageMutationCalls.Load())
			}
			if strings.Contains(previewString(map[string]any{"data": result}), "Full body for work-new") {
				t.Fatalf("mutation output leaked body: %#v", result)
			}
		})
	}

	t.Run("explicit id dry run reads the subject but does not write", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		preview := decodePreview(t, h.run("archive", "--id", "work-new", "--dry-run", "--json"))
		target := previewTargets(t, preview)[0]
		if preview["operation"] != "archive" || preview["applied"] != false || target["id"] != "work-new" || target["subject"] == nil {
			t.Fatalf("preview = %#v", preview)
		}
		if cmd, _ := preview["apply_command"].(string); !strings.HasPrefix(cmd, "maildo archive --config ") || !strings.HasSuffix(cmd, " --json --account work --id work-new") {
			t.Fatalf("apply command = %v", preview["apply_command"])
		}
		if fake.MessageMutationCalls.Load() != 0 || fake.MessageMetadataOnlyReadCalls.Load() != 1 {
			t.Fatalf("dry-run calls mutation=%d metadata=%d", fake.MessageMutationCalls.Load(), fake.MessageMetadataOnlyReadCalls.Load())
		}
	})

	t.Run("query dry run resolves target but does not write", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		preview := decodePreview(t, h.run("archive", "--query", "anything", "--latest", "--dry-run", "--json"))
		selector := preview["selector"].(map[string]any)
		if selector["kind"] != "query_latest" || previewTargets(t, preview)[0]["id"] != "work-new" {
			t.Fatalf("query preview = %#v", preview)
		}
		if fake.MessageMutationCalls.Load() != 0 {
			t.Fatalf("query dry-run wrote: %d", fake.MessageMutationCalls.Load())
		}
	})

	t.Run("internet id dry run names the message without writing", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		preview := decodePreview(t, h.run("archive", "--internet-id", "<work-new@example.test>", "--dry-run", "--json"))
		if previewTargets(t, preview)[0]["id"] != "work-new" || fake.MessageMutationCalls.Load() != 0 {
			t.Fatalf("internet dry-run preview=%#v mutation=%d", preview, fake.MessageMutationCalls.Load())
		}
	})

	t.Run("folder targeted dry runs resolve target folder without writing", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			args []string
			op   string
		}{
			{name: "move", args: []string{"move", "--id", "work-new", "--to", "inbox", "--dry-run", "--json"}, op: "move"},
			{name: "copy", args: []string{"copy", "--id", "work-new", "--to", "inbox", "--dry-run", "--json"}, op: "copy"},
			{name: "restore", args: []string{"restore", "--id", "work-new", "--to", "inbox", "--dry-run", "--json"}, op: "restore"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				h, fake := seededMutationHarness(t)
				preview := decodePreview(t, h.run(tt.args...))
				folder, _ := preview["target_folder"].(map[string]any)
				if preview["operation"] != tt.op || folder["id"] != "inbox-id" {
					t.Fatalf("folder dry-run preview = %#v", preview)
				}
				if fake.MessageMutationCalls.Load() != 0 {
					t.Fatalf("folder dry-run wrote mutation=%d", fake.MessageMutationCalls.Load())
				}
			})
		}
	})

	t.Run("applied internet id mutation resolves metadata and writes once", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		var result mutationResult
		decodeOK(t, h.run("mark", "read", "--internet-id", "<work-new@example.test>", "--json"), &result)
		if result.Operation != "mark_read" || !result.Applied || result.MessageID != "work-new" || result.Selector.Kind != "internet_id" {
			t.Fatalf("internet mutation result = %#v", result)
		}
		if fake.MessageMetadataOnlyReadCalls.Load() != 1 || fake.MessageMutationCalls.Load() != 1 {
			t.Fatalf("internet mutation calls metadata=%d mutation=%d", fake.MessageMetadataOnlyReadCalls.Load(), fake.MessageMutationCalls.Load())
		}
	})

	t.Run("applied query first mutation uses first selector", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		var result mutationResult
		decodeOK(t, h.run("flag", "set", "--query", "anything", "--first", "--json"), &result)
		if result.Operation != "flag_set" || !result.Applied || result.Selector.Kind != "query_first" || result.MessageID == "" {
			t.Fatalf("query first mutation result = %#v", result)
		}
		if fake.MessageMutationCalls.Load() != 1 {
			t.Fatalf("query first mutation calls = %d", fake.MessageMutationCalls.Load())
		}
	})
}

func TestCategoriesCommandsValidationDiffAndOutputModes(t *testing.T) {
	h, fake := seededMutationHarness(t)
	var listed categoryListResult
	decodeOK(t, h.run("categories", "list", "--internet-id", "<work-new@example.test>", "--json"), &listed)
	if listed.MessageID != "work-new" || listed.Count != 2 || listed.Categories[0] != "Contracts" || fake.MessageMetadataOnlyReadCalls.Load() != 1 {
		t.Fatalf("category list = %#v metadata=%d", listed, fake.MessageMetadataOnlyReadCalls.Load())
	}
	if got := strings.TrimSpace(h.run("categories", "list", "--id", "work-new", "--json", "--jq", ".count").Stdout); got != "2" {
		t.Fatalf("jq category count = %q", got)
	}
	if got := strings.TrimSpace(h.run("categories", "list", "--id", "work-new", "--template", "{{.count}}").Stdout); got != "2" {
		t.Fatalf("template category count = %q", got)
	}
	var envelope struct {
		Data categoryListResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("categories", "list", "--id", "work-new", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["operation"] != "categories_list" || envelope.Meta.Extra["category_count"] != float64(2) {
		t.Fatalf("category meta = %#v", envelope.Meta.Extra)
	}
	decodeOK(t, h.run("categories", "list", "--id", "work-new", "--dry-run", "--json"), &categoryListResult{})

	setHarness, setFake := seededMutationHarness(t)
	var set mutationResult
	decodeOK(t, setHarness.run("categories", "set", "--id", "work-new", "--category", " Client ", "--category", "Urgent", "--json"), &set)
	if set.Operation != "categories_set" || set.Categories.Count != 2 || set.Categories.After[0] != "Client" || set.Categories.After[1] != "Urgent" || setFake.CategorySetCalls.Load() != 1 {
		t.Fatalf("category set = %#v calls=%d", set, setFake.CategorySetCalls.Load())
	}
	assertErrorCode(t, setHarness.run("categories", "set", "--id", "work-new", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, setHarness.run("categories", "set", "--id", "work-new", "--category", "Client", "--category", " Client ", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, setHarness.run("categories", "set", "--id", "work-new", "--category", "  ", "--json"), errs.CodeInvalidSelector)

	clearHarness, clearFake := seededMutationHarness(t)
	var cleared mutationResult
	decodeOK(t, clearHarness.run("categories", "clear", "--id", "work-new", "--category", "Client", "--category", "Missing", "--json"), &cleared)
	if cleared.Categories.Count != 1 || len(cleared.Categories.Removed) != 1 || cleared.Categories.Removed[0] != "Client" ||
		len(cleared.Categories.Skipped) != 1 || cleared.Categories.Skipped[0] != "Missing" ||
		clearFake.CategoryListCalls.Load() != 1 || clearFake.CategorySetCalls.Load() != 1 {
		t.Fatalf("category clear = %#v list=%d set=%d", cleared, clearFake.CategoryListCalls.Load(), clearFake.CategorySetCalls.Load())
	}
	preview := decodePreview(t, clearHarness.run("categories", "clear", "--id", "work-new", "--category", "Missing", "--dry-run", "--json"))
	if clearFake.CategoryListCalls.Load() != 2 || clearFake.CategorySetCalls.Load() != 1 || !strings.Contains(previewString(preview), `"skipped":["Missing"]`) {
		t.Fatalf("named clear preview=%#v list=%d set=%d", preview, clearFake.CategoryListCalls.Load(), clearFake.CategorySetCalls.Load())
	}

	allHarness, allFake := seededMutationHarness(t)
	decodePreview(t, allHarness.run("categories", "clear", "--id", "work-new", "--dry-run", "--json"))
	if allFake.CategoryListCalls.Load() != 0 || allFake.CategorySetCalls.Load() != 0 {
		t.Fatalf("clear-all dry-run read/wrote list=%d set=%d", allFake.CategoryListCalls.Load(), allFake.CategorySetCalls.Load())
	}
	var allCleared mutationResult
	decodeOK(t, allHarness.run("categories", "clear", "--id", "work-new", "--json"), &allCleared)
	if allCleared.Categories.Count != 0 || allFake.CategorySetCalls.Load() != 1 {
		t.Fatalf("clear all = %#v set=%d", allCleared, allFake.CategorySetCalls.Load())
	}
}

func TestMutationSelectorScopeAndCapabilityValidation(t *testing.T) {
	h, _ := seededMutationHarness(t)
	assertErrorCode(t, h.run("archive", "--json"), errs.CodeInvalidSelector)
	repeated := decodePreview(t, h.run("archive", "--id", "work-new", "--id", "work-old", "--json"))
	if repeated["selector"].(map[string]any)["kind"] != "ids" || repeated["count"] != float64(2) || !strings.HasSuffix(repeated["apply_command"].(string), " --apply") {
		t.Fatalf("repeated id preview = %#v", repeated)
	}
	assertErrorCode(t, h.run("archive", "--id", "work-new", "--id", "work-new", "--json"), errs.CodeInvalidSelector)
	tooManyIDsArgs := []string{"archive", "--json"}
	for i := 0; i <= maxExplicitMutationTargets; i++ {
		tooManyIDsArgs = append(tooManyIDsArgs, "--id", "msg-"+strconv.Itoa(i))
	}
	assertErrorCode(t, h.run(tooManyIDsArgs...), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--id", "  ", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--id", "work-new", "--internet-id", "<work-new@example.test>", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--query", "anything", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--query", "anything", "--latest", "--first", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--all", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--query", "anything", "--all", "--latest", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--query", "anything", "--all", "--first", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--id", "work-new", "--max-targets", "2", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--query", "anything", "--all", "--max-targets", "0", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--query", "anything", "--all", "--max-targets", "1001", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--id", "work-new", "--dry-run", "--apply", "--json"), errs.CodeInvalidSelector)
	assertErrorCode(t, h.run("archive", "--id", "work-new", "--account", "work", "--account", "personal", "--json"), errs.CodeInvalidAccountScope)
	assertFrameworkUsage(t, h.run("archive", "--id", "work-new", "--all-accounts", "--json"))
	assertErrorCodeExit(t, h.run("move", "--id", "work-new", "--to", "missing-folder", "--json"), errs.CodeFolderNotFound, 5)
	assertErrorCode(t, h.run("move", "--id", "work-new", "--to", "Reports", "--json"), errs.CodeAmbiguousFolder)

	stale := testkit.NewFakeStaleMutationProvider("fake")
	staleHarness, _ := seedMutationHarness(t, stale)
	assertErrorCode(t, staleHarness.run("archive", "--id", "work-new", "--json"), errs.CodeCapabilityNotImplemented)
	assertErrorCode(t, staleHarness.run("categories", "list", "--id", "work-new", "--json"), errs.CodeCapabilityNotImplemented)
	if stale.MessageMutationCalls.Load() != 0 || stale.CategorySetCalls.Load() != 0 {
		t.Fatalf("stale provider wrote mutation=%d categorySet=%d", stale.MessageMutationCalls.Load(), stale.CategorySetCalls.Load())
	}

	pending := testkit.NewFakeReadProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages(), testkit.SampleThreads())
	pendingHarness, _ := seedMutationHarness(t, pending)
	assertErrorCode(t, pendingHarness.run("archive", "--id", "work-new", "--json"), errs.CodeUnsupportedCapability)
}

func TestBulkMutationRepeatedIDsAndActionLog(t *testing.T) {
	h, fake := seededMutationHarness(t)
	preview := decodePreview(t, h.run("mark", "read", "--id", "work-new", "--id", "work-old", "--json"))
	if preview["selector"].(map[string]any)["kind"] != "ids" || preview["apply_command"] == nil || previewTargets(t, preview)[1]["id"] != "work-old" {
		t.Fatalf("bulk preview = %#v", preview)
	}
	if fake.MessageMutationCalls.Load() != 0 || actionLogExists(t, h) {
		t.Fatalf("preview wrote mutation=%d log=%t", fake.MessageMutationCalls.Load(), actionLogExists(t, h))
	}
	var result bulkMutationResult
	decodeOK(t, h.run("mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json"), &result)
	if result.Operation != "mark_read" || result.Counts.Total != 2 || result.Counts.Succeeded != 2 || !result.ActionLogStatus.Written {
		t.Fatalf("bulk result = %#v", result)
	}
	if got := fake.MutationCallLog; strings.Join(got, ",") != "work:work-new,work:work-old" {
		t.Fatalf("mutation order = %#v", got)
	}
	entries := readActionLog(t, h)
	if len(entries) != 1 || entries[0].Command != "mark read" || entries[0].Operation != mail.MutationMarkRead || entries[0].Counts.Succeeded != 2 {
		t.Fatalf("action log entries = %#v", entries)
	}
}

func TestBulkMutationOperationMatrixAndState(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		operation string
		check     func(t *testing.T, h *cliHarness, result bulkMutationResult)
	}{
		{
			name:      "move",
			args:      []string{"move", "--id", "work-new", "--id", "work-old", "--to", "archive", "--apply", "--json"},
			operation: string(mail.MutationMove),
			check: func(t *testing.T, h *cliHarness, result bulkMutationResult) {
				assertReadFolder(t, h, "work-new", "archive-id")
				assertReadFolder(t, h, "work-old", "archive-id")
				if result.Targets[0].TargetFolder == nil || result.Targets[0].TargetFolder.ID != "archive-id" {
					t.Fatalf("move target folder = %#v", result.Targets[0].TargetFolder)
				}
			},
		},
		{
			name:      "copy",
			args:      []string{"copy", "--id", "work-new", "--id", "work-old", "--to", "inbox", "--apply", "--json"},
			operation: string(mail.MutationCopy),
			check: func(t *testing.T, h *cliHarness, result bulkMutationResult) {
				assertReadFolder(t, h, "work-new", "inbox-id")
				for _, row := range result.Targets {
					if row.ResultMessageID == "" || row.ResultMessageID == row.ID {
						t.Fatalf("copy result id = %#v", row)
					}
					assertReadFolder(t, h, row.ResultMessageID, "inbox-id")
				}
			},
		},
		{
			name:      "archive",
			args:      []string{"archive", "--id", "work-new", "--id", "work-old", "--apply", "--json"},
			operation: string(mail.MutationArchive),
			check: func(t *testing.T, h *cliHarness, _ bulkMutationResult) {
				assertReadFolder(t, h, "work-new", "archive-id")
				assertReadFolder(t, h, "work-old", "archive-id")
			},
		},
		{
			name:      "delete",
			args:      []string{"delete", "--id", "work-new", "--id", "work-old", "--apply", "--json"},
			operation: string(mail.MutationDelete),
			check: func(t *testing.T, h *cliHarness, _ bulkMutationResult) {
				assertReadFolder(t, h, "work-new", "deleted-id")
				assertReadFolder(t, h, "work-old", "deleted-id")
			},
		},
		{
			name:      "restore",
			args:      []string{"restore", "--id", "work-new", "--id", "work-old", "--to", "inbox", "--apply", "--json"},
			operation: string(mail.MutationRestore),
			check: func(t *testing.T, h *cliHarness, _ bulkMutationResult) {
				assertReadFolder(t, h, "work-new", "inbox-id")
				assertReadFolder(t, h, "work-old", "inbox-id")
			},
		},
		{
			name:      "junk",
			args:      []string{"junk", "--id", "work-new", "--id", "work-old", "--apply", "--json"},
			operation: string(mail.MutationJunk),
			check: func(t *testing.T, h *cliHarness, _ bulkMutationResult) {
				assertReadFolder(t, h, "work-new", "junk-id")
				assertReadFolder(t, h, "work-old", "junk-id")
			},
		},
		{
			name:      "mark read",
			args:      []string{"mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json"},
			operation: string(mail.MutationMarkRead),
			check: func(t *testing.T, h *cliHarness, _ bulkMutationResult) {
				assertReadState(t, h, "work-new", true)
				assertReadState(t, h, "work-old", true)
			},
		},
		{
			name:      "mark unread",
			args:      []string{"mark", "unread", "--id", "work-new", "--id", "work-old", "--apply", "--json"},
			operation: string(mail.MutationMarkUnread),
			check: func(t *testing.T, h *cliHarness, _ bulkMutationResult) {
				assertReadState(t, h, "work-new", false)
				assertReadState(t, h, "work-old", false)
			},
		},
		{
			name:      "flag set",
			args:      []string{"flag", "set", "--id", "work-new", "--id", "work-old", "--apply", "--json"},
			operation: string(mail.MutationFlagSet),
			check: func(t *testing.T, _ *cliHarness, result bulkMutationResult) {
				for _, row := range result.Targets {
					if row.FlagState == nil || !row.FlagState.After {
						t.Fatalf("flag set row = %#v", row)
					}
				}
			},
		},
		{
			name:      "flag clear",
			args:      []string{"flag", "clear", "--id", "work-new", "--id", "work-old", "--apply", "--json"},
			operation: string(mail.MutationFlagClear),
			check: func(t *testing.T, _ *cliHarness, result bulkMutationResult) {
				for _, row := range result.Targets {
					if row.FlagState == nil || row.FlagState.After {
						t.Fatalf("flag clear row = %#v", row)
					}
				}
			},
		},
		{
			name:      "categories set",
			args:      []string{"categories", "set", "--id", "work-new", "--id", "work-old", "--category", "Client", "--category", "Urgent", "--apply", "--json"},
			operation: string(mail.MutationCategoriesSet),
			check: func(t *testing.T, h *cliHarness, _ bulkMutationResult) {
				assertCategories(t, h, "work-new", []string{"Client", "Urgent"})
				assertCategories(t, h, "work-old", []string{"Client", "Urgent"})
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, fake := seededMutationHarness(t)
			var result bulkMutationResult
			decodeOK(t, h.run(tt.args...), &result)
			if result.Operation != tt.operation || result.Counts.Total != 2 || result.Counts.Succeeded != 2 || !result.ActionLogStatus.Written {
				t.Fatalf("bulk result = %#v", result)
			}
			if fake.MessageMutationCalls.Load()+fake.CategorySetCalls.Load() != 2 {
				t.Fatalf("write calls mutation=%d category=%d", fake.MessageMutationCalls.Load(), fake.CategorySetCalls.Load())
			}
			tt.check(t, h, result)
			entries := readActionLog(t, h)
			if len(entries) != 1 || entries[0].Operation != mail.MessageMutationOperation(tt.operation) || entries[0].Counts.Succeeded != 2 {
				t.Fatalf("action log entries = %#v", entries)
			}
		})
	}
}

func TestBulkMutationIdempotentRepeatRuns(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		operation mail.MessageMutationOperation
	}{
		{name: "mark read", args: []string{"mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json"}, operation: mail.MutationMarkRead},
		{name: "flag clear", args: []string{"flag", "clear", "--id", "work-new", "--id", "work-old", "--apply", "--json"}, operation: mail.MutationFlagClear},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, fake := seededMutationHarness(t)
			for i := 0; i < 2; i++ {
				var result bulkMutationResult
				decodeOK(t, h.run(tt.args...), &result)
				if result.Operation != string(tt.operation) || result.Counts.Total != 2 || result.Counts.Succeeded != 2 || result.Counts.Failed != 0 || result.Counts.Skipped != 0 {
					t.Fatalf("run %d result = %#v", i+1, result)
				}
			}
			if fake.MessageMutationCalls.Load() != 4 {
				t.Fatalf("mutation calls = %d, want 4", fake.MessageMutationCalls.Load())
			}
			entries := readActionLog(t, h)
			if len(entries) != 2 {
				t.Fatalf("entry count = %d, want 2: %#v", len(entries), entries)
			}
			for _, entry := range entries {
				if entry.Operation != tt.operation || entry.Counts.Succeeded != 2 || entry.Counts.Failed != 0 || entry.Counts.Skipped != 0 {
					t.Fatalf("repeat entry = %#v", entry)
				}
			}
		})
	}
}

func TestBulkMutationOutputModesAndMetadata(t *testing.T) {
	h, _ := seededMutationHarness(t)
	human := h.run("mark", "read", "--id", "work-new", "--id", "work-old", "--apply")
	decodeOK(t, human, nil)
	if !strings.Contains(human.Stdout, "Marked 2 messages in work as read.") || !strings.Contains(human.Stdout, "Contract update") {
		t.Fatalf("human bulk stdout = %q", human.Stdout)
	}

	jqHarness, _ := seededMutationHarness(t)
	if got := strings.TrimSpace(jqHarness.run("mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json", "--jq", ".counts.succeeded").Stdout); got != "2" {
		t.Fatalf("bulk jq = %q", got)
	}

	templateHarness, _ := seededMutationHarness(t)
	if got := strings.TrimSpace(templateHarness.run("mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--template", "{{.counts.total}}/{{.counts.succeeded}}").Stdout); got != "2/2" {
		t.Fatalf("bulk template = %q", got)
	}

	metaHarness, _ := seededMutationHarness(t)
	var envelope struct {
		Data bulkMutationResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, metaHarness.run("mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["target_count"] != float64(2) || envelope.Meta.Extra["action_log_written"] != true {
		t.Fatalf("bulk meta = %#v", envelope.Meta.Extra)
	}
}

func TestSingleMutationWritesActionLogAndApplyIsNoop(t *testing.T) {
	h, fake := seededMutationHarness(t)
	var result mutationResult
	decodeOK(t, h.run("archive", "--id", "work-new", "--apply", "--json"), &result)
	if !result.Applied || !result.ActionLogStatus.Written || result.ActionLogStatus.RelativePath != audit.RelativePath {
		t.Fatalf("single result = %#v", result)
	}
	if fake.MessageMutationCalls.Load() != 1 {
		t.Fatalf("mutation calls = %d", fake.MessageMutationCalls.Load())
	}
	entries := readActionLog(t, h)
	if len(entries) != 1 || entries[0].Selector.Kind != "id" || entries[0].Counts.Succeeded != 1 {
		t.Fatalf("entries = %#v", entries)
	}

	dry, dryFake := seededMutationHarness(t)
	decodePreview(t, dry.run("archive", "--id", "work-new", "--dry-run", "--json"))
	if dryFake.MessageMutationCalls.Load() != 0 || actionLogExists(t, dry) {
		t.Fatalf("dry-run wrote mutation=%d log=%t", dryFake.MessageMutationCalls.Load(), actionLogExists(t, dry))
	}
}

func TestSingleMutationProviderFailureIsLoggedAndReturnsUnderlyingError(t *testing.T) {
	fake := testkit.NewFakeMutationProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages())
	fake.MutationErrByMessage = map[string]error{
		"work-new": errs.New(errs.CodeMessageNotFound, "message not found"),
	}
	h, _ := seedMutationHarness(t, fake)
	res := h.run("mark", "read", "--id", "work-new", "--json")
	assertErrorCodeExit(t, res, errs.CodeMessageNotFound, 5)
	entries := readActionLog(t, h)
	if len(entries) != 1 || entries[0].Counts.Failed != 1 || entries[0].Targets[0].Status != "failed" {
		t.Fatalf("failure log = %#v", entries)
	}
}

func TestBulkMutationIDsFileSelectors(t *testing.T) {
	t.Run("bare preview and apply", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		path := filepath.Join(h.root, "ids.txt")
		if err := os.WriteFile(path, []byte("work-new\r\n\nwork-old\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		preview := decodePreview(t, h.run("flag", "set", "--ids-file", path, "--json"))
		if sel := preview["selector"].(map[string]any); sel["ids_file"] != "ids.txt" || sel["target_count"] != float64(2) {
			t.Fatalf("ids-file preview = %#v", preview)
		}
		var result bulkMutationResult
		decodeOK(t, h.run("flag", "set", "--ids-file", path, "--apply", "--json"), &result)
		if result.Counts.Succeeded != 2 || fake.MessageMutationCalls.Load() != 2 {
			t.Fatalf("ids-file result = %#v calls=%d", result, fake.MessageMutationCalls.Load())
		}
		rawLog, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath))
		if err != nil {
			t.Fatalf("read action log: %v", err)
		}
		if strings.Contains(string(rawLog), h.root) || strings.Contains(string(rawLog), path) {
			t.Fatalf("ids-file action log leaked local path: %s", rawLog)
		}
	})

	t.Run("account qualified order", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		path := filepath.Join(h.root, "qualified.txt")
		if err := os.WriteFile(path, []byte("personal\tpersonal-new\nwork\twork-new\nwork\twork-old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var result bulkMutationResult
		decodeOK(t, h.run("mark", "read", "--ids-file", path, "--apply", "--json"), &result)
		if result.Selector.Kind != "ids_file_qualified" || result.Counts.Succeeded != 3 {
			t.Fatalf("qualified result = %#v", result)
		}
		if got := strings.Join(fake.MutationCallLog, ","); got != "personal:personal-new,work:work-new,work:work-old" {
			t.Fatalf("qualified order = %s", got)
		}
	})

	t.Run("invalid files", func(t *testing.T) {
		h, _ := seededMutationHarness(t)
		write := func(name string, data []byte) string {
			t.Helper()
			path := filepath.Join(h.root, name)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}
		for _, path := range []string{
			filepath.Join(h.root, "missing.txt"),
			write("empty.txt", []byte("\n\r\n")),
			write("mixed.txt", []byte("work-new\nwork\twork-old\n")),
			write("duplicate-bare.txt", []byte("work-new\nwork-new\n")),
			write("duplicate-qualified.txt", []byte("work\twork-new\nwork\twork-new\n")),
			write("extra.txt", []byte("work\twork-new\textra\n")),
			write("missing-account.txt", []byte("\twork-new\n")),
			write("missing-id.txt", []byte("work\t \n")),
			write("invalid-utf8.txt", []byte{0xff, 0xfe}),
			write("long.txt", []byte(strings.Repeat("a", maxIDsFileLineBytes+1))),
		} {
			assertErrorCode(t, h.run("archive", "--ids-file", path, "--json"), errs.CodeInvalidSelector)
		}
		tooLarge := filepath.Join(h.root, "large.txt")
		if err := os.WriteFile(tooLarge, []byte(strings.Repeat("a", maxIDsFileBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		assertErrorCode(t, h.run("archive", "--ids-file", tooLarge, "--json"), errs.CodeInvalidSelector)
		var rows strings.Builder
		for i := 0; i <= maxExplicitMutationTargets; i++ {
			rows.WriteString("msg-")
			rows.WriteString(strconv.Itoa(i))
			rows.WriteByte('\n')
		}
		assertErrorCode(t, h.run("archive", "--ids-file", write("too-many-ids.txt", []byte(rows.String())), "--json"), errs.CodeInvalidSelector)
	})
}

func TestBulkMutationQueryAllSelectors(t *testing.T) {
	t.Run("preview and apply", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		preview := decodePreview(t, h.run("archive", "--query", "anything", "--all", "--max-targets", "10", "--json"))
		if sel := preview["selector"].(map[string]any); sel["kind"] != "query_all" || sel["target_count"] != float64(4) || sel["max_targets"] != float64(10) {
			t.Fatalf("query-all preview = %#v", preview)
		}
		if cmd, _ := preview["apply_command"].(string); !strings.HasSuffix(cmd, " --json --account work --id work-new --id work-dupe-b --id work-dupe-a --id work-old --apply") {
			t.Fatalf("apply command = %v", preview["apply_command"])
		}
		if fake.MessageMutationCalls.Load() != 0 {
			t.Fatalf("query-all preview wrote %d mutations", fake.MessageMutationCalls.Load())
		}
		var result bulkMutationResult
		decodeOK(t, h.run("archive", "--query", "anything", "--all", "--max-targets", "10", "--apply", "--json"), &result)
		if result.Selector.Kind != "query_all" || result.Counts.Succeeded != 4 || !result.ActionLogStatus.Written {
			t.Fatalf("query-all result = %#v", result)
		}
	})

	t.Run("zero targets", func(t *testing.T) {
		fake := testkit.NewFakeMutationProvider("fake", map[string][]mail.MessageSummary{"work": nil}, map[string]map[string]mail.Message{"work": {}})
		h, _ := seedMutationHarness(t, fake)
		preview := decodePreview(t, h.run("archive", "--query", "none", "--all", "--json"))
		if preview["count"] != float64(0) || preview["apply_command"] != nil {
			t.Fatalf("zero preview = %#v", preview)
		}
		var result bulkMutationResult
		decodeOK(t, h.run("archive", "--query", "none", "--all", "--apply", "--json"), &result)
		if result.Counts.Total != 0 || result.ActionLogStatus.Written || result.ActionLogStatus.Reason != "no_targets" || actionLogExists(t, h) {
			t.Fatalf("zero apply = %#v log=%t", result, actionLogExists(t, h))
		}
	})

	t.Run("limit overflow", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		res := h.run("archive", "--query", "anything", "--all", "--max-targets", "2", "--json")
		assertErrorCode(t, res, errs.CodeBulkTargetLimitExceeded)
		if fake.MessageMutationCalls.Load() != 0 || actionLogExists(t, h) {
			t.Fatalf("overflow wrote mutation=%d log=%t", fake.MessageMutationCalls.Load(), actionLogExists(t, h))
		}
	})
}

func TestBulkMutationPartialFailuresAndAccountStops(t *testing.T) {
	t.Run("message not found continues", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		res := h.run("mark", "read", "--id", "work-new", "--id", "missing", "--id", "work-old", "--apply", "--json")
		assertErrorCodeExit(t, res, errs.CodeBulkPartialFailure, 5)
		if res.Stdout != "" {
			t.Fatalf("partial failure wrote stdout: %s", res.Stdout)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		counts := body["counts"].(map[string]any)
		if counts["succeeded"] != float64(2) || counts["failed"] != float64(1) || fake.MessageMutationCalls.Load() != 3 {
			t.Fatalf("partial body=%#v calls=%d", body, fake.MessageMutationCalls.Load())
		}
		if body["underlying_error_code"] != errs.CodeMessageNotFound {
			t.Fatalf("underlying error code = %#v", body["underlying_error_code"])
		}
		if entries := readActionLog(t, h); len(entries) != 1 || entries[0].Counts.Failed != 1 {
			t.Fatalf("partial log = %#v", entries)
		}
	})

	t.Run("pre-mutation account stop skips and does not log when nothing attempted", func(t *testing.T) {
		stale := testkit.NewFakeStaleMutationProvider("fake")
		h, _ := seedMutationHarness(t, stale)
		res := h.run("archive", "--id", "work-new", "--id", "work-old", "--apply", "--json")
		assertErrorCode(t, res, errs.CodeBulkPartialFailure)
		if res.Stdout != "" || actionLogExists(t, h) || stale.MessageMutationCalls.Load() != 0 {
			t.Fatalf("pre-stop stdout=%q log=%t calls=%d", res.Stdout, actionLogExists(t, h), stale.MessageMutationCalls.Load())
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		status := body["action_log_status"].(map[string]any)
		if status["reason"] != "not_attempted" || body["mutation_attempted"] != false {
			t.Fatalf("pre-stop body = %#v", body)
		}
	})

	t.Run("account-wide failure skips later same-account targets", func(t *testing.T) {
		fake := testkit.NewFakeMutationProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages())
		fake.MutationErrByMessage = map[string]error{
			"work-new": errs.New(errs.CodeAuthRepairRequired, "credentials need repair"),
		}
		h, _ := seedMutationHarness(t, fake)
		path := filepath.Join(h.root, "targets.txt")
		if err := os.WriteFile(path, []byte("work\twork-new\npersonal\tpersonal-new\nwork\twork-old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := h.run("mark", "read", "--ids-file", path, "--apply", "--json")
		assertErrorCodeExit(t, res, errs.CodeBulkPartialFailure, 3)
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		targets := body["targets"].([]any)
		if targets[0].(map[string]any)["status"] != "failed" || targets[1].(map[string]any)["status"] != "succeeded" || targets[2].(map[string]any)["status"] != "skipped" {
			t.Fatalf("target statuses = %#v", targets)
		}
		if got := strings.Join(fake.MutationCallLog, ","); got != "work:work-new,personal:personal-new" {
			t.Fatalf("mutation calls = %s", got)
		}
	})

	t.Run("rate limit exit wins over skipped targets", func(t *testing.T) {
		fake := testkit.NewFakeMutationProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages())
		fake.MutationErrByMessage = map[string]error{
			"work-new": rungrad.NewError(rungrad.ExitRateLimited, "rate limited"),
		}
		h, _ := seedMutationHarness(t, fake)
		res := h.run("mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json")
		assertErrorCodeExit(t, res, errs.CodeBulkPartialFailure, rungrad.ExitRateLimited)
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		targets := body["targets"].([]any)
		if targets[0].(map[string]any)["status"] != "failed" || targets[1].(map[string]any)["status"] != "skipped" {
			t.Fatalf("target statuses = %#v", targets)
		}
	})
}

func TestActionLogWriteFailureTakesPrecedence(t *testing.T) {
	t.Run("all provider attempts succeeded", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		makeActionLogDirectory(t, h)
		res := h.run("mark", "read", "--id", "work-new", "--id", "work-old", "--apply", "--json")
		assertErrorCode(t, res, errs.CodeActionLogWriteFailed)
		if strings.Contains(res.Stderr, h.root) || res.Stdout != "" || fake.MessageMutationCalls.Load() != 2 {
			t.Fatalf("log failure stdout=%q calls=%d", res.Stdout, fake.MessageMutationCalls.Load())
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["partial_failure"]; ok {
			t.Fatalf("log failure without target failures marked partial: %#v", body["partial_failure"])
		}
	})

	t.Run("provider partial also occurred", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		makeActionLogDirectory(t, h)
		res := h.run("mark", "read", "--id", "work-new", "--id", "missing", "--apply", "--json")
		assertErrorCode(t, res, errs.CodeActionLogWriteFailed)
		var body map[string]any
		if err := json.Unmarshal([]byte(res.Stderr), &body); err != nil {
			t.Fatal(err)
		}
		counts := body["counts"].(map[string]any)
		if counts["failed"] != float64(1) || fake.MessageMutationCalls.Load() != 2 {
			t.Fatalf("log precedence body=%#v calls=%d", body, fake.MessageMutationCalls.Load())
		}
		if body["partial_failure"] != true {
			t.Fatalf("partial failure marker = %#v", body["partial_failure"])
		}
		if body["underlying_error_code"] != errs.CodeMessageNotFound {
			t.Fatalf("underlying error code = %#v", body["underlying_error_code"])
		}
	})
}

func TestBulkCategoryMutations(t *testing.T) {
	t.Run("named clear reads once per target before patching", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		var result bulkMutationResult
		decodeOK(t, h.run("categories", "clear", "--id", "work-new", "--id", "work-old", "--category", "Client", "--apply", "--json"), &result)
		if result.Counts.Succeeded != 2 || fake.CategoryListCalls.Load() != 2 || fake.CategorySetCalls.Load() != 2 {
			t.Fatalf("named clear result=%#v list=%d set=%d", result, fake.CategoryListCalls.Load(), fake.CategorySetCalls.Load())
		}
	})

	t.Run("clear all patches without reads", func(t *testing.T) {
		h, fake := seededMutationHarness(t)
		var result bulkMutationResult
		decodeOK(t, h.run("categories", "clear", "--id", "work-new", "--id", "work-old", "--apply", "--json"), &result)
		if result.Counts.Succeeded != 2 || fake.CategoryListCalls.Load() != 0 || fake.CategorySetCalls.Load() != 2 {
			t.Fatalf("clear all result=%#v list=%d set=%d", result, fake.CategoryListCalls.Load(), fake.CategorySetCalls.Load())
		}
	})
}

func TestMutationOutputModesAndSecretSweep(t *testing.T) {
	h, fake := seededMutationHarness(t)
	var result mutationResult
	res := h.run("mark", "read", "--id", "work-new", "--json")
	decodeOK(t, res, &result)
	for _, forbidden := range []string{
		"seed-access-token-work",
		"seed-refresh-token-work",
		h.root,
		"Full body for work-new",
	} {
		if strings.Contains(res.Stdout+res.Stderr, forbidden) {
			t.Fatalf("mutation output leaked %q\nstdout=%s\nstderr=%s", forbidden, res.Stdout, res.Stderr)
		}
	}
	rawLog, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath))
	if err != nil {
		t.Fatalf("read action log: %v", err)
	}
	rawLogText := string(rawLog)
	for _, forbidden := range []string{
		"seed-access-token-work",
		"seed-refresh-token-work",
		h.root,
		"Full body for work-new",
	} {
		if strings.Contains(rawLogText, forbidden) {
			t.Fatalf("action log leaked %q", forbidden)
		}
	}
	if got := strings.TrimSpace(h.run("mark", "read", "--id", "work-old", "--json", "--jq", ".read_state.after").Stdout); got != "true" {
		t.Fatalf("jq mark read = %q", got)
	}
	if got := strings.TrimSpace(h.run("flag", "set", "--id", "work-new", "--template", "{{.operation}}").Stdout); got != "flag_set" {
		t.Fatalf("template flag set = %q", got)
	}
	var envelope struct {
		Data mutationResult `json:"data"`
		Meta struct {
			Extra map[string]any `json:"extra"`
		} `json:"meta"`
	}
	decodeOK(t, h.run("archive", "--id", "work-new", "--json", "--include-meta"), &envelope)
	if envelope.Meta.Extra["operation"] != "archive" || envelope.Meta.Extra["selector_kind"] != "id" {
		t.Fatalf("mutation meta = %#v", envelope.Meta.Extra)
	}
	assertFrameworkUsage(t, h.run("archive", "--id", "work-new", "--plain"))
	if bad := h.run("archive", "--id", "work-new", "--dry-run", "--include-meta", "--json"); bad.Exit == 0 {
		t.Fatal("mutation --include-meta --dry-run succeeded")
	}
	if fake.MessageMutationCalls.Load() == 0 {
		t.Fatal("output-mode test did not exercise fake mutation writes")
	}
}

func assertReadFolder(t *testing.T, h *cliHarness, id, folderID string) {
	t.Helper()
	var result readResult
	decodeOK(t, h.run("read", "--id", id, "--json"), &result)
	if result.Message.Folder.ID != folderID {
		t.Fatalf("message %s folder = %#v, want %s", id, result.Message.Folder, folderID)
	}
}

func assertReadState(t *testing.T, h *cliHarness, id string, want bool) {
	t.Helper()
	var result readResult
	decodeOK(t, h.run("read", "--id", id, "--json"), &result)
	if result.Message.IsRead != want {
		t.Fatalf("message %s read = %t, want %t", id, result.Message.IsRead, want)
	}
}

func assertCategories(t *testing.T, h *cliHarness, id string, want []string) {
	t.Helper()
	var result categoryListResult
	decodeOK(t, h.run("categories", "list", "--id", id, "--json"), &result)
	if strings.Join(result.Categories, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("message %s categories = %#v, want %#v", id, result.Categories, want)
	}
}

func seededMutationHarness(t *testing.T) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	return seedMutationHarness(t, testkit.NewFakeMutationProvider("fake", testkit.SampleMessages(), testkit.SampleFullMessages()))
}

func seedMutationHarness(t *testing.T, fake *testkit.FakeProvider) (*cliHarness, *testkit.FakeProvider) {
	t.Helper()
	h := newFakeHarness(t, fake)
	work := workAccount()
	personal := maildoconfig.Account{
		Name:              "personal",
		Provider:          "fake",
		Email:             "personal@example.test",
		DisplayName:       "Personal",
		ProviderAccountID: "fake-personal",
		AuthRef:           "fake/personal",
		DefaultFrom:       "personal@example.test",
	}
	seedRegistry(t, h.configPath, "work", []maildoconfig.Account{work, personal})
	if err := testkit.SeedCredential(filepath.Dir(h.configPath), work, nil); err != nil {
		t.Fatalf("seed work credential: %v", err)
	}
	if err := testkit.SeedCredential(filepath.Dir(h.configPath), personal, nil); err != nil {
		t.Fatalf("seed personal credential: %v", err)
	}
	return h, fake
}

func actionLogExists(t *testing.T, h *cliHarness) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(h.root, audit.RelativePath))
	return err == nil
}

func readActionLog(t *testing.T, h *cliHarness) []audit.Entry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, audit.RelativePath))
	if err != nil {
		t.Fatalf("read action log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	entries := make([]audit.Entry, 0, len(lines))
	for _, line := range lines {
		var entry audit.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode action log line: %v\n%s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

func makeActionLogDirectory(t *testing.T, h *cliHarness) {
	t.Helper()
	path := filepath.Join(h.root, audit.RelativePath)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("make action log directory: %v", err)
	}
}

func previewTargets(t *testing.T, preview map[string]any) []map[string]any {
	t.Helper()
	raw, ok := preview["targets"].([]any)
	if !ok {
		t.Fatalf("preview has no targets: %#v", preview)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, row := range raw {
		out = append(out, row.(map[string]any))
	}
	return out
}

// Archiving a message that is already in Archive changes nothing, and the
// result says so instead of "Archived".
func TestMoveToTheSameFolderSaysNothingChanged(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("Archive", "From: Clara <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Old invoice\r\nMessage-ID: <old@example.org>\r\n\r\nPaid.\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "invoice", "--folder", "Archive", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	res := h.run("archive", "--id", found.Messages[0].ID)
	if res.Exit != 0 || !strings.Contains(res.Stdout, "is already in Archive, so nothing changed.") || strings.Contains(res.Stdout, "Archived") {
		t.Fatalf("archive in Archive: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}

// A change to many messages says how far it got, in a terminal.
func TestBulkChangeShowsProgressInATerminal(t *testing.T) {
	every := bulkProgressEvery
	bulkProgressEvery = 0
	defer func() { bulkProgressEvery = every }()
	h := newSendHarness(t)
	for i := 0; i < 3; i++ {
		for _, from := range []string{"deals", "promo"} {
			h.server.Append("INBOX", fmt.Sprintf("From: %s@shop.example\r\nTo: me@posteo.de\r\nSubject: Deal %d\r\nMessage-ID: <%s%d@shop.example>\r\n\r\nCheap.\r\n", from, i, from, i), time.Now())
		}
	}
	res := testutil.RunWith(h.app(), testutil.Options{
		OutputTerminal: true, OutputTerminalSet: true,
		LookupEnv: func(key string) (string, bool) { v, ok := h.env[key]; return v, ok },
	}, "--config", h.configPath, "archive", "--query", "from:deals@shop.example", "--all", "--apply")
	if res.Exit != 0 || !strings.Contains(res.Stderr, "2 of 3 messages done...") {
		t.Fatalf("archive: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
	quiet := h.run("archive", "--query", "from:promo@shop.example", "--all", "--apply")
	if quiet.Exit != 0 || strings.Contains(quiet.Stderr, "messages done") {
		t.Fatalf("progress without a terminal: %s", quiet.Stderr)
	}
}

// An account that cannot do something says so in words, without the
// internal name of the feature.
func TestUnsupportedFeatureIsExplainedInWords(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Clara <clara@example.org>\r\nTo: me@posteo.de\r\nSubject: Hi\r\nMessage-ID: <hi@example.org>\r\n\r\nHi.\r\n", time.Now())
	var found searchResult
	if res := h.run("search", "hi", "--json"); res.JSON(&found) != nil || len(found.Messages) != 1 {
		t.Fatalf("search: %s%s", res.Stdout, res.Stderr)
	}
	res := h.run("categories", "list", "--id", found.Messages[0].ID)
	if res.Exit == 0 || !strings.Contains(res.Stderr, "cannot use categories, which only Outlook and Gmail accounts have") || strings.Contains(res.Stderr, "categories_read") {
		t.Fatalf("categories on IMAP: exit %d\n%s", res.Exit, res.Stderr)
	}
}
