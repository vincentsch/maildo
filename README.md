# maildo

<p align="center"><strong>Your email from the command line, for you and your coding agent.</strong></p>

maildo is an open-source command line tool for email, a bit like `gh` is for
GitHub. You log in once, and then you handle your mail from the terminal.
maildo uses IMAP and SMTP, so it works with most mail services, including
Gmail and iCloud. Outlook accounts go through Microsoft's own API, and Gmail
accounts can also use Google's.

In a terminal you get readable tables. Scripts and coding agents get stable
JSON. Before maildo changes more than one message, it shows you what it would
do. You can also set limits for each account, for example that an agent may
write drafts but not send them.

<p align="center"><a href="docs/quickstart.md">Quickstart</a> · <a href="docs/providers.md">Providers</a> · <a href="docs/safety.md">Safety</a> · <a href="docs/search.md">Search</a> · <a href="AGENTS.md">AGENTS.md</a></p>

## Set it up with your coding agent

Paste this into your coding agent:

```text
Set up maildo for me.
Use https://github.com/vincentsch/maildo: read its AGENTS.md and follow
the steps. Log me in to my mail account, show me my newest mail, and set
safe limits for what you may do with it.
```

The agent installs maildo and asks which address to use. You type the
password yourself, in your own terminal, so the agent never sees it. Before
the agent touches your mail, it asks what it may do and saves that as the
account's policy. maildo checks the policy on every command. To start,
AGENTS.md suggests drafts only. With that, the agent can read mail and write
drafts, but it can't send anything. [The agent guide](docs/agents.md) has
the details.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/vincentsch/maildo/main/install.sh | sh
```

If the installer prints a PATH command, run it. If you have Go, you can also
run `go install github.com/vincentsch/maildo/cmd/maildo@latest`.

maildo runs on Linux and macOS. It doesn't support Windows yet. On Windows,
run it inside WSL (Windows Subsystem for Linux).

## Log in

```sh
maildo login
```

maildo asks for your address and password and finds the mail servers itself.
Many services, Gmail and iCloud among them, want an app password instead of
your normal one. An app password is a separate password for one program,
which you create in your account's security settings. maildo tells you where.
Check the result with `maildo doctor`. The [provider guides](docs/providers.md)
cover each service, including the setup for Outlook and the Gmail API.

## Read and search

```text
$ maildo list
5 messages in INBOX (work)

    ID       FROM                     SUBJECT                       DATE
*   7d40c06  noreply@service.example  (no subject)                  4 days ago
*+  4706326  Clara Client             Screenshot of the bug         5 days ago
*   f25dbcc  Clara Client             Re: Project kickoff           6 days ago
    20d378d  Clara Client             Project kickoff               Oct 19
*+  9eac029  Vendor Billing           Invoice 2026-104              Oct 18

* unread  ! flagged  + attachment
```

Use the short ID from the first column in later commands:

```sh
maildo read 9eac029
maildo thread f25dbcc
maildo search "from:billing invoice newer:30d"
maildo attachments download --id 9eac029 --all --to ~/Downloads
```

Add `--json` for scripts. The JSON has the full message ids and a short
preview of each message. The [search syntax](docs/search.md) page explains
operators such as `from:` and `is:unread`, and how to give dates.

## Change mail

```sh
maildo mark read 9eac029
maildo archive 9eac029
maildo archive --query "from:deals@shop.example" --all
```

A change to one message happens right away. When a command would change
more than one message, for example several ids or every match of a search
with `--all`, maildo first shows what it would do. Then it prints the command
that makes the change:

```text
Would archive 15 messages in work:

  ID       FROM         SUBJECT              FOLDER  DATE
  51e0ff6  Promo Deals  Deal of the day #15  INBOX   yesterday
  ...

Nothing was changed. To archive these messages, run:
  maildo archive --account work --ids-file /home/you/.config/maildo/previews/archive-20261026-101500-5f2c9a1e.txt --apply
```

That command changes exactly the messages in the preview. It does not search
again, so mail that arrives in between is left alone. maildo writes every
change to a local action log.

## Write and send

```sh
maildo reply 9eac029 --body "Thanks, paid today."
maildo draft send 3c1f2a7
maildo send --to anna@example.com --subject "Report" --body-file report.md --markdown --attach report.pdf
```

`reply` and `forward` save a draft unless you add `--send`. A forward takes
the original's attachments along. To leave them out, add
`--include-attachments none`.

Add `--dry-run` to any send to see the whole message as it would go out,
with an approval code:

```text
$ maildo draft send 3c1f2a7 --dry-run
Would send this message from work:

  From:     Me <me@work.example>
  To:       Clara Client <clara@client.example>
  Subject:  Re: Contract draft for October

  Thanks, I will read the notes today.

Nothing was sent. The approval code for this message is k7q2m9xd.
To send it, run:
  maildo draft send --account work --id imap-message/v1/RHJhZnRz/1/7 --approve k7q2m9xd --yes
```

In a terminal, maildo asks before it sends. Scripts and agents run the
printed command. If the message changes in between, its code no longer
matches and maildo sends nothing. A code sends its message once, so running
the command twice does not send the mail twice. For IMAP accounts, a copy
of each sent message ends up in the Sent folder.

## Limits for agents

```sh
maildo config set policy.drafts_only true --account work
maildo config set policy.allow_recipients "@mycompany.example" --account work
maildo config set policy.max_bulk 50 --account work
```

| Setting | What it allows |
| --- | --- |
| `read_only` | reading only |
| `drafts_only` | reading and drafts, no sending and no other changes |
| `allow_recipients` | mail only to these addresses and domains |
| `max_bulk` | at most this many messages changed by one command |

Anyone can make a limit stricter. To turn one off or make it looser, a
person has to confirm it at a terminal. [Safety and policies](docs/safety.md)
has the details, including what these limits cannot stop.

## Providers

| Account | How maildo connects | Tested |
| --- | --- | --- |
| Any IMAP service, such as Gmail or Posteo, or your own server | IMAP and SMTP with a password or app password | against Dovecot and GreenMail servers |
| Fastmail | IMAP and SMTP with an app password | as an IMAP account |
| Outlook.com and Microsoft 365 | Microsoft Graph with your own app registration | live with a Microsoft 365 work account; Outlook.com not yet |
| Gmail through the Gmail API | your own Google OAuth app | not yet tested against live accounts |

## Docs

- [Quickstart](docs/quickstart.md)
- [Providers](docs/providers.md)
- [Search syntax](docs/search.md)
- [Safety and policies](docs/safety.md)
- [Using maildo from a coding agent](docs/agents.md)
- [Troubleshooting](docs/troubleshooting.md)
- [FAQ](docs/faq.md)
- [Command reference](docs/commands/index.md)

maildo stores its settings and saved logins in your config folder. Saved
passwords and tokens are plain files that only your user can read.
[SECURITY.md](SECURITY.md) explains what that means for you.

## Build from source

[CONTRIBUTING.md](CONTRIBUTING.md) covers building and testing, including the
test mail servers in Docker. [RELEASE.md](RELEASE.md) describes how a release
is made. Report security issues as described in [SECURITY.md](SECURITY.md).

Built with [rungrad](https://rungrad.com).

## License

[MIT](LICENSE). Made by [Vincent Schmalbach](https://www.vincentschmalbach.com/).
