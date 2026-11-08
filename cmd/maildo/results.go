package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/output"
)

const (
	authStatusValid          = "valid"
	authStatusLoggedOut      = "logged_out"
	authStatusRefreshed      = "refreshed"
	authStatusRepairRequired = "auth_repair_required"

	credentialPresent = "present"
	credentialAbsent  = "absent"
)

// The result structs below are the JSON of the auth commands, which scripts
// rely on. Booleans are always written, so false is never missing. Optional
// strings are left out only when they do not apply. ActiveAccount is a
// pointer, so JSON shows null when no account is active.
type loginResult struct {
	Account         string  `json:"account"`
	Provider        string  `json:"provider"`
	Operation       string  `json:"operation"`
	Created         bool    `json:"created"`
	Reauthenticated bool    `json:"reauthenticated"`
	Email           string  `json:"email,omitempty"`
	DisplayName     string  `json:"display_name,omitempty"`
	Active          bool    `json:"active"`
	AuthRef         string  `json:"auth_ref"`
	AuthStatus      string  `json:"auth_status"`
	ActiveAccount   *string `json:"active_account"`
	// unnamed is set when mail from this account would show only the
	// address in From, because neither the provider nor sender_name gives
	// a name. Gmail does not tell maildo the name.
	unnamed bool
}

type listResult struct {
	Accounts []listAccount `json:"accounts"`
}

type listAccount struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Email         string `json:"email,omitempty"`
	DisplayName   string `json:"display_name,omitempty"`
	Active        bool   `json:"active"`
	AuthRef       string `json:"auth_ref"`
	AuthStatus    string `json:"auth_status"`
	LoginCommand  string `json:"login_command,omitempty"`
	RepairCommand string `json:"repair_command,omitempty"`
}

type statusResult struct {
	Account             string `json:"account"`
	Provider            string `json:"provider"`
	Email               string `json:"email,omitempty"`
	DisplayName         string `json:"display_name,omitempty"`
	Active              bool   `json:"active"`
	AuthStatus          string `json:"auth_status"`
	CredentialCache     string `json:"credential_cache"`
	RefreshChecked      bool   `json:"refresh_checked"`
	CredentialChanged   bool   `json:"credential_changed"`
	RotatedRefreshToken bool   `json:"rotated_refresh_token"`
	LoginCommand        string `json:"login_command,omitempty"`
	RepairCommand       string `json:"repair_command,omitempty"`
}

type refreshResult struct {
	Account             string `json:"account"`
	Provider            string `json:"provider"`
	Operation           string `json:"operation"`
	AuthStatus          string `json:"auth_status"`
	CredentialCache     string `json:"credential_cache"`
	RefreshChecked      bool   `json:"refresh_checked"`
	CredentialChanged   bool   `json:"credential_changed"`
	RotatedRefreshToken bool   `json:"rotated_refresh_token"`
	LoginCommand        string `json:"login_command,omitempty"`
	RepairCommand       string `json:"repair_command,omitempty"`
}

type repairResult struct {
	Account             string `json:"account"`
	Provider            string `json:"provider"`
	Operation           string `json:"operation"`
	Repaired            bool   `json:"repaired"`
	AuthStatus          string `json:"auth_status"`
	CredentialCache     string `json:"credential_cache"`
	RefreshChecked      bool   `json:"refresh_checked"`
	CredentialChanged   bool   `json:"credential_changed"`
	RotatedRefreshToken bool   `json:"rotated_refresh_token"`
	Active              bool   `json:"active"`
}

type switchResult struct {
	Account       string  `json:"account"`
	Provider      string  `json:"provider"`
	Operation     string  `json:"operation"`
	ActiveAccount *string `json:"active_account"`
}

type logoutResult struct {
	Account                string  `json:"account"`
	Provider               string  `json:"provider"`
	Operation              string  `json:"operation"`
	CredentialCacheDeleted bool    `json:"credential_cache_deleted"`
	ActiveAccount          *string `json:"active_account"`
}

type renameResult struct {
	Account                 string  `json:"account"`
	Provider                string  `json:"provider"`
	Operation               string  `json:"operation"`
	OldName                 string  `json:"old_name"`
	NewName                 string  `json:"new_name"`
	AuthRef                 string  `json:"auth_ref"`
	ActiveAccount           *string `json:"active_account"`
	CredentialCacheMigrated bool    `json:"credential_cache_migrated"`
}

type removeResult struct {
	Account                string  `json:"account"`
	Provider               string  `json:"provider"`
	Operation              string  `json:"operation"`
	Removed                bool    `json:"removed"`
	CredentialCacheDeleted bool    `json:"credential_cache_deleted"`
	ActiveAccount          *string `json:"active_account"`
}

func activePtr(active string) *string {
	if active == "" {
		return nil
	}
	v := active
	return &v
}

func credentialState(present bool) string {
	if present {
		return credentialPresent
	}
	return credentialAbsent
}

func statusFromCache(present bool) string {
	if present {
		return authStatusValid
	}
	return authStatusLoggedOut
}

func loginCommand(name string) string {
	return "maildo auth login --name " + name
}

func repairCommand(name string) string {
	return "maildo auth repair " + name
}

// safeMeta returns the metadata for --include-meta. It goes into the output
// as it is, so config paths, login file paths, server answers and tokens must
// never be put in it.
func safeMeta(commandPath, operation, account, providerName, active string) output.Meta {
	extra := map[string]any{"command_path": commandPath}
	if operation != "" {
		extra["operation"] = operation
	}
	if account != "" {
		extra["account"] = account
	}
	if providerName != "" {
		extra["provider"] = providerName
	}
	if active != "" {
		extra["active_account"] = active
	}
	return output.Meta{Extra: extra}
}

// The human views below are short sentences. The details are in the JSON.
func humanLogin(r loginResult) func(io.Writer) {
	return func(w io.Writer) {
		who := r.Email
		if who == "" {
			who = r.Account
		}
		if r.Reauthenticated {
			fmt.Fprintf(w, "Logged in again to %s (account %q, %s).\n", who, r.Account, r.Provider)
		} else {
			fmt.Fprintf(w, "Logged in to %s as account %q (%s).\n", who, r.Account, r.Provider)
		}
		if r.Active {
			fmt.Fprintf(w, "%q is the active account. Try: maildo list\n", r.Account)
		} else {
			fmt.Fprintf(w, "The active account is still %s. Use this one with --account %s\n", activeText(r.ActiveAccount), r.Account)
		}
		if r.unnamed {
			fmt.Fprintf(w, "Mail you send shows only your address in From. To add your name, run: maildo config set sender_name \"Your Name\" --account %s\n", r.Account)
		}
	}
}

// authStatusText says what an auth status means for a person.
func authStatusText(status string) string {
	switch status {
	case authStatusValid:
		return "logged in"
	case authStatusRefreshed:
		return "logged in, login refreshed"
	case authStatusLoggedOut:
		return "logged out"
	case authStatusRepairRequired:
		return "login needs repair"
	default:
		return strings.ReplaceAll(status, "_", " ")
	}
}

func humanList(r listResult) func(io.Writer) {
	return func(w io.Writer) {
		if len(r.Accounts) == 0 {
			fmt.Fprintln(w, "No accounts yet. Run: maildo login")
			return
		}
		t := newTable("", "ACCOUNT", "ADDRESS", "PROVIDER", "STATUS")
		var next []string
		for _, account := range r.Accounts {
			mark := ""
			if account.Active {
				mark = "*"
			}
			t.add(mark, account.Name, account.Email, account.Provider, authStatusText(account.AuthStatus))
			if account.RepairCommand != "" {
				next = append(next, fmt.Sprintf("To repair %s, run: %s", account.Name, account.RepairCommand))
			} else if account.LoginCommand != "" {
				next = append(next, fmt.Sprintf("To log in to %s, run: %s", account.Name, account.LoginCommand))
			}
		}
		t.render(w, 0, nil)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "* active account")
		for _, line := range next {
			fmt.Fprintln(w, line)
		}
	}
}

func humanStatus(r statusResult) func(io.Writer) {
	return func(w io.Writer) {
		who := r.Account
		if r.Email != "" {
			who += " (" + r.Email + ")"
		}
		active := ""
		if r.Active {
			active = ", the active account"
		}
		fmt.Fprintf(w, "%s: %s%s.\n", who, authStatusText(r.AuthStatus), active)
		if r.CredentialCache != credentialPresent {
			fmt.Fprintln(w, "No login is saved on this computer.")
		}
		if r.RepairCommand != "" {
			fmt.Fprintf(w, "To repair it, run: %s\n", r.RepairCommand)
		} else if r.LoginCommand != "" {
			fmt.Fprintf(w, "To log in, run: %s\n", r.LoginCommand)
		}
	}
}

func humanRefresh(r refreshResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "%s: %s.\n", r.Account, authStatusText(r.AuthStatus))
		if r.RepairCommand != "" {
			fmt.Fprintf(w, "To repair it, run: %s\n", r.RepairCommand)
		} else if r.LoginCommand != "" {
			fmt.Fprintf(w, "To log in, run: %s\n", r.LoginCommand)
		}
	}
}

func humanRepair(r repairResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "Repaired the login for %q (%s).\n", r.Account, r.Provider)
	}
}

func humanSwitch(r switchResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "The active account is now %s.\n", activeText(r.ActiveAccount))
	}
}

func humanLogout(r logoutResult) func(io.Writer) {
	return func(w io.Writer) {
		if r.CredentialCacheDeleted {
			fmt.Fprintf(w, "Logged out of %q (%s) and deleted its saved login. The account stays; log in again with maildo login.\n", r.Account, r.Provider)
		} else {
			fmt.Fprintf(w, "%q (%s) had no saved login.\n", r.Account, r.Provider)
		}
		fmt.Fprintf(w, "The active account is %s.\n", activeText(r.ActiveAccount))
	}
}

func humanRename(r renameResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "Renamed the account %q to %q. The active account is %s.\n", r.OldName, r.NewName, activeText(r.ActiveAccount))
	}
}

func humanRemove(r removeResult) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "Removed the account %q (%s)", r.Account, r.Provider)
		if r.CredentialCacheDeleted {
			fmt.Fprint(w, " and its saved login")
		}
		fmt.Fprintf(w, ". The active account is %s.\n", activeText(r.ActiveAccount))
	}
}

func activeText(active *string) string {
	if active == nil || *active == "" {
		return "not set"
	}
	return *active
}

// The preview helpers describe a change without making it. They never hold
// login file paths or secrets.
func previewLogin(providerName, account string, setActive, reauth bool, extra ...output.Field) output.DryRunPreview {
	body := []output.Field{
		{Name: "provider", Value: providerName},
		{Name: "account", Value: account},
		{Name: "set_active", Value: strconv.FormatBool(setActive)},
		{Name: "reauthenticated", Value: strconv.FormatBool(reauth)},
		{Name: "auth_ref", Value: maildoconfig.DeriveAuthRef(providerName, account)},
	}
	body = append(body, extra...)
	return output.DryRunPreview{
		Method: "login",
		Path:   maildoconfig.DeriveAuthRef(providerName, account),
		Body:   body,
	}
}

func previewRefresh(account maildoconfig.Account) output.DryRunPreview {
	return output.DryRunPreview{
		Method: "refresh",
		Path:   account.AuthRef,
		Body: []output.Field{
			{Name: "account", Value: account.Name},
			{Name: "provider", Value: account.Provider},
			{Name: "would_refresh", Value: "true"},
		},
	}
}

func previewRepair(account maildoconfig.Account, extra ...output.Field) output.DryRunPreview {
	body := []output.Field{
		{Name: "account", Value: account.Name},
		{Name: "provider", Value: account.Provider},
	}
	body = append(body, extra...)
	return output.DryRunPreview{
		Method: "repair",
		Path:   account.AuthRef,
		Body:   body,
	}
}

func previewSwitch(account maildoconfig.Account, previous string) output.DryRunPreview {
	return output.DryRunPreview{
		Method: "switch",
		Path:   account.Name,
		Body: []output.Field{
			{Name: "account", Value: account.Name},
			{Name: "new_active", Value: account.Name},
			{Name: "previous_active", Value: previous},
		},
	}
}

func previewLogout(account maildoconfig.Account, cachePresent bool) output.DryRunPreview {
	return output.DryRunPreview{
		Method: "logout",
		Path:   account.AuthRef,
		Body: []output.Field{
			{Name: "account", Value: account.Name},
			{Name: "provider", Value: account.Provider},
			{Name: "credential_cache", Value: credentialState(cachePresent)},
		},
	}
}

func previewRename(account *maildoconfig.Account, newName, activeChange string) output.DryRunPreview {
	return output.DryRunPreview{
		Method: "rename",
		Path:   account.AuthRef,
		Body: []output.Field{
			{Name: "old_name", Value: account.Name},
			{Name: "new_name", Value: newName},
			{Name: "new_auth_ref", Value: maildoconfig.DeriveAuthRef(account.Provider, newName)},
			{Name: "active_change", Value: activeChange},
		},
	}
}

func previewRemove(account maildoconfig.Account, activeEffect string, cachePresent bool) output.DryRunPreview {
	return output.DryRunPreview{
		Method: "remove",
		Path:   account.Name,
		Body: []output.Field{
			{Name: "account", Value: account.Name},
			{Name: "active_effect", Value: activeEffect},
			{Name: "credential_cache", Value: credentialState(cachePresent)},
		},
	}
}

// humanAuthPreview says in a sentence what an auth or login --dry-run would
// do, with the server settings a login would save.
func humanAuthPreview(p output.DryRunPreview) func(io.Writer) {
	field := func(name string) string {
		for _, f := range p.Body {
			if f.Name == name {
				return f.Value
			}
		}
		return ""
	}
	return func(w io.Writer) {
		account, providerName := field("account"), field("provider")
		switch p.Method {
		case "login":
			verb := "log in"
			if field("reauthenticated") == "true" {
				verb = "log in again"
			}
			fmt.Fprintf(w, "Would %s and save the login as account %q (%s)", verb, account, providerName)
			if field("set_active") == "true" {
				fmt.Fprint(w, ", the active account")
			}
			fmt.Fprintln(w, ".")
			server := func(kind string) {
				if host := field(kind + "_host"); host != "" {
					fmt.Fprintf(w, "  %-8s %s:%s, %s TLS\n", strings.ToUpper(kind)+":", host, field(kind+"_port"), field(kind+"_tls"))
				}
			}
			if email := field("email"); email != "" {
				fmt.Fprintf(w, "  %-8s %s\n", "Address:", email)
			}
			server("imap")
			server("smtp")
			if ca := field("ca_file"); ca != "" {
				fmt.Fprintf(w, "  %-8s %s\n", "CA file:", ca)
			}
			if field("client_id_configured") == "false" {
				fmt.Fprintf(w, "No %s client id is set yet, so the real login would stop. See docs/providers/%s.md.\n", providerName, providerName)
			}
		case "refresh":
			fmt.Fprintf(w, "Would refresh the saved login for %q (%s).\n", account, providerName)
		case "repair":
			fmt.Fprintf(w, "Would repair the saved login for %q (%s).\n", account, providerName)
		case "switch":
			fmt.Fprintf(w, "Would make %q the active account", account)
			if previous := field("previous_active"); previous != "" {
				fmt.Fprintf(w, " instead of %q", previous)
			}
			fmt.Fprintln(w, ".")
		case "logout":
			if field("credential_cache") == credentialPresent {
				fmt.Fprintf(w, "Would log out of %q (%s) and delete its saved login.\n", account, providerName)
			} else {
				fmt.Fprintf(w, "%q (%s) has no saved login to delete.\n", account, providerName)
			}
		case "rename":
			fmt.Fprintf(w, "Would rename the account %q to %q.", field("old_name"), field("new_name"))
			if field("active_change") != "unchanged" {
				fmt.Fprint(w, " It stays the active account.")
			}
			fmt.Fprintln(w)
		case "remove":
			fmt.Fprintf(w, "Would remove the account %q", account)
			if field("credential_cache") == credentialPresent {
				fmt.Fprint(w, " and delete its saved login")
			}
			fmt.Fprint(w, ".")
			switch effect := field("active_effect"); {
			case strings.HasPrefix(effect, "set:"):
				fmt.Fprintf(w, " %q would become the active account.", strings.TrimPrefix(effect, "set:"))
			case effect == "cleared":
				fmt.Fprint(w, " No account would be active.")
			}
			fmt.Fprintln(w)
		default:
			fmt.Fprintf(w, "Would %s %s.\n", p.Method, p.Path)
		}
		fmt.Fprintln(w, "Nothing was changed. To do it, run the command again without --dry-run.")
	}
}

func writeAuthPreview(f *rungrad.Factory, p output.DryRunPreview) error {
	return writeReadablePreview(f, p, humanAuthPreview(p))
}
