package mail

import (
	"errors"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

func TestProviderMetadataRejectsProviderInappropriateFields(t *testing.T) {
	if _, err := NewProviderMetadata(errs.ProviderOutlook, MetadataMessageSummary, SummaryProviderMetadata{
		Source:     MetadataSourceMicrosoftGraph,
		UserLabels: []string{"client"},
	}); productCode(err) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("outlook summary with Gmail labels error = %v", err)
	}
	if _, err := NewProviderMetadata(errs.ProviderGmail, MetadataMessageSummary, SummaryProviderMetadata{
		Source: MetadataSourceGmailAPI,
	}); productCode(err) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("gmail summary without authoritative labels/basis error = %v", err)
	}
	labels := []string{}
	if _, err := NewProviderMetadata(errs.ProviderGmail, MetadataMessageSummary, SummaryProviderMetadata{
		Source:             MetadataSourceGmailAPI,
		UserLabels:         labels,
		SummaryFolderBasis: SummaryBasisSystem,
	}); err != nil {
		t.Fatalf("gmail summary with authoritative empty labels rejected: %v", err)
	}
}

func TestProviderMetadataRejectsEmptyAndInvalidEnums(t *testing.T) {
	if _, err := NewProviderMetadata(errs.ProviderOutlook, MetadataFolder, FolderProviderMetadata{}); productCode(err) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("empty metadata error = %v", err)
	}
	if _, err := NewProviderMetadata(errs.ProviderIMAP, MetadataFolder, FolderProviderMetadata{
		Source:     MetadataSourceIMAP,
		SpecialUse: []ProviderSpecialUse{ProviderSpecialUse("bad")},
	}); productCode(err) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("invalid special-use error = %v", err)
	}
	if _, err := NewProviderMetadata(errs.ProviderGmail, MetadataMessage, MessageProviderMetadata{
		Source: MetadataSourceGmailAPI,
	}); err != nil {
		t.Fatalf("gmail metadata-only message metadata rejected: %v", err)
	}
	starred := false
	if _, err := NewProviderMetadata(errs.ProviderGmail, MetadataMessage, MessageProviderMetadata{
		Source:  MetadataSourceGmailAPI,
		Starred: &starred,
	}); err != nil {
		t.Fatalf("gmail message metadata rejected: %v", err)
	}
	flagged := false
	if _, err := NewProviderMetadata(errs.ProviderGmail, MetadataMessage, MessageProviderMetadata{
		Source:  MetadataSourceGmailAPI,
		Flagged: &flagged,
	}); productCode(err) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("gmail message with Outlook flag error = %v", err)
	}
}

func TestCloneProviderMetadataReturnsOwnedPointers(t *testing.T) {
	starred := true
	meta, err := NewProviderMetadata(errs.ProviderGmail, MetadataMessage, MessageProviderMetadata{
		Source:  MetadataSourceGmailAPI,
		Starred: &starred,
	})
	if err != nil {
		t.Fatal(err)
	}
	clone, err := CloneProviderMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	if clone == meta || clone.Starred == meta.Starred || clone.Starred == nil || !*clone.Starred {
		t.Fatalf("clone did not own pointers: original=%p/%p clone=%p/%p", meta, meta.Starred, clone, clone.Starred)
	}
	*meta.Starred = false
	if !*clone.Starred {
		t.Fatal("clone observed caller mutation")
	}
}

func TestSMTPDirectSendMetadataValidationAndClone(t *testing.T) {
	meta, err := NewProviderMetadata(errs.ProviderIMAP, MetadataSMTPDirectSend, SMTPDirectSendProviderMetadata{
		AcceptanceKnown:        true,
		ServerMessageIDPresent: false,
		SMTPUTF8Used:           true,
	})
	if err != nil {
		t.Fatalf("valid smtp metadata rejected: %v", err)
	}
	clone, err := CloneProviderMetadata(meta)
	if err != nil {
		t.Fatalf("clone smtp metadata: %v", err)
	}
	if clone == meta || !clone.AcceptanceKnown || clone.ServerMessageIDPresent || !clone.SMTPUTF8Used {
		t.Fatalf("bad smtp clone: original=%p clone=%p value=%#v", meta, clone, clone)
	}
	if _, err := NewProviderMetadata(errs.ProviderFastmail, MetadataSMTPDirectSend, SMTPDirectSendProviderMetadata{
		AcceptanceKnown: true,
	}); err != nil {
		t.Fatalf("fastmail smtp metadata rejected: %v", err)
	}
	for _, tt := range []struct {
		name     string
		provider errs.ProviderName
		use      ProviderMetadataUse
		value    SMTPDirectSendProviderMetadata
	}{
		{
			name:     "outlook",
			provider: errs.ProviderOutlook,
			use:      MetadataSMTPDirectSend,
			value:    SMTPDirectSendProviderMetadata{AcceptanceKnown: true},
		},
		{
			name:     "wrong-use",
			provider: errs.ProviderIMAP,
			use:      MetadataDraftSend,
			value:    SMTPDirectSendProviderMetadata{AcceptanceKnown: true},
		},
		{
			name:     "unknown-acceptance",
			provider: errs.ProviderIMAP,
			use:      MetadataSMTPDirectSend,
			value:    SMTPDirectSendProviderMetadata{},
		},
		{
			name:     "server-message-id",
			provider: errs.ProviderIMAP,
			use:      MetadataSMTPDirectSend,
			value:    SMTPDirectSendProviderMetadata{AcceptanceKnown: true, ServerMessageIDPresent: true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewProviderMetadata(tt.provider, tt.use, tt.value); productCode(err) != errs.CodeSharedProviderContractInvalid {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func productCode(err error) string {
	var product *errs.Error
	if !errors.As(err, &product) {
		return ""
	}
	return product.Code()
}
