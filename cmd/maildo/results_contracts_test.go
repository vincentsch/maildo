package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/vincentsch/rungrad"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestMutationResultConvertersPreservePresenceFactsAndCloneMetadata(t *testing.T) {
	changed := false
	facts := &mail.WriteOutcomeFacts{
		Outcome:             mail.WriteOutcomeSucceeded,
		WriteMayHaveLanded:  true,
		AcceptedByProvider:  true,
		ConfirmedAfterWrite: true,
		Succeeded:           true,
	}
	meta, err := mail.NewFakeProviderMetadataForTest(mail.MetadataMessageMutation, mail.MutationProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		t.Fatal(err)
	}
	target := mutationTarget{
		Selection: provider.AccountSelection{Scope: "single", Accounts: []provider.ResolvedAccount{{Name: "work", Provider: "fake"}}},
		Account:   provider.ResolvedAccount{Name: "work", Provider: "fake"},
		Selector:  mail.MessageSelector{Kind: mail.SelectorID, ID: "selected-id"},
		MessageID: "selected-id",
	}
	providerResult := mail.MessageMutationResult{
		MessageID:         "provider-id",
		InternetMessageID: "<provider@example.test>",
		Changed:           &changed,
		WriteOutcomeFacts: facts,
		ProviderMeta:      meta,
	}

	single := mutationResultFromProvider(target, mail.MutationMove, true, providerResult)
	if single.Changed == nil || *single.Changed || single.Changed == &changed {
		t.Fatalf("changed pointer = %#v", single.Changed)
	}
	if single.WriteOutcomeFacts == nil || single.WriteOutcomeFacts == facts || single.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("write facts = %#v", single.WriteOutcomeFacts)
	}
	if single.ProviderMeta == nil || single.ProviderMeta == meta {
		t.Fatalf("metadata was not cloned")
	}

	row := appliedTargetFromResult(target, mail.MutationMove, providerResult, nil)
	if row.Changed == nil || *row.Changed || row.Changed == &changed {
		t.Fatalf("row changed pointer = %#v", row.Changed)
	}
	if row.WriteOutcomeFacts == nil || row.WriteOutcomeFacts == facts || row.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("row write facts = %#v", row.WriteOutcomeFacts)
	}
	if row.ProviderMeta == nil || row.ProviderMeta == meta {
		t.Fatalf("row metadata was not cloned")
	}
}

func TestGmailMutationOutputIncludesChangedAndProviderLabelMetadata(t *testing.T) {
	changed := true
	finalLabels := []string{"INBOX", "Label_42"}
	meta, err := mail.NewProviderMetadata(errs.ProviderGmail, mail.MetadataMessageMutation, mail.MutationProviderMetadata{
		Source:        mail.MetadataSourceGmailAPI,
		MutationBasis: mail.MutationBasisGmailLabels,
		FinalLabelIDs: &finalLabels,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := mutationTarget{
		Selection:    provider.AccountSelection{Scope: "single", Accounts: []provider.ResolvedAccount{{Name: "gmail-home", Provider: "gmail"}}},
		Account:      provider.ResolvedAccount{Name: "gmail-home", Provider: "gmail"},
		Selector:     mail.MessageSelector{Kind: mail.SelectorID, ID: "g-2"},
		SelectorKind: "id",
		MessageID:    "g-2",
	}
	result := mutationResultFromProvider(target, mail.MutationCopy, true, mail.MessageMutationResult{
		MessageID:    "g-2",
		TargetFolder: &mail.FolderSummary{ID: "Label_42", DisplayName: "Project Alpha"},
		Changed:      &changed,
		WriteOutcomeFacts: &mail.WriteOutcomeFacts{
			Outcome:             mail.WriteOutcomeSucceeded,
			WriteMayHaveLanded:  true,
			AcceptedByProvider:  true,
			ConfirmedAfterWrite: true,
			Succeeded:           true,
		},
		ProviderMeta: meta,
	})
	result.ActionLogStatus = mail.ActionLogStatus{Written: true, EntryCount: 1, RelativePath: "audit/actions.jsonl"}

	target.Summary = &mail.MessageSummary{Subject: "Kickoff", From: mail.Recipient{Name: "Clara Client", Address: "clara@example.test"}}
	var human bytes.Buffer
	f := &rungrad.Factory{Flags: &rungrad.GlobalFlags{JSON: true}}
	humanMutationResult(f, result, target, mutationPlan{Operation: mail.MutationCopy})(&human)
	if got, want := human.String(), "Copied \"Kickoff\" from Clara Client to Project Alpha.\n"; got != want {
		t.Fatalf("human output = %q, want %q", got, want)
	}

	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["changed"] != true || got["outcome"] != string(mail.WriteOutcomeSucceeded) {
		t.Fatalf("mutation json = %#v", got)
	}
	providerMeta, ok := got["provider_metadata"].(map[string]any)
	if !ok || providerMeta["mutation_basis"] != string(mail.MutationBasisGmailLabels) {
		t.Fatalf("provider metadata = %#v", got["provider_metadata"])
	}
	labels, ok := providerMeta["final_label_ids"].([]any)
	if !ok || len(labels) != 2 || labels[0] != "INBOX" || labels[1] != "Label_42" {
		t.Fatalf("final label ids = %#v", providerMeta["final_label_ids"])
	}
	actionLog, ok := got["action_log_status"].(map[string]any)
	if !ok || actionLog["written"] != true || actionLog["relative_path"] != "audit/actions.jsonl" {
		t.Fatalf("action log status = %#v", got["action_log_status"])
	}
}

// Previews say whether a read or flag change would do anything only when a
// search told maildo the message's state. They never claim "unchanged" for
// a message they did not look at.
func TestMutationPreviewsReportWouldChangeOnlyWhenKnown(t *testing.T) {
	acct := provider.ResolvedAccount{Name: "work", Provider: "imap"}
	set := mutationTargetSet{Targets: []mutationTarget{
		{Account: acct, MessageID: "unread", Summary: &mail.MessageSummary{IsRead: false}},
		{Account: acct, MessageID: "read", Summary: &mail.MessageSummary{IsRead: true}},
		{Account: acct, MessageID: "by-id"},
		{Account: acct, MessageID: "described", Summary: &mail.MessageSummary{IsRead: true}, Described: true},
	}}
	preview := buildMutationPreview(set, mutationPlan{Operation: mail.MutationMarkRead}, nil, nil, "")
	want := map[string]string{"unread": "true", "read": "false", "by-id": "unknown", "described": "unknown"}
	for _, row := range preview.Targets {
		got := "unknown"
		if row.WouldChange != nil {
			got = fmt.Sprint(*row.WouldChange)
		}
		if got != want[row.ID] {
			t.Fatalf("%s: would_change = %s, want %s", row.ID, got, want[row.ID])
		}
	}
	archive := buildMutationPreview(set, mutationPlan{Operation: mail.MutationArchive}, nil, nil, "")
	for _, row := range archive.Targets {
		if row.WouldChange != nil {
			t.Fatalf("archive preview claims would_change for %s", row.ID)
		}
	}
}

func TestAppliedMutationFailureFlattensWriteOutcomeFacts(t *testing.T) {
	attempt, err := errs.NewProviderWriteAttempt("gmail-home", errs.ProviderGmail, errs.WriteGmailCopy)
	if err != nil {
		t.Fatal(err)
	}
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		t.Fatal(err)
	}
	writeErr := errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
	target := mutationTarget{
		Account:   provider.ResolvedAccount{Name: "gmail-home", Provider: "gmail"},
		Selector:  mail.MessageSelector{Kind: mail.SelectorID, ID: "g-2"},
		MessageID: "g-2",
	}

	row := appliedTargetFromResult(target, mail.MutationCopy, mail.MessageMutationResult{}, writeErr)
	if row.Status != "failed" || row.WriteOutcomeFacts == nil || row.Outcome != mail.WriteOutcomeUnknown || !row.WriteMayHaveLanded || !row.AcceptedByProvider {
		t.Fatalf("row = %#v", row)
	}
	for _, key := range []string{"outcome", "accepted_by_provider", "confirmed_after_write", "succeeded"} {
		if _, ok := row.ErrorDetails[key]; ok {
			t.Fatalf("reserved write key %q nested in details %#v", key, row.ErrorDetails)
		}
	}
	// Whether the change may have happened is told with the error, so a
	// script finds it in error_details.
	if row.ErrorDetails["write_may_have_landed"] != true || row.ErrorDetails["retry_safe"] != false {
		t.Fatalf("error details = %#v", row.ErrorDetails)
	}
	if row.ErrorDetails["reason"] != string(errs.OutcomePostWriteReadFailed) || row.ErrorDetails["operation"] != string(errs.WriteGmailCopy) {
		t.Fatalf("error details = %#v", row.ErrorDetails)
	}
}

func TestMergeCategoryChangeRespectsNonNilEmptyProviderSlices(t *testing.T) {
	got := &mail.CategoryChange{
		Before:  []string{},
		After:   []string{},
		Removed: []string{},
		Skipped: []string{},
		Count:   0,
	}
	fallback := &mail.CategoryChange{
		Before:  []string{"Project/A", "Project/B"},
		After:   []string{"Project/B"},
		Removed: []string{"Project/A"},
		Skipped: []string{"Missing"},
		Count:   1,
	}

	merged := mergeCategoryChange(got, fallback)
	if len(merged.After) != 0 || len(merged.Before) != 0 || len(merged.Removed) != 0 || len(merged.Skipped) != 0 || merged.Count != 0 {
		t.Fatalf("non-nil empty slices were overwritten: %#v", merged)
	}
}

func TestFolderMutationResultPreservesDescendantsAndWriteFacts(t *testing.T) {
	facts := &mail.WriteOutcomeFacts{Outcome: mail.WriteOutcomeNoOp, RetrySafe: true, Succeeded: true}
	meta, err := mail.NewFakeProviderMetadataForTest(mail.MetadataFolderMutationResult, mail.FolderProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		t.Fatal(err)
	}
	result := folderMutationResultFromProvider(provider.AccountSelection{Scope: "single"}, mail.FolderMutationResult{
		Operation:              mail.FolderOperationDelete,
		DescendantFactsPresent: true,
		DescendantFacts: mail.FolderDescendantFacts{
			Count:  1,
			Known:  true,
			Sample: []mail.FolderDescendant{{ID: "child", DisplayName: "Child"}},
		},
		WriteOutcomeFacts: facts,
		ProviderMeta:      meta,
	}, true)
	if result.DescendantFacts == nil || len(result.DescendantFacts.Sample) != 1 || result.DescendantFacts.Sample[0].ID != "child" {
		t.Fatalf("descendant facts = %#v", result.DescendantFacts)
	}
	if result.WriteOutcomeFacts == nil || result.WriteOutcomeFacts == facts || result.Outcome != mail.WriteOutcomeNoOp {
		t.Fatalf("write facts = %#v", result.WriteOutcomeFacts)
	}
	if result.ProviderMeta == nil || result.ProviderMeta == meta {
		t.Fatalf("metadata was not cloned")
	}
	result.DescendantFacts.Sample[0].ID = "mutated"
	if result.DescendantFacts.Sample[0].ID == "child" {
		t.Fatal("sanity check failed")
	}
}

func TestWriteFactsFlattenInCommandDTOJSON(t *testing.T) {
	body, err := json.Marshal(draftWriteResult{
		Operation: "draft_create",
		DraftID:   "draft-1",
		WriteOutcomeFacts: &mail.WriteOutcomeFacts{
			Outcome:             mail.WriteOutcomeSucceeded,
			WriteMayHaveLanded:  true,
			AcceptedByProvider:  true,
			ConfirmedAfterWrite: true,
			Succeeded:           true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["outcome"] != string(mail.WriteOutcomeSucceeded) || got["confirmed_after_write"] != true {
		t.Fatalf("json = %#v", got)
	}
	// A success does not carry the fields that only matter after a failure.
	for _, key := range []string{"write_may_have_landed", "retry_safe"} {
		if _, ok := got[key]; ok {
			t.Fatalf("a successful result has %q: %#v", key, got)
		}
	}
	if _, nested := got["write_outcome"]; nested {
		t.Fatalf("write facts nested unexpectedly: %#v", got)
	}
}
