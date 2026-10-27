package outlook

import (
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

func graphFolderMeta(use mail.ProviderMetadataUse) *mail.FolderProviderMetadata {
	return mustOutlookMetadata(use, mail.FolderProviderMetadata{Source: mail.MetadataSourceMicrosoftGraph})
}

func graphProtectedFolderMeta(alias string) *mail.FolderProviderMetadata {
	return mustOutlookMetadata(mail.MetadataFolderMutationPreflight, mail.FolderProviderMetadata{
		Source:         mail.MetadataSourceMicrosoftGraph,
		ProtectedAlias: mail.ProtectedFolderAlias(alias),
	})
}

func graphSummaryMeta() *mail.SummaryProviderMetadata {
	return mustOutlookMetadata(mail.MetadataMessageSummary, mail.SummaryProviderMetadata{Source: mail.MetadataSourceMicrosoftGraph})
}

func graphMessageMeta() *mail.MessageProviderMetadata {
	return mustOutlookMetadata(mail.MetadataMessage, mail.MessageProviderMetadata{Source: mail.MetadataSourceMicrosoftGraph})
}

func graphAttachmentMeta(use mail.ProviderMetadataUse) *mail.AttachmentProviderMetadata {
	return mustOutlookMetadata(use, mail.AttachmentProviderMetadata{Source: mail.MetadataSourceMicrosoftGraph})
}

func graphMutationMeta() *mail.MutationProviderMetadata {
	return mustOutlookMetadata(mail.MetadataMessageMutation, mail.MutationProviderMetadata{Source: mail.MetadataSourceMicrosoftGraph})
}

func graphDraftMeta(use mail.ProviderMetadataUse) *mail.DraftProviderMetadata {
	return mustOutlookMetadata(use, mail.DraftProviderMetadata{Source: mail.MetadataSourceMicrosoftGraph})
}

func mustOutlookMetadata[T mail.ProviderMetadataValue](use mail.ProviderMetadataUse, value T) *T {
	meta, err := mail.NewProviderMetadata(errs.ProviderOutlook, use, value)
	if err != nil {
		panic(err)
	}
	return meta
}
