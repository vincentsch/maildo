package mail

import (
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

func TestResolveFolderRefSuccess(t *testing.T) {
	folders := resolverFolders()
	for _, tt := range []struct {
		name      string
		raw       string
		wantID    string
		matchedBy string
	}{
		{name: "well-known alias", raw: "inbox", wantID: "inbox-id", matchedBy: MatchWellKnown},
		{name: "provider alias normalization", raw: "sentitems", wantID: "sent-id", matchedBy: MatchWellKnown},
		{name: "deleted alias normalization", raw: "deleteditems", wantID: "deleted-id", matchedBy: MatchWellKnown},
		{name: "exact id", raw: "acme-id", wantID: "acme-id", matchedBy: MatchID},
		{name: "id beats path", raw: "foo/bar", wantID: "foo/bar", matchedBy: MatchID},
		{name: "nested path", raw: "Clients/ACME", wantID: "acme-id", matchedBy: MatchPath},
		{name: "case-insensitive path", raw: "clients/acme", wantID: "acme-id", matchedBy: MatchPath},
		{name: "exact path segment before ci", raw: "CaseParent/PathExact", wantID: "path-exact-id", matchedBy: MatchPath},
		{name: "exact display", raw: "Archive1", wantID: "archive-1", matchedBy: MatchExactDisplay},
		{name: "case-insensitive display", raw: "archive1", wantID: "archive-1", matchedBy: MatchCIDisplay},
		{name: "alias beats id", raw: "inbox", wantID: "inbox-id", matchedBy: MatchWellKnown},
		{name: "exact display before ci", raw: "Exact", wantID: "exact-id", matchedBy: MatchExactDisplay},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := ParseFolderRef(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ResolveFolderRef(scope(), ref, folders)
			if err != nil {
				t.Fatalf("ResolveFolderRef(%q) error: %v", tt.raw, err)
			}
			if got.Folder.ID != tt.wantID || got.MatchedBy != tt.matchedBy {
				t.Fatalf("ResolveFolderRef(%q) = id %q matched %q, want id %q matched %q", tt.raw, got.Folder.ID, got.MatchedBy, tt.wantID, tt.matchedBy)
			}
		})
	}
}

func TestResolveFolderRefDoesNotUseLocalizedDisplayForAlias(t *testing.T) {
	got, err := ResolveFolderRef(scope(), FolderRef{Raw: "inbox", Kind: FolderRefWellKnown}, resolverFolders())
	if err != nil {
		t.Fatal(err)
	}
	if got.Folder.DisplayName != "Posteingang" || got.Folder.WellKnownName != WellKnownInbox {
		t.Fatalf("localized inbox resolution = %#v", got.Folder)
	}
}

func TestResolveFolderRefWellKnownAliasNormalization(t *testing.T) {
	folders := []Folder{
		{ID: "sent-id", DisplayName: "Gesendete Elemente", WellKnownName: WellKnownSent, Path: "Gesendete Elemente"},
		{ID: "deleted-id", DisplayName: "Gelöscht", WellKnownName: WellKnownDeleted, Path: "Gelöscht"},
		{ID: "junk-id", DisplayName: "Junk-E-Mail", WellKnownName: WellKnownJunk, Path: "Junk-E-Mail"},
	}
	for _, tt := range []struct {
		raw    string
		wantID string
	}{
		{raw: "sentitems", wantID: "sent-id"},
		{raw: "deleteditems", wantID: "deleted-id"},
		{raw: "junkemail", wantID: "junk-id"},
		{raw: "spam", wantID: "junk-id"},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			ref, err := ParseFolderRef(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ResolveFolderRef(scope(), ref, folders)
			if err != nil {
				t.Fatalf("ResolveFolderRef(%q) error: %v", tt.raw, err)
			}
			if got.Folder.ID != tt.wantID || got.MatchedBy != MatchWellKnown {
				t.Fatalf("ResolveFolderRef(%q) = id %q matched %q, want id %q matched %q", tt.raw, got.Folder.ID, got.MatchedBy, tt.wantID, MatchWellKnown)
			}
		})
	}
}

func TestResolveFolderRefNotFound(t *testing.T) {
	for _, raw := range []string{"missing", "/inbox", "Clients//ACME", "Clients/", "Clients/ACME Display"} {
		t.Run(raw, func(t *testing.T) {
			ref, err := ParseFolderRef(raw)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ResolveFolderRef(scope(), ref, resolverFolders())
			assertResolveError(t, err, errs.CodeFolderNotFound, 5, raw)
		})
	}
}

func TestResolveFolderRefEmptyRefNotFound(t *testing.T) {
	ref, err := ParseFolderRef(" ")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveFolderRef(scope(), ref, resolverFolders())
	assertResolveError(t, err, errs.CodeFolderNotFound, 5, "")
}

func TestResolveFolderRefAmbiguous(t *testing.T) {
	for _, tt := range []struct {
		name      string
		raw       string
		matchedBy string
	}{
		{name: "duplicate display", raw: "Reports", matchedBy: MatchExactDisplay},
		{name: "duplicate ci display", raw: "CASE", matchedBy: MatchCIDisplay},
		{name: "duplicate well-known", raw: "spam", matchedBy: MatchWellKnown},
		{name: "ambiguous path segment", raw: "Clients/Dup", matchedBy: MatchPath},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := ParseFolderRef(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ResolveFolderRef(scope(), ref, resolverFolders())
			e := assertResolveError(t, err, errs.CodeAmbiguousFolder, 1, tt.raw)
			details := e.ErrorDetails().(map[string]any)
			if details["matched_by"] != tt.matchedBy {
				t.Fatalf("matched_by = %#v, want %q; details=%#v", details["matched_by"], tt.matchedBy, details)
			}
			candidates, ok := details["candidates"].([]FolderCandidate)
			if !ok || len(candidates) == 0 {
				t.Fatalf("candidates = %#v, want non-empty []FolderCandidate", details["candidates"])
			}
			if len(candidates) > maxFolderCandidates {
				t.Fatalf("candidates len = %d, want capped at %d", len(candidates), maxFolderCandidates)
			}
		})
	}
}

func TestResolveFolderRefAmbiguousCandidatesAreBounded(t *testing.T) {
	folders := make([]Folder, 0, maxFolderCandidates+3)
	for i := 0; i < maxFolderCandidates+3; i++ {
		id := "dupe-" + string(rune('a'+i))
		folders = append(folders, Folder{ID: id, DisplayName: "Dupe", Path: "Dupe"})
	}
	ref, err := ParseFolderRef("Dupe")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveFolderRef(scope(), ref, folders)
	e := assertResolveError(t, err, errs.CodeAmbiguousFolder, 1, "Dupe")
	details := e.ErrorDetails().(map[string]any)
	candidates, ok := details["candidates"].([]FolderCandidate)
	if !ok {
		t.Fatalf("candidates = %#v, want []FolderCandidate", details["candidates"])
	}
	if len(candidates) != maxFolderCandidates {
		t.Fatalf("candidates len = %d, want %d", len(candidates), maxFolderCandidates)
	}
}

func assertResolveError(t *testing.T, err error, code string, exit int, ref string) *errs.Error {
	t.Helper()
	if err == nil {
		t.Fatal("ResolveFolderRef succeeded, want error")
	}
	e, ok := err.(*errs.Error)
	if !ok {
		t.Fatalf("error type = %T, want *errs.Error", err)
	}
	if e.Code() != code || e.ExitCode() != exit {
		t.Fatalf("error = code %q exit %d, want code %q exit %d", e.Code(), e.ExitCode(), code, exit)
	}
	details := e.ErrorDetails().(map[string]any)
	if details["account"] != "work" || details["provider"] != "fake" || details["ref"] != ref || details["code"] != code {
		t.Fatalf("details = %#v", details)
	}
	if _, ok := details["ref_kind"].(string); !ok {
		t.Fatalf("ref_kind missing from details: %#v", details)
	}
	return e
}

func scope() FolderResolveScope {
	return FolderResolveScope{Account: "work", Provider: "fake"}
}

func resolverFolders() []Folder {
	return []Folder{
		{ID: "inbox-id", DisplayName: "Posteingang", WellKnownName: WellKnownInbox, Path: "Posteingang"},
		{ID: "inbox", DisplayName: "Literal Inbox ID", Path: "Literal Inbox ID"},
		{ID: "sent-id", DisplayName: "Gesendete Elemente", WellKnownName: WellKnownSent, Path: "Gesendete Elemente"},
		{ID: "deleted-id", DisplayName: "Gelöscht", WellKnownName: WellKnownDeleted, Path: "Gelöscht"},
		{ID: "junk-1", DisplayName: "Junk", WellKnownName: WellKnownJunk, Path: "Junk"},
		{ID: "junk-2", DisplayName: "Spam", WellKnownName: WellKnownJunk, Path: "Spam"},
		{ID: "clients-id", DisplayName: "Clients", Path: "Clients", ChildCount: 3},
		{ID: "acme-id", DisplayName: "ACME", ParentPath: "Clients", Path: "Clients/ACME"},
		{ID: "dup-1", DisplayName: "Dup", ParentPath: "Clients", Path: "Clients/Dup"},
		{ID: "dup-2", DisplayName: "Dup", ParentPath: "Clients", Path: "Clients/Dup"},
		{ID: "foo-id", DisplayName: "foo", Path: "foo", ChildCount: 1},
		{ID: "foo-child", DisplayName: "bar", ParentPath: "foo", Path: "foo/bar"},
		{ID: "foo/bar", DisplayName: "ID Wins", Path: "ID Wins"},
		{ID: "archive-1", DisplayName: "Archive1", Path: "Archive1"},
		{ID: "reports-1", DisplayName: "Reports", Path: "Reports"},
		{ID: "reports-2", DisplayName: "Reports", Path: "Reports"},
		{ID: "case-1", DisplayName: "Case", Path: "Case"},
		{ID: "case-2", DisplayName: "case", Path: "case"},
		{ID: "exact-id", DisplayName: "Exact", Path: "Exact"},
		{ID: "exact-ci-id", DisplayName: "exact", Path: "exact"},
		{ID: "path-display-id", DisplayName: "Clients/ACME", Path: "Clients/ACME Display"},
		{ID: "case-parent", DisplayName: "CaseParent", Path: "CaseParent", ChildCount: 2},
		{ID: "path-exact-id", DisplayName: "PathExact", ParentPath: "CaseParent", Path: "CaseParent/PathExact"},
		{ID: "path-ci-id", DisplayName: "pathexact", ParentPath: "CaseParent", Path: "CaseParent/pathexact"},
	}
}
