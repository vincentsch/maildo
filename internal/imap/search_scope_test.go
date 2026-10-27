package imap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// One "invoice" message in each standard folder, so a test can see which
// folders a search looked in.
func seedEveryFolder(t *testing.T) *memServer {
	t.Helper()
	s := startMemServer(t, "Archive", "Sent", "Drafts", "Trash", "Junk")
	for i, folder := range []string{"INBOX", "Archive", "Sent", "Drafts", "Trash", "Junk"} {
		s.add(folder, testMessage{from: "billing@vendor.example", subject: "Invoice in " + folder, body: "invoice"}.raw(), day(i))
	}
	return s
}

func sortedFolders(rows []mail.MessageSummary) string {
	var out []string
	for _, row := range rows {
		out = append(out, row.Folder.DisplayName)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestSearchScopeLeavesOutAndPicksFoldersByRole(t *testing.T) {
	s := seedEveryFolder(t)
	c := s.client()
	ctx := context.Background()
	run := func(req mail.SearchRequest) []mail.MessageSummary {
		t.Helper()
		req.Query = mustQuery(t, "invoice")
		req.Limit = 25
		out, err := c.SearchMessages(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return out.Messages
	}

	if got := sortedFolders(run(mail.SearchRequest{})); got != "Archive,Drafts,INBOX,Sent" {
		t.Fatalf("default search looked in %s; Trash and Junk must be left out", got)
	}
	if got := sortedFolders(run(mail.SearchRequest{SkipRoles: []string{mail.WellKnownSent, mail.WellKnownDrafts}})); got != "Archive,INBOX" {
		t.Fatalf("search without Sent and Drafts looked in %s", got)
	}
	only := run(mail.SearchRequest{OnlyRoles: []string{mail.WellKnownDeleted, mail.WellKnownJunk}})
	if got := sortedFolders(only); got != "Junk,Trash" {
		t.Fatalf("search in Trash and Junk looked in %s", got)
	}
	for _, row := range only {
		if row.Folder.WellKnownName != mail.WellKnownDeleted && row.Folder.WellKnownName != mail.WellKnownJunk {
			t.Fatalf("row folder = %+v, want its role", row.Folder)
		}
	}
	trash := folderFor(t, "Trash")
	if got := sortedFolders(run(mail.SearchRequest{Folder: &trash, SkipRoles: []string{mail.WellKnownDeleted}, OnlyRoles: []string{mail.WellKnownInbox}})); got != "Trash" {
		t.Fatalf("an explicit folder must win over the roles, searched %s", got)
	}
}

func TestSearchInAllMailLeavesOutDraftsAndSentByCriteria(t *testing.T) {
	all := imapFolderRecord{folder: mail.Folder{DisplayName: "All Mail"}, mailbox: "[Gmail]/All Mail", specialUse: []mail.ProviderSpecialUse{mail.SpecialUseAll}}
	inbox := imapFolderRecord{folder: mail.Folder{DisplayName: "INBOX", WellKnownName: mail.WellKnownInbox}, mailbox: "INBOX"}
	trash := imapFolderRecord{folder: mail.Folder{DisplayName: "Trash", WellKnownName: mail.WellKnownDeleted}, mailbox: "[Gmail]/Trash", specialUse: []mail.ProviderSpecialUse{mail.SpecialUseTrash}}
	snapshot := imapFolderSnapshot{selectable: []imapFolderRecord{inbox, all, trash}}

	folders, allMail := scopedFolders(snapshot, []string{mail.WellKnownSent, mail.WellKnownDrafts}, nil)
	if !allMail || len(folders) != 1 || folders[0].mailbox != all.mailbox {
		t.Fatalf("folders = %+v allMail=%v, want All Mail alone", folders, allMail)
	}
	folders, allMail = scopedFolders(snapshot, nil, []string{mail.WellKnownDeleted})
	if allMail || len(folders) != 1 || folders[0].mailbox != trash.mailbox {
		t.Fatalf("only Trash = %+v allMail=%v", folders, allMail)
	}

	c := &accountClient{}
	c.setup.Email = "me@gmail.example"
	var criteria goimap.SearchCriteria
	c.skipRolesInAllMail(&criteria, []string{mail.WellKnownSent, mail.WellKnownDrafts})
	if len(criteria.NotFlag) != 1 || criteria.NotFlag[0] != goimap.FlagDraft {
		t.Fatalf("drafts are not left out: %+v", criteria.NotFlag)
	}
	if len(criteria.Not) != 1 || len(criteria.Not[0].Header) != 1 || criteria.Not[0].Header[0].Value != "me@gmail.example" {
		t.Fatalf("sent mail is not left out: %+v", criteria.Not)
	}
}

func TestCompleteSearchFailsWhenMatchesAreLeftOut(t *testing.T) {
	s := startMemServer(t)
	for i := 0; i < 5; i++ {
		s.add("INBOX", testMessage{from: "a@example.net", subject: fmt.Sprintf("Report %d", i), body: "report"}.raw(), day(i))
	}
	old := maxSortCandidates
	maxSortCandidates = 3
	t.Cleanup(func() { maxSortCandidates = old })
	c := s.client()
	ctx := context.Background()

	shown, err := c.SearchMessages(ctx, mail.SearchRequest{Query: mustQuery(t, "report"), Limit: 25})
	if err != nil {
		t.Fatalf("a search for reading must still answer: %v", err)
	}
	if len(shown.Messages) != 3 || len(shown.Warnings) == 0 {
		t.Fatalf("display search = %v warnings %v", subjects(shown.Messages), shown.Warnings)
	}
	_, err = c.SearchMessages(ctx, mail.SearchRequest{Query: mustQuery(t, "report"), Limit: 25, Complete: true})
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeReadLimitExceeded || !strings.Contains(product.Message(), "every match") {
		t.Fatalf("complete search error = %v", err)
	}
}

// headerBlindSession answers header searches with no matches, as GreenMail
// does for the Message-ID searches threads use.
type headerBlindSession struct {
	imapserver.Session
}

func (s headerBlindSession) Search(kind imapserver.NumKind, criteria *goimap.SearchCriteria, options *goimap.SearchOptions) (*goimap.SearchData, error) {
	if criteriaUsesHeader(criteria) {
		if kind == imapserver.NumKindUID {
			return &goimap.SearchData{All: goimap.UIDSet{}}, nil
		}
		return &goimap.SearchData{All: goimap.SeqSet{}}, nil
	}
	return s.Session.Search(kind, criteria, options)
}

func (s headerBlindSession) Namespace() (*goimap.NamespaceData, error) {
	return s.Session.(imapserver.SessionNamespace).Namespace()
}

func (s headerBlindSession) Move(w *imapserver.MoveWriter, numSet goimap.NumSet, dest string) error {
	return s.Session.(imapserver.SessionMove).Move(w, numSet, dest)
}

func criteriaUsesHeader(c *goimap.SearchCriteria) bool {
	if c == nil {
		return false
	}
	if len(c.Header) > 0 {
		return true
	}
	for _, pair := range c.Or {
		if criteriaUsesHeader(&pair[0]) || criteriaUsesHeader(&pair[1]) {
			return true
		}
	}
	return false
}

func TestThreadIsIncompleteWhenTheServerCannotSearchHeaders(t *testing.T) {
	s := startMemServerWith(t, func(inner imapserver.Session) imapserver.Session { return headerBlindSession{inner} }, "Sent")
	uid := s.add("INBOX", testMessage{from: "Clara <client@client.test>", subject: "Project kickoff", messageID: "<t1@client.test>", body: "Shall we?"}.raw(), day(2))
	s.add("Sent", testMessage{from: "Me <me@example.com>", subject: "Re: Project kickoff", messageID: "<t2@example.com>", references: "<t1@client.test>", inReplyTo: "<t1@client.test>", body: "Yes"}.raw(), day(2))
	c := s.client()
	id, err := formatMessageID("INBOX", 1, uid)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := c.ReadThread(context.Background(), mail.ReadThreadRequest{Selector: mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: id}})
	if err != nil {
		t.Fatal(err)
	}
	if thread.ProviderMeta == nil || thread.ProviderMeta.Complete == nil || *thread.ProviderMeta.Complete {
		t.Fatalf("thread of %d messages claims to be complete although the server could not search headers", len(thread.Messages))
	}
}
