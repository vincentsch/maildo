# Contributing to maildo

maildo is a Go program built on [rungrad](https://rungrad.com). Use the Go
version from [go.mod](go.mod); with `GOTOOLCHAIN=auto`, Go can download it
for you. Release builds use `CGO_ENABLED=0`, so the binary needs nothing
else installed to run.

## Build and check a change

On Linux or macOS with Make:

```sh
make build
./bin/maildo --version
make test race lint docs-check conformance
```

`make race` needs a C compiler. maildo does not support Windows yet, but
`make lint` checks that the code still builds for it.

## Test against real mail servers

Fakes miss things that real servers do. So every IMAP and SMTP command also
runs against Dovecot and GreenMail in Docker:

```sh
make integration
```

That starts both servers with a new test certificate authority, runs the
tests in `internal/integration`, and removes the containers. To leave the
servers running while you work:

```sh
make mailservers-up
eval "$(scripts/mailservers.sh env)"
go test -tags integration ./internal/integration/...
make mailservers-down
```

Dovecot marks its standard folders with SPECIAL-USE and supports CONDSTORE.
GreenMail has neither, so it is a good stand-in for older servers. It also
has quirks of its own, and the integration tests point them out where they
matter.

The containers are named `maildo-dev-dovecot` and `maildo-dev-greenmail`.
If their names or host ports clash with something on your machine, change
them with the `MAILDO_IT_*` variables in `scripts/mailservers.sh`.

## Where things are

| Path | What it holds |
| --- | --- |
| `cmd/maildo` | commands, output and the safety checks around changes and sends |
| `internal/imap` | the IMAP and SMTP provider (also used for Fastmail) |
| `internal/gmail`, `internal/outlook` | the Gmail API and Microsoft Graph providers |
| `internal/provider` | what every provider implements, and the account policy |
| `internal/search` | the search language |
| `internal/discover` | finding mail servers from an address |
| `internal/mimecompose` | building and parsing MIME messages |
| `internal/testkit` | fake providers and in-process IMAP and SMTP servers for tests |
| `internal/integration` | the tests against Dovecot and GreenMail |

## Help text and docs

After you change commands or flags, run `make docs` and then
`make docs-check`. `make docs` writes `docs/commands/` and the help files in
`cmd/maildo/testdata/help/`, so don't edit those by hand. When you change
what the code does, also update these hand-written files:

- the guides in `docs/` and the README
- [AGENTS.md](AGENTS.md) and `skills/maildo/SKILL.md`, which coding agents
  follow

Write plain English: short sentences, concrete commands, "you". Say what a
reader can do and what a result means. Don't claim anything that the code
and the tests don't show. The Outlook and Gmail API providers have only
been tested against fakes, so say so where it matters.

## Test without real accounts

Tests use fakes, the in-process servers in `internal/testkit` and the
Docker servers. Never put a real account, password or token into a test, a
fixture or a log. Test what users see, such as messages and exit codes, and
what reached the server.

Report security issues as described in [SECURITY.md](SECURITY.md).
