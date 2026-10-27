package imap

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	smtpclient "github.com/emersion/go-smtp"
	"github.com/vincentsch/maildo/internal/auth"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
)

func TestCapabilities(t *testing.T) {
	for _, name := range []string{"imap", "fastmail"} {
		t.Run(name, func(t *testing.T) {
			caps, err := NewProvider(name, Options{}).Capabilities(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(caps) != len(provider.AllCapabilities()) {
				t.Fatalf("capabilities = %d, want %d", len(caps), len(provider.AllCapabilities()))
			}
			unsupported := map[provider.Capability]bool{
				provider.CapCategoriesRead:      true,
				provider.CapCategoriesMutate:    true,
				provider.CapRulesRead:           true,
				provider.CapRulesMutate:         true,
				provider.CapArchivePolicyReport: true,
			}
			implemented := map[provider.Capability]bool{
				provider.CapFoldersRead:        true,
				provider.CapFoldersMutate:      true,
				provider.CapMessagesSearch:     true,
				provider.CapMessagesList:       true,
				provider.CapMessagesRead:       true,
				provider.CapThreadsRead:        true,
				provider.CapAttachmentsRead:    true,
				provider.CapMessagesMove:       true,
				provider.CapMessagesCopy:       true,
				provider.CapMessagesArchive:    true,
				provider.CapMessagesDelete:     true,
				provider.CapMessagesRestore:    true,
				provider.CapMessagesJunk:       true,
				provider.CapMessagesMarkRead:   true,
				provider.CapMessagesFlag:       true,
				provider.CapSendDirect:         true,
				provider.CapDraftCreate:        true,
				provider.CapDraftRead:          true,
				provider.CapDraftUpdate:        true,
				provider.CapDraftSend:          true,
				provider.CapReplyDraftCreate:   true,
				provider.CapForwardDraftCreate: true,
				provider.CapAttachmentsMutate:  true,
			}
			for _, cap := range provider.AllCapabilities() {
				want := provider.CapSupportedNotImpl
				if unsupported[cap] {
					want = provider.CapUnsupported
				}
				if implemented[cap] {
					want = provider.CapImplemented
				}
				if got := caps.State(cap); got != want {
					t.Fatalf("%s = %s, want %s", cap, got, want)
				}
			}
		})
	}
}

func TestSetupParserGenericAndFastmail(t *testing.T) {
	parser := SetupParser{}
	res, err := parser.Resolve(context.Background(), provider.SetupResolveContext{
		Provider: "imap",
		Flags: setupFlags{
			"email":     {Changed: true, Value: "Me@Exämple.COM"},
			"imap-host": {Changed: true, Value: "IMAP.Example.COM"},
			"imap-port": {Changed: true, Value: "993"},
			"imap-tls":  {Changed: true, Value: "implicit"},
			"smtp-host": {Changed: true, Value: "2001:db8::1"},
			"smtp-port": {Changed: true, Value: "465"},
			"smtp-tls":  {Changed: true, Value: "starttls"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	setup := res.Setup.MailServer
	if setup == nil || setup.Email != "Me@xn--exmple-cua.com" || setup.Username != setup.Email ||
		setup.IMAP.Host != "imap.example.com" || setup.SMTP.Host != "2001:db8::1" ||
		setup.IMAP.Port != 993 || setup.SMTP.TLS != maildoconfig.TLSStartTLS {
		t.Fatalf("generic setup = %#v", setup)
	}
	if got := previewNames(res.Preview); !reflect.DeepEqual(got, []string{"email", "username", "imap_host", "imap_port", "imap_tls", "smtp_host", "smtp_port", "smtp_tls", "ca_file"}) {
		t.Fatalf("preview fields = %#v", got)
	}

	fastmail, err := parser.Resolve(context.Background(), provider.SetupResolveContext{
		Provider: "fastmail",
		Flags: setupFlags{
			"email":    {Changed: true, Value: "Me@Fastmail.COM"},
			"username": {Changed: true, Value: "me@fastmail.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fastmail.Setup.MailServer; got == nil || got.Email != "Me@fastmail.com" || got.Username != got.Email ||
		got.IMAP != maildoconfig.FastmailIMAPEndpoint() || got.SMTP != maildoconfig.FastmailSMTPEndpoint() {
		t.Fatalf("fastmail setup = %#v", got)
	}
	_, err = parser.Resolve(context.Background(), provider.SetupResolveContext{Provider: "fastmail", Flags: setupFlags{"imap-host": {Changed: true, Value: "imap.example.test"}}})
	if err == nil {
		t.Fatal("Fastmail endpoint override accepted")
	}
	_, err = parser.Resolve(context.Background(), provider.SetupResolveContext{Provider: "fastmail", Flags: setupFlags{"email": {Changed: true, Value: "me@fastmail.com"}, "username": {Changed: true, Value: "alias@fastmail.com"}}})
	if err == nil {
		t.Fatal("Fastmail mismatched username accepted")
	}
}

func TestCredentialCodecClosedShapeAndHealth(t *testing.T) {
	codec := NewCredentialCodec("imap")
	binding := auth.CredentialBinding{Provider: "imap", Account: "work"}
	cache, err := newPasswordCredential("imap", "work", "me@example.com", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := codec.Encode(binding, cache)
	if err != nil {
		t.Fatal(err)
	}
	reg := &secretRecorder{}
	decoded, err := codec.Decode(reg, binding, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Destroy()
	if !reflect.DeepEqual(reg.values, []string{"secret"}) {
		t.Fatalf("registered secrets = %#v", reg.values)
	}
	status, err := codec.LocalHealth(maildoconfig.ProviderSetup{MailServer: &maildoconfig.MailServerSetup{Email: "me@example.com", Username: "me@example.com", IMAP: maildoconfig.ServerEndpoint{Host: "imap.example.com", Port: 993, TLS: maildoconfig.TLSImplicit}, SMTP: maildoconfig.ServerEndpoint{Host: "smtp.example.com", Port: 465, TLS: maildoconfig.TLSImplicit}}}, binding, decoded)
	if err != nil || status != provider.LocalUsable {
		t.Fatalf("LocalHealth = %v, %v", status, err)
	}
	badInputs := [][]byte{
		[]byte(`{"version":1,"version":1,"provider":"imap","account_name":"work","configured_email":"me@example.com","password":"secret"}`),
		[]byte(`{"version":1,"provider":"imap","account_name":"work","configured_email":"me@example.com","password":"secret","future":true}`),
		[]byte(`{"version":1,"provider":"imap","account_name":"work","configured_email":"me@example.com","password":"\ud800"}`),
		[]byte(`{"version":1,"provider":"imap","account_name":"work","configured_email":"me@example.com","password":""}`),
	}
	for _, raw := range badInputs {
		if _, err := codec.Decode(&secretRecorder{}, binding, raw); !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
			t.Fatalf("Decode(%s) err = %v", raw, err)
		}
	}
	rotated, err := codec.RefreshTokenRotated(binding, decoded, decoded)
	if err != nil || rotated {
		t.Fatalf("RefreshTokenRotated = %t, %v", rotated, err)
	}
}

func TestCredentialCodecRegistersPasswordBeforeStrictShapeRejection(t *testing.T) {
	codec := NewCredentialCodec("imap")
	binding := auth.CredentialBinding{Provider: "imap", Account: "work"}
	raw := []byte(`{"version":1,"provider":"imap","account_name":"work","configured_email":"me@example.com","password":"secret","future":true}`)
	reg := &secretRecorder{}

	if _, err := codec.Decode(reg, binding, raw); !errors.Is(err, provider.ErrCredentialCapabilityClosed) {
		t.Fatalf("Decode err = %v", err)
	}
	if !reflect.DeepEqual(reg.values, []string{"secret"}) {
		t.Fatalf("registered secrets = %#v", reg.values)
	}
	if !allZero(raw) {
		t.Fatalf("Decode did not zero opaque input: %#v", raw)
	}
}

func TestInteractiveAuthVerifiesIMAPThenSMTPWithoutSubmission(t *testing.T) {
	runtime := &interactiveRuntime{env: map[string]string{envPassword: "app-password"}}
	smtpCommandsCh := make(chan []string, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t,
			func(conn net.Conn) { serveIMAPImplicit(t, conn) },
			func(conn net.Conn) { smtpCommandsCh <- serveSMTPImplicit(t, conn, 235) },
		),
		TLSConfig: insecureTLSConfig,
	})
	result, err := p.InteractiveAuth(context.Background(), provider.InteractiveAuthContext{
		Runtime:   runtime,
		Operation: provider.InteractiveLogin,
		Provider:  "imap",
		Account:   "work",
		Setup:     maildoconfig.ProviderSetup{MailServer: validSetup()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Credential.Destroy()
	if result.Identity.Email != "me@example.com" || result.Identity.Assurance != provider.IdentityConfigured {
		t.Fatalf("identity = %#v", result.Identity)
	}
	smtpCommands := waitCommands(t, smtpCommandsCh)
	for _, command := range smtpCommands {
		upper := strings.ToUpper(command)
		if strings.HasPrefix(upper, "MAIL ") || strings.HasPrefix(upper, "RCPT ") || strings.HasPrefix(upper, "DATA") || strings.HasPrefix(upper, "BDAT") {
			t.Fatalf("SMTP verification submitted mail command %q", command)
		}
	}
	if !contains(runtime.values, "app-password") {
		t.Fatalf("runtime did not register password: %#v", runtime.values)
	}
}

func TestSMTPDirectSendSubmitsPreparedMIMEAndReportsMetadata(t *testing.T) {
	transcripts := make(chan smtpSubmissionTranscript, 1)
	setup := *validSetup()
	client := &accountClient{
		provider: "imap",
		account:  "work",
		setup:    setup,
		password: []byte("app-password"),
		opts: Options{
			DialContext: queuedDialer(t, func(conn net.Conn) {
				transcripts <- serveSMTPSubmissionSession(t, conn, true)
			}, refuseSentCopy),
			TLSConfig: insecureTLSConfig,
		},
		done: make(chan struct{}),
	}
	defer client.Destroy()

	prepared, err := NewDirectSendPreparer("imap").PrepareDirectSend(context.Background(), provider.ComposeIdentity{
		Account:     "work",
		Provider:    "imap",
		Email:       "me@example.com",
		DisplayName: "Me",
	}, smtpOutboundTextMessage())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()

	outcome, err := client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.MessageID != "" || outcome.WriteOutcomeFacts == nil || outcome.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded ||
		!outcome.WriteOutcomeFacts.AcceptedByProvider || outcome.WriteOutcomeFacts.RetrySafe {
		t.Fatalf("direct send outcome = %#v", outcome)
	}
	if outcome.ProviderMeta == nil || !outcome.ProviderMeta.AcceptanceKnown || outcome.ProviderMeta.ServerMessageIDPresent || outcome.ProviderMeta.SMTPUTF8Used {
		t.Fatalf("direct send metadata = %#v", outcome.ProviderMeta)
	}
	transcript := waitSMTPSubmission(t, transcripts)
	mailCommand, ok := commandWithPrefix(transcript.commands, "MAIL ")
	if !ok {
		t.Fatalf("SMTP commands missing MAIL = %#v", transcript.commands)
	}
	if !strings.HasPrefix(mailCommand, "MAIL FROM:<me@example.com> BODY=8BITMIME SIZE=") ||
		strings.Contains(mailCommand, "SMTPUTF8") ||
		!containsCommandPrefix(transcript.commands, "RCPT TO:<to@example.net>") ||
		!containsCommandPrefix(transcript.commands, "RCPT TO:<copy@example.net>") ||
		!containsCommandPrefix(transcript.commands, "RCPT TO:<hidden@example.net>") ||
		!containsCommandPrefix(transcript.commands, "DATA") {
		t.Fatalf("SMTP commands = %#v", transcript.commands)
	}
	assertMAILSizeEqualsSubmittedData(t, mailCommand, transcript.data)
	if strings.Contains(strings.ToLower(transcript.data), "\r\nbcc:") ||
		!strings.Contains(transcript.data, "Subject: SMTP Subject") ||
		!strings.Contains(transcript.data, "Hello via SMTP") {
		t.Fatalf("submitted MIME = %q", transcript.data)
	}
}

func TestSMTPDirectSendRequiresSMTPUTF8BeforeDATA(t *testing.T) {
	transcripts := make(chan smtpSubmissionTranscript, 1)
	setup := *validSetup()
	client := &accountClient{
		provider: "imap",
		account:  "work",
		setup:    setup,
		password: []byte("app-password"),
		opts: Options{
			DialContext: queuedDialer(t, func(conn net.Conn) {
				transcripts <- serveSMTPSubmissionSession(t, conn, false)
			}),
			TLSConfig: insecureTLSConfig,
		},
		done: make(chan struct{}),
	}
	defer client.Destroy()

	msg := smtpOutboundTextMessage()
	msg.To = []mail.Recipient{{Address: "søren@example.net"}}
	prepared, err := NewDirectSendPreparer("imap").PrepareDirectSend(context.Background(), provider.ComposeIdentity{
		Account:  "work",
		Provider: "imap",
		Email:    "me@example.com",
	}, msg)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()

	_, err = client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
	if err == nil {
		t.Fatal("SendMessage succeeded without SMTPUTF8")
	}
	assertProductCode(t, err, errs.CodeUnsupportedCapability)
	assertProductReason(t, err, "smtputf8_required")
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeNotApplied || !facts.RetrySafe || facts.WriteMayHaveLanded {
		t.Fatalf("write facts = %#v, ok=%t, err=%v", facts, ok, err)
	}
	transcript := waitSMTPSubmission(t, transcripts)
	for _, command := range transcript.commands {
		upper := strings.ToUpper(command)
		if strings.HasPrefix(upper, "MAIL ") || strings.HasPrefix(upper, "RCPT ") || strings.HasPrefix(upper, "DATA") {
			t.Fatalf("SMTPUTF8 failure crossed submission command %q; commands=%#v", command, transcript.commands)
		}
	}
}

func TestSMTPDirectSendUsesCanonicalUTF8MailParameters(t *testing.T) {
	transcripts := make(chan smtpSubmissionTranscript, 1)
	setup := *validSetup()
	client := &accountClient{
		provider: "imap",
		account:  "work",
		setup:    setup,
		password: []byte("app-password"),
		opts: Options{
			DialContext: queuedDialer(t, func(conn net.Conn) {
				transcripts <- serveSMTPSubmissionSession(t, conn, true)
			}, refuseSentCopy),
			TLSConfig: insecureTLSConfig,
		},
		done: make(chan struct{}),
	}
	defer client.Destroy()

	msg := smtpOutboundTextMessage()
	msg.To = []mail.Recipient{{Address: "søren@example.net"}}
	prepared, err := NewDirectSendPreparer("imap").PrepareDirectSend(context.Background(), provider.ComposeIdentity{
		Account:  "work",
		Provider: "imap",
		Email:    "me@example.com",
	}, msg)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()

	if _, err := client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared}); err != nil {
		t.Fatal(err)
	}
	transcript := waitSMTPSubmission(t, transcripts)
	mailCommand, ok := commandWithPrefix(transcript.commands, "MAIL ")
	if !ok {
		t.Fatalf("SMTP commands missing MAIL = %#v", transcript.commands)
	}
	if !strings.HasPrefix(mailCommand, "MAIL FROM:<me@example.com> BODY=8BITMIME SIZE=") || !strings.HasSuffix(mailCommand, " SMTPUTF8") {
		t.Fatalf("MAIL command = %q", mailCommand)
	}
	assertMAILSizeEqualsSubmittedData(t, mailCommand, transcript.data)
}

func TestSMTPDirectSendAcceptsFinal251(t *testing.T) {
	transcripts := make(chan smtpSubmissionTranscript, 1)
	setup := *validSetup()
	client := &accountClient{
		provider: "imap",
		account:  "work",
		setup:    setup,
		password: []byte("app-password"),
		opts: Options{
			DialContext: queuedDialer(t, func(conn net.Conn) {
				transcripts <- serveSMTPSubmissionScenario(t, conn, smtpSubmissionOptions{finalReply: "251 2.0.0 accepted elsewhere"})
			}, refuseSentCopy),
			TLSConfig: insecureTLSConfig,
		},
		done: make(chan struct{}),
	}
	defer client.Destroy()

	prepared, err := NewDirectSendPreparer("imap").PrepareDirectSend(context.Background(), provider.ComposeIdentity{
		Account:  "work",
		Provider: "imap",
		Email:    "me@example.com",
	}, smtpOutboundTextMessage())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()

	outcome, err := client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.WriteOutcomeFacts == nil || outcome.WriteOutcomeFacts.Outcome != mail.WriteOutcomeSucceeded {
		t.Fatalf("outcome = %#v", outcome)
	}
	_ = waitSMTPSubmission(t, transcripts)
}

func TestSMTPDirectSendRejectsOversizedAdvertisedSizeBeforeMAIL(t *testing.T) {
	transcripts := make(chan smtpSubmissionTranscript, 1)
	setup := *validSetup()
	client := &accountClient{
		provider: "imap",
		account:  "work",
		setup:    setup,
		password: []byte("app-password"),
		opts: Options{
			DialContext: queuedDialer(t, func(conn net.Conn) {
				transcripts <- serveSMTPSubmissionScenario(t, conn, smtpSubmissionOptions{sizeLine: "250 SIZE 128"})
			}),
			TLSConfig: insecureTLSConfig,
		},
		done: make(chan struct{}),
	}
	defer client.Destroy()

	prepared, err := NewDirectSendPreparer("imap").PrepareDirectSend(context.Background(), provider.ComposeIdentity{
		Account:  "work",
		Provider: "imap",
		Email:    "me@example.com",
	}, smtpOutboundTextMessage())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()

	_, err = client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
	assertProductCode(t, err, errs.CodeMessageTooLarge)
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeNotApplied || facts.WriteMayHaveLanded {
		t.Fatalf("write facts = %#v, ok=%t, err=%v", facts, ok, err)
	}
	transcript := waitSMTPSubmission(t, transcripts)
	for _, command := range transcript.commands {
		if strings.HasPrefix(strings.ToUpper(command), "MAIL ") {
			t.Fatalf("oversized SMTP transaction issued MAIL: %#v", transcript.commands)
		}
	}
}

func TestSMTPDirectSendClassifiesRecipientRepliesAndResets(t *testing.T) {
	for _, tt := range []struct {
		name          string
		rcptReplies   []string
		code          string
		reason        string
		outcome       errs.WriteOutcome
		rejectedIndex int
		acceptedCount int
	}{
		{
			name:          "temporary first recipient",
			rcptReplies:   []string{"450 4.2.0 mailbox busy"},
			code:          errs.CodeProviderConnectionError,
			reason:        "recipient_temporarily_rejected",
			outcome:       errs.WriteOutcomeNotApplied,
			rejectedIndex: 1,
			acceptedCount: 0,
		},
		{
			name:          "permanent second recipient",
			rcptReplies:   []string{"250 2.1.5 ok", "550 5.1.1 no such user"},
			code:          errs.CodeRecipientRejected,
			outcome:       errs.WriteOutcomeRejected,
			rejectedIndex: 2,
			acceptedCount: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transcripts := make(chan smtpSubmissionTranscript, 1)
			setup := *validSetup()
			client := &accountClient{
				provider: "imap",
				account:  "work",
				setup:    setup,
				password: []byte("app-password"),
				opts: Options{
					DialContext: queuedDialer(t, func(conn net.Conn) {
						transcripts <- serveSMTPSubmissionScenario(t, conn, smtpSubmissionOptions{rcptReplies: tt.rcptReplies})
					}),
					TLSConfig: insecureTLSConfig,
				},
				done: make(chan struct{}),
			}
			defer client.Destroy()

			prepared, err := NewDirectSendPreparer("imap").PrepareDirectSend(context.Background(), provider.ComposeIdentity{
				Account:  "work",
				Provider: "imap",
				Email:    "me@example.com",
			}, smtpOutboundTextMessage())
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Destroy()

			_, err = client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
			assertProductCode(t, err, tt.code)
			if tt.reason != "" {
				assertProductReason(t, err, tt.reason)
			}
			details := productErrorDetails(t, err)
			if details["rejected_index"] != tt.rejectedIndex || details["accepted_count"] != tt.acceptedCount || details["total_count"] != 3 {
				t.Fatalf("recipient details = %#v", details)
			}
			facts, ok := errs.WriteOutcomeFactsFromError(err)
			if !ok || facts.Outcome != tt.outcome {
				t.Fatalf("write facts = %#v, ok=%t, err=%v", facts, ok, err)
			}
			transcript := waitSMTPSubmission(t, transcripts)
			if !containsCommandPrefix(transcript.commands, "RSET") {
				t.Fatalf("recipient failure did not RSET: %#v", transcript.commands)
			}
			if containsCommandPrefix(transcript.commands, "DATA") {
				t.Fatalf("recipient failure reached DATA: %#v", transcript.commands)
			}
		})
	}
}

func TestSMTPDirectSendFinalInvalidReplyIsUnknown(t *testing.T) {
	transcripts := make(chan smtpSubmissionTranscript, 1)
	setup := *validSetup()
	client := &accountClient{
		provider: "imap",
		account:  "work",
		setup:    setup,
		password: []byte("app-password"),
		opts: Options{
			DialContext: queuedDialer(t, func(conn net.Conn) {
				transcripts <- serveSMTPSubmissionScenario(t, conn, smtpSubmissionOptions{finalReply: "350 unexpected continuation"})
			}),
			TLSConfig: insecureTLSConfig,
		},
		done: make(chan struct{}),
	}
	defer client.Destroy()

	prepared, err := NewDirectSendPreparer("imap").PrepareDirectSend(context.Background(), provider.ComposeIdentity{
		Account:  "work",
		Provider: "imap",
		Email:    "me@example.com",
	}, smtpOutboundTextMessage())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Destroy()

	_, err = client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
	assertProductCode(t, err, errs.CodeVerificationFailed)
	assertProductReason(t, err, string(errs.OutcomeAcceptanceResponseLost))
	facts, ok := errs.WriteOutcomeFactsFromError(err)
	if !ok || facts.Outcome != errs.WriteOutcomeUnknown || facts.RetrySafe || facts.AcceptedByProvider {
		t.Fatalf("write facts = %#v, ok=%t, err=%v", facts, ok, err)
	}
	_ = waitSMTPSubmission(t, transcripts)
}

func TestSMTPDirectSendLostAcceptanceCannotResubmitCarrier(t *testing.T) {
	for _, providerName := range []string{"imap", "fastmail"} {
		for _, scenario := range []struct {
			name string
			opts smtpSubmissionOptions
		}{
			{"disconnect after complete DATA", smtpSubmissionOptions{disconnectAfterDATA: true}},
			{"ambiguous final response", smtpSubmissionOptions{finalReply: "350 unexpected continuation"}},
		} {
			t.Run(providerName+"/"+scenario.name, func(t *testing.T) {
				transcripts := make(chan smtpSubmissionTranscript, 1)
				dials := 0
				dial := queuedDialer(t, func(conn net.Conn) {
					transcripts <- serveSMTPSubmissionScenario(t, conn, scenario.opts)
				})
				client := &accountClient{
					provider: providerName, account: "work", setup: *validSetup(), password: []byte("app-password"),
					opts: Options{
						DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
							dials++
							return dial(ctx, network, address)
						},
						TLSConfig: insecureTLSConfig,
					},
					done: make(chan struct{}),
				}
				defer client.Destroy()
				prepared, err := NewDirectSendPreparer(providerName).PrepareDirectSend(context.Background(), provider.ComposeIdentity{
					Account: "work", Provider: providerName, Email: "me@example.com",
				}, smtpOutboundTextMessage())
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.Destroy()
				outcome, err := client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
				assertProductCode(t, err, errs.CodeVerificationFailed)
				facts, ok := errs.WriteOutcomeFactsFromError(err)
				if !ok || facts.Outcome != errs.WriteOutcomeUnknown || facts.RetrySafe || !facts.WriteMayHaveLanded || facts.AcceptedByProvider || facts.Succeeded || facts.ConfirmedAfterWrite {
					t.Fatalf("lost acceptance facts = %#v, ok=%t", facts, ok)
				}
				if outcome.WriteOutcomeFacts != nil || outcome.ProviderMeta != nil || outcome.MessageID != "" {
					t.Fatalf("lost acceptance returned success data: %#v", outcome)
				}
				transcript := waitSMTPSubmission(t, transcripts)
				if !containsCommandPrefix(transcript.commands, "DATA") || transcript.data == "" {
					t.Fatal("fixture did not observe transmitted message")
				}
				_, retryErr := client.SendMessage(context.Background(), mail.SendMessageRequest{Prepared: prepared})
				if retryErr == nil || dials != 1 {
					t.Fatalf("carrier resubmission err=%v, dials=%d", retryErr, dials)
				}
			})
		}
	}
}

func TestInteractiveAuthRejectsLineBreakingPasswordBeforeDial(t *testing.T) {
	dialed := false
	p := NewProvider("imap", Options{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected dial")
		},
		TLSConfig: insecureTLSConfig,
	})
	_, err := p.InteractiveAuth(context.Background(), provider.InteractiveAuthContext{
		Runtime:   &interactiveRuntime{env: map[string]string{envPassword: "app\r\nNOOP"}},
		Operation: provider.InteractiveLogin,
		Provider:  "imap",
		Account:   "work",
		Setup:     maildoconfig.ProviderSetup{MailServer: validSetup()},
	})
	if err == nil {
		t.Fatal("InteractiveAuth succeeded with line-breaking password")
	}
	if dialed {
		t.Fatal("provider dialed before rejecting line-breaking password")
	}
}

func TestInteractiveAuthUsesExplicitEnvironmentPasswordBeforeCache(t *testing.T) {
	codec := NewCredentialCodec("imap")
	binding := auth.CredentialBinding{Provider: "imap", Account: "work"}
	cache, err := newPasswordCredential("imap", "work", "me@example.com", []byte("cached-password"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := codec.Encode(binding, cache)
	cache.Destroy()
	if err != nil {
		t.Fatal(err)
	}
	defer zeroBytes(raw)

	source := &recordingCredentialSource{present: true, data: raw}
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t,
			func(conn net.Conn) { serveIMAPImplicit(t, conn) },
			func(conn net.Conn) { _ = serveSMTPImplicit(t, conn, 235) },
		),
		TLSConfig: insecureTLSConfig,
	})
	result, err := p.InteractiveAuth(context.Background(), provider.InteractiveAuthContext{
		Runtime:   &interactiveRuntime{env: map[string]string{envPassword: "env-password"}},
		Operation: provider.InteractiveLogin,
		Provider:  "imap",
		Account:   "work",
		Setup:     maildoconfig.ProviderSetup{MailServer: validSetup()},
		Existing:  source,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Credential.Destroy()
	if source.calls != 0 {
		t.Fatalf("existing credential source calls = %d, want 0", source.calls)
	}
}

func TestInteractiveAuthCredentialRejectionMapping(t *testing.T) {
	p := NewProvider("fastmail", Options{
		DialContext: queuedDialer(t,
			func(conn net.Conn) { serveIMAPImplicit(t, conn) },
			func(conn net.Conn) { _ = serveSMTPImplicit(t, conn, 535) },
		),
		TLSConfig: insecureTLSConfig,
	})
	_, err := p.InteractiveAuth(context.Background(), provider.InteractiveAuthContext{
		Runtime:   &interactiveRuntime{env: map[string]string{envPassword: "bad-password"}},
		Operation: provider.InteractiveLogin,
		Provider:  "fastmail",
		Account:   "personal",
		Setup:     maildoconfig.ProviderSetup{MailServer: fastmailSetup()},
	})
	assertProductCode(t, err, errs.CodeProviderIdentityUnavailable)
}

func TestExistingInteractiveAuthCredentialRejectionNeedsRepair(t *testing.T) {
	p := NewProvider("fastmail", Options{
		DialContext: queuedDialer(t,
			func(conn net.Conn) { serveIMAPImplicit(t, conn) },
			func(conn net.Conn) { _ = serveSMTPImplicit(t, conn, 535) },
		),
		TLSConfig: insecureTLSConfig,
	})
	_, err := p.InteractiveAuth(context.Background(), provider.InteractiveAuthContext{
		Runtime:   &interactiveRuntime{env: map[string]string{envPassword: "bad-password"}},
		Operation: provider.InteractiveLogin,
		Provider:  "fastmail",
		Account:   "personal",
		Setup:     maildoconfig.ProviderSetup{MailServer: fastmailSetup()},
		Existing:  &recordingCredentialSource{},
	})
	assertProductCode(t, err, errs.CodeAuthRepairRequired)
}

// Once the SMTP login worked, a server that answers QUIT badly does not make
// the login fail.
func TestSMTPVerificationIgnoresFailedQuit(t *testing.T) {
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t,
			func(conn net.Conn) { serveIMAPImplicit(t, conn) },
			func(conn net.Conn) { _ = serveSMTPImplicitWithQuitCode(t, conn, 235, 500) },
		),
		TLSConfig: insecureTLSConfig,
	})
	_, err := p.InteractiveAuth(context.Background(), provider.InteractiveAuthContext{
		Runtime:   &interactiveRuntime{env: map[string]string{envPassword: "app-password"}},
		Operation: provider.InteractiveLogin,
		Provider:  "imap",
		Account:   "work",
		Setup:     maildoconfig.ProviderSetup{MailServer: validSetup()},
	})
	if err != nil {
		t.Fatalf("login failed because of QUIT: %v", err)
	}
}

func TestAccountClientSessionFactoriesProvideAuthenticatedClients(t *testing.T) {
	smtpCommandsCh := make(chan []string, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t,
			func(conn net.Conn) { serveIMAPDependencySession(t, conn) },
			func(conn net.Conn) { smtpCommandsCh <- serveSMTPDependencySession(t, conn) },
		),
		TLSConfig: insecureTLSConfig,
	})
	cred, err := newPasswordCredential("imap", "work", "me@example.com", []byte("app-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer cred.Destroy()
	rawClient, err := p.NewAccountClient(context.Background(), provider.StoredAuthContext{
		Provider:   "imap",
		Account:    "work",
		Setup:      maildoconfig.ProviderSetup{MailServer: validSetup()},
		Credential: testCredentialSession{current: cred},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rawClient.Destroy()
	client := rawClient.(*accountClient)

	sawIMAP := false
	if err := client.withIMAPSession(context.Background(), func(session *imapSession) error {
		return session.withCommand(context.Background(), func(dep *imapclient.Client) error {
			if dep == nil {
				t.Fatal("nil IMAP dependency client")
			}
			sawIMAP = true
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if !sawIMAP {
		t.Fatal("IMAP callback did not run")
	}

	sawSMTP := false
	if err := client.withSMTPSession(context.Background(), func(session *smtpSession) (smtpSessionOutcome, error) {
		err := session.withCommand(context.Background(), func(dep *smtpclient.Client) error {
			if dep == nil {
				t.Fatal("nil SMTP dependency client")
			}
			if ok, _ := dep.Extension("AUTH"); !ok {
				t.Fatal("SMTP AUTH extension missing after authentication")
			}
			sawSMTP = true
			return nil
		})
		return smtpSessionOutcomeVerification, err
	}); err != nil {
		var product *errs.Error
		if errors.As(err, &product) {
			t.Fatalf("smtp session: code=%s details=%v commands=%v", product.Code(), product.Details(), waitCommands(t, smtpCommandsCh))
		}
		t.Fatalf("smtp session: %#v", err)
	}
	if !sawSMTP {
		t.Fatal("SMTP callback did not run")
	}
	smtpCommands := waitCommands(t, smtpCommandsCh)
	for _, command := range smtpCommands {
		upper := strings.ToUpper(command)
		if strings.HasPrefix(upper, "MAIL ") || strings.HasPrefix(upper, "RCPT ") || strings.HasPrefix(upper, "DATA") || strings.HasPrefix(upper, "BDAT") {
			t.Fatalf("SMTP session factory submitted mail command %q", command)
		}
	}
}

func TestAccountClientSessionCleansUpAfterPanic(t *testing.T) {
	imapDone := make(chan struct{}, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPDependencySession(t, conn)
			imapDone <- struct{}{}
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = client.withIMAPSession(context.Background(), func(*imapSession) error {
			panic("callback panic")
		})
	}()
	if recovered != "callback panic" {
		t.Fatalf("recovered = %#v", recovered)
	}
	select {
	case <-imapDone:
	case <-time.After(5 * time.Second):
		t.Fatal("IMAP session did not log out after panic")
	}
}

func TestAccountClientDestroyCancelsBlockedCommand(t *testing.T) {
	selectStarted := make(chan struct{}, 1)
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPBlockedSelectSession(t, conn, selectStarted)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)

	sessionDone := make(chan error, 1)
	go func() {
		sessionDone <- client.withIMAPSession(context.Background(), func(session *imapSession) error {
			return session.withCommand(context.Background(), func(dep *imapclient.Client) error {
				_, err := dep.Select("INBOX", nil).Wait()
				return err
			})
		})
	}()
	select {
	case <-selectStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked command did not start")
	}

	destroyDone := make(chan struct{}, 1)
	go func() {
		client.Destroy()
		destroyDone <- struct{}{}
	}()
	select {
	case <-destroyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Destroy did not cancel blocked command")
	}
	select {
	case err := <-sessionDone:
		if err == nil {
			t.Fatal("blocked command returned nil error after Destroy")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not finish after Destroy")
	}
}

func TestAccountClientIMAPDependencyLiteralLimit(t *testing.T) {
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveIMAPLiteralLimitSession(t, conn)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	err := client.withIMAPSession(context.Background(), func(session *imapSession) error {
		return session.withCommand(context.Background(), func(dep *imapclient.Client) error {
			_, err := dep.Select("INBOX", nil).Wait()
			return err
		})
	})
	assertProductCode(t, err, errs.CodeProviderDataError)
	assertProductReason(t, err, string(errs.ProviderDataReasonLiteralLimitExceeded))
}

func TestAccountClientSMTPDependencyReplyLimit(t *testing.T) {
	p := NewProvider("imap", Options{
		DialContext: queuedDialer(t, func(conn net.Conn) {
			serveSMTPReplyLimitSession(t, conn)
		}),
		TLSConfig: insecureTLSConfig,
	})
	client := newTestAccountClient(t, p)
	defer client.Destroy()

	err := client.withSMTPSession(context.Background(), func(session *smtpSession) (smtpSessionOutcome, error) {
		return smtpSessionOutcomeVerification, session.withCommand(context.Background(), func(dep *smtpclient.Client) error {
			return dep.Noop()
		})
	})
	assertProductCode(t, err, errs.CodeProviderDataError)
	assertProductReason(t, err, string(errs.ProviderDataReasonLineLimitExceeded))
}

type setupFlags map[string]provider.SetupFlagValue

func (f setupFlags) Lookup(name string) (provider.SetupFlagValue, bool) {
	v, ok := f[name]
	return v, ok
}

func previewNames(fields []provider.SetupPreviewField) []string {
	out := make([]string, len(fields))
	for i, field := range fields {
		out[i] = field.Name
	}
	return out
}

type secretRecorder struct{ values []string }

func (r *secretRecorder) RegisterSecret(value string) error {
	r.values = append(r.values, value)
	return nil
}

type recordingCredentialSource struct {
	present bool
	data    []byte
	calls   int
}

func (s *recordingCredentialSource) WithBytes(ctx context.Context, fn func([]byte) error) (provider.CredentialPresence, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return provider.CredentialAbsent, err
	}
	if !s.present {
		return provider.CredentialAbsent, nil
	}
	return provider.CredentialPresent, fn(append([]byte(nil), s.data...))
}

type interactiveRuntime struct {
	secretRecorder
	env map[string]string
}

func (r *interactiveRuntime) LookupSecretEnv(name string) (string, bool, error) {
	v, ok := r.env[name]
	return v, ok, nil
}

func (*interactiveRuntime) OpenBrowser(context.Context, string) error {
	return provider.ErrProviderRuntimeDenied
}
func (*interactiveRuntime) WriteNotice(string) error { return nil }

type testCredentialSession struct {
	current provider.Credential
}

func (s testCredentialSession) WithCurrent(ctx context.Context, fn func(provider.CredentialLease) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(testCredentialLease{current: s.current})
}

type testCredentialLease struct {
	current provider.Credential
}

func (l testCredentialLease) Current() provider.Credential {
	return l.current
}

func (testCredentialLease) Replace(provider.Credential) error {
	return provider.ErrCredentialCapabilityClosed
}

func validSetup() *maildoconfig.MailServerSetup {
	return &maildoconfig.MailServerSetup{
		Email:    "me@example.com",
		Username: "me@example.com",
		IMAP:     maildoconfig.ServerEndpoint{Host: "imap.example.com", Port: 993, TLS: maildoconfig.TLSImplicit},
		SMTP:     maildoconfig.ServerEndpoint{Host: "smtp.example.com", Port: 465, TLS: maildoconfig.TLSImplicit},
	}
}

func newTestAccountClient(t *testing.T, p *Provider) *accountClient {
	t.Helper()
	cred, err := newPasswordCredential("imap", "work", "me@example.com", []byte("app-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer cred.Destroy()
	rawClient, err := p.NewAccountClient(context.Background(), provider.StoredAuthContext{
		Provider:   "imap",
		Account:    "work",
		Setup:      maildoconfig.ProviderSetup{MailServer: validSetup()},
		Credential: testCredentialSession{current: cred},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := rawClient.(*accountClient)
	if !ok {
		rawClient.Destroy()
		t.Fatalf("client type = %T", rawClient)
	}
	return client
}

func fastmailSetup() *maildoconfig.MailServerSetup {
	return &maildoconfig.MailServerSetup{
		Email:    "me@fastmail.com",
		Username: "me@fastmail.com",
		IMAP:     maildoconfig.FastmailIMAPEndpoint(),
		SMTP:     maildoconfig.FastmailSMTPEndpoint(),
	}
}

func queuedDialer(t *testing.T, handlers ...func(net.Conn)) DialContextFunc {
	t.Helper()
	var mu sync.Mutex
	queue := append([]func(net.Conn){}, handlers...)
	return func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		if len(queue) == 0 {
			mu.Unlock()
			t.Fatal("unexpected dial")
		}
		handler := queue[0]
		queue = queue[1:]
		mu.Unlock()
		client, server := net.Pipe()
		go handler(server)
		return client, nil
	}
}

func waitCommands(t *testing.T, ch <-chan []string) []string {
	t.Helper()
	select {
	case commands := <-ch:
		return commands
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for protocol transcript")
		return nil
	}
}

type smtpSubmissionTranscript struct {
	commands []string
	data     string
}

type smtpSubmissionOptions struct {
	advertiseSMTPUTF8   bool
	advertise8BitMIME   bool
	sizeLine            string
	mailReply           string
	rcptReplies         []string
	dataReply           string
	finalReply          string
	disconnectAfterDATA bool
}

func waitSMTPSubmission(t *testing.T, ch <-chan smtpSubmissionTranscript) smtpSubmissionTranscript {
	t.Helper()
	select {
	case transcript := <-ch:
		return transcript
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SMTP submission transcript")
		return smtpSubmissionTranscript{}
	}
}

func serveSMTPSubmissionSession(t *testing.T, conn net.Conn, advertiseSMTPUTF8 bool) smtpSubmissionTranscript {
	t.Helper()
	return serveSMTPSubmissionScenario(t, conn, smtpSubmissionOptions{advertiseSMTPUTF8: advertiseSMTPUTF8})
}

func serveSMTPSubmissionScenario(t *testing.T, conn net.Conn, opts smtpSubmissionOptions) smtpSubmissionTranscript {
	t.Helper()
	defer conn.Close()
	if opts.sizeLine == "" {
		opts.sizeLine = "250 SIZE 104857600"
	}
	if opts.mailReply == "" {
		opts.mailReply = "250 2.1.0 ok"
	}
	if opts.dataReply == "" {
		opts.dataReply = "354 end with dot"
	}
	if opts.finalReply == "" {
		opts.finalReply = "250 2.0.0 accepted"
	}
	if !opts.advertise8BitMIME {
		opts.advertise8BitMIME = true
	}
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("smtp handshake: %v", err)
		return smtpSubmissionTranscript{}
	}
	r := bufio.NewReader(tlsConn)
	var transcript smtpSubmissionTranscript
	writeLine(t, tlsConn, "220 smtp.example.test")
	line := readLine(t, r)
	transcript.commands = append(transcript.commands, line)
	if line != "EHLO localhost" {
		t.Errorf("smtp ehlo = %q", line)
		return transcript
	}
	writeLine(t, tlsConn, "250-smtp.example.test")
	writeLine(t, tlsConn, "250-AUTH PLAIN")
	if opts.advertise8BitMIME {
		writeLine(t, tlsConn, "250-8BITMIME")
	}
	if opts.advertiseSMTPUTF8 {
		writeLine(t, tlsConn, "250-SMTPUTF8")
	}
	writeLine(t, tlsConn, opts.sizeLine)
	line = readLine(t, r)
	transcript.commands = append(transcript.commands, line)
	if !strings.HasPrefix(line, "AUTH PLAIN ") {
		t.Errorf("smtp auth = %q", line)
		return transcript
	}
	writeLine(t, tlsConn, "235 2.7.0 ok")
	rcptIndex := 0
	for {
		line = readLine(t, r)
		transcript.commands = append(transcript.commands, line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "MAIL "):
			writeLine(t, tlsConn, opts.mailReply)
		case strings.HasPrefix(upper, "RCPT "):
			reply := "250 2.1.5 ok"
			if rcptIndex < len(opts.rcptReplies) {
				reply = opts.rcptReplies[rcptIndex]
			}
			rcptIndex++
			writeLine(t, tlsConn, reply)
		case strings.HasPrefix(upper, "DATA"):
			writeLine(t, tlsConn, opts.dataReply)
			if !strings.HasPrefix(opts.dataReply, "354") {
				continue
			}
			var b strings.Builder
			for {
				dataLine := readLine(t, r)
				if dataLine == "." {
					break
				}
				b.WriteString(dataLine)
				b.WriteString("\r\n")
			}
			transcript.data = b.String()
			if opts.disconnectAfterDATA {
				return transcript
			}
			writeLine(t, tlsConn, opts.finalReply)
			return transcript
		case strings.HasPrefix(upper, "RSET"):
			writeLine(t, tlsConn, "250 2.0.0 reset")
			return transcript
		case strings.HasPrefix(upper, "QUIT"):
			writeLine(t, tlsConn, "221 bye")
			return transcript
		default:
			t.Errorf("unexpected smtp command = %q", line)
			return transcript
		}
	}
}

func containsCommandPrefix(commands []string, prefix string) bool {
	_, ok := commandWithPrefix(commands, prefix)
	return ok
}

func commandWithPrefix(commands []string, prefix string) (string, bool) {
	for _, command := range commands {
		if strings.HasPrefix(command, prefix) {
			return command, true
		}
	}
	return "", false
}

func assertMAILSizeEqualsSubmittedData(t *testing.T, command string, data string) {
	t.Helper()
	_, rawSize, ok := strings.Cut(command, " SIZE=")
	if !ok {
		t.Fatalf("MAIL command missing SIZE parameter: %q", command)
	}
	rawSize, _, _ = strings.Cut(rawSize, " ")
	size, err := strconv.Atoi(rawSize)
	if err != nil {
		t.Fatalf("MAIL command SIZE is not numeric: %q", command)
	}
	if got := len([]byte(data)); got != size {
		t.Fatalf("MAIL command SIZE = %d, submitted data bytes = %d", size, got)
	}
}

func productErrorDetails(t *testing.T, err error) map[string]any {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("err = %v, want product details", err)
	}
	return product.Details()
}

func smtpOutboundTextMessage() mail.OutboundMessage {
	return mail.OutboundMessage{
		To:      []mail.Recipient{{Address: "to@example.net"}},
		CC:      []mail.Recipient{{Address: "copy@example.net"}},
		BCC:     []mail.Recipient{{Address: "hidden@example.net"}},
		Subject: "SMTP Subject",
		Body: mail.OutboundBody{
			Format:      mail.BodyFormatText,
			ContentType: "text/plain",
			Text:        "Hello via SMTP",
			Bytes:       len("Hello via SMTP"),
			Present:     true,
		},
	}
}

// serveIMAPImplicit is a server for login checks: it accepts the login and
// then expects LOGOUT.
func serveIMAPImplicit(t *testing.T, conn net.Conn) {
	serveIMAPDependencySession(t, conn)
}

func serveIMAPDependencySession(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("imap handshake: %v", err)
		return
	}
	r := bufio.NewReader(tlsConn)
	writeLine(t, tlsConn, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] ready")
	line := readLine(t, r)
	fields := strings.Fields(line)
	if len(fields) < 4 || !strings.EqualFold(fields[1], "AUTHENTICATE") || !strings.EqualFold(fields[2], "PLAIN") {
		t.Errorf("imap dependency auth command = %q", line)
		return
	}
	writeLine(t, tlsConn, fields[0]+" OK [CAPABILITY IMAP4rev1] authenticated")
	line = readLine(t, r)
	fields = strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[1], "LOGOUT") {
		t.Errorf("imap dependency logout command = %q", line)
		return
	}
	writeLine(t, tlsConn, "* BYE")
	writeLine(t, tlsConn, fields[0]+" OK done")
}

func serveSMTPImplicit(t *testing.T, conn net.Conn, authCode int) []string {
	return serveSMTPImplicitWithQuitCode(t, conn, authCode, 221)
}

func serveSMTPImplicitWithQuitCode(t *testing.T, conn net.Conn, authCode, quitCode int) []string {
	t.Helper()
	defer conn.Close()
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("smtp handshake: %v", err)
		return nil
	}
	r := bufio.NewReader(tlsConn)
	var commands []string
	writeLine(t, tlsConn, "220 smtp.example.test")
	line := readLine(t, r)
	commands = append(commands, line)
	if line != "EHLO localhost" {
		t.Errorf("smtp ehlo = %q", line)
		return commands
	}
	writeLine(t, tlsConn, "250-smtp.example.test")
	writeLine(t, tlsConn, "250-AUTH PLAIN")
	writeLine(t, tlsConn, "250 SIZE 100")
	line = readLine(t, r)
	commands = append(commands, line)
	if !strings.HasPrefix(line, "AUTH PLAIN ") {
		t.Errorf("smtp auth = %q", line)
		return commands
	}
	if authCode == 235 {
		writeLine(t, tlsConn, "235 2.7.0 ok")
		line = readLine(t, r)
		commands = append(commands, line)
		if line != "QUIT" {
			t.Errorf("smtp quit = %q", line)
			return commands
		}
		writeLine(t, tlsConn, fmt.Sprintf("%d bye", quitCode))
		return commands
	}
	writeLine(t, tlsConn, "535 5.7.8 invalid")
	return commands
}

func serveSMTPDependencySession(t *testing.T, conn net.Conn) []string {
	t.Helper()
	defer conn.Close()
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("smtp handshake: %v", err)
		return nil
	}
	r := bufio.NewReader(tlsConn)
	var commands []string
	writeLine(t, tlsConn, "220 smtp.example.test")
	line := readLine(t, r)
	commands = append(commands, line)
	if line != "EHLO localhost" {
		t.Errorf("smtp dependency ehlo = %q", line)
		return commands
	}
	writeLine(t, tlsConn, "250-smtp.example.test")
	writeLine(t, tlsConn, "250-AUTH PLAIN")
	writeLine(t, tlsConn, "250 SIZE 100")
	line = readLine(t, r)
	commands = append(commands, line)
	if !strings.HasPrefix(line, "AUTH PLAIN ") {
		t.Errorf("smtp dependency auth = %q", line)
		return commands
	}
	writeLine(t, tlsConn, "235 2.7.0 ok")
	line = readLine(t, r)
	commands = append(commands, line)
	if line != "QUIT" {
		t.Errorf("smtp dependency quit = %q", line)
		return commands
	}
	writeLine(t, tlsConn, "221 bye")
	_ = tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = tlsConn.Read(make([]byte, 1))
	return commands
}

func serveIMAPLiteralLimitSession(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("imap handshake: %v", err)
		return
	}
	r := bufio.NewReader(tlsConn)
	writeLine(t, tlsConn, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] ready")
	line := readLine(t, r)
	fields := strings.Fields(line)
	if len(fields) < 4 || !strings.EqualFold(fields[1], "AUTHENTICATE") || !strings.EqualFold(fields[2], "PLAIN") {
		t.Errorf("imap auth command = %q", line)
		return
	}
	writeLine(t, tlsConn, fields[0]+" OK [CAPABILITY IMAP4rev1] authenticated")
	line = readLine(t, r)
	fields = strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[1], "SELECT") {
		t.Errorf("imap select command = %q", line)
		return
	}
	writeLine(t, tlsConn, fmt.Sprintf("* 1 FETCH (BODY[] {%d}", limits.IMAPMaxMetadataLiteralBytes+1))
}

func serveIMAPBlockedSelectSession(t *testing.T, conn net.Conn, selectStarted chan<- struct{}) {
	t.Helper()
	defer conn.Close()
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("imap handshake: %v", err)
		return
	}
	r := bufio.NewReader(tlsConn)
	writeLine(t, tlsConn, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] ready")
	line := readLine(t, r)
	fields := strings.Fields(line)
	if len(fields) < 4 || !strings.EqualFold(fields[1], "AUTHENTICATE") || !strings.EqualFold(fields[2], "PLAIN") {
		t.Errorf("imap auth command = %q", line)
		return
	}
	writeLine(t, tlsConn, fields[0]+" OK [CAPABILITY IMAP4rev1] authenticated")
	line = readLine(t, r)
	fields = strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[1], "SELECT") {
		t.Errorf("imap select command = %q", line)
		return
	}
	selectStarted <- struct{}{}
	_ = tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = tlsConn.Read(make([]byte, 1))
}

func serveSMTPReplyLimitSession(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	tlsConn := tls.Server(conn, serverTLSConfig(t))
	if err := tlsConn.Handshake(); err != nil {
		t.Errorf("smtp handshake: %v", err)
		return
	}
	r := bufio.NewReader(tlsConn)
	writeLine(t, tlsConn, "220 smtp.example.test")
	line := readLine(t, r)
	if line != "EHLO localhost" {
		t.Errorf("smtp ehlo = %q", line)
		return
	}
	writeLine(t, tlsConn, "250-smtp.example.test")
	writeLine(t, tlsConn, "250-AUTH PLAIN")
	writeLine(t, tlsConn, "250 SIZE 100")
	line = readLine(t, r)
	if !strings.HasPrefix(line, "AUTH PLAIN ") {
		t.Errorf("smtp auth = %q", line)
		return
	}
	writeLine(t, tlsConn, "235 2.7.0 ok")
	line = readLine(t, r)
	if line != "NOOP" {
		t.Errorf("smtp noop = %q", line)
		return
	}
	for remaining := SMTPMaxReplyBytes + 1; remaining > 0; {
		text := strings.Repeat("x", 128)
		if remaining <= int64(len(text)+6) {
			text = strings.Repeat("x", int(remaining)-6)
			writeLine(t, tlsConn, "250 "+text)
			return
		}
		writeLine(t, tlsConn, "250-"+text)
		remaining -= int64(len(text) + 6)
	}
}

func writeLine(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(line + "\r\n")); err != nil {
		t.Errorf("write %q: %v", line, err)
	}
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Errorf("read line: %v", err)
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

func insecureTLSConfig(string) *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

func serverTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	cert := testCert(t)
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"imap.example.com", "smtp.example.com", "imap.fastmail.com", "smtp.fastmail.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func contains(values []string, value string) bool {
	for _, got := range values {
		if got == value {
			return true
		}
	}
	return false
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func assertProductCode(t *testing.T, err error, code string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("err = %v, want product code %s", err, code)
	}
	if product.Code() != code {
		t.Fatalf("code = %s, want %s (err=%v)", product.Code(), code, err)
	}
}

func assertProductReason(t *testing.T, err error, reason string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) {
		t.Fatalf("err = %v, want product reason %s", err, reason)
	}
	if got := product.Details()["reason"]; got != reason {
		t.Fatalf("reason = %#v, want %s (err=%v)", got, reason, err)
	}
}

// refuseSentCopy answers the IMAP connection maildo opens after a send to
// save a copy in Sent. These tests are about SMTP, so it hangs up; the copy
// then becomes a warning.
func refuseSentCopy(conn net.Conn) { _ = conn.Close() }
