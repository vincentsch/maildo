// Package limits holds the caps on how much maildo reads from a provider in
// one command, and how many requests it runs at once. They keep a huge or
// broken mailbox from using up memory or time.
package limits

const (
	OutlookMaxPages                                     = 1000
	OutlookMaxFolders                                   = 10_000
	GmailMaxPages                                       = 100
	GmailMaxHydratedMessages                            = 1_000
	GmailHydrationConcurrency                           = 8
	GmailMaxLabels                                      = 5_000
	GmailMaxHeaderCount                                 = 200
	GmailMaxAddressCount                                = 200
	GmailMaxMIMEDepth                                   = 32
	GmailMaxMIMEParts                                   = 1_000
	GmailMaxComposeVerificationDiscardedRootHeaderCount = 64

	IMAPMaxMailboxes                = 512
	IMAPMaxHierarchyDepth           = 32
	IMAPStatusConcurrency           = 8
	IMAPSearchWindowUIDs            = 500
	IMAPMaxCandidatesPerMailbox     = 2_000
	IMAPMaxCandidatesTotal          = 10_000
	IMAPMaxHeaderCount              = 200
	IMAPMaxAddressCount             = 200
	IMAPMaxMIMEDepth                = 32
	IMAPMaxMIMEParts                = 1_000
	IMAPMaxThreadCandidates         = 5_000
	IMAPMaxThreadRelationIDs        = 10_000
	IMAPMaxThreadSearchBatches      = 2_000
	IMAPMaxRelationTokensPerMessage = 100
)

const (
	GmailMaxJSONResponseBytes                           int64 = 8 * 1024 * 1024
	GmailMaxErrorResponseBytes                          int64 = 64 * 1024
	GmailMaxRawEncodedBytes                             int64 = 1_398_104
	GmailMaxAttachmentEncodedBytes                      int64 = 34_952_536
	GmailMaxHeaderBytes                                 int64 = 256 * 1024
	GmailMaxComposeRawResponseBytes                     int64 = 35_018_072
	GmailMaxComposeVerificationDecodedRFCBytes          int64 = 36_700_160
	GmailMaxComposeVerificationEncodedBytes             int64 = 48_933_548
	GmailMaxComposeVerificationResponseBytes            int64 = 48_999_084
	GmailMaxComposeVerificationDiscardedRootHeaderBytes int64 = 262_144
	GmailMaxAttachmentStructureResponseBytes            int64 = 43_341_144
	GmailMaxAttachmentContentResponseBytes              int64 = 35_018_072
	OAuthMaxResponseBytes                               int64 = 1 * 1024 * 1024
	OAuthMaxCallbackQueryBytes                          int64 = 8 * 1024

	IMAPMaxMailboxNameBytes       int64 = 1_024
	IMAPMaxListStatusBytes        int64 = 8 * 1024 * 1024
	IMAPMaxHeaderBytes            int64 = 256 * 1024
	IMAPMaxLineBytes              int64 = 64 * 1024
	IMAPMaxMetadataLiteralBytes   int64 = 8 * 1024 * 1024
	IMAPMaxEncodedAttachmentBytes int64 = 75 * 1024 * 1024
)
