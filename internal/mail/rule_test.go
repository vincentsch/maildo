package mail

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuleMutationProviderSourceContract(t *testing.T) {
	for _, source := range []RuleMutationProviderSource{RuleMutationSourceMicrosoftGraph, RuleMutationSourceFake} {
		metadata, err := NewRuleMutationProviderMetadata(source)
		if err != nil || metadata.Source != source {
			t.Fatalf("source %q: metadata=%#v err=%v", source, metadata, err)
		}
		body, _ := json.Marshal(metadata)
		if string(body) != `{"source":"`+string(source)+`"}` {
			t.Fatalf("metadata JSON = %s", body)
		}
	}
	for _, source := range []RuleMutationProviderSource{"", "unknown", "bad\nsource", "secret-token-sentinel"} {
		metadata, err := NewRuleMutationProviderMetadata(source)
		if err == nil || metadata.Source != "" || (source != "" && strings.Contains(err.Error(), string(source))) {
			t.Fatalf("invalid source %q: metadata=%#v err=%v", source, metadata, err)
		}
	}
}

func TestRuleMutationProviderStatusContract(t *testing.T) {
	status, err := NewRuleMutationProviderStatus(false, false)
	if err != nil || status.Warnings == nil || len(status.Warnings) != 0 {
		t.Fatalf("empty status = %#v err=%v", status, err)
	}
	body, _ := json.Marshal(status)
	if string(body) != `{"has_error":false,"is_read_only":false,"warnings":[]}` {
		t.Fatalf("status JSON = %s", body)
	}
	warnings := []RuleMutationProviderWarning{RuleMutationWarningProviderMarkedReadOnly}
	status, err = NewRuleMutationProviderStatus(false, true, warnings...)
	warnings[0] = "changed"
	if err != nil || len(status.Warnings) != 1 || status.Warnings[0] != RuleMutationWarningProviderMarkedReadOnly {
		t.Fatalf("warning status = %#v err=%v", status, err)
	}
	for _, warnings := range [][]RuleMutationProviderWarning{{"unknown"}, {RuleMutationWarningProviderMarkedReadOnly, RuleMutationWarningProviderMarkedReadOnly}} {
		if _, err := NewRuleMutationProviderStatus(false, true, warnings...); err == nil {
			t.Fatalf("warnings %#v were accepted", warnings)
		}
	}
}
