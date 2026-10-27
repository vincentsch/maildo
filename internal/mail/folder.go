package mail

import (
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
)

// FolderRefKind says what kind of folder argument the user typed, judged
// before maildo asks the provider.
type FolderRefKind string

const (
	FolderRefWellKnown FolderRefKind = "well_known"
	FolderRefPath      FolderRefKind = "path"
	FolderRefOpaque    FolderRefKind = "opaque"
)

const (
	// MatchWellKnown through MatchCIDisplay say how maildo found the folder.
	// They appear in JSON output and must not change.
	MatchWellKnown    = "well_known"
	MatchID           = "id"
	MatchPath         = "path"
	MatchExactDisplay = "exact_display_name"
	MatchCIDisplay    = "case_insensitive_display_name"
)

const (
	// The standard folders have these names on every provider, whatever the
	// folder is called in the user's language. Each provider maps its own
	// names onto them.
	WellKnownInbox   = "inbox"
	WellKnownDrafts  = "drafts"
	WellKnownSent    = "sent"
	WellKnownDeleted = "deleted"
	WellKnownArchive = "archive"
	WellKnownJunk    = "junk"
)

var wellKnownAliases = map[string]string{
	"inbox":        WellKnownInbox,
	"drafts":       WellKnownDrafts,
	"sent":         WellKnownSent,
	"sentitems":    WellKnownSent,
	"deleted":      WellKnownDeleted,
	"deleteditems": WellKnownDeleted,
	"archive":      WellKnownArchive,
	"junk":         WellKnownJunk,
	"junkemail":    WellKnownJunk,
	"spam":         WellKnownJunk,
}

// NormalizeWellKnown returns the standard name for an alias such as "spam".
func NormalizeWellKnown(alias string) (string, bool) {
	canonical, ok := wellKnownAliases[strings.ToLower(strings.TrimSpace(alias))]
	return canonical, ok
}

// FolderRef is the folder argument with surrounding spaces removed, and its
// kind. Raw stays as typed, so errors and metadata show the user's input.
type FolderRef struct {
	Raw  string        `json:"raw"`
	Kind FolderRefKind `json:"kind"`
}

// Folder is a folder as every provider reports it. Details that only one
// provider has go into ProviderMeta.
type Folder struct {
	Account          string                  `json:"account"`
	Provider         string                  `json:"provider"`
	ID               string                  `json:"id"`
	DisplayName      string                  `json:"display_name"`
	WellKnownName    string                  `json:"well_known_name,omitempty"`
	ParentID         string                  `json:"parent_id,omitempty"`
	ParentPath       string                  `json:"parent_path,omitempty"`
	Path             string                  `json:"path,omitempty"`
	TotalCount       int                     `json:"total_count"`
	TotalCountKnown  bool                    `json:"total_count_known"`
	UnreadCount      int                     `json:"unread_count"`
	UnreadCountKnown bool                    `json:"unread_count_known"`
	ChildCount       int                     `json:"child_count,omitempty"`
	ProviderMeta     *FolderProviderMetadata `json:"provider_metadata,omitempty"`
}

type FolderListDetail string

const (
	FolderListDetailFull     FolderListDetail = ""
	FolderListDetailIdentity FolderListDetail = "identity"
	FolderListDetailSelected FolderListDetail = "selected"
)

type FolderListRequest struct {
	Detail    FolderListDetail `json:"detail,omitempty"`
	DetailIDs []string         `json:"detail_ids,omitempty"`
}

func NewFolderListRequest(detail FolderListDetail, detailIDs []string) (FolderListRequest, error) {
	switch detail {
	case FolderListDetailFull, FolderListDetailIdentity:
		if len(detailIDs) != 0 {
			return FolderListRequest{}, errs.NewSharedProviderContractInvalid()
		}
		return FolderListRequest{Detail: detail, DetailIDs: []string{}}, nil
	case FolderListDetailSelected:
		if len(detailIDs) == 0 {
			return FolderListRequest{}, errs.NewSharedProviderContractInvalid()
		}
		seen := make(map[string]struct{}, len(detailIDs))
		out := make([]string, 0, len(detailIDs))
		for _, id := range detailIDs {
			if id == "" {
				return FolderListRequest{}, errs.NewSharedProviderContractInvalid()
			}
			if _, exists := seen[id]; exists {
				return FolderListRequest{}, errs.NewSharedProviderContractInvalid()
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
		return FolderListRequest{Detail: detail, DetailIDs: out}, nil
	default:
		return FolderListRequest{}, errs.NewSharedProviderContractInvalid()
	}
}

// FolderList is all folders of one account.
type FolderList struct {
	Folders []Folder `json:"folders"`
}

// FolderResolveScope names the account a folder argument is looked up in.
// Only error messages and their details use it.
type FolderResolveScope struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
}

// FolderCandidate is one of several folders that match a name. It leaves
// out provider details, so errors never show secrets or local paths.
type FolderCandidate struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Path        string `json:"path,omitempty"`
}

// FolderResolution is the folder maildo found for an argument. MatchedBy
// says which rule matched, so scripts can check the choice.
type FolderResolution struct {
	Ref         FolderRef `json:"ref"`
	Folder      Folder    `json:"folder"`
	MatchedBy   string    `json:"matched_by"`
	Suggestions []string  `json:"suggestions,omitempty"`
}

// FolderMutationOperation names a folder change, such as a rename. The
// values appear in output and do not change.
type FolderMutationOperation string

const (
	FolderOperationCreate FolderMutationOperation = "folder_create"
	FolderOperationRename FolderMutationOperation = "folder_rename"
	FolderOperationDelete FolderMutationOperation = "folder_delete"
)

const (
	FolderVerificationProviderResponse = "provider_response"
	FolderVerificationReadBack         = "read_back"
	FolderCountsSourceRefreshed        = "refreshed"
)

// FolderCreateRequest asks a provider to create a folder at the top level,
// or under a parent folder that maildo already found.
type FolderCreateRequest struct {
	DisplayName string         `json:"display_name"`
	Parent      *FolderSummary `json:"parent_folder,omitempty"`
}

// FolderRenamePreflightRequest asks a provider whether it protects a folder,
// before maildo tries to rename it.
type FolderRenamePreflightRequest struct {
	Target Folder `json:"target_folder"`
}

// FolderDeletePreflightRequest asks a provider whether it protects a folder
// and how many messages it holds, before maildo tries to delete it.
type FolderDeletePreflightRequest struct {
	Target Folder `json:"target_folder"`
}

// FolderRenameRequest renames a folder maildo found to a checked new name.
type FolderRenameRequest struct {
	Target         Folder                  `json:"target_folder"`
	NewDisplayName string                  `json:"new_display_name"`
	Preflight      FolderMutationPreflight `json:"preflight,omitempty"`
}

// FolderDeleteRequest deletes a folder, with the counts from the check just
// before. The command asks for confirmation; the provider does not.
type FolderDeleteRequest struct {
	Target    Folder                  `json:"target_folder"`
	Preflight FolderMutationPreflight `json:"preflight,omitempty"`
}

// FolderMutationPreflight is what a provider reports, without changing
// anything, before a folder rename or delete. For a delete it also has the
// message counts; the Known flags tell zero apart from unknown.
type FolderMutationPreflight struct {
	Operation              FolderMutationOperation `json:"operation,omitempty"`
	Target                 FolderSummary           `json:"target_folder,omitempty"`
	Protected              bool                    `json:"protected"`
	ProtectionReason       string                  `json:"protection_reason,omitempty"`
	TotalItemCount         int                     `json:"total_item_count,omitempty"`
	TotalItemCountKnown    bool                    `json:"total_item_count_known"`
	ChildFolderCount       int                     `json:"child_folder_count,omitempty"`
	ChildFolderKnown       bool                    `json:"child_folder_count_known"`
	CountsSource           string                  `json:"counts_source,omitempty"`
	DescendantFacts        FolderDescendantFacts   `json:"-"`
	DescendantFactsPresent bool                    `json:"-"`
	ProviderMeta           *FolderProviderMetadata `json:"provider_metadata,omitempty"`
}

// FolderMutationResult is what a provider reports after a folder change. The
// command adds its own fields, such as the account, to the output.
type FolderMutationResult struct {
	Operation              FolderMutationOperation `json:"operation"`
	ParentFolder           *FolderSummary          `json:"parent_folder,omitempty"`
	TargetFolder           *FolderSummary          `json:"target_folder,omitempty"`
	CreatedFolder          *FolderSummary          `json:"created_folder,omitempty"`
	RenamedFolder          *FolderSummary          `json:"renamed_folder,omitempty"`
	DeletedFolder          *FolderSummary          `json:"deleted_folder,omitempty"`
	OldDisplayName         string                  `json:"old_display_name,omitempty"`
	NewDisplayName         string                  `json:"new_display_name,omitempty"`
	TotalItemCount         int                     `json:"total_item_count,omitempty"`
	TotalItemCountKnown    bool                    `json:"total_item_count_known,omitempty"`
	ChildFolderCount       int                     `json:"child_folder_count,omitempty"`
	ChildFolderKnown       bool                    `json:"child_folder_count_known,omitempty"`
	CountsSource           string                  `json:"counts_source,omitempty"`
	VerificationStatus     string                  `json:"verification_status,omitempty"`
	DescendantFacts        FolderDescendantFacts   `json:"-"`
	DescendantFactsPresent bool                    `json:"-"`
	*WriteOutcomeFacts
	ProviderMeta *FolderProviderMetadata `json:"provider_metadata,omitempty"`
}

type FolderDescendant struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Path        string `json:"path,omitempty"`
}

type FolderDescendantFacts struct {
	Count     uint64             `json:"descendant_count"`
	Known     bool               `json:"descendant_count_known"`
	Sample    []FolderDescendant `json:"descendant_sample"`
	Truncated bool               `json:"descendant_sample_truncated"`
}

// ValidateFolderDisplayName checks a new folder name before maildo calls the
// provider. Providers may add rules of their own. These rules make sure a
// folder path means the same thing on every provider.
func ValidateFolderDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	// Check for control characters before trimming, which would remove tabs
	// and newlines at the ends.
	switch {
	case name == "":
		return "", invalidFolderName(raw, "empty", "the folder name is empty")
	case strings.Contains(name, "/"):
		err := invalidFolderName(raw, "reserved_path_separator", `a folder name cannot contain "/", because maildo uses "/" between the folders of a path`)
		return "", errs.WithDetails(err, map[string]any{"hint": "to make a folder inside another one, use maildo folders create <name> --parent <folder>"})
	case containsASCIIControl(raw):
		return "", invalidFolderName(raw, "control_character", "a folder name cannot contain control characters such as tabs or line breaks")
	default:
		return name, nil
	}
}

func invalidFolderName(raw, reason, message string) error {
	details := map[string]any{
		"field":  "display_name",
		"reason": reason,
	}
	if strings.TrimSpace(raw) != "" {
		details["display_name"] = strings.TrimSpace(raw)
	}
	return errs.NewWithDetails(errs.CodeInvalidFolderName, message, details)
}

func containsASCIIControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
