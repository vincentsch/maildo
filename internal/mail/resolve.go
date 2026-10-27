package mail

import (
	"fmt"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
)

const maxFolderCandidates = 10

// ResolveFolderRef finds the folder a user meant in one account's folder
// list. It tries these in order, and the first match wins:
//
//   - a standard folder name such as "inbox" or "spam"
//   - the provider's folder id
//   - a path with slashes, such as Clients/Acme
//   - the exact display name
//   - the display name in any case
func ResolveFolderRef(scope FolderResolveScope, ref FolderRef, folders []Folder) (FolderResolution, error) {
	raw := strings.TrimSpace(ref.Raw)
	if raw == "" {
		return FolderResolution{}, notFound(scope, ref, "folder ref must not be empty")
	}

	// Standard names come before ids, so "inbox" means the inbox even when a
	// provider has some other folder whose id is "inbox".
	if canonical, ok := wellKnownAliases[strings.ToLower(raw)]; ok {
		matches := byWellKnown(folders, canonical)
		switch len(matches) {
		case 1:
			return resolved(ref, matches[0], MatchWellKnown), nil
		case 0:
		default:
			return FolderResolution{}, ambiguous(scope, ref, MatchWellKnown, matches)
		}
	}

	if f, found := firstByID(folders, raw); found {
		return resolved(ref, f, MatchID), nil
	}

	if strings.Contains(raw, "/") {
		res, err := resolvePath(scope, ref, folders)
		if err != nil {
			return FolderResolution{}, err
		}
		return res, nil
	}

	exact := byExactDisplay(folders, raw)
	if len(exact) == 1 {
		return resolved(ref, exact[0], MatchExactDisplay), nil
	}
	if len(exact) > 1 {
		return FolderResolution{}, ambiguous(scope, ref, MatchExactDisplay, exact)
	}

	ci := byCIDisplay(folders, raw)
	if len(ci) == 1 {
		return resolved(ref, ci[0], MatchCIDisplay), nil
	}
	if len(ci) > 1 {
		return FolderResolution{}, ambiguous(scope, ref, MatchCIDisplay, ci)
	}

	return FolderResolution{}, notFound(scope, ref, "")
}

func resolvePath(scope FolderResolveScope, ref FolderRef, folders []Folder) (FolderResolution, error) {
	// Paths are split at "/" and matched by display name, so a folder whose
	// name holds a "/" can only be picked by its id.
	segs := strings.Split(strings.TrimSpace(ref.Raw), "/")
	for _, s := range segs {
		if s == "" {
			return FolderResolution{}, notFound(scope, ref, "the folder path has an empty part; remove slashes at the start or end, and double slashes")
		}
	}
	parentPath := ""
	var current Folder
	for _, seg := range segs {
		children := childrenOf(folders, parentPath)
		matches := exactThenCI(children, seg)
		switch {
		case len(matches) == 0:
			return FolderResolution{}, notFound(scope, ref, "")
		case len(matches) > 1:
			return FolderResolution{}, ambiguous(scope, ref, MatchPath, matches)
		default:
			current = matches[0]
			parentPath = current.Path
		}
	}
	return resolved(ref, current, MatchPath), nil
}

// resolved builds every successful result, so they all have the same JSON
// shape and matched-by label.
func resolved(ref FolderRef, folder Folder, matchedBy string) FolderResolution {
	return FolderResolution{Ref: ref, Folder: folder, MatchedBy: matchedBy}
}

func byWellKnown(folders []Folder, canonical string) []Folder {
	var out []Folder
	for _, f := range folders {
		if f.WellKnownName == canonical {
			out = append(out, f)
		}
	}
	return out
}

func firstByID(folders []Folder, id string) (Folder, bool) {
	for _, f := range folders {
		if f.ID == id {
			return f, true
		}
	}
	return Folder{}, false
}

func byExactDisplay(folders []Folder, display string) []Folder {
	var out []Folder
	for _, f := range folders {
		if f.DisplayName == display {
			out = append(out, f)
		}
	}
	return out
}

func byCIDisplay(folders []Folder, display string) []Folder {
	var out []Folder
	for _, f := range folders {
		if strings.EqualFold(f.DisplayName, display) {
			out = append(out, f)
		}
	}
	return out
}

func childrenOf(folders []Folder, parentPath string) []Folder {
	var out []Folder
	for _, f := range folders {
		if f.ParentPath == parentPath {
			out = append(out, f)
		}
	}
	return out
}

func exactThenCI(folders []Folder, display string) []Folder {
	// Matching in any case is only a fallback, so a folder named "Work" next
	// to "work" does not make an exact match ambiguous.
	if exact := byExactDisplay(folders, display); len(exact) > 0 {
		return exact
	}
	return byCIDisplay(folders, display)
}

// notFound returns the error for a folder that does not exist in the account.
func notFound(scope FolderResolveScope, ref FolderRef, msg string) error {
	if msg == "" {
		msg = fmt.Sprintf("account %q (%s) has no folder %q", scope.Account, scope.Provider, ref.Raw)
	}
	return errs.NewWithDetails(errs.CodeFolderNotFound, msg, map[string]any{
		"account":  scope.Account,
		"provider": scope.Provider,
		"ref":      ref.Raw,
		"ref_kind": string(ref.Kind),
	})
}

// ambiguous returns the error for a name that matches several folders. It
// lists just enough of each for the user to pick one by id or path.
func ambiguous(scope FolderResolveScope, ref FolderRef, matchedBy string, matches []Folder) error {
	return errs.NewWithDetails(errs.CodeAmbiguousFolder,
		fmt.Sprintf("%q matches more than one folder in account %q (%s); pick one of the candidates by its id from maildo folders list, or by its full path", ref.Raw, scope.Account, scope.Provider),
		map[string]any{
			"account":    scope.Account,
			"provider":   scope.Provider,
			"ref":        ref.Raw,
			"ref_kind":   string(ref.Kind),
			"matched_by": matchedBy,
			"candidates": candidates(matches),
		})
}

// candidates lists at most maxFolderCandidates of the matches, with the
// fields that maildo folders list also shows, so the user can tell them
// apart.
func candidates(matches []Folder) []FolderCandidate {
	n := len(matches)
	if n > maxFolderCandidates {
		n = maxFolderCandidates
	}
	out := make([]FolderCandidate, 0, n)
	for _, f := range matches[:n] {
		out = append(out, FolderCandidate{ID: f.ID, DisplayName: f.DisplayName, Path: f.Path})
	}
	return out
}
