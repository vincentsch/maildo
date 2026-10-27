package provider

import (
	"context"

	"github.com/vincentsch/maildo/internal/mail"
)

// ArchivePolicyReporter reports what a provider says about archiving and
// retention for one account. The report leaves out provider URLs and raw
// admin data.
type ArchivePolicyReporter interface {
	ArchivePolicyReport(context.Context, mail.ArchivePolicyReportRequest) (mail.ArchivePolicyReport, error)
}
