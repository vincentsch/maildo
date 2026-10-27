package mail

import (
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

func TestNewPreparedSendSummaryCountsAndPresence(t *testing.T) {
	msg := OutboundMessage{
		From:    &Recipient{Name: "Work", Address: "work@example.test"},
		To:      []Recipient{{Address: "to@example.test"}},
		CC:      []Recipient{{Address: "cc@example.test"}},
		BCC:     []Recipient{{Address: "bcc@example.test"}},
		ReplyTo: []Recipient{{Address: "reply@example.test"}},
		Subject: " Subject ",
		Body: OutboundBody{
			Format: BodyFormatHTML,
			Text:   "readable",
			HTML:   "<p>readable</p>",
		},
	}
	summary, err := NewPreparedSendSummary(msg)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.ToCount != 1 || summary.CCCount != 1 || summary.BCCCount != 1 || summary.ReplyToCount != 1 || summary.AttachmentCount != 0 {
		t.Fatalf("counts = %#v", summary)
	}
	if !summary.SubjectPresent || !summary.TextBodyPresent || !summary.HTMLBodyPresent || !summary.SenderOverridePresent {
		t.Fatalf("presence = %#v", summary)
	}
}

func TestPreparedSendSummaryTextIgnoresHTMLConvenienceCopy(t *testing.T) {
	summary, err := NewPreparedSendSummary(OutboundMessage{
		To:      []Recipient{{Address: "to@example.test"}},
		Subject: "x",
		Body: OutboundBody{
			Format: BodyFormatText,
			Text:   "hello",
			HTML:   "<p>hello</p>",
		},
	})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if !summary.TextBodyPresent || summary.HTMLBodyPresent {
		t.Fatalf("text summary = %#v", summary)
	}
}

func TestCheckedUint32LenOverflow(t *testing.T) {
	if _, err := checkedUint32Len(uint64(^uint32(0)) + 1); productCode(err) != errs.CodeSharedProviderContractInvalid {
		t.Fatalf("overflow error = %v", err)
	}
}
