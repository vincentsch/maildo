package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestCreateFolderUsesCreateAndReadBack(t *testing.T) {
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPCreateFolderSession(t, conn)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	result, err := client.CreateFolder(context.Background(), mail.FolderCreateRequest{DisplayName: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation != mail.FolderOperationCreate || result.CreatedFolder == nil ||
		result.CreatedFolder.DisplayName != "Projects" || result.CreatedFolder.Path != "Projects" {
		t.Fatalf("created result = %#v", result)
	}
	if result.VerificationStatus != mail.FolderVerificationReadBack ||
		result.WriteOutcomeFacts == nil || result.Outcome != errs.WriteOutcomeSucceeded || !result.ConfirmedAfterWrite {
		t.Fatalf("write facts/status = %#v status=%q", result.WriteOutcomeFacts, result.VerificationStatus)
	}
	if result.ProviderMeta == nil || result.ProviderMeta.Source != mail.MetadataSourceIMAP {
		t.Fatalf("provider metadata = %#v", result.ProviderMeta)
	}
}

func TestRenameFolderUsesRenameAndReadBack(t *testing.T) {
	targetID, err := formatFolderID("Projects")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPRenameFolderSession(t, conn)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	result, err := client.RenameFolder(context.Background(), mail.FolderRenameRequest{
		Target:         mail.Folder{ID: targetID},
		NewDisplayName: "Clients",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation != mail.FolderOperationRename || result.RenamedFolder == nil ||
		result.RenamedFolder.DisplayName != "Clients" || result.OldDisplayName != "Projects" ||
		result.NewDisplayName != "Clients" {
		t.Fatalf("rename result = %#v", result)
	}
	if result.VerificationStatus != mail.FolderVerificationReadBack ||
		result.WriteOutcomeFacts == nil || result.Outcome != errs.WriteOutcomeSucceeded {
		t.Fatalf("rename facts/status = %#v status=%q", result.WriteOutcomeFacts, result.VerificationStatus)
	}
}

func TestDeleteFolderUsesDeleteAndReadBack(t *testing.T) {
	targetID, err := formatFolderID("Empty")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPDeleteFolderSession(t, conn)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	result, err := client.DeleteFolder(context.Background(), mail.FolderDeleteRequest{
		Target: mail.Folder{ID: targetID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation != mail.FolderOperationDelete || result.DeletedFolder == nil ||
		result.DeletedFolder.DisplayName != "Empty" || result.TotalItemCount != 0 ||
		!result.TotalItemCountKnown || result.CountsSource != mail.FolderCountsSourceRefreshed {
		t.Fatalf("delete result = %#v", result)
	}
	if result.VerificationStatus != mail.FolderVerificationReadBack ||
		result.WriteOutcomeFacts == nil || result.Outcome != errs.WriteOutcomeSucceeded {
		t.Fatalf("delete facts/status = %#v status=%q", result.WriteOutcomeFacts, result.VerificationStatus)
	}
}

func TestCopyMessageUsesUIDCopyAndCopyUIDReadBack(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := formatFolderID("Archive")
	if err != nil {
		t.Fatal(err)
	}
	commands := make(chan []string, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPCopyMessageSession(t, conn, commands)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	result, err := client.CopyMessage(context.Background(), mail.MessageCopyRequest{
		MessageID: messageID,
		Folder:    mail.Folder{ID: targetID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID != messageID || result.ResultMessageID == "" || result.TargetFolder == nil ||
		result.TargetFolder.DisplayName != "Archive" || result.Outcome != errs.WriteOutcomeSucceeded {
		t.Fatalf("copy result = %#v", result)
	}
	client.Destroy()
	transcript := waitCommands(t, commands)
	if !transcriptHasCommand(transcript, "UID COPY") {
		t.Fatalf("transcript missing UID COPY: %#v", transcript)
	}
	if transcriptHasCommand(transcript, "UID MOVE") || transcriptHasCommand(transcript, "UID STORE") {
		t.Fatalf("copy used unexpected commands: %#v", transcript)
	}
}

func TestMoveMessageUsesUIDMoveAndCopyUIDReadBack(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := formatFolderID("Archive")
	if err != nil {
		t.Fatal(err)
	}
	commands := make(chan []string, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPMoveMessageSession(t, conn, commands)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	result, err := client.MoveMessage(context.Background(), mail.MessageMoveRequest{
		MessageID: messageID,
		Folder:    mail.Folder{ID: targetID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID != messageID || result.ResultMessageID == "" || result.TargetFolder == nil ||
		result.TargetFolder.DisplayName != "Archive" {
		t.Fatalf("move result = %#v", result)
	}
	if result.Changed == nil || !*result.Changed || result.Outcome != errs.WriteOutcomeSucceeded || !result.ConfirmedAfterWrite {
		t.Fatalf("move changed/facts = %#v changed=%v", result.WriteOutcomeFacts, result.Changed)
	}
	client.Destroy()
	transcript := waitCommands(t, commands)
	if !transcriptHasCommand(transcript, "UID MOVE") {
		t.Fatalf("transcript missing UID MOVE: %#v", transcript)
	}
	if transcriptHasCommand(transcript, "UID STORE") || transcriptHasCommand(transcript, "UID EXPUNGE") || transcriptHasCommand(transcript, "EXPUNGE") {
		t.Fatalf("move used fallback commands: %#v", transcript)
	}
}

func TestDualRevisionEnableRejectsContradictoryBad(t *testing.T) {
	commands := make(chan []string, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPContradictoryEnableSession(t, conn, commands)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	_, err := client.ListFolders(context.Background(), mail.FolderListRequest{})
	assertProductCode(t, err, errs.CodeProviderDataError)
	assertProductReason(t, err, string(errs.ProviderDataReasonInvalidProtocol))
	client.Destroy()
	transcript := waitCommands(t, commands)
	if !transcriptHasCommand(transcript, "ENABLE") {
		t.Fatalf("transcript missing ENABLE: %#v", transcript)
	}
	if transcriptHasCommand(transcript, "LIST") || transcriptHasCommand(transcript, "LOGOUT") {
		t.Fatalf("session continued after contradictory ENABLE: %#v", transcript)
	}
}

func TestMessageMutationRejectsUIDNotStickySourceOrTarget(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := formatFolderID("Archive")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		target bool
		source bool
	}{
		{name: "target", target: true},
		{name: "source", source: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := make(chan []string, 1)
			p := NewProvider("imap", Options{
				DialContext: queuedDialer(t, func(conn net.Conn) {
					serveIMAPCopyUIDNotStickySession(t, conn, commands, tt.target, tt.source)
				}),
				TLSConfig: insecureTLSConfig,
			})
			client := newTestAccountClient(t, p)
			defer client.Destroy()

			_, err := client.CopyMessage(context.Background(), mail.MessageCopyRequest{
				MessageID: messageID,
				Folder:    mail.Folder{ID: targetID},
			})
			assertProductCode(t, err, errs.CodeUnsupportedCapability)
			assertProductReason(t, err, "persistent_uids_unavailable")
			assertProductDetail(t, err, "requirement", "stable_uid")
			client.Destroy()
			transcript := waitCommands(t, commands)
			if transcriptHasCommand(transcript, "UID COPY") {
				t.Fatalf("UID COPY sent despite UIDNOTSTICKY: %#v", transcript)
			}
		})
	}
}

func TestCopyMessageRejectsMalformedCopyUIDEvidence(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := formatFolderID("Archive")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		response func(tag string) []string
	}{
		{
			name: "untagged placement",
			response: func(tag string) []string {
				return []string{"* OK [COPYUID 91 9 44] copied", tag + " OK copied"}
			},
		},
		{
			name: "duplicate copyuid",
			response: func(tag string) []string {
				return []string{"* OK [COPYUID 91 9 44] copied", tag + " OK [COPYUID 91 9 44] copied"}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := make(chan []string, 1)
			p := NewProvider("imap", Options{
				DialContext: queuedDialer(t, func(conn net.Conn) {
					serveIMAPCopyAcceptedResponseSession(t, conn, commands, tt.response)
				}),
				TLSConfig: insecureTLSConfig,
			})
			client := newTestAccountClient(t, p)
			defer client.Destroy()

			_, err := client.CopyMessage(context.Background(), mail.MessageCopyRequest{
				MessageID: messageID,
				Folder:    mail.Folder{ID: targetID},
			})
			assertProductCode(t, err, errs.CodeVerificationFailed)
			assertProductReason(t, err, string(errs.OutcomeProviderResultMalformed))
			assertUnknownOutcome(t, err)
			client.Destroy()
			transcript := waitCommands(t, commands)
			if !transcriptHasCommand(transcript, "UID COPY") {
				t.Fatalf("transcript missing UID COPY: %#v", transcript)
			}
			if transcriptHasCommand(transcript, "UID MOVE") || countCommands(transcript, "UID COPY") != 1 {
				t.Fatalf("unexpected copy transcript: %#v", transcript)
			}
		})
	}
}

func TestMoveMessageRejectsActiveRev2CopyUIDPlacement(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := formatFolderID("Archive")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		response func(tag string) []string
	}{
		{
			name: "tagged only",
			response: func(tag string) []string {
				return []string{tag + " OK [COPYUID 91 9 44] moved"}
			},
		},
		{
			name: "after removal",
			response: func(tag string) []string {
				return []string{"* 1 EXPUNGE", "* OK [COPYUID 91 9 44] copied", tag + " OK moved"}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := make(chan []string, 1)
			p := NewProvider("imap", Options{
				DialContext: queuedDialer(t, func(conn net.Conn) {
					serveIMAPRev2MoveAcceptedResponseSession(t, conn, commands, tt.response)
				}),
				TLSConfig: insecureTLSConfig,
			})
			client := newTestAccountClient(t, p)
			defer client.Destroy()

			_, err := client.MoveMessage(context.Background(), mail.MessageMoveRequest{
				MessageID: messageID,
				Folder:    mail.Folder{ID: targetID},
			})
			assertProductCode(t, err, errs.CodeVerificationFailed)
			assertProductReason(t, err, string(errs.OutcomeProviderResultMalformed))
			assertUnknownOutcome(t, err)
			client.Destroy()
			transcript := waitCommands(t, commands)
			if !transcriptHasCommand(transcript, "UID MOVE") {
				t.Fatalf("transcript missing UID MOVE: %#v", transcript)
			}
			if transcriptHasCommand(transcript, "UID STORE") || transcriptHasCommand(transcript, "EXPUNGE") {
				t.Fatalf("move used fallback commands: %#v", transcript)
			}
		})
	}
}

func TestSetReadStateUsesUIDStoreAndReadBack(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	commands := make(chan []string, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPMarkReadSession(t, conn, commands)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	result, err := client.SetMessageReadState(context.Background(), mail.MessageReadStateRequest{
		MessageID: messageID,
		IsRead:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadState == nil || result.ReadState.Before == nil || *result.ReadState.Before ||
		!result.ReadState.After || result.Changed == nil || !*result.Changed {
		t.Fatalf("read-state result = %#v", result)
	}
	if result.ProviderMeta == nil || result.ProviderMeta.ConcurrencyGuarded == nil || !*result.ProviderMeta.ConcurrencyGuarded {
		t.Fatalf("store metadata = %#v", result.ProviderMeta)
	}
	client.Destroy()
	transcript := waitCommands(t, commands)
	store := transcriptCommandLine(transcript, "UID STORE")
	if store == "" {
		t.Fatalf("transcript missing UID STORE: %#v", transcript)
	}
	if !strings.Contains(store, "+FLAGS") || strings.Contains(strings.ToUpper(store), ".SILENT") || strings.Contains(store, "\\Deleted") {
		t.Fatalf("unexpected STORE command: %q", store)
	}
}

func TestStoreRejectsReadOnlyAndUIDValidityResetBeforeAttempt(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		response imapStoreSelectResponse
		wantCode string
		reason   string
	}{
		{
			name:     "read only",
			response: imapStoreSelectResponse{status: imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, opts: imapSelectOptions{ResponseReadWrite: false, HighestModSeq: 100}},
			wantCode: errs.CodeProviderForbidden,
			reason:   "provider_policy",
		},
		{
			name:     "uidvalidity reset",
			response: imapStoreSelectResponse{status: imapTestMailboxStatus{messages: 2, uidValidity: 78, uidNext: 10}, opts: imapSelectOptions{ResponseReadWrite: true, HighestModSeq: 100}},
			wantCode: errs.CodeMessageNotFound,
			reason:   "uidvalidity_changed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := make(chan []string, 1)
			p := NewProvider("imap", Options{
				DialContext: queuedDialer(t, func(conn net.Conn) {
					serveIMAPStoreSelectFailureSession(t, conn, commands, tt.response)
				}),
				TLSConfig: insecureTLSConfig,
			})
			client := newTestAccountClient(t, p)
			defer client.Destroy()

			_, err := client.SetMessageReadState(context.Background(), mail.MessageReadStateRequest{
				MessageID: messageID,
				IsRead:    true,
			})
			assertProductCode(t, err, tt.wantCode)
			assertProductReason(t, err, tt.reason)
			client.Destroy()
			transcript := waitCommands(t, commands)
			if transcriptHasCommand(transcript, "UID STORE") {
				t.Fatalf("UID STORE sent after pre-attempt failure: %#v", transcript)
			}
		})
	}
}

func TestStoreRejectsUnsafeConditionalStateBeforeAttempt(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	emptyPermanentFlags := ""
	for _, tt := range []struct {
		name       string
		selectOpts imapSelectOptions
		storeFetch bool
	}{
		{
			name:       "missing selected modseq state",
			selectOpts: imapSelectOptions{ResponseReadWrite: true},
		},
		{
			name:       "empty permanent flags",
			selectOpts: imapSelectOptions{ResponseReadWrite: true, HighestModSeq: 100, PermanentFlags: &emptyPermanentFlags},
			storeFetch: true,
		},
		{
			name:       "missing target modseq",
			selectOpts: imapSelectOptions{ResponseReadWrite: true, HighestModSeq: 100},
			storeFetch: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := make(chan []string, 1)
			p := NewProvider("imap", Options{
				DialContext: queuedDialer(t, func(conn net.Conn) {
					serveIMAPStoreUnsafeStateSession(t, conn, commands, tt.selectOpts, tt.storeFetch, tt.name == "missing target modseq")
				}),
				TLSConfig: insecureTLSConfig,
			})
			client := newTestAccountClient(t, p)
			defer client.Destroy()

			_, err := client.SetMessageReadState(context.Background(), mail.MessageReadStateRequest{
				MessageID: messageID,
				IsRead:    true,
			})
			if tt.name == "empty permanent flags" {
				assertProductCode(t, err, errs.CodeUnsupportedCapability)
				assertProductReason(t, err, "permanent_flag_unavailable")
			} else {
				assertProductCode(t, err, errs.CodeProviderDataError)
				assertProductReason(t, err, "invalid_protocol")
			}
			client.Destroy()
			transcript := waitCommands(t, commands)
			if transcriptHasCommand(transcript, "UID STORE") {
				t.Fatalf("UID STORE sent after unsafe conditional state: %#v", transcript)
			}
		})
	}
}

func TestStoreUsesUnguardedPathForNoModSeq(t *testing.T) {
	messageID, err := formatMessageID("INBOX", 77, 9)
	if err != nil {
		t.Fatal(err)
	}
	commands := make(chan []string, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPNoModSeqStoreSession(t, conn, commands)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	result, err := client.SetMessageReadState(context.Background(), mail.MessageReadStateRequest{
		MessageID: messageID,
		IsRead:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderMeta == nil || result.ProviderMeta.ConcurrencyGuarded == nil || *result.ProviderMeta.ConcurrencyGuarded {
		t.Fatalf("store metadata = %#v", result.ProviderMeta)
	}
	client.Destroy()
	transcript := waitCommands(t, commands)
	store := transcriptCommandLine(transcript, "UID STORE")
	if store == "" {
		t.Fatalf("transcript missing UID STORE: %#v", transcript)
	}
	if strings.Contains(strings.ToUpper(store), "UNCHANGEDSINCE") {
		t.Fatalf("NOMODSEQ STORE used guard: %q", store)
	}
}

func serveIMAPCreateFolderSession(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE")

	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LIST")
	if !strings.Contains(line, `"" ""`) {
		t.Errorf("root LIST command = %q", line)
		return
	}
	writeLine(t, tlsConn, `* LIST (\Noselect) "/" ""`)
	writeLine(t, tlsConn, tag+" OK done")

	tag, line = readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LIST")
	if !strings.Contains(line, `"" "*"`) || !strings.Contains(strings.ToUpper(line), "RETURN (SPECIAL-USE)") {
		t.Errorf("snapshot LIST command = %q", line)
		return
	}
	writeLine(t, tlsConn, `* LIST (\Noselect) "/" ""`)
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, tag+" OK done")
	serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"INBOX": {messages: 1, unseen: 0, uidValidity: 77, uidNext: 10},
	}, 1)

	tag, line = readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "CREATE")
	if !strings.Contains(line, "Projects") {
		t.Errorf("CREATE command = %q", line)
		return
	}
	writeLine(t, tlsConn, tag+" OK created")

	tag, line = readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST (\Noselect) "/" ""`)
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, `* LIST () "/" "Projects"`)
	writeLine(t, tlsConn, tag+" OK done")
	serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"INBOX":    {messages: 1, unseen: 0, uidValidity: 77, uidNext: 10},
		"Projects": {messages: 0, unseen: 0, uidValidity: 88, uidNext: 1},
	}, 2)
	serveLogout(t, tlsConn, r)
}

func serveIMAPContradictoryEnableSession(t *testing.T, conn net.Conn, commands chan<- []string) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 IMAP4rev2 ENABLE")
	var seen []string
	defer func() { commands <- seen }()

	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	assertIMAPCommand(t, line, "ENABLE")
	writeLine(t, tlsConn, "* ENABLED IMAP4rev2")
	writeLine(t, tlsConn, tag+" BAD not enabled")
}

func serveIMAPRenameFolderSession(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE")

	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, `* LIST () "/" "Projects"`)
	writeLine(t, tlsConn, tag+" OK done")
	serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"INBOX":    {messages: 1, unseen: 0, uidValidity: 77, uidNext: 10},
		"Projects": {messages: 0, unseen: 0, uidValidity: 88, uidNext: 1},
	}, 2)

	tag, line = readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "RENAME")
	if !strings.Contains(line, "Projects") || !strings.Contains(line, "Clients") {
		t.Errorf("RENAME command = %q", line)
		return
	}
	writeLine(t, tlsConn, tag+" OK renamed")

	tag, line = readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, `* LIST () "/" "Clients"`)
	writeLine(t, tlsConn, tag+" OK done")
	serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"INBOX":   {messages: 1, unseen: 0, uidValidity: 77, uidNext: 10},
		"Clients": {messages: 0, unseen: 0, uidValidity: 89, uidNext: 1},
	}, 2)
	serveLogout(t, tlsConn, r)
}

func serveIMAPDeleteFolderSession(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE")

	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST () "/" "Empty"`)
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, tag+" OK done")
	serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"Empty": {messages: 0, unseen: 0, uidValidity: 88, uidNext: 1},
		"INBOX": {messages: 1, unseen: 0, uidValidity: 77, uidNext: 10},
	}, 2)
	serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"Empty": {messages: 0, unseen: 0, uidValidity: 88, uidNext: 1},
	}, 1)

	tag, line = readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "DELETE")
	if !strings.Contains(line, "Empty") {
		t.Errorf("DELETE command = %q", line)
		return
	}
	writeLine(t, tlsConn, tag+" OK deleted")

	tag, line = readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, tag+" OK done")
	serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"INBOX": {messages: 1, unseen: 0, uidValidity: 77, uidNext: 10},
	}, 1)
	serveLogout(t, tlsConn, r)
}

func serveIMAPCopyMessageSession(t *testing.T, conn net.Conn, commands chan<- []string) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE UIDPLUS")
	var seen []string
	defer func() { commands <- seen }()

	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, `* LIST (\Archive) "/" "Archive"`)
	writeLine(t, tlsConn, tag+" OK done")
	seen = append(seen, serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"Archive": {messages: 7, unseen: 0, uidValidity: 91, uidNext: 44},
		"INBOX":   {messages: 2, unseen: 1, uidValidity: 77, uidNext: 10},
	}, 2)...)

	seen = append(seen, serveSelect(t, tlsConn, r, "Archive", false, imapTestMailboxStatus{messages: 7, uidValidity: 91, uidNext: 44}, nil))
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", false, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, tlsConn, r, 9, "", 123, "<copy@example.com>", true))

	tag, line = readIMAPCommand(t, r)
	seen = append(seen, line)
	if !isIMAPCommand(line, "UID COPY") {
		t.Errorf("copy command = %q", line)
		return
	}
	writeLine(t, tlsConn, tag+" OK [COPYUID 91 9 44] copied")

	seen = append(seen, serveSelect(t, tlsConn, r, "Archive", false, imapTestMailboxStatus{messages: 8, uidValidity: 91, uidNext: 45}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, tlsConn, r, 44, "", 123, "<copy@example.com>", true))
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveCopySnapshot(t *testing.T, conn net.Conn, r *bufio.Reader) []string {
	t.Helper()
	var seen []string
	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	assertIMAPCommand(t, line, "LIST")
	statuses := map[string]imapTestMailboxStatus{
		"Archive": {messages: 7, unseen: 0, uidValidity: 91, uidNext: 44},
		"INBOX":   {messages: 2, unseen: 1, uidValidity: 77, uidNext: 10},
	}
	writeLine(t, conn, `* LIST () "/" "INBOX"`)
	writeLine(t, conn, `* LIST (\Archive) "/" "Archive"`)
	// IMAP4rev2 servers answer LIST ... RETURN (STATUS ...) inline, as
	// LIST-STATUS describes, so the client sends no STATUS commands.
	if strings.Contains(strings.ToUpper(line), "STATUS (") {
		for _, mailbox := range []string{"INBOX", "Archive"} {
			st := statuses[mailbox]
			writeLine(t, conn, fmt.Sprintf(`* STATUS %s (MESSAGES %d UNSEEN %d UIDNEXT %d UIDVALIDITY %d)`, quoteMailbox(mailbox), st.messages, st.unseen, st.uidNext, st.uidValidity))
		}
		writeLine(t, conn, tag+" OK done")
		return seen
	}
	writeLine(t, conn, tag+" OK done")
	seen = append(seen, serveStatusCommands(t, conn, r, statuses, 2)...)
	return seen
}

func serveCopyPreflight(t *testing.T, conn net.Conn, r *bufio.Reader) []string {
	t.Helper()
	seen := serveCopySnapshot(t, conn, r)
	seen = append(seen, serveSelect(t, conn, r, "Archive", false, imapTestMailboxStatus{messages: 7, uidValidity: 91, uidNext: 44}, nil))
	seen = append(seen, serveSelect(t, conn, r, "INBOX", false, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, conn, r, 9, "", 123, "<copy@example.com>", true))
	return seen
}

func serveIMAPCopyUIDNotStickySession(t *testing.T, conn net.Conn, commands chan<- []string, targetUIDNotSticky bool, sourceUIDNotSticky bool) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE UIDPLUS")
	var seen []string
	defer func() { commands <- seen }()

	seen = append(seen, serveCopySnapshot(t, tlsConn, r)...)

	targetOpts := imapSelectOptions{}
	if targetUIDNotSticky {
		targetOpts.UIDNotSticky = true
	}
	seen = append(seen, serveSelectCommand(t, tlsConn, r, "EXAMINE", "Archive", imapTestMailboxStatus{messages: 7, uidValidity: 91, uidNext: 44}, targetOpts))
	if targetUIDNotSticky {
		seen = append(seen, serveLogout(t, tlsConn, r))
		return
	}
	sourceOpts := imapSelectOptions{}
	if sourceUIDNotSticky {
		sourceOpts.UIDNotSticky = true
	}
	seen = append(seen, serveSelectCommand(t, tlsConn, r, "EXAMINE", "INBOX", imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, sourceOpts))
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveIMAPCopyAcceptedResponseSession(t *testing.T, conn net.Conn, commands chan<- []string, response func(tag string) []string) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE UIDPLUS")
	var seen []string
	defer func() { commands <- seen }()

	seen = append(seen, serveCopyPreflight(t, tlsConn, r)...)

	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	if !isIMAPCommand(line, "UID COPY") {
		t.Errorf("copy command = %q", line)
		return
	}
	for _, raw := range response(tag) {
		writeLine(t, tlsConn, raw)
	}
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveIMAPMoveMessageSession(t *testing.T, conn net.Conn, commands chan<- []string) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE UIDPLUS MOVE")
	var seen []string
	defer func() { commands <- seen }()

	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, `* LIST (\Archive) "/" "Archive"`)
	writeLine(t, tlsConn, tag+" OK done")
	seen = append(seen, serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"Archive": {messages: 7, unseen: 0, uidValidity: 91, uidNext: 44},
		"INBOX":   {messages: 2, unseen: 1, uidValidity: 77, uidNext: 10},
	}, 2)...)

	seen = append(seen, serveSelect(t, tlsConn, r, "Archive", false, imapTestMailboxStatus{messages: 7, uidValidity: 91, uidNext: 44}, nil))
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", false, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, tlsConn, r, 9, "", 123, "<move@example.com>", true))
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", true, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))

	tag, line = readIMAPCommand(t, r)
	seen = append(seen, line)
	if !isIMAPCommand(line, "UID MOVE") {
		t.Errorf("move command = %q", line)
		return
	}
	writeLine(t, tlsConn, "* OK [COPYUID 91 9 44] copied")
	writeLine(t, tlsConn, "* 1 EXPUNGE")
	writeLine(t, tlsConn, tag+" OK moved")

	seen = append(seen, serveSelect(t, tlsConn, r, "Archive", false, imapTestMailboxStatus{messages: 8, uidValidity: 91, uidNext: 45}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, tlsConn, r, 44, "", 123, "<move@example.com>", true))
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", false, imapTestMailboxStatus{messages: 1, uidValidity: 77, uidNext: 10}, nil))
	seen = append(seen, serveEmptyFetch(t, tlsConn, r, 9))
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveIMAPRev2MoveAcceptedResponseSession(t *testing.T, conn net.Conn, commands chan<- []string, response func(tag string) []string) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 IMAP4rev2 ENABLE")
	var seen []string
	defer func() { commands <- seen }()

	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	assertIMAPCommand(t, line, "ENABLE")
	writeLine(t, tlsConn, "* ENABLED IMAP4rev2")
	writeLine(t, tlsConn, tag+" OK enabled")

	seen = append(seen, serveCopySnapshot(t, tlsConn, r)...)
	seen = append(seen, serveSelect(t, tlsConn, r, "Archive", false, imapTestMailboxStatus{messages: 7, uidValidity: 91, uidNext: 44}, nil))
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", false, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, tlsConn, r, 9, "", 123, "<move@example.com>", true))
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", true, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))

	tag, line = readIMAPCommand(t, r)
	seen = append(seen, line)
	if !isIMAPCommand(line, "UID MOVE") {
		t.Errorf("move command = %q", line)
		return
	}
	for _, raw := range response(tag) {
		writeLine(t, tlsConn, raw)
	}
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveIMAPMarkReadSession(t *testing.T, conn net.Conn, commands chan<- []string) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE CONDSTORE")
	var seen []string
	defer func() { commands <- seen }()

	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, tlsConn, `* LIST () "/" "INBOX"`)
	writeLine(t, tlsConn, tag+" OK done")
	seen = append(seen, serveStatusCommands(t, tlsConn, r, map[string]imapTestMailboxStatus{
		"INBOX": {messages: 2, unseen: 1, uidValidity: 77, uidNext: 10},
	}, 1)...)
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", false, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, tlsConn, r, 9, "", 123, "<read@example.com>", true))
	seen = append(seen, serveSelect(t, tlsConn, r, "INBOX", true, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, map[string]uint64{"HIGHESTMODSEQ": 100}))
	seen = append(seen, serveStoreFetch(t, tlsConn, r, 9, "", 55))

	tag, line = readIMAPCommand(t, r)
	seen = append(seen, line)
	if !isIMAPCommand(line, "UID STORE") {
		t.Errorf("store command = %q", line)
		return
	}
	writeLine(t, tlsConn, `* 1 FETCH (UID 9 FLAGS (\Seen) MODSEQ (56))`)
	writeLine(t, tlsConn, tag+" OK stored")

	seen = append(seen, serveStoreFetch(t, tlsConn, r, 9, `\Seen`, 0))
	seen = append(seen, serveLogout(t, tlsConn, r))
}

type imapStoreSelectResponse struct {
	status imapTestMailboxStatus
	opts   imapSelectOptions
}

func serveIMAPStoreSelectFailureSession(t *testing.T, conn net.Conn, commands chan<- []string, selectResponse imapStoreSelectResponse) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE CONDSTORE")
	var seen []string
	defer func() { commands <- seen }()

	seen = append(seen, serveStorePreflight(t, tlsConn, r)...)
	seen = append(seen, serveSelectCommand(t, tlsConn, r, "SELECT", "INBOX", selectResponse.status, selectResponse.opts))
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveIMAPStoreUnsafeStateSession(t *testing.T, conn net.Conn, commands chan<- []string, selectOpts imapSelectOptions, serveFetch bool, missingTargetModSeq bool) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE CONDSTORE")
	var seen []string
	defer func() { commands <- seen }()

	seen = append(seen, serveStorePreflight(t, tlsConn, r)...)
	seen = append(seen, serveSelectCommand(t, tlsConn, r, "SELECT", "INBOX", imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, selectOpts))
	if serveFetch {
		modSeq := uint64(55)
		if missingTargetModSeq {
			modSeq = 0
		}
		seen = append(seen, serveStoreFetch(t, tlsConn, r, 9, "", modSeq))
	}
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveIMAPNoModSeqStoreSession(t *testing.T, conn net.Conn, commands chan<- []string) {
	t.Helper()
	defer conn.Close()
	tlsConn, r := startIMAPMutationSession(t, conn, "IMAP4rev1 SPECIAL-USE CONDSTORE")
	var seen []string
	defer func() { commands <- seen }()

	seen = append(seen, serveStorePreflight(t, tlsConn, r)...)
	seen = append(seen, serveSelectCommand(t, tlsConn, r, "SELECT", "INBOX", imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, imapSelectOptions{ResponseReadWrite: true, NoModSeq: true}))
	seen = append(seen, serveStoreFetch(t, tlsConn, r, 9, "", 0))

	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	if !isIMAPCommand(line, "UID STORE") {
		t.Errorf("store command = %q", line)
		return
	}
	writeLine(t, tlsConn, `* 1 FETCH (UID 9 FLAGS (\Seen))`)
	writeLine(t, tlsConn, tag+" OK stored")

	seen = append(seen, serveStoreFetch(t, tlsConn, r, 9, `\Seen`, 0))
	seen = append(seen, serveLogout(t, tlsConn, r))
}

func serveStorePreflight(t *testing.T, conn net.Conn, r *bufio.Reader) []string {
	t.Helper()
	var seen []string
	tag, line := readIMAPCommand(t, r)
	seen = append(seen, line)
	assertIMAPCommand(t, line, "LIST")
	writeLine(t, conn, `* LIST () "/" "INBOX"`)
	writeLine(t, conn, tag+" OK done")
	seen = append(seen, serveStatusCommands(t, conn, r, map[string]imapTestMailboxStatus{
		"INBOX": {messages: 2, unseen: 1, uidValidity: 77, uidNext: 10},
	}, 1)...)
	seen = append(seen, serveSelect(t, conn, r, "INBOX", false, imapTestMailboxStatus{messages: 2, uidValidity: 77, uidNext: 10}, nil))
	seen = append(seen, serveMessageHeaderFetch(t, conn, r, 9, "", 123, "<read@example.com>", true))
	return seen
}

type imapTestMailboxStatus struct {
	messages    uint32
	unseen      uint32
	uidValidity uint32
	uidNext     uint32
}

func startIMAPMutationSession(t *testing.T, conn net.Conn, caps string) (net.Conn, *bufio.Reader) {
	t.Helper()
	tlsConn := tlsServer(t, conn)
	r := bufio.NewReader(tlsConn)
	writeLine(t, tlsConn, "* OK [CAPABILITY "+caps+" AUTH=PLAIN SASL-IR] ready")
	tag, line := readIMAPCommand(t, r)
	if !isIMAPCommand(line, "AUTHENTICATE") {
		t.Errorf("auth command = %q", line)
		return tlsConn, r
	}
	writeLine(t, tlsConn, tag+" OK [CAPABILITY "+caps+"] authenticated")
	return tlsConn, r
}

func serveStatusCommands(t *testing.T, conn net.Conn, r *bufio.Reader, statuses map[string]imapTestMailboxStatus, count int) []string {
	t.Helper()
	seen := make([]string, 0, count)
	for i := 0; i < count; i++ {
		tag, line := readIMAPCommand(t, r)
		seen = append(seen, line)
		assertIMAPCommand(t, line, "STATUS")
		mailbox := commandMailbox(line, 2)
		status, ok := statuses[mailbox]
		if !ok {
			t.Errorf("unexpected STATUS mailbox %q in %q", mailbox, line)
			return seen
		}
		writeLine(t, conn, fmt.Sprintf(`* STATUS %s (MESSAGES %d UNSEEN %d UIDNEXT %d UIDVALIDITY %d)`, quoteMailbox(mailbox), status.messages, status.unseen, status.uidNext, status.uidValidity))
		writeLine(t, conn, tag+" OK done")
	}
	return seen
}

func serveSelect(t *testing.T, conn net.Conn, r *bufio.Reader, mailbox string, readWrite bool, status imapTestMailboxStatus, extra map[string]uint64) string {
	t.Helper()
	opts := imapSelectOptions{
		ResponseReadWrite: readWrite,
		HighestModSeq:     extra["HIGHESTMODSEQ"],
	}
	command := "EXAMINE"
	if readWrite {
		command = "SELECT"
	}
	return serveSelectCommand(t, conn, r, command, mailbox, status, opts)
}

type imapSelectOptions struct {
	ResponseReadWrite bool
	UIDNotSticky      bool
	HighestModSeq     uint64
	NoModSeq          bool
	PermanentFlags    *string
	OmitPermanent     bool
}

func serveSelectCommand(t *testing.T, conn net.Conn, r *bufio.Reader, command string, mailbox string, status imapTestMailboxStatus, opts imapSelectOptions) string {
	t.Helper()
	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, command)
	if got := commandMailbox(line, 2); got != mailbox {
		t.Errorf("select mailbox = %q from %q, want %q", got, line, mailbox)
		return line
	}
	writeLine(t, conn, fmt.Sprintf("* %d EXISTS", status.messages))
	writeLine(t, conn, `* FLAGS (\Seen \Flagged \Deleted)`)
	if !opts.OmitPermanent {
		permanent := `\Seen \Flagged \Deleted \*`
		if opts.PermanentFlags != nil {
			permanent = *opts.PermanentFlags
		}
		writeLine(t, conn, `* OK [PERMANENTFLAGS (`+permanent+`)] flags`)
	}
	writeLine(t, conn, fmt.Sprintf("* OK [UIDVALIDITY %d] valid", status.uidValidity))
	writeLine(t, conn, fmt.Sprintf("* OK [UIDNEXT %d] next", status.uidNext))
	if opts.UIDNotSticky {
		writeLine(t, conn, "* OK [UIDNOTSTICKY] volatile")
	}
	if opts.HighestModSeq > 0 {
		writeLine(t, conn, fmt.Sprintf("* OK [HIGHESTMODSEQ %d] modseq", opts.HighestModSeq))
	}
	if opts.NoModSeq {
		writeLine(t, conn, "* OK [NOMODSEQ] no mod-sequence tracking")
	}
	mode := "READ-ONLY"
	if opts.ResponseReadWrite {
		mode = "READ-WRITE"
	}
	writeLine(t, conn, tag+" OK ["+mode+"] selected")
	return line
}

func serveMessageHeaderFetch(t *testing.T, conn net.Conn, r *bufio.Reader, uid uint32, flags string, size int64, messageID string, present bool) string {
	t.Helper()
	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "UID FETCH")
	if !strings.Contains(line, fmt.Sprintf(" %d ", uid)) {
		t.Errorf("fetch uid command = %q", line)
		return line
	}
	if present {
		header := "Message-ID: " + messageID + "\r\n\r\n"
		fmt.Fprintf(conn, "* 1 FETCH (UID %d FLAGS (%s) RFC822.SIZE %d BODY[HEADER.FIELDS (MESSAGE-ID)] {%d}\r\n%s)\r\n", uid, flags, size, len(header), header)
	}
	writeLine(t, conn, tag+" OK done")
	return line
}

func serveStoreFetch(t *testing.T, conn net.Conn, r *bufio.Reader, uid uint32, flags string, modSeq uint64) string {
	t.Helper()
	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "UID FETCH")
	if !strings.Contains(line, fmt.Sprintf(" %d ", uid)) {
		t.Errorf("store fetch uid command = %q", line)
		return line
	}
	if modSeq > 0 {
		writeLine(t, conn, fmt.Sprintf(`* 1 FETCH (UID %d FLAGS (%s) MODSEQ (%d))`, uid, flags, modSeq))
	} else {
		writeLine(t, conn, fmt.Sprintf(`* 1 FETCH (UID %d FLAGS (%s))`, uid, flags))
	}
	writeLine(t, conn, tag+" OK done")
	return line
}

func serveEmptyFetch(t *testing.T, conn net.Conn, r *bufio.Reader, uid uint32) string {
	t.Helper()
	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "UID FETCH")
	if !strings.Contains(line, fmt.Sprintf(" %d ", uid)) {
		t.Errorf("empty fetch uid command = %q", line)
		return line
	}
	writeLine(t, conn, tag+" OK done")
	return line
}

func serveLogout(t *testing.T, conn net.Conn, r *bufio.Reader) string {
	t.Helper()
	tag, line := readIMAPCommand(t, r)
	assertIMAPCommand(t, line, "LOGOUT")
	writeLine(t, conn, "* BYE")
	writeLine(t, conn, tag+" OK done")
	return line
}

func readIMAPCommand(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	line := readLine(t, r)
	fields := strings.Fields(line)
	if len(fields) == 0 {
		t.Errorf("empty IMAP command")
		return "", line
	}
	return fields[0], line
}

func assertIMAPCommand(t *testing.T, line, command string) {
	t.Helper()
	if !isIMAPCommand(line, command) {
		t.Errorf("command = %q, want %s", line, command)
	}
}

func isIMAPCommand(line, command string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	got := strings.ToUpper(fields[1])
	if got == "UID" && len(fields) >= 3 {
		got += " " + strings.ToUpper(fields[2])
	}
	return got == strings.ToUpper(command)
}

func commandMailbox(line string, index int) string {
	fields := strings.Fields(line)
	if len(fields) <= index {
		return ""
	}
	return strings.Trim(fields[index], `"`)
}

func quoteMailbox(mailbox string) string {
	if mailbox == "INBOX" {
		return mailbox
	}
	return `"` + mailbox + `"`
}

func transcriptHasCommand(lines []string, command string) bool {
	return transcriptCommandLine(lines, command) != ""
}

func countCommands(lines []string, command string) int {
	count := 0
	for _, line := range lines {
		if isIMAPCommand(line, command) {
			count++
		}
	}
	return count
}

func transcriptCommandLine(lines []string, command string) string {
	for _, line := range lines {
		if isIMAPCommand(line, command) {
			return line
		}
	}
	return ""
}

func assertProductDetail(t *testing.T, err error, key string, want any) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("err = %v, want product detail %s", err, key)
	}
	if got := product.Details()[key]; got != want {
		t.Fatalf("%s = %#v, want %#v (err=%v)", key, got, want, err)
	}
}

func assertUnknownOutcome(t *testing.T, err error) {
	t.Helper()
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeUnknown || !facts.WriteMayHaveLanded || facts.RetrySafe || facts.ConfirmedAfterWrite || facts.Succeeded {
		t.Fatalf("write outcome facts = %#v, ok=%t", facts, ok)
	}
}

var (
	_ provider.FolderManager  = (*accountClient)(nil)
	_ provider.MessageMutator = (*accountClient)(nil)
)
