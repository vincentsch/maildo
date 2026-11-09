package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vincentsch/maildo/internal/discover"
	"github.com/vincentsch/maildo/internal/imap"
	"github.com/vincentsch/maildo/internal/testkit"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/testutil"
)

// mailServerHarness runs the real IMAP and SMTP providers against servers in
// the test process, with an empty config directory.
type mailServerHarness struct {
	t          *testing.T
	root       string
	configPath string
	server     *testkit.MailServer
	env        map[string]string
}

func newMailServerHarness(t *testing.T, user string, mailboxes ...string) *mailServerHarness {
	t.Helper()
	root := t.TempDir()
	return &mailServerHarness{
		t:          t,
		root:       root,
		configPath: filepath.Join(root, "config.yaml"),
		server:     testkit.StartMailServer(t, user, "app-password", mailboxes...),
		env:        map[string]string{"MAILDO_IMAP_PASSWORD": "app-password"},
	}
}

func (h *mailServerHarness) app() *rungrad.App {
	deps, err := depsWithMailServerOptions(imap.Options{DialContext: h.server.Dial, TLSConfig: h.server.TLSConfig})
	if err != nil {
		h.t.Fatal(err)
	}
	return newApp(deps)
}

func (h *mailServerHarness) run(args ...string) testutil.Result {
	h.t.Helper()
	return testutil.RunWith(h.app(), testutil.Options{
		LookupEnv: func(key string) (string, bool) {
			v, ok := h.env[key]
			return v, ok
		},
	}, append([]string{"--config", h.configPath}, args...)...)
}

// Every IMAP and Fastmail login example in the help must work on a machine
// that has never run maildo, both as a dry run and for real.
func TestLoginExamplesWorkFromAnEmptyConfig(t *testing.T) {
	for _, path := range []string{"login", "auth login"} {
		cmd, _, err := newApp(productionDeps()).Root().Find(strings.Fields(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, example := range strings.Split(cmd.Example, "\n") {
			args := strings.Fields(strings.TrimSpace(example))
			// Drop a leading VAR=value; the harness sets the password.
			for len(args) > 0 && strings.Contains(args[0], "=") {
				args = args[1:]
			}
			if len(args) < 2 {
				continue
			}
			args = args[1:]
			user := flagValue(args, "--email")
			// Outlook and Gmail need a browser, and an address maildo cannot
			// look up offline needs a terminal; the other examples must work.
			if strings.Contains(example, "outlook") || strings.Contains(example, "gmail") || user == "" {
				continue
			}
			if _, preset := discover.PresetFor(user); !preset && flagValue(args, "--imap-host") == "" && !strings.Contains(example, "--provider") {
				continue
			}
			t.Run(example, func(t *testing.T) {
				h := newMailServerHarness(t, user, "Sent", "Trash")
				if strings.Contains(example, "--dry-run") {
					args = slicesDelete(args, "--dry-run")
				}
				dry := h.run(append(args, "--dry-run")...)
				if dry.Exit != 0 || !strings.Contains(dry.Stdout, "Would log in") {
					t.Fatalf("dry run exit %d: %s%s", dry.Exit, dry.Stdout, dry.Stderr)
				}
				real := h.run(args...)
				if real.Exit != 0 {
					t.Fatalf("login exit %d: %s%s", real.Exit, real.Stdout, real.Stderr)
				}
				if !strings.Contains(real.Stdout, "Logged in") {
					t.Fatalf("login output = %q", real.Stdout)
				}
				folders := h.run("folders", "list", "--json")
				if folders.Exit != 0 || !strings.Contains(folders.Stdout, `"INBOX"`) {
					t.Fatalf("folders after login: exit %d %s%s", folders.Exit, folders.Stdout, folders.Stderr)
				}
			})
		}
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(arg, name+"="); ok {
			return v
		}
	}
	return ""
}

// A first login needs only the address and the password: maildo knows the
// servers of large mail services and suggests the account name.
func TestInteractiveLoginAsksOnlyForTheAddress(t *testing.T) {
	h := newMailServerHarness(t, "me@gmail.com")
	res := testutil.RunWith(h.app(), testutil.Options{
		Stdin:       strings.NewReader("me@gmail.com\n\n"),
		Terminal:    true,
		TerminalSet: true,
		LookupEnv: func(key string) (string, bool) {
			v, ok := h.env[key]
			return v, ok
		},
	}, "--config", h.configPath, "login")
	if res.Exit != 0 {
		t.Fatalf("login exit %d: %s%s", res.Exit, res.Stdout, res.Stderr)
	}
	for _, want := range []string{"Email address:", "Found the servers for Gmail", "imap.gmail.com:993", "app password", "Account name [gmail]:"} {
		if !strings.Contains(res.Stderr, want) {
			t.Fatalf("stderr is missing %q:\n%s", want, res.Stderr)
		}
	}
	folders := h.run("folders", "list", "--account", "gmail", "--json")
	if folders.Exit != 0 {
		t.Fatalf("folders after login: %s%s", folders.Stdout, folders.Stderr)
	}
}

// For a domain maildo cannot look up, login says so and asks for the
// servers; Enter takes the usual port and TLS mode.
func TestInteractiveLoginAsksForServersItCannotFind(t *testing.T) {
	h := newMailServerHarness(t, "me@self-hosted.example")
	input := strings.Join([]string{
		"me@self-hosted.example",           // Email address
		"",                                 // Account name, take the suggestion
		"mail.self-hosted.example", "", "", // IMAP host, port, TLS
		"mail.self-hosted.example", "", "", // SMTP host, port, TLS
	}, "\n") + "\n"
	res := testutil.RunWith(h.app(), testutil.Options{
		Stdin:       strings.NewReader(input),
		Terminal:    true,
		TerminalSet: true,
		LookupEnv: func(key string) (string, bool) {
			v, ok := h.env[key]
			return v, ok
		},
	}, "--config", h.configPath, "login")
	if res.Exit != 0 {
		t.Fatalf("login exit %d: %s%s", res.Exit, res.Stdout, res.Stderr)
	}
	want := []string{
		"Email address:",
		"Account name [self-hosted]:",
		"maildo could not find the mail servers for me@self-hosted.example",
		"IMAP host:", "IMAP port [993]:", "IMAP TLS mode, implicit or starttls [implicit]:",
		"SMTP host:", "SMTP port [465]:", "SMTP TLS mode, implicit or starttls [implicit]:",
	}
	last := -1
	for _, w := range want {
		at := strings.Index(res.Stderr, w)
		if at < 0 || at < last {
			t.Fatalf("stderr is missing %q or has it out of order:\n%s", w, res.Stderr)
		}
		last = at
	}
	if !strings.Contains(res.Stdout, "Logged in") {
		t.Fatalf("login output = %q", res.Stdout)
	}
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"host: mail.self-hosted.example", "port: 993", "port: 465"} {
		if !strings.Contains(string(data), w) {
			t.Fatalf("config is missing %q:\n%s", w, data)
		}
	}
	if folders := h.run("folders", "list", "--json"); folders.Exit != 0 {
		t.Fatalf("folders after login: %s%s", folders.Stdout, folders.Stderr)
	}
}

// A first login that cannot reach the server must not suggest commands that
// need the account it could not create.
func TestFirstLoginErrorsDoNotPointAtTheMissingAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "Run maildo login again with the right server settings"},
		{"dns", &net.DNSError{Err: "no such host", Name: "imap.wrong.example", IsNotFound: true}, "log in again with the right --imap-host or --smtp-host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, err := depsWithMailServerOptions(imap.Options{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, tc.err
			}})
			if err != nil {
				t.Fatal(err)
			}
			res := testutil.RunWith(newApp(deps), testutil.Options{
				LookupEnv: func(key string) (string, bool) {
					if key == "MAILDO_IMAP_PASSWORD" {
						return "app-password", true
					}
					return "", false
				},
			}, "--config", filepath.Join(t.TempDir(), "config.yaml"), "login", "--email", "me@posteo.de", "--name", "t")
			if res.Exit == 0 || !strings.Contains(res.Stderr, tc.want) {
				t.Fatalf("login exit %d, stderr:\n%s", res.Exit, res.Stderr)
			}
			for _, bad := range []string{"--account t", "auth repair t"} {
				if strings.Contains(res.Stderr, bad) {
					t.Fatalf("stderr suggests %q for an account that does not exist:\n%s", bad, res.Stderr)
				}
			}
		})
	}
}

func TestLoginWithAnUnknownProviderSaysWhatWorks(t *testing.T) {
	h := newMailServerHarness(t, "me@posteo.de")
	res := h.run("login", "--provider", "yahoo", "--name", "x")
	if res.Exit == 0 || !strings.Contains(res.Stderr, `maildo has no provider "yahoo"`) || !strings.Contains(res.Stderr, "Leave out --provider and give your address with --email") {
		t.Fatalf("login exit %d:\n%s", res.Exit, res.Stderr)
	}
}

// Without a terminal, login names only the settings that have no default.
func TestLoginWithoutServersNamesOnlyTheHosts(t *testing.T) {
	h := newMailServerHarness(t, "me@self-hosted.example")
	res := h.run("login", "--email", "me@self-hosted.example")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "maildo needs more settings to log in; give --imap-host and --smtp-host") {
		t.Fatalf("login exit %d: %s", res.Exit, res.Stderr)
	}
}

func TestInteractiveFastmailLoginAsksForTheAddress(t *testing.T) {
	h := newMailServerHarness(t, "me@fastmail.com")
	res := testutil.RunWith(h.app(), testutil.Options{
		Stdin:       strings.NewReader("\nme@fastmail.com\n"),
		Terminal:    true,
		TerminalSet: true,
		LookupEnv: func(key string) (string, bool) {
			v, ok := h.env[key]
			return v, ok
		},
	}, "--config", h.configPath, "login", "--provider", "fastmail")
	if res.Exit != 0 {
		t.Fatalf("login exit %d: %s%s", res.Exit, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "Email:") || !strings.Contains(res.Stdout, "me@fastmail.com") {
		t.Fatalf("fastmail login:\n%s\n%s", res.Stdout, res.Stderr)
	}
}

// Scripts and agents log in with one command: the address, and the password
// in MAILDO_IMAP_PASSWORD.
func TestLoginWithOnlyAnEmailAddress(t *testing.T) {
	h := newMailServerHarness(t, "me@posteo.de")
	res := h.run("login", "--email", "me@posteo.de", "--json")
	if res.Exit != 0 {
		t.Fatalf("login exit %d: %s%s", res.Exit, res.Stdout, res.Stderr)
	}
	var out loginResult
	if err := res.JSON(&out); err != nil || out.Account != "posteo" || out.Provider != "imap" {
		t.Fatalf("login = %+v, %v", out, err)
	}
	again := h.run("login", "--email", "me@posteo.de", "--json")
	if err := again.JSON(&out); err != nil || out.Account != "posteo" || !out.Reauthenticated {
		t.Fatalf("second login = %+v (exit %d %s)", out, again.Exit, again.Stderr)
	}
}

func TestMicrosoftAddressesExplainOAuth(t *testing.T) {
	h := newMailServerHarness(t, "me@outlook.com")
	res := h.run("login", "--email", "me@outlook.com")
	if res.Exit == 0 || !strings.Contains(res.Stderr, "--provider outlook") {
		t.Fatalf("outlook.com login exit %d: %s", res.Exit, res.Stderr)
	}
}

func TestDoctorChecksAnIMAPAccount(t *testing.T) {
	h := newMailServerHarness(t, "me@posteo.de", "Sent", "Trash", "Archive", "Junk", "Drafts")
	if res := h.run("login", "--email", "me@posteo.de"); res.Exit != 0 {
		t.Fatalf("login: %s", res.Stderr)
	}
	res := h.run("doctor", "--json")
	var out doctorResult
	if err := res.JSON(&out); err != nil || res.Exit != 0 || len(out.Accounts) != 1 {
		t.Fatalf("doctor exit %d %v: %s%s", res.Exit, err, res.Stdout, res.Stderr)
	}
	acct := out.Accounts[0]
	var names []string
	for _, check := range acct.Checks {
		names = append(names, check.Name+"="+check.Status)
	}
	if got := strings.Join(names, " "); got != "TLS=ok Login=ok IMAP=ok Folders=ok SMTP=ok" || !acct.OK {
		t.Fatalf("checks = %s", got)
	}
	if works := strings.Join(acct.Works, " "); !strings.Contains(works, "archive") || !strings.Contains(works, "send") {
		t.Fatalf("works = %s", works)
	}
	human := h.run("doctor")
	if human.Exit != 0 || !strings.Contains(human.Stdout, "ok       Login") {
		t.Fatalf("human doctor: %s%s", human.Stdout, human.Stderr)
	}
}

// doctor names the real cause of a connection failure, the way other
// commands do, and does not tell the user to run doctor.
func TestDoctorExplainsAClosedPort(t *testing.T) {
	h := newMailServerHarness(t, "me@posteo.de")
	if res := h.run("login", "--email", "me@posteo.de"); res.Exit != 0 {
		t.Fatalf("login: %s", res.Stderr)
	}
	deps, err := depsWithMailServerOptions(imap.Options{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}})
	if err != nil {
		t.Fatal(err)
	}
	res := testutil.RunWith(newApp(deps), testutil.Options{}, "--config", h.configPath, "doctor")
	if res.Exit == 0 || !strings.Contains(res.Stdout, "FAILED   TLS      the IMAP server posteo.de:993 refused the connection (nothing is listening on that port)") {
		t.Fatalf("doctor exit %d:\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
	for _, bad := range []string{"contract", "maildo doctor"} {
		if strings.Contains(res.Stdout, bad) {
			t.Fatalf("doctor output contains %q:\n%s", bad, res.Stdout)
		}
	}
}

func TestDoctorFailsWhenTheLoginDoes(t *testing.T) {
	h := newMailServerHarness(t, "me@posteo.de")
	if res := h.run("login", "--email", "me@posteo.de"); res.Exit != 0 {
		t.Fatalf("login: %s", res.Stderr)
	}
	// The server now knows only another user, so it rejects the saved
	// password, and doctor must say so with exit status 1.
	h.server = testkit.StartMailServer(t, "someone-else@posteo.de", "other")
	res := h.run("doctor")
	if res.Exit == 0 || !strings.Contains(res.Stdout, "FAILED") {
		t.Fatalf("doctor with a rejected password: exit %d\n%s%s", res.Exit, res.Stdout, res.Stderr)
	}
}

// slicesDelete returns args without the word drop.
func slicesDelete(args []string, drop string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a != drop {
			out = append(out, a)
		}
	}
	return out
}
