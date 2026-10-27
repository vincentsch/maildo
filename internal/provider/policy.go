package provider

import (
	"fmt"
	"strings"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

// accessKind sorts the client accesses into what a policy can allow.
type accessKind int

const (
	accessRead accessKind = iota
	accessDraft
	accessSend
	accessChange
)

var accessKinds = map[ClientAccessKind]accessKind{
	ClientAccessDraftCreate:                              accessDraft,
	ClientAccessDraftUpdate:                              accessDraft,
	ClientAccessDraftAttachmentsMutate:                   accessDraft,
	ClientAccessReplyDraftCreate:                         accessDraft,
	ClientAccessForwardDraftCreate:                       accessDraft,
	ClientAccessForwardDraftCreateWithAttachments:        accessDraft,
	ClientAccessForwardDraftCreatePreviewWithAttachments: accessDraft,
	ClientAccessDirectSend:                               accessSend,
	ClientAccessDraftSend:                                accessSend,
	ClientAccessReplyDraftSend:                           accessSend,
	ClientAccessForwardDraftSend:                         accessSend,
	ClientAccessForwardDraftSendWithAttachments:          accessSend,
	ClientAccessForwardDraftSendPreviewWithAttachments:   accessSend,
	ClientAccessCategoriesMutate:                         accessChange,
	ClientAccessFoldersMutate:                            accessChange,
	ClientAccessMessageMove:                              accessChange,
	ClientAccessMessageCopy:                              accessChange,
	ClientAccessMessageArchive:                           accessChange,
	ClientAccessMessageDelete:                            accessChange,
	ClientAccessMessageRestore:                           accessChange,
	ClientAccessMessageJunk:                              accessChange,
	ClientAccessMessageMarkRead:                          accessChange,
	ClientAccessMessageFlag:                              accessChange,
	ClientAccessRulesMutate:                              accessChange,
}

// CheckPolicy refuses what the account's policy does not allow. read_only
// allows reading only. drafts_only also allows writing drafts, but no
// sending and no changes to the mailbox.
func CheckPolicy(account ResolvedAccount, access ClientAccessKind) error {
	kind := accessKinds[access]
	p := account.Policy
	switch {
	case kind == accessRead:
		return nil
	case p.ReadOnly:
		return policyDenied(account, "read_only", "this account is read-only")
	case p.DraftsOnly && kind != accessDraft:
		return policyDenied(account, "drafts_only", "this account may only write drafts")
	}
	return nil
}

// CheckRecipients refuses to send to anyone outside policy.allow_recipients.
func CheckRecipients(account ResolvedAccount, recipients []mail.Recipient) error {
	allowed := account.Policy.AllowRecipients
	if len(allowed) == 0 {
		return nil
	}
	var refused []string
	for _, r := range recipients {
		if !recipientAllowed(allowed, r.Address) {
			refused = append(refused, r.Address)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	err := policyDenied(account, "allow_recipients", fmt.Sprintf("the account's policy does not allow sending to %s", strings.Join(refused, ", ")))
	return errs.WithDetails(err, map[string]any{"refused_recipients": refused})
}

// CheckBulkSize refuses bulk changes larger than policy.max_bulk.
func CheckBulkSize(account ResolvedAccount, count int) error {
	limit := account.Policy.MaxBulk
	if limit <= 0 || count <= limit {
		return nil
	}
	err := policyDenied(account, "max_bulk", fmt.Sprintf("the account's policy allows changing at most %d messages at once; this would change %d", limit, count))
	return errs.WithDetails(err, map[string]any{"max_bulk": limit, "target_count": count})
}

// recipientAllowed matches an address against entries that are addresses
// (ann@example.com) or domains (@example.com or example.com). A domain also
// matches its subdomains.
func recipientAllowed(allowed []string, address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return false
	}
	domain := address[at+1:]
	for _, entry := range allowed {
		entry = strings.ToLower(strings.TrimSpace(entry))
		switch {
		case entry == "":
		case strings.Contains(strings.TrimPrefix(entry, "@"), "@"):
			if entry == address {
				return true
			}
		default:
			d := strings.TrimPrefix(entry, "@")
			if domain == d || strings.HasSuffix(domain, "."+d) {
				return true
			}
		}
	}
	return false
}

func policyDenied(account ResolvedAccount, rule, message string) error {
	return errs.NewWithDetails(errs.CodePolicyDenied, message, map[string]any{
		"account":  account.Name,
		"provider": account.Provider,
		"policy":   rule,
	})
}

func clonePolicy(p maildoconfig.Policy) maildoconfig.Policy {
	p.AllowRecipients = append([]string(nil), p.AllowRecipients...)
	return p
}
