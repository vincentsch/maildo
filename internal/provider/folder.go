package provider

import (
	"context"

	"github.com/vincentsch/maildo/internal/mail"
)

// FolderReader lists and reads folders.
type FolderReader interface {
	ListFolders(ctx context.Context, req mail.FolderListRequest) (mail.FolderList, error)
}

// FolderManager creates, renames and deletes folders. The Preflight methods
// only read; commands call them before a change that removes something.
type FolderManager interface {
	PrepareFolderRename(context.Context, mail.FolderRenamePreflightRequest) (mail.FolderMutationPreflight, error)
	PrepareFolderDelete(context.Context, mail.FolderDeletePreflightRequest) (mail.FolderMutationPreflight, error)
	CreateFolder(context.Context, mail.FolderCreateRequest) (mail.FolderMutationResult, error)
	RenameFolder(context.Context, mail.FolderRenameRequest) (mail.FolderMutationResult, error)
	DeleteFolder(context.Context, mail.FolderDeleteRequest) (mail.FolderMutationResult, error)
}

func safeClientIdentity(client AccountClient) (providerName, accountName string, err error) {
	defer func() {
		if recover() != nil {
			providerName, accountName = "", ""
			err = ErrAccountClientInvalid
		}
	}()
	return client.ProviderName(), client.AccountName(), nil
}
