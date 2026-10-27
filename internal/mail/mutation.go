package mail

import (
	"time"

	"github.com/vincentsch/maildo/internal/errs"
)

// MessageMutationOperation names a change to a message, such as archive. The
// names appear in output and do not change.
type MessageMutationOperation string

const (
	MutationMove            MessageMutationOperation = "move"
	MutationCopy            MessageMutationOperation = "copy"
	MutationArchive         MessageMutationOperation = "archive"
	MutationDelete          MessageMutationOperation = "delete"
	MutationRestore         MessageMutationOperation = "restore"
	MutationJunk            MessageMutationOperation = "junk"
	MutationMarkRead        MessageMutationOperation = "mark_read"
	MutationMarkUnread      MessageMutationOperation = "mark_unread"
	MutationFlagSet         MessageMutationOperation = "flag_set"
	MutationFlagClear       MessageMutationOperation = "flag_clear"
	MutationCategoriesList  MessageMutationOperation = "categories_list"
	MutationCategoriesSet   MessageMutationOperation = "categories_set"
	MutationCategoriesClear MessageMutationOperation = "categories_clear"
)

// MessageMutationRequest names the message for a change that needs nothing
// else, such as archive or delete.
type MessageMutationRequest struct {
	MessageID string `json:"message_id"`
}

// MessageMoveRequest names the message and the folder to move it to.
type MessageMoveRequest struct {
	MessageID string `json:"message_id"`
	Folder    Folder `json:"folder"`
}

// MessageCopyRequest names the message and the folder to copy it to.
type MessageCopyRequest struct {
	MessageID string `json:"message_id"`
	Folder    Folder `json:"folder"`
}

// MessageRestoreRequest names the message and the folder to restore it to.
// It works like a move but stays a separate operation in output.
type MessageRestoreRequest struct {
	MessageID string `json:"message_id"`
	Folder    Folder `json:"folder"`
}

// MessageReadStateRequest marks one message read or unread.
type MessageReadStateRequest struct {
	MessageID string `json:"message_id"`
	IsRead    bool   `json:"is_read"`
}

// MessageFlagRequest sets or clears the flag of one message.
type MessageFlagRequest struct {
	MessageID string `json:"message_id"`
	Flagged   bool   `json:"flagged"`
}

// MessageCategoryListRequest lists the categories of one message. The
// command looks up a Message-ID first, so the provider always gets an id.
type MessageCategoryListRequest struct {
	MessageID string `json:"message_id"`
}

// MessageCategorySetRequest replaces all categories of a message. The command
// works out additions and removals before it sends this.
type MessageCategorySetRequest struct {
	MessageID  string                      `json:"message_id"`
	Categories []string                    `json:"categories"`
	Mode       MessageCategoryMutationMode `json:"-"`
	Requested  []string                    `json:"-"`
}

type MessageCategoryMutationMode string

const (
	CategoryMutationSet        MessageCategoryMutationMode = "set"
	CategoryMutationClearNamed MessageCategoryMutationMode = "clear_named"
	CategoryMutationClearAll   MessageCategoryMutationMode = "clear_all"
)

func NewMessageCategorySetRequest(messageID string, mode MessageCategoryMutationMode, categories []string, requested []string) (MessageCategorySetRequest, error) {
	if messageID == "" {
		return MessageCategorySetRequest{}, errs.NewSharedProviderContractInvalid()
	}
	cats, err := cloneUniqueCategorySlice(categories, mode == CategoryMutationSet)
	if err != nil {
		return MessageCategorySetRequest{}, err
	}
	req, err := cloneUniqueCategorySlice(requested, mode == CategoryMutationClearNamed)
	if err != nil {
		return MessageCategorySetRequest{}, err
	}
	switch mode {
	case CategoryMutationSet:
		if len(req) != 0 || len(cats) == 0 {
			return MessageCategorySetRequest{}, errs.NewSharedProviderContractInvalid()
		}
	case CategoryMutationClearAll:
		if categories == nil || requested == nil || len(cats) != 0 || len(req) != 0 {
			return MessageCategorySetRequest{}, errs.NewSharedProviderContractInvalid()
		}
	case CategoryMutationClearNamed:
		if len(req) == 0 {
			return MessageCategorySetRequest{}, errs.NewSharedProviderContractInvalid()
		}
	default:
		return MessageCategorySetRequest{}, errs.NewSharedProviderContractInvalid()
	}
	return MessageCategorySetRequest{MessageID: messageID, Categories: cats, Mode: mode, Requested: req}, nil
}

func cloneUniqueCategorySlice(in []string, requireNonEmpty bool) ([]string, error) {
	if len(in) == 0 {
		if requireNonEmpty {
			return nil, errs.NewSharedProviderContractInvalid()
		}
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value == "" {
			return nil, errs.NewSharedProviderContractInvalid()
		}
		if _, ok := seen[value]; ok {
			return nil, errs.NewSharedProviderContractInvalid()
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

// BoolChange is a yes/no state before and after a change. Some providers
// report only the state after.
type BoolChange struct {
	Before *bool `json:"before,omitempty"`
	After  bool  `json:"after"`
}

// CategoryChange is the category list after a change, with what the command
// worked out, such as names it removed or names that were not there.
type CategoryChange struct {
	Before  []string `json:"before,omitempty"`
	After   []string `json:"after"`
	Removed []string `json:"removed,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
	Count   int      `json:"count"`
}

// MessageMutationResult is what a provider reports after changing a message.
// A field stays empty when the provider cannot know it for sure.
type MessageMutationResult struct {
	MessageID         string          `json:"message_id"`
	ResultMessageID   string          `json:"result_message_id,omitempty"`
	InternetMessageID string          `json:"internet_message_id,omitempty"`
	SourceFolder      *FolderSummary  `json:"source_folder,omitempty"`
	TargetFolder      *FolderSummary  `json:"target_folder,omitempty"`
	ReadState         *BoolChange     `json:"read_state,omitempty"`
	FlagState         *BoolChange     `json:"flag_state,omitempty"`
	Categories        *CategoryChange `json:"categories,omitempty"`
	Changed           *bool           `json:"changed,omitempty"`
	*WriteOutcomeFacts
	ProviderMeta *MutationProviderMetadata `json:"provider_metadata,omitempty"`
}

// MessageCategoryListResult lists the categories of one message.
type MessageCategoryListResult struct {
	MessageID         string                    `json:"message_id"`
	InternetMessageID string                    `json:"internet_message_id,omitempty"`
	Categories        []string                  `json:"categories"`
	Count             int                       `json:"count"`
	ProviderMeta      *MutationProviderMetadata `json:"provider_metadata,omitempty"`
}

// BulkSelectorSummary says how the messages of a bulk change were picked,
// for example from an ids file.
type BulkSelectorSummary struct {
	Kind           string `json:"kind"`
	Query          string `json:"query,omitempty"`
	IDsFile        string `json:"ids_file,omitempty"`
	TargetCount    int    `json:"target_count"`
	MaxTargets     int    `json:"max_targets,omitempty"`
	MatchedAtLeast int    `json:"matched_at_least,omitempty"`
}

// PlannedMutationTarget is one message a bulk change will touch, as previews
// and the action log show it. It holds no body.
type PlannedMutationTarget struct {
	Account           string                   `json:"account"`
	Provider          string                   `json:"provider"`
	Operation         MessageMutationOperation `json:"operation"`
	ID                string                   `json:"id"`
	InternetMessageID string                   `json:"internet_message_id,omitempty"`
	Folder            *FolderSummary           `json:"folder,omitempty"`
	From              Recipient                `json:"from,omitempty"`
	Subject           string                   `json:"subject,omitempty"`
	MessageTime       *time.Time               `json:"message_time,omitempty"`
	IsRead            *bool                    `json:"is_read,omitempty"`
	HasAttachments    *bool                    `json:"has_attachments,omitempty"`
}

// AppliedMutationTarget is how the change went for one message.
type AppliedMutationTarget struct {
	Account           string                   `json:"account"`
	Provider          string                   `json:"provider"`
	Operation         MessageMutationOperation `json:"operation"`
	ID                string                   `json:"id"`
	InternetMessageID string                   `json:"internet_message_id,omitempty"`
	Status            string                   `json:"status"`
	Reason            string                   `json:"reason,omitempty"`
	ErrorCode         string                   `json:"error_code,omitempty"`
	ErrorDetails      map[string]any           `json:"error_details,omitempty"`
	ResultMessageID   string                   `json:"result_message_id,omitempty"`
	SourceFolder      *FolderSummary           `json:"source_folder,omitempty"`
	TargetFolder      *FolderSummary           `json:"target_folder,omitempty"`
	ReadState         *BoolChange              `json:"read_state,omitempty"`
	FlagState         *BoolChange              `json:"flag_state,omitempty"`
	Categories        *CategoryChange          `json:"categories,omitempty"`
	Changed           *bool                    `json:"changed,omitempty"`
	*WriteOutcomeFacts
	ProviderMeta *MutationProviderMetadata `json:"provider_metadata,omitempty"`
}

// BulkMutationCounts counts the messages of a bulk change by how it went.
type BulkMutationCounts struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// ActionLogStatus says whether the change was written to the action log. It
// never holds the path of the config folder.
type ActionLogStatus struct {
	Written      bool   `json:"written"`
	EntryCount   int    `json:"entry_count"`
	RelativePath string `json:"relative_path"`
	Reason       string `json:"reason,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
}
