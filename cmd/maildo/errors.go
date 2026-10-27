package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

// explanation is an error told for people: what went wrong, the cause maildo
// knows, and what to do next.
type explanation struct {
	message string
	cause   string
	lines   []string // shown under the message, for example failed targets
	next    string
}

// renderError replaces rungrad's default error output. People get the
// message with its cause and a next step. Scripts get JSON with the same
// message, the cause and the next step in a "hint" field.
func renderError(ctx rungrad.ErrorContext) error {
	where := errorAnywhere
	if ctx.Command != nil && ctx.Command.Annotations[annotationNewAccount] != "" {
		where = errorInFirstLogin
	}
	ex := explainFor(ctx.Err, where)
	if ctx.MachineOutput {
		body := map[string]any{"error": ex.message, "exit_code": ctx.ExitCode}
		var product *errs.Error
		if errors.As(ctx.Err, &product) {
			for k, v := range product.ErrorDetails().(map[string]any) {
				body[k] = v
			}
		} else if detailer, ok := ctx.Err.(interface{ ErrorDetails() any }); ok {
			if details, ok := detailer.ErrorDetails().(map[string]any); ok {
				for k, v := range details {
					body[k] = v
				}
			}
		}
		if ex.cause != "" {
			body["cause"] = ex.cause
		}
		if ex.next != "" {
			body["hint"] = ex.next
		}
		b, err := output.StableJSON(body)
		if err != nil {
			return err
		}
		_, err = ctx.Stderr.Write(ctx.RedactJSON(b))
		return err
	}
	writeExplanation(ctx.Stderr, ex, ctx.RedactString)
	return nil
}

func writeExplanation(w io.Writer, ex explanation, redact func(string) string) {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	fmt.Fprintln(w, "Error:", redact(ex.message))
	if ex.cause != "" {
		fmt.Fprintln(w, "Cause:", redact(ex.cause))
	}
	for _, line := range ex.lines {
		fmt.Fprintln(w, "  "+redact(line))
	}
	if ex.next != "" {
		next := ex.next
		next = strings.ToUpper(next[:1]) + next[1:]
		fmt.Fprintln(w, "Next: ", redact(next))
	}
}

// annotationNewAccount marks a login of an account that does not exist
// yet. Its errors must not suggest commands that need the account, such as
// maildo doctor --account or maildo auth repair.
const annotationNewAccount = "maildo.new_account"

// errorPlace says where an error is shown, so its next step never points
// at the command the user is already running, or at an account that does
// not exist yet.
type errorPlace int

const (
	errorAnywhere errorPlace = iota
	// errorInFirstLogin is the login of an account that does not exist yet.
	errorInFirstLogin
	// errorInDoctor is a failed check inside maildo doctor.
	errorInDoctor
)

func explain(err error) explanation {
	return explainFor(err, errorAnywhere)
}

func explainFor(err error, place errorPlace) explanation {
	var bulk bulkPartialFailureError
	if errors.As(err, &bulk) {
		return bulk.explain()
	}
	var product *errs.Error
	if !errors.As(err, &product) {
		return explanation{message: err.Error()}
	}
	d := product.Details()
	str := func(key string) string {
		v, _ := d[key].(string)
		return v
	}
	account := str("account")
	reason := str("reason")
	ex := explanation{message: product.Message(), cause: str("cause")}
	// Specific messages from the providers stay as they are; only the
	// generic ones are replaced below.
	generic := genericMessages[product.Message()]
	if msg := str("server_message"); msg != "" {
		ex.cause = "the server said: " + msg
	}
	server := "mail server"
	switch {
	case str("protocol") == "imap":
		server = "IMAP server"
	case str("protocol") == "smtp":
		server = "SMTP server"
	case str("provider") == "outlook":
		server = "Microsoft Graph service"
	case str("provider") == "gmail":
		server = "Gmail API"
	}
	where := server
	if host := str("host"); host != "" {
		where = fmt.Sprintf("%s %s:%v", server, host, d["port"])
	}
	runDoctor := "run maildo doctor"
	if account != "" {
		runDoctor = "run maildo doctor --account " + account
	}
	repair := "maildo auth repair " + account
	newAccount := place == errorInFirstLogin
	switch place {
	case errorInFirstLogin:
		account = ""
		runDoctor = "run maildo login again with the right server settings"
	case errorInDoctor:
		runDoctor = "if a server setting is wrong, fix it with " + repair
	}

	switch product.Code() {
	case errs.CodeProviderConnectionError:
		switch reason {
		case "dns":
			ex.message = "could not find the " + where
			ex.cause = firstNonEmpty(ex.cause, "the host name does not exist in DNS")
			ex.next = "check the server name; " + repairOrLogin(account, "--imap-host or --smtp-host")
		case "refused":
			ex.message = "the " + where + " refused the connection"
			ex.cause = firstNonEmpty(ex.cause, "nothing is listening on that port")
			ex.next = "check the port; IMAP usually uses 993 and SMTP 465 or 587. " + upperFirst(runDoctor)
		case "timeout":
			ex.message = "the " + where + " did not answer in time"
			if str("phase") == "greeting" && str("tls_mode") == "starttls" {
				ex.cause = "the server sent no greeting; ports 993 and 465 expect TLS from the start"
				ex.next = "try --imap-tls implicit or --smtp-tls implicit, or " + runDoctor
			} else {
				ex.next = "check your network connection and the port, then " + runDoctor
			}
		case "tls_verification_failed":
			ex.message = "the " + where + " has a certificate maildo does not trust"
			ex.next = "if the server uses its own certificate authority, log in again with --ca-file <ca.pem>; otherwise check the host name"
		case "tls_not_offered":
			ex.message = "the " + where + " did not start TLS"
			ex.cause = firstNonEmpty(ex.cause, "the port expects a plain connection first")
			ex.next = "this port probably needs STARTTLS: use --imap-tls starttls or --smtp-tls starttls, or the TLS port (993 for IMAP, 465 for SMTP)"
		case "tls_handshake_failed":
			ex.message = "TLS with the " + where + " failed"
			ex.next = runDoctor
		case "tls_required":
			ex.message = "the " + where + " does not offer STARTTLS, and maildo never sends a password without encryption"
			ex.next = "use the TLS port instead (993 for IMAP, 465 for SMTP) with --imap-tls implicit or --smtp-tls implicit"
		case "dial":
			ex.message = "could not connect to the " + where
			ex.next = runDoctor
		case "connection_lost":
			ex.message = "the connection to the " + where + " broke off"
			ex.next = "try again; if it keeps happening, " + runDoctor
		case "recipient_temporarily_rejected":
			ex.message = "the " + where + " did not take " + recipientNumber(d) + " for now"
			ex.next = "the server may take it after a few minutes"
		case "protocol_greeting":
			ex.message = "the " + where + " did not answer like a mail server"
			ex.next = "check the port, then " + runDoctor
		default:
			ex.message = "could not reach the " + where
			ex.next = runDoctor
		}
	case errs.CodeProviderIdentityUnavailable:
		if reason == "credential_rejected" {
			ex.message = "the server rejected the username or password"
			ex.next = "check them and try again. Many services, Gmail and iCloud among them, want an app password instead of your normal password"
		}
	case errs.CodeAuthRepairRequired:
		switch reason {
		case "credential_rejected":
			ex.message = fmt.Sprintf("the server rejected the saved password for %q", account)
			ex.next = "the password may have changed or been revoked; run " + repair
			if newAccount {
				ex.next = "check the password and try again"
			}
		case "unsupported_authentication":
			ex.message = "the " + where + " offers no login method maildo supports (PLAIN or LOGIN)"
		default:
			if account != "" {
				ex.next = "run " + repair
			}
		}
	case errs.CodeProviderForbidden:
		ex.message = "the " + where + " refused the request"
		ex.next = "your account may not have permission for it; " + runDoctor
	case errs.CodeProviderRateLimited:
		ex.message = "the provider is limiting requests"
		ex.next = "wait a minute and try again"
	case errs.CodeProviderDataError:
		switch reason {
		case "line_limit_exceeded", "literal_limit_exceeded":
			ex.message = "the " + where + " sent a response larger than maildo accepts"
		case "invalid_mime", "invalid_header":
			ex.message = "a message on the server is malformed in a way maildo cannot read"
		default:
			ex.message = "the " + where + " sent a response maildo did not expect"
		}
		ex.next = runDoctor + "; if the problem stays, report it with the output of --json"
	case errs.CodeProtectedFolder:
		name := firstNonEmpty(str("path"), str("display_name"), "the folder")
		// These say more than the provider's "is protected" text.
		generic = generic || reason == "special_use_folder" || reason == "protected_folder"
		switch reason {
		case "special_use_folder", "protected_folder":
			ex.message = fmt.Sprintf("%q is one of the account's standard folders, so maildo will not rename or delete it", name)
		case "has_children":
			ex.message = fmt.Sprintf("%q has subfolders, and maildo deletes only empty folders without subfolders", name)
			ex.next = "delete its subfolders first, then delete the folder"
		case "not_empty":
			ex.message = fmt.Sprintf("%q still holds messages, and maildo deletes only empty folders without subfolders", name)
			ex.next = "move or delete its messages first, then delete the folder"
		case "counts_unknown":
			ex.next = "try again later; maildo deletes a folder only when it can see that the folder is empty"
		}
	case errs.CodeMessageNotFound:
		if reason == "uidvalidity_changed" {
			ex.message = "the message id is out of date because the server renumbered that folder"
			ex.next = "search for the message again to get a current id"
		} else if ex.next == "" {
			ex.next = "search again with maildo search; message ids change when mail moves to another folder"
		}
	case errs.CodeDraftNotFound:
		if ex.next == "" {
			ex.next = "run maildo list Drafts. A draft that was sent is in Sent now. On IMAP accounts a draft gets a new id when it changes, so preview it again before you send it"
		}
	case errs.CodeFolderNotFound:
		ex.next = "run maildo folders list to see the folder names"
	case errs.CodeMissingActiveAccount:
		ex.next = "run maildo login to set up an account"
	case errs.CodeUnknownAccount:
		ex.next = "run maildo auth list to see your accounts"
	case errs.CodeReadLimitExceeded:
		ex.next = firstNonEmpty(str("remediation"), "narrow the search, for example with --folder or after:")
	case errs.CodeUnsupportedCapability:
		if capability := str("capability"); capability != "" {
			// Say it in words instead of the feature's internal name.
			generic = true
			who := "this account"
			if account != "" {
				who = fmt.Sprintf("the account %q", account)
			}
			ex.message = fmt.Sprintf("%s (%s) cannot %s", who, str("provider"), capabilityText(capability))
		}
		if role := str("role"); reason == "special_use_role_missing" && role != "" {
			name := strings.ToUpper(role[:1]) + role[1:]
			ex.message = fmt.Sprintf("this account has no %s folder", name)
			ex.next = "create it with maildo folders create " + name
			if account != "" {
				ex.next += " --account " + account
			}
			if strings.EqualFold(role, "archive") {
				ex.next += ", or file the mail with maildo move --to <folder>"
			}
		}
	case errs.CodePolicyDenied:
		// The hint does not hand out the command that loosens the policy:
		// an agent that hits a limit should ask the user, not change it.
		owner := "Only the account's owner can change the policy, in a terminal (see maildo config --help)"
		switch str("policy") {
		case "read_only":
			ex.next = "Nothing was changed. The account's policy allows reading only. " + owner
		case "drafts_only":
			ex.next = "Nothing was changed. The account's policy allows drafts only, so save the message as a draft instead. " + owner
		case "allow_recipients":
			ex.next = "Nothing was sent. The account's policy allows mail only to the addresses in policy.allow_recipients. " + owner
		case "max_bulk":
			ex.next = firstNonEmpty(str("hint"), "Nothing was changed. Narrow the selection to fewer messages. "+owner)
		}
	case errs.CodeRecipientRejected:
		// Only SMTP reports a refused recipient by its place in the list.
		ex.message = "the SMTP server refused " + recipientNumber(d)
		ex.next = "check the address"
	case errs.CodeInvalidConfig:
		// Only a broken config file gets this advice. The other config
		// errors say what to do in their own message.
		if msg := product.Message(); strings.HasPrefix(msg, "the config file") || strings.HasPrefix(msg, "config ") {
			ex.next = "fix the config file, or remove the account with maildo auth remove and log in again"
		}
	}
	if !generic {
		ex.message = product.Message()
	}
	if ex.next == "" {
		ex.next = str("hint")
	}
	// For changes, say whether anything happened.
	switch str("outcome") {
	case "not_applied", "rejected":
		ex.next = joinSentences("Nothing was changed or sent, so it is safe to try again", ex.next)
	case "unknown":
		if op := str("operation"); strings.Contains(op, "send") || strings.HasSuffix(op, "_reply") || strings.HasSuffix(op, "_forward") {
			ex.next = joinSentences("maildo could not confirm whether the mail went out; look in your Sent folder before you try again, or it may be sent twice", ex.next)
		} else {
			ex.next = joinSentences("maildo could not confirm whether the change happened; check before you try again", ex.next)
		}
		if id := str("draft_id"); id != "" {
			ex.next = joinSentences(strings.TrimSuffix(ex.next, "."), "the draft is "+id)
		}
	}
	return ex
}

func joinSentences(first, second string) string {
	if second == "" {
		return first + "."
	}
	return first + ". " + strings.ToUpper(second[:1]) + second[1:]
}

// genericMessages are the error texts that say nothing beyond the error
// code. The renderer replaces them with a sentence that names the server and
// the cause.
var genericMessages = map[string]bool{
	"provider connection failed":                 true,
	"provider returned invalid protocol data":    true,
	"provider rejected the operation":            true,
	"provider identity is unavailable":           true,
	"stored credential was rejected by provider": true,
	"provider authentication is unsupported":     true,
	"folder is protected":                        true,
	"message not found":                          true,
	"provider capability is unavailable":         true,
	"recipient was rejected by provider":         true,
}

// recipientNumber names a recipient the SMTP server refused by its place in
// the list: "recipient 2 of 3", or "a recipient" when the count is unknown.
func recipientNumber(d map[string]any) string {
	index, total := fmt.Sprint(d["rejected_index"]), fmt.Sprint(d["total_count"])
	if d["rejected_index"] == nil || d["total_count"] == nil {
		return "a recipient"
	}
	if total == "1" {
		return "the recipient"
	}
	return "recipient " + index + " of " + total
}

func repairOrLogin(account, flags string) string {
	if account == "" {
		return "log in again with the right " + flags
	}
	return fmt.Sprintf("run maildo auth repair %s with the right %s", account, flags)
}

func capabilityText(capability string) string {
	text := strings.ReplaceAll(strings.TrimPrefix(capability, "messages_"), "_", " ")
	switch capability {
	case "reply_draft_create":
		return "create reply drafts"
	case "forward_draft_create":
		return "create forward drafts"
	case "draft_create":
		return "create drafts"
	case "categories_read", "categories_mutate":
		return "use categories, which only Outlook and Gmail accounts have"
	case "rules_read", "rules_mutate":
		return "use Inbox rules, which only Outlook accounts have"
	}
	return "do this (" + text + ")"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
