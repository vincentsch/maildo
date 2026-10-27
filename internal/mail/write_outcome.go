package mail

import "github.com/vincentsch/maildo/internal/errs"

type WriteOutcome = errs.WriteOutcome
type WriteOutcomeFacts = errs.WriteOutcomeFacts

const (
	WriteOutcomeNotApplied   = errs.WriteOutcomeNotApplied
	WriteOutcomeRejected     = errs.WriteOutcomeRejected
	WriteOutcomeSucceeded    = errs.WriteOutcomeSucceeded
	WriteOutcomeNoOp         = errs.WriteOutcomeNoOp
	WriteOutcomeKnownPartial = errs.WriteOutcomeKnownPartial
	WriteOutcomeUnknown      = errs.WriteOutcomeUnknown
)

func CloneWriteOutcomeFacts(in *WriteOutcomeFacts) *WriteOutcomeFacts {
	return errs.CloneWriteOutcomeFacts(in)
}
