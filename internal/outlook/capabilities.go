package outlook

import (
	"context"

	"github.com/vincentsch/maildo/internal/provider"
)

// Capabilities says what maildo can do with an Outlook account.
func (p *Provider) Capabilities(_ context.Context) (provider.Capabilities, error) {
	all := provider.AllCapabilities()
	caps := make(provider.Capabilities, len(all))
	for _, c := range all {
		caps[c] = provider.CapSupportedNotImpl
	}
	caps[provider.CapFoldersRead] = provider.CapImplemented
	caps[provider.CapFoldersMutate] = provider.CapImplemented
	caps[provider.CapMessagesSearch] = provider.CapImplemented
	caps[provider.CapMessagesList] = provider.CapImplemented
	caps[provider.CapMessagesRead] = provider.CapImplemented
	caps[provider.CapThreadsRead] = provider.CapImplemented
	caps[provider.CapDraftCreate] = provider.CapImplemented
	caps[provider.CapDraftRead] = provider.CapImplemented
	caps[provider.CapDraftUpdate] = provider.CapImplemented
	caps[provider.CapDraftSend] = provider.CapImplemented
	caps[provider.CapSendDirect] = provider.CapImplemented
	caps[provider.CapReplyDraftCreate] = provider.CapImplemented
	caps[provider.CapForwardDraftCreate] = provider.CapImplemented
	caps[provider.CapAttachmentsRead] = provider.CapImplemented
	caps[provider.CapAttachmentsMutate] = provider.CapImplemented
	caps[provider.CapMessagesMove] = provider.CapImplemented
	caps[provider.CapMessagesCopy] = provider.CapImplemented
	caps[provider.CapMessagesArchive] = provider.CapImplemented
	caps[provider.CapMessagesDelete] = provider.CapImplemented
	caps[provider.CapMessagesRestore] = provider.CapImplemented
	caps[provider.CapMessagesJunk] = provider.CapImplemented
	caps[provider.CapMessagesMarkRead] = provider.CapImplemented
	caps[provider.CapMessagesFlag] = provider.CapImplemented
	caps[provider.CapCategoriesRead] = provider.CapImplemented
	caps[provider.CapCategoriesMutate] = provider.CapImplemented
	caps[provider.CapRulesRead] = provider.CapImplemented
	caps[provider.CapRulesMutate] = provider.CapImplemented
	caps[provider.CapArchivePolicyReport] = provider.CapImplemented
	return caps, nil
}
