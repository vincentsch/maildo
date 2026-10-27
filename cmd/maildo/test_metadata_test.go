package main

import (
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

func fakeDraftProviderMetadataForTest(t *testing.T, use mail.ProviderMetadataUse) *mail.DraftProviderMetadata {
	t.Helper()
	meta, err := mail.NewFakeProviderMetadataForTest(use, mail.DraftProviderMetadata{Source: mail.MetadataSourceFakeForTest})
	if err != nil {
		t.Fatalf("fake draft metadata: %v", err)
	}
	return meta
}
