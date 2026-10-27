package outlook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

// draftSendServer serves one draft with the given attachments and records
// the sendMail body.
func draftSendServer(t *testing.T, attachments []map[string]any, props []map[string]any) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/d1":
			m := map[string]any{
				"id": "d1", "isDraft": true, "changeKey": "ck1", "subject": "Re: Plan",
				"body":         map[string]any{"contentType": "html", "content": "<p>Fine with me.</p>"},
				"toRecipients": []any{map[string]any{"emailAddress": map[string]any{"address": "clara@example.test"}}},
			}
			if strings.Contains(r.URL.Query().Get("$expand"), "attachments") {
				m["attachments"] = attachments
				m["singleValueExtendedProperties"] = props
			}
			writeJSON(w, m)
		case r.Method == http.MethodPost && r.URL.Path == "/me/sendMail":
			data, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(data, &sent)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/me/messages/d1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]any { mu.Lock(); defer mu.Unlock(); return sent }
}

func fileAttachment(name string, data []byte, inline bool) map[string]any {
	return map[string]any{
		"@odata.type": "#microsoft.graph.fileAttachment", "name": name, "contentType": "application/octet-stream",
		"size": len(data) + 200, "isInline": inline, "contentBytes": base64.StdEncoding.EncodeToString(data),
	}
}

// A sent draft keeps its thread headers, its inline images and its HTML
// body, and the copy goes out with sendMail.
func TestSendDraftSendsTheCheckedCopy(t *testing.T) {
	srv, sent := draftSendServer(t,
		[]map[string]any{fileAttachment("plan.pdf", []byte("PDF"), false), fileAttachment("logo.png", []byte("PNG"), true)},
		[]map[string]any{{"id": "String 0x1042", "value": "<plan@example.test>"}, {"id": "String 0x1039", "value": "<a@example.test> <plan@example.test>"}})
	var checked mail.SendContent
	out, err := testReplyClient(srv).SendDraft(context.Background(), mail.SendDraftRequest{DraftID: "d1", Check: func(c mail.SendContent) error {
		checked = c
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out.WriteOutcomeFacts == nil || out.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded || len(out.Warnings) != 0 {
		t.Fatalf("outcome = %#v", out)
	}
	if checked.HTML != "<p>Fine with me.</p>" || len(checked.Attachments) != 2 || checked.Attachments[0].Size != 3 || !checked.Attachments[1].Inline {
		t.Fatalf("checked content = %#v", checked)
	}
	message := sent()["message"].(map[string]any)
	body := message["body"].(map[string]any)
	if body["contentType"] != "html" || body["content"] != "<p>Fine with me.</p>" {
		t.Fatalf("sent body = %#v", body)
	}
	if attachments := message["attachments"].([]any); len(attachments) != 2 || attachments[1].(map[string]any)["isInline"] != true {
		t.Fatalf("sent attachments = %#v", attachments)
	}
	if props := message["singleValueExtendedProperties"].([]any); len(props) != 2 {
		t.Fatalf("sent thread properties = %#v", props)
	}
}

// Attached emails and links to files cannot go into sendMail. maildo says
// so before anyone approves the send.
func TestValidateDraftSendRefusesAttachmentsSendMailCannotCarry(t *testing.T) {
	srv, _ := draftSendServer(t, []map[string]any{{"@odata.type": "#microsoft.graph.itemAttachment", "name": "forwarded.eml"}}, nil)
	_, err := testReplyClient(srv).ValidateDraftSend(context.Background(), mail.SendDraftRequest{DraftID: "d1"})
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeUnsupportedAttachment || !strings.Contains(err.Error(), "forwarded.eml") {
		t.Fatalf("err = %v", err)
	}
}

// More than 3 MB of attachments does not fit into one sendMail request.
func TestValidateDraftSendRefusesTooMuchAttachmentData(t *testing.T) {
	big := make([]byte, maxSendAttachmentBytes/2+1)
	srv, _ := draftSendServer(t, []map[string]any{fileAttachment("a.bin", big, false), fileAttachment("b.bin", big, false)}, nil)
	_, err := testReplyClient(srv).ValidateDraftSend(context.Background(), mail.SendDraftRequest{DraftID: "d1"})
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeAttachmentTooLarge || !strings.Contains(err.Error(), "3 MB") {
		t.Fatalf("err = %v", err)
	}
}

// maildo send --attach works on Outlook up to 3 MB and says what to do
// above that.
func TestDirectSendCarriesAttachments(t *testing.T) {
	identity := provider.ComposeIdentity{Account: "work", Provider: "outlook", Email: "work@example.test"}
	msg := mail.OutboundMessage{
		To: []mail.Recipient{{Address: "clara@example.test"}}, Subject: "Invoice",
		Body:        mail.OutboundBody{Format: mail.BodyFormatText, ContentType: "text", Text: "Attached.", Present: true},
		Attachments: []mail.AttachmentUpload{{Name: "invoice.pdf", ContentType: "application/pdf", Size: 3, Bytes: []byte("PDF")}},
	}
	prepared, err := DirectSendPreparer{}.PrepareDirectSend(context.Background(), identity, msg)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()
	srv, sent := draftSendServer(t, nil, nil)
	if _, err := testReplyClient(srv).SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared}); err != nil {
		t.Fatal(err)
	}
	attachments := sent()["message"].(map[string]any)["attachments"].([]any)
	if len(attachments) != 1 || attachments[0].(map[string]any)["contentBytes"] != base64.StdEncoding.EncodeToString([]byte("PDF")) {
		t.Fatalf("sent attachments = %#v", attachments)
	}

	msg.Attachments = []mail.AttachmentUpload{{Name: "huge.bin", Bytes: make([]byte, maxSendAttachmentBytes+1)}}
	_, err = DirectSendPreparer{}.PrepareDirectSend(context.Background(), identity, msg)
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeAttachmentTooLarge {
		t.Fatalf("err = %v", err)
	}
	if details, _ := product.ErrorDetails().(map[string]any); !strings.Contains(details["hint"].(string), "from Outlook itself") {
		t.Fatalf("details = %#v", product.ErrorDetails())
	}
}
