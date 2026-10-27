package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"github.com/vincentsch/rungrad"
)

type doctorModule struct {
	defs []cmdDef
}

func newDoctorModule(h *handlers) doctorModule {
	defs := []cmdDef{{
		path: "doctor", use: "doctor", short: "Check an account's servers, login and folders", groupID: "auth",
		examples: []string{
			"maildo doctor",
			"maildo doctor --account work",
			"maildo doctor --all-accounts --json",
		},
		related:     []string{"auth status", "login"},
		outputModes: []string{rungrad.OutputModeHuman, rungrad.OutputModeJSON, rungrad.OutputModeJQ, rungrad.OutputModeTemplate},
		args:        cobra.NoArgs,
		configure: func(cmd *cobra.Command) {
			provider.ConfigureAccountFlags(cmd, provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		},
		run: h.doctor,
	}}
	return doctorModule{defs: markMailboxAuth(defs, "doctor")}
}

func (m doctorModule) Groups() []rungrad.Group { return nil }

func (m doctorModule) Commands() []*rungrad.Command {
	return []*rungrad.Command{m.defs[0].command()}
}

func (m doctorModule) Catalog() []rungrad.CommandSpec {
	return []rungrad.CommandSpec{m.defs[0].catalog()}
}

type doctorAccount struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
	Email    string `json:"email,omitempty"`
	OK       bool   `json:"ok"`
	mail.Diagnosis
}

type doctorResult struct {
	Accounts []doctorAccount `json:"accounts"`
}

// doctor checks each selected account and reports what works. It exits
// with status 1 when a check failed, so scripts can use it as a health
// check.
func (h *handlers) doctor(f *rungrad.Factory, cmd *cobra.Command, _ []string) error {
	return provider.WithAccountClientScope(cmd.Context(), func(scope provider.AccountClientScope) error {
		sel, err := provider.ResolveAccounts(scope, cmd, h.deps.Providers, h.registryLoader(f), h.clientBinder(f), provider.ScopeRequest{Mode: provider.ModeMulti, AllowAll: true})
		if err != nil {
			return err
		}
		var result doctorResult
		failed := 0
		for _, acct := range sel.Accounts {
			f.Infof("Checking %s...", acct.Name)
			entry := doctorAccount{Account: acct.Name, Provider: acct.Provider, Email: acct.Email}
			entry.Diagnosis = h.diagnose(cmd.Context(), scope, acct)
			entry.OK = true
			for i, check := range entry.Checks {
				if check.Status == "failed" {
					entry.OK = false
				}
				if check.Err != nil {
					entry.Checks[i].Detail = doctorDetail(check.Err)
				}
			}
			if !entry.OK {
				failed++
			}
			result.Accounts = append(result.Accounts, entry)
		}
		if err := f.WriteResult(result, humanDoctor(result)); err != nil {
			return err
		}
		if failed > 0 {
			return errs.New(errs.CodeDoctorCheckFailed, fmt.Sprintf("checks failed for %d of %d accounts", failed, len(result.Accounts)))
		}
		return nil
	})
}

// diagnose uses the provider's own step-by-step check when it has one (IMAP
// does), and otherwise checks that maildo can sign in and list folders.
func (h *handlers) diagnose(ctx context.Context, scope provider.AccountClientScope, acct provider.ResolvedAccount) mail.Diagnosis {
	var d mail.Diagnosis
	err := provider.WithAccountClient(ctx, scope, acct, provider.ClientAccessDiagnose, func(diagnoser provider.Diagnoser) error {
		d = diagnoser.Diagnose(ctx)
		return nil
	})
	if err == nil {
		return d
	}
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != errs.CodeCapabilityNotImplemented {
		return mail.Diagnosis{Checks: []mail.DiagnosisCheck{{Name: "Login", Status: "failed", Detail: doctorDetail(err)}}}
	}
	d = mail.Diagnosis{}
	err = provider.WithAccountClient(ctx, scope, acct, provider.ClientAccessFoldersRead, func(reader provider.FolderReader) error {
		list, err := reader.ListFolders(ctx, mail.FolderListRequest{Detail: mail.FolderListDetailIdentity})
		if err != nil {
			return err
		}
		d.Checks = append(d.Checks,
			mail.DiagnosisCheck{Name: "Login", Status: "ok", Detail: "signed in"},
			mail.DiagnosisCheck{Name: "Folders", Status: "ok", Detail: fmt.Sprintf("%d folders", len(list.Folders))})
		return nil
	})
	if err != nil {
		d.Checks = append(d.Checks, mail.DiagnosisCheck{Name: "Login", Status: "failed", Detail: doctorDetail(err)})
	}
	return d
}

// doctorDetail explains a failed check the way other commands explain
// errors: what went wrong, the cause and what to do.
func doctorDetail(err error) string {
	ex := explainFor(err, errorInDoctor)
	text := ex.message
	if ex.cause != "" {
		text += " (" + ex.cause + ")"
	}
	return joinSentences(text, ex.next)
}

func humanDoctor(r doctorResult) func(io.Writer) {
	return func(w io.Writer) {
		for i, acct := range r.Accounts {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "%s (%s", acct.Account, acct.Provider)
			if acct.Email != "" {
				fmt.Fprintf(w, ", %s", acct.Email)
			}
			fmt.Fprintln(w, ")")
			for _, check := range acct.Checks {
				fmt.Fprintf(w, "  %-8s %-8s %s\n", statusMark(check.Status), check.Name, check.Detail)
			}
			if len(acct.Works) > 0 {
				fmt.Fprintf(w, "  Works: %s\n", strings.Join(acct.Works, ", "))
			}
			for _, limit := range acct.Limits {
				fmt.Fprintf(w, "  Limit: %s\n", limit)
			}
		}
	}
}

func statusMark(status string) string {
	switch status {
	case "ok":
		return "ok"
	case "warning":
		return "warning"
	case "failed":
		return "FAILED"
	default:
		return status
	}
}
