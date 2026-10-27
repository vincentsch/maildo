package provider

import (
	"context"
	"fmt"

	"github.com/vincentsch/maildo/internal/errs"
)

// Capability names something a provider can do. The names are machine
// values and must not change.
type Capability string

const (
	CapMessagesSearch      Capability = "messages_search"
	CapMessagesList        Capability = "messages_list"
	CapMessagesRead        Capability = "messages_read"
	CapThreadsRead         Capability = "threads_read"
	CapFoldersRead         Capability = "folders_read"
	CapFoldersMutate       Capability = "folders_mutate"
	CapDraftCreate         Capability = "draft_create"
	CapDraftRead           Capability = "draft_read"
	CapDraftUpdate         Capability = "draft_update"
	CapDraftSend           Capability = "draft_send"
	CapSendDirect          Capability = "send_direct"
	CapReplyDraftCreate    Capability = "reply_draft_create"
	CapForwardDraftCreate  Capability = "forward_draft_create"
	CapAttachmentsRead     Capability = "attachments_read"
	CapAttachmentsMutate   Capability = "attachments_mutate"
	CapMessagesMove        Capability = "messages_move"
	CapMessagesCopy        Capability = "messages_copy"
	CapMessagesArchive     Capability = "messages_archive"
	CapMessagesDelete      Capability = "messages_delete"
	CapMessagesRestore     Capability = "messages_restore"
	CapMessagesJunk        Capability = "messages_junk"
	CapMessagesMarkRead    Capability = "messages_mark_read"
	CapMessagesFlag        Capability = "messages_flag"
	CapCategoriesRead      Capability = "categories_read"
	CapCategoriesMutate    Capability = "categories_mutate"
	CapRulesRead           Capability = "rules_read"
	CapRulesMutate         Capability = "rules_mutate"
	CapArchivePolicyReport Capability = "archive_policy_report"
)

var allCapabilities = []Capability{
	CapMessagesSearch,
	CapMessagesList,
	CapMessagesRead,
	CapThreadsRead,
	CapFoldersRead,
	CapFoldersMutate,
	CapDraftCreate,
	CapDraftRead,
	CapDraftUpdate,
	CapDraftSend,
	CapSendDirect,
	CapReplyDraftCreate,
	CapForwardDraftCreate,
	CapAttachmentsRead,
	CapAttachmentsMutate,
	CapMessagesMove,
	CapMessagesCopy,
	CapMessagesArchive,
	CapMessagesDelete,
	CapMessagesRestore,
	CapMessagesJunk,
	CapMessagesMarkRead,
	CapMessagesFlag,
	CapCategoriesRead,
	CapCategoriesMutate,
	CapRulesRead,
	CapRulesMutate,
	CapArchivePolicyReport,
}

func AllCapabilities() []Capability {
	return append([]Capability(nil), allCapabilities...)
}

// CapabilityState says whether maildo can do something with a provider.
type CapabilityState string

const (
	CapImplemented      CapabilityState = "implemented"
	CapSupportedNotImpl CapabilityState = "provider_supported_not_implemented"
	CapUnsupported      CapabilityState = "provider_unsupported"
	CapUnknown          CapabilityState = "unknown"
)

// Capabilities is a provider's fixed table of what it can do. Building it
// must not read saved logins or call the provider.
type Capabilities map[Capability]CapabilityState

// State returns the state of cap, or CapUnknown when the table lacks it.
func (c Capabilities) State(cap Capability) CapabilityState {
	if s, ok := c[cap]; ok {
		return s
	}
	return CapUnknown
}

// capabilityDescriptions are kept apart from the capability names so the
// text can change without changing the names scripts see.
var capabilityDescriptions = map[Capability]string{
	CapMessagesSearch:      "Search messages through the provider API.",
	CapMessagesList:        "List messages in a folder or provider result page.",
	CapMessagesRead:        "Fetch a full message by id or supported selector.",
	CapThreadsRead:         "Read a provider conversation or thread.",
	CapFoldersRead:         "List, resolve, or read mail folders by provider id.",
	CapFoldersMutate:       "Create, rename, or delete mail folders.",
	CapDraftCreate:         "Create a draft message.",
	CapDraftRead:           "Read an existing draft message.",
	CapDraftUpdate:         "Update an existing draft message.",
	CapDraftSend:           "Send an existing draft message.",
	CapSendDirect:          "Send a message without a stored draft.",
	CapReplyDraftCreate:    "Create a provider reply or reply-all draft.",
	CapForwardDraftCreate:  "Create a provider forward draft.",
	CapAttachmentsRead:     "List and download message attachments.",
	CapAttachmentsMutate:   "Add or remove draft attachments.",
	CapMessagesMove:        "Move messages to another folder.",
	CapMessagesCopy:        "Copy messages to another folder.",
	CapMessagesArchive:     "Archive messages.",
	CapMessagesDelete:      "Delete messages.",
	CapMessagesRestore:     "Restore deleted messages.",
	CapMessagesJunk:        "Mark or move messages as junk/spam.",
	CapMessagesMarkRead:    "Mark messages read or unread.",
	CapMessagesFlag:        "Set or clear message flags.",
	CapCategoriesRead:      "Read message categories or labels.",
	CapCategoriesMutate:    "Set or clear message categories or labels.",
	CapRulesRead:           "List and read server-side rules or filters.",
	CapRulesMutate:         "Create, update, enable, disable, or delete server-side rules.",
	CapArchivePolicyReport: "Read a provider archive or retention policy report.",
}

// Describe returns the description of cap for people.
func Describe(cap Capability) string {
	return capabilityDescriptions[cap]
}

type CapabilityInfo struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Description string `json:"description"`
}

// CapabilityReport lists what one account's provider can do.
type CapabilityReport struct {
	Account      string           `json:"account"`
	Provider     string           `json:"provider"`
	Capabilities []CapabilityInfo `json:"capabilities"`
}

// NewCapabilityReport lists every capability, always in the same order.
func NewCapabilityReport(acct ResolvedAccount, caps Capabilities) CapabilityReport {
	out := CapabilityReport{
		Account:      acct.Name,
		Provider:     acct.Provider,
		Capabilities: make([]CapabilityInfo, 0, len(allCapabilities)),
	}
	for _, c := range allCapabilities {
		out.Capabilities = append(out.Capabilities, CapabilityInfo{
			Name:        string(c),
			State:       string(caps.State(c)),
			Description: Describe(c),
		})
	}
	return out
}

// CheckCapability refuses anything the provider does not support, anything
// maildo has not built yet, and anything missing from the table.
func CheckCapability(acct ResolvedAccount, caps Capabilities, c Capability) error {
	switch caps.State(c) {
	case CapImplemented:
		return nil
	case CapSupportedNotImpl:
		return errs.NewWithDetails(errs.CodeCapabilityNotImplemented, fmt.Sprintf(
			"account %q (%s) cannot do this yet (%s)", acct.Name, acct.Provider, c),
			map[string]any{"account": acct.Name, "provider": acct.Provider, "capability": string(c)})
	default:
		return errs.NewWithDetails(errs.CodeUnsupportedCapability, fmt.Sprintf(
			"account %q (%s) cannot do this, because the provider does not support it (%s)", acct.Name, acct.Provider, c),
			map[string]any{"account": acct.Name, "provider": acct.Provider, "capability": string(c)})
	}
}

// Capabilities returns one provider's capability table, after checking it is
// complete.
func (r *Registry) Capabilities(ctx context.Context, name string) (Capabilities, error) {
	p, _, ok := r.Get(name)
	if !ok {
		return nil, errs.New(errs.CodeProviderNotAvailable, fmt.Sprintf("provider %q is not available", name))
	}
	caps, err := p.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	return validateCapabilityTable(caps)
}

func validateCapabilityTable(caps Capabilities) (Capabilities, error) {
	if caps == nil {
		return nil, errs.NewSharedProviderContractInvalid()
	}
	out := make(Capabilities, len(allCapabilities))
	for cap, state := range caps {
		if !isKnownCapability(cap) || !validCapabilityState(state) {
			return nil, errs.NewSharedProviderContractInvalid()
		}
		out[cap] = state
	}
	for _, cap := range allCapabilities {
		if _, ok := out[cap]; !ok {
			return nil, errs.NewSharedProviderContractInvalid()
		}
	}
	return out, nil
}

func isKnownCapability(cap Capability) bool {
	for _, known := range allCapabilities {
		if cap == known {
			return true
		}
	}
	return false
}

func validCapabilityState(state CapabilityState) bool {
	switch state {
	case CapImplemented, CapSupportedNotImpl, CapUnsupported:
		return true
	default:
		return false
	}
}
