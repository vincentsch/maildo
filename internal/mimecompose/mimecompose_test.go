package mimecompose

import (
	"bytes"
	"errors"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/mail"
)

func TestDocumentEncodeProjectionAndBccPolicy(t *testing.T) {
	msg := Message{
		From:     mail.Recipient{Name: "Work", Address: "work@example.test"},
		To:       []mail.Recipient{{Address: "to@example.test"}},
		BCC:      []mail.Recipient{{Address: "hidden@example.test"}},
		Subject:  "Subject",
		TextBody: []byte("plain"),
		HTMLBody: []byte("<p>html</p>"),
		Attachments: []BinaryPart{{
			Role:        PartRoleAttachment,
			ContentType: "text/plain",
			Filename:    "note.txt",
			Bytes:       []byte("attachment"),
		}},
	}
	doc, err := NewDocument(msg, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	encoded, err := doc.Encode(EncodeOptions{
		BccPolicy:   BccHeaderPolicyOmit,
		AddressMode: AddressHeaderASCII,
		Sources: Sources{
			Now:       func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			MessageID: func() (string, error) { return "id123@example.test", nil },
			Boundary:  deterministicBoundaries("related-boundary", "alternative-boundary", "mixed-boundary"),
		},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	defer encoded.Destroy()
	raw := string(encoded.Bytes)
	if stringsContains(raw, "\r\nBcc:") || !stringsContains(raw, "multipart/mixed") || !stringsContains(raw, "Message-ID: <id123@example.test>") {
		t.Fatalf("encoded message = %q", raw)
	}
	if len(encoded.Projection.Attachments) != 1 || encoded.Projection.Attachments[0].DecodedSize != uint64(len("attachment")) {
		t.Fatalf("projection attachments = %#v", encoded.Projection.Attachments)
	}
}

func TestRootHeaderPolicyRejectsUnsafePreservation(t *testing.T) {
	base := Message{
		From:     mail.Recipient{Address: "work@example.test"},
		Subject:  "Subject",
		TextBody: []byte("body"),
	}
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "DKIM-Signature", err: ErrUnsupportedMIME},
		{name: "Content-MD5", err: ErrUnsupportedMIME},
		{name: "Received", err: ErrInvalidSemanticInput},
		{name: "X-Unsafe", err: ErrInvalidSemanticInput},
	} {
		msg := base
		msg.RootHeaders = []Header{{Name: tt.name, Value: "value"}}
		if _, err := NewDocument(msg, DefaultLimits()); !errors.Is(err, tt.err) {
			t.Fatalf("NewDocument %s error = %v", tt.name, err)
		}
	}

	msg := base
	msg.RootHeaders = []Header{{Name: "Comments", Value: "safe note"}}
	doc, err := NewDocument(msg, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument allowed header: %v", err)
	}
	encoded, err := doc.Encode(EncodeOptions{
		BccPolicy:   BccHeaderPolicyRetain,
		AddressMode: AddressHeaderASCII,
		Sources: Sources{
			Now:       func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			MessageID: func() (string, error) { return "id123@example.test", nil },
			Boundary:  deterministicBoundaries("b"),
		},
	})
	if err != nil {
		t.Fatalf("Encode allowed header: %v", err)
	}
	defer encoded.Destroy()
	if !strings.Contains(string(encoded.Bytes), "\r\nComments: safe note\r\n") {
		t.Fatalf("allowed header not emitted:\n%s", encoded.Bytes)
	}
}

func TestParseSignaturePolicyAndVerificationDiscardLimits(t *testing.T) {
	rawWithSignature := []byte("DKIM-Signature: v=1\r\nFrom: work@example.test\r\nSubject: x\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
	if _, err := ParseDocument(rawWithSignature, DefaultLimits()); !errors.Is(err, ErrUnsupportedMIME) {
		t.Fatalf("ParseDocument signature error = %v", err)
	}
	if _, err := ParseVerificationProjection(rawWithSignature, DefaultVerificationLimits()); err != nil {
		t.Fatalf("verification signature discard: %v", err)
	}

	limits := DefaultVerificationLimits()
	limits.MaxDiscardedRootHeaderCount = 1
	rawWithTwoTraceHeaders := []byte("Received: one\r\nReceived: two\r\nFrom: work@example.test\r\nSubject: x\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
	if _, err := ParseVerificationProjection(rawWithTwoTraceHeaders, limits); !errors.Is(err, ErrHeaderLimitExceeded) {
		t.Fatalf("discard count error = %v", err)
	} else if fact, ok := LimitViolationFromError(err); !ok || fact.Kind != LimitDiscardedRootHeaders || fact.Limit != 1 || fact.ObservedAtLeast != 2 {
		t.Fatalf("discard count fact = %#v ok=%t", fact, ok)
	}

	limits = DefaultVerificationLimits()
	limits.MaxDiscardedRootHeaderBytes = 20
	rawWithLargeTraceHeader := []byte("Received: 12345678901234567890\r\nFrom: work@example.test\r\nSubject: x\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
	if _, err := ParseVerificationProjection(rawWithLargeTraceHeader, limits); !errors.Is(err, ErrHeaderLimitExceeded) {
		t.Fatalf("discard byte error = %v", err)
	} else if fact, ok := LimitViolationFromError(err); !ok || fact.Kind != LimitDiscardedRootHeaderBytes || fact.Limit != 20 || fact.ObservedAtLeast != 21 {
		t.Fatalf("discard byte fact = %#v ok=%t", fact, ok)
	}
}

func TestVerificationProjectionUsesRawWireLimitSeparately(t *testing.T) {
	limits := DefaultVerificationLimits()
	limits.Semantic.MaxEncodedBytes = 120
	limits.MaxRawBytes = 512
	body := strings.Repeat("=41=\r\n", 20)
	raw := []byte("From: work@example.test\r\nSubject: x\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + body)
	if len(raw) <= int(limits.Semantic.MaxEncodedBytes) {
		t.Fatalf("raw length %d did not exceed semantic encoded limit", len(raw))
	}
	if _, err := ParseDocument(raw, limits.Semantic); !errors.Is(err, ErrEncodedMessageTooLarge) {
		t.Fatalf("ParseDocument raw-size error = %v", err)
	}
	projection, err := ParseVerificationProjection(raw, limits)
	if err != nil {
		t.Fatalf("ParseVerificationProjection: %v", err)
	}
	defer projection.Destroy()
	if got := string(projection.TextBody); got != strings.Repeat("A", 20) {
		t.Fatalf("decoded body = %q", got)
	}
}

// Attachment names survive an encode and a parse: non-ASCII names are
// written in RFC 2231 and RFC 2047 form, and control characters, which
// made such drafts impossible to send, become "_".
func TestAttachmentNamesRoundTrip(t *testing.T) {
	names := []string{"report.pdf", "Übersicht Q3 – final.pdf", "Grüße " + strings.Repeat("ä", 60) + ".txt", "a\x1b[31mred.txt"}
	msg := Message{From: mail.Recipient{Address: "work@example.test"}, Subject: "Files", TextBody: []byte("see files")}
	for _, name := range names {
		msg.Attachments = append(msg.Attachments, BinaryPart{Role: PartRoleAttachment, ContentType: "application/octet-stream", Filename: name, Bytes: []byte("x")})
	}
	doc, err := NewDocument(msg, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	encoded, err := doc.Encode(EncodeOptions{BccPolicy: BccHeaderPolicyOmit, AddressMode: AddressHeaderASCII})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	defer encoded.Destroy()
	if bytes.Contains(encoded.Bytes, []byte("Ü")) || bytes.Contains(encoded.Bytes, []byte("\x1b")) {
		t.Fatalf("raw bytes left in the headers:\n%s", encoded.Bytes)
	}
	parsed, err := ParseDocument(encoded.Bytes, DefaultLimits())
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	defer parsed.Destroy()
	back, err := parsed.Message()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{names[0], names[1], names[2], "a_[31mred.txt"}
	if len(back.Attachments) != len(want) {
		t.Fatalf("attachments = %#v", back.Attachments)
	}
	for i, name := range want {
		if back.Attachments[i].Filename != name {
			t.Fatalf("attachment %d = %q, want %q", i, back.Attachments[i].Filename, name)
		}
	}
	if _, err := ParseVerificationProjection(encoded.Bytes, DefaultVerificationLimits()); err != nil {
		t.Fatalf("verification parse: %v", err)
	}
}

func TestEncodeWrapsQuotedPrintableAndUsesSenderDomainMessageID(t *testing.T) {
	doc, err := NewDocument(Message{
		From:     mail.Recipient{Address: "work@example.test"},
		Subject:  "Subject",
		TextBody: []byte(strings.Repeat("é", 100)),
	}, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	encoded, err := doc.Encode(EncodeOptions{BccPolicy: BccHeaderPolicyRetain, AddressMode: AddressHeaderASCII})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	defer encoded.Destroy()
	raw := string(encoded.Bytes)
	if !strings.Contains(raw, "@example.test>") {
		t.Fatalf("production Message-ID did not use sender domain:\n%s", raw)
	}
	_, body, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("encoded message missing body separator:\n%s", raw)
	}
	for _, line := range strings.Split(strings.TrimSuffix(body, "\r\n"), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("quoted-printable line length = %d line=%q", len(line), line)
		}
	}
}

func TestEncodeRetriesBoundaryDuplicatesAndDescendantCollisions(t *testing.T) {
	doc, err := NewDocument(Message{
		From:     mail.Recipient{Address: "work@example.test"},
		Subject:  "Subject",
		TextBody: []byte("plain"),
		HTMLBody: []byte("<p>html</p>"),
		Related: []BinaryPart{{
			Role:        PartRoleRelated,
			ContentType: "image/png",
			ContentID:   "image@example.test",
			Bytes:       []byte("image"),
		}},
	}, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	encoded, err := doc.Encode(EncodeOptions{
		BccPolicy:   BccHeaderPolicyRetain,
		AddressMode: AddressHeaderASCII,
		Sources: Sources{
			Now:       func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			MessageID: func() (string, error) { return "id123@example.test", nil },
			Boundary:  deterministicBoundaries("dup", "dup", "du", "alt"),
		},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	defer encoded.Destroy()
	raw := string(encoded.Bytes)
	if strings.Count(raw, `boundary="dup"`) != 1 || strings.Count(raw, `boundary="alt"`) != 1 || strings.Contains(raw, `boundary="du"`) {
		t.Fatalf("boundary retry output:\n%s", raw)
	}
}

func TestAddressModeValidatesMailboxes(t *testing.T) {
	if _, err := NewDocument(Message{
		From:     mail.Recipient{Address: "bad..local@example.test"},
		Subject:  "Subject",
		TextBody: []byte("body"),
	}, DefaultLimits()); !errors.Is(err, ErrInvalidSemanticInput) {
		t.Fatalf("bad mailbox NewDocument error = %v", err)
	}

	doc, err := NewDocument(Message{
		From:     mail.Recipient{Address: "søren@example.test"},
		Subject:  "Subject",
		TextBody: []byte("body"),
	}, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument SMTPUTF8 mailbox: %v", err)
	}
	if _, err := doc.Encode(EncodeOptions{
		BccPolicy:   BccHeaderPolicyRetain,
		AddressMode: AddressHeaderASCII,
		Sources: Sources{
			Now:       func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			MessageID: func() (string, error) { return "id123@example.test", nil },
			Boundary:  deterministicBoundaries("b"),
		},
	}); !errors.Is(err, ErrInvalidSemanticInput) {
		t.Fatalf("ASCII non-ASCII local error = %v", err)
	}
	if encoded, err := doc.Encode(EncodeOptions{
		BccPolicy:   BccHeaderPolicyRetain,
		AddressMode: AddressHeaderSMTPUTF8,
		Sources: Sources{
			Now:       func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			MessageID: func() (string, error) { return "id123@example.test", nil },
			Boundary:  deterministicBoundaries("b"),
		},
	}); err != nil {
		t.Fatalf("SMTPUTF8 encode: %v", err)
	} else {
		encoded.Destroy()
	}
}

func TestDocumentApplyIsAtomicOnInvalidPatch(t *testing.T) {
	doc, err := NewDocument(Message{
		From:     mail.Recipient{Address: "work@example.test"},
		Subject:  "Subject",
		TextBody: []byte("before"),
	}, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	before, err := doc.Projection()
	if err != nil {
		t.Fatalf("Projection before: %v", err)
	}
	err = doc.Apply(Patch{HTMLRootContentID: StringPatch{Set: true, Value: "<root@example.test>"}})
	if !errors.Is(err, ErrInvalidSemanticInput) {
		t.Fatalf("Apply error = %v", err)
	}
	after, err := doc.Projection()
	if err != nil {
		t.Fatalf("Projection after: %v", err)
	}
	if !bytes.Equal(before.TextBody, after.TextBody) || after.HTMLRootContentID != "" {
		t.Fatalf("document changed: before=%#v after=%#v", before, after)
	}
}

func TestDocumentApplyRejectsLargePatchAtomically(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxEncodedBytes = 8
	doc, err := NewDocument(Message{
		From:     mail.Recipient{Address: "work@example.test"},
		Subject:  "Subject",
		TextBody: []byte("before"),
	}, limits)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	err = doc.Apply(Patch{TextBody: BytesPatch{Set: true, Value: []byte("too large")}})
	if !errors.Is(err, ErrEncodedMessageTooLarge) {
		t.Fatalf("Apply error = %v", err)
	}
	after, err := doc.Projection()
	if err != nil {
		t.Fatalf("Projection after: %v", err)
	}
	if string(after.TextBody) != "before" {
		t.Fatalf("document changed after rejected patch: %#v", after)
	}
}

func TestDocumentEncodedLimitFacts(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxEncodedBytes = 120
	doc, err := NewDocument(Message{
		From:     mail.Recipient{Address: "work@example.test"},
		Subject:  "Subject",
		TextBody: []byte("body"),
	}, limits)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	_, err = doc.Encode(EncodeOptions{
		BccPolicy:   BccHeaderPolicyRetain,
		AddressMode: AddressHeaderASCII,
		Sources: Sources{
			Now:       func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			MessageID: func() (string, error) { return "id123@example.test", nil },
			Boundary:  deterministicBoundaries("b"),
		},
	})
	if !errors.Is(err, ErrEncodedMessageTooLarge) {
		t.Fatalf("Encode error = %v", err)
	}
	fact, ok := LimitViolationFromError(err)
	if !ok || fact.Kind != LimitEncodedMessageBytes || fact.Limit != 120 || fact.ObservedAtLeast != 121 {
		t.Fatalf("limit fact = %#v ok=%t", fact, ok)
	}
}

func TestDocumentDestroyZeroesAndRejectsUse(t *testing.T) {
	doc, err := NewDocument(Message{From: mail.Recipient{Address: "work@example.test"}, TextBody: []byte("secret")}, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc.Destroy()
	doc.Destroy()
	if _, err := doc.Projection(); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("Projection after destroy = %v", err)
	}
}

func deterministicBoundaries(values ...string) func() (string, error) {
	i := 0
	return func() (string, error) {
		if i >= len(values) {
			return values[len(values)-1], nil
		}
		value := values[i]
		i++
		return value, nil
	}
}

func stringsContains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}

// A message to many people used to put every address on one header line
// and broke the 998-byte line limit, so reply-all to a big list failed.
func TestEncodeFoldsLongAddressLists(t *testing.T) {
	var to, cc []mail.Recipient
	for i := 0; i < 100; i++ {
		to = append(to, mail.Recipient{Name: "Person Number", Address: "user" + strings.Repeat("x", i%7) + "@client.example"})
	}
	for i := 0; i < 50; i++ {
		cc = append(cc, mail.Recipient{Address: "cc@example.test"})
	}
	doc, err := NewDocument(Message{
		From:     mail.Recipient{Address: "work@example.test"},
		To:       to,
		CC:       cc,
		BCC:      to[:40],
		Subject:  "Many",
		TextBody: []byte("body"),
	}, DefaultLimits())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	encoded, err := doc.Encode(EncodeOptions{BccPolicy: BccHeaderPolicyRetain, AddressMode: AddressHeaderASCII})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	defer encoded.Destroy()
	head, _, _ := strings.Cut(string(encoded.Bytes), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if len(line) > 78 {
			t.Fatalf("header line of %d bytes: %q", len(line), line)
		}
	}
	parsed, err := netmail.ReadMessage(bytes.NewReader(encoded.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"To": 100, "Cc": 50, "Bcc": 40} {
		got, err := parsed.Header.AddressList(name)
		if err != nil || len(got) != want {
			t.Fatalf("%s: %d addresses, err %v; want %d", name, len(got), err, want)
		}
	}
	if got, _ := parsed.Header.AddressList("To"); got[1].Name != "Person Number" || got[1].Address != "userx@client.example" {
		t.Fatalf("second To address = %+v", got[1])
	}
}
