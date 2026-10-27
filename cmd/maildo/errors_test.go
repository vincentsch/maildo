package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

// Each error must name the cause and a next step instead of a bare
// "provider connection failed".
func TestErrorsExplainCauseAndNextStep(t *testing.T) {
	conn := func(reason string, extra map[string]any) error {
		d := map[string]any{"account": "work", "provider": "imap", "protocol": "imap", "operation": "imap_auth", "reason": reason, "host": "imap.example.net", "port": 993, "tls_mode": "implicit"}
		for k, v := range extra {
			d[k] = v
		}
		return errs.NewWithDetails(errs.CodeProviderConnectionError, "provider connection failed", d)
	}
	for _, tt := range []struct {
		name string
		err  error
		want []string
	}{
		{"unknown host", conn("dns", nil), []string{"could not find the IMAP server imap.example.net:993", "maildo auth repair work"}},
		{"port closed", conn("refused", nil), []string{"refused the connection", "993"}},
		{"untrusted certificate", conn("tls_verification_failed", map[string]any{"cause": "x509: certificate signed by unknown authority"}), []string{"certificate maildo does not trust", "unknown authority", "--ca-file"}},
		{"TLS on a plain port", conn("tls_not_offered", nil), []string{"did not start TLS", "--imap-tls starttls"}},
		{"STARTTLS on a TLS port", conn("timeout", map[string]any{"phase": "greeting", "tls_mode": "starttls"}), []string{"did not answer in time", "--imap-tls implicit"}},
		{"wrong password at login", errs.NewWithDetails(errs.CodeProviderIdentityUnavailable, "provider identity is unavailable", map[string]any{"reason": "credential_rejected", "account": "work", "provider": "imap"}), []string{"rejected the username or password", "app password"}},
		{"saved password rejected", errs.NewWithDetails(errs.CodeAuthRepairRequired, "stored credential was rejected by provider", map[string]any{"reason": "credential_rejected", "account": "work", "provider": "imap", "protocol": "imap"}), []string{"rejected the saved password", "maildo auth repair work"}},
		{"folder not empty", errs.NewWithDetails(errs.CodeProtectedFolder, "folder is protected", map[string]any{"reason": "not_empty", "path": "Receipts"}), []string{`"Receipts" still holds messages`}},
		{"folder with subfolders", errs.NewWithDetails(errs.CodeProtectedFolder, "folder is protected", map[string]any{"reason": "has_children", "path": "Clients"}), []string{`"Clients" has subfolders`}},
		{"stale id", errs.NewWithDetails(errs.CodeMessageNotFound, "message not found", map[string]any{"reason": "uidvalidity_changed"}), []string{"out of date", "Search for the message again"}},
		{"server said no", errs.WithDetails(errs.NewWithDetails(errs.CodeProviderDataError, "provider returned invalid protocol data", map[string]any{"protocol": "imap", "reason": "invalid_protocol"}), map[string]any{"server_message": "Mailbox doesn't exist"}), []string{"the server said: Mailbox doesn't exist"}},
		{"recipient refused for now", errs.NewSMTPRecipientTemporarilyRejected("work", errs.ProviderIMAP, 2, 1, 3), []string{"did not take recipient 2 of 3 for now", "after a few minutes"}},
		{"recipient refused", errs.NewSMTPRecipientRejected("work", errs.ProviderIMAP, 1, 0, 1), []string{"the SMTP server refused the recipient", "Check the address"}},
		{"write not applied", errs.NewWithDetails(errs.CodeProviderConnectionError, "Outlook send was not submitted", map[string]any{"outcome": "not_applied", "provider": "outlook"}), []string{"Outlook send was not submitted", "safe to try again"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeExplanation(&buf, explain(tt.err), nil)
			for _, want := range tt.want {
				if !strings.Contains(buf.String(), want) {
					t.Fatalf("missing %q in:\n%s", want, buf.String())
				}
			}
		})
	}
}

func TestMissingStandardFolderNamesTheFolder(t *testing.T) {
	err := errs.NewWithDetails(errs.CodeUnsupportedCapability, "provider capability is unavailable", map[string]any{
		"account": "gm", "provider": "imap", "capability": "draft_create", "reason": "special_use_role_missing", "role": "Drafts",
	})
	ex := explain(err)
	if ex.message != "this account has no Drafts folder" || ex.next != "create it with maildo folders create Drafts --account gm" {
		t.Fatalf("explanation = %+v", ex)
	}
}

func TestNonEmptyFolderIsNotCalledProtected(t *testing.T) {
	err := errs.NewWithDetails(errs.CodeProtectedFolder, "folder is protected", map[string]any{
		"path": "Receipts 2026", "reason": "not_empty",
	})
	ex := explain(err)
	if strings.Contains(ex.message, "protected") || !strings.Contains(ex.message, "still holds messages") {
		t.Fatalf("explanation = %+v", ex)
	}
}
