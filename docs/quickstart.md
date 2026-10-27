# Quickstart

This page takes you from the install to reading and sorting your own mail.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/vincentsch/maildo/main/install.sh | sh
maildo --version
```

The installer works on Linux and macOS. It puts `maildo` in `~/.local/bin`,
or in the folder you set in `INSTALL_DIR`, and checks the download against
its SHA-256 checksum. If it prints a PATH command, run it.

maildo does not support Windows yet, because it cannot make its saved logins
private there. On Windows, install and run it inside WSL (Windows Subsystem
for Linux).

If you have Go installed:

```sh
go install github.com/vincentsch/maildo/cmd/maildo@latest
```

## Log in

```sh
maildo login
```

```text
Email address: me@posteo.de
Account name [posteo]:
Found the servers for Posteo (built into maildo):
  IMAP posteo.de:993 (TLS)
  SMTP posteo.de:465 (TLS)
Use your Posteo password.
Password for me@posteo.de:
Checking the login with posteo.de...
Logged in to me@posteo.de as account "posteo" (imap).
"posteo" is the active account. Try: maildo list
```

maildo knows the servers of the large mail services. For other addresses it
looks at the domain's autoconfig file and at Thunderbird's provider
database. Then it checks DNS. If none of that works, it asks you for the
servers.

Many services want an app password, which you create in the account's
security settings. maildo prints where. The [provider guides](providers.md)
list them.

In a script, give everything as flags and put the password in an
environment variable:

```sh
MAILDO_IMAP_PASSWORD='app-password' maildo login --email me@posteo.de --name posteo
```

Then check that everything works:

```sh
maildo doctor
```

`doctor` checks the connection and the login one step at a time. It also
finds the standard folders, such as Sent and Archive, and says which maildo
commands work with your server.

## Look around

```sh
maildo list                        # newest mail in the inbox
maildo list Archive --unread
maildo search "from:anna newer:7d"
maildo folders list
```

Tables show a short ID for each message. Use it in the next command:

```sh
maildo read 9eac029
maildo thread 9eac029
maildo attachments download --id 9eac029 --all --to ~/Downloads
```

maildo remembers the short IDs of the last 5,000 messages it showed you on
this computer. It refuses an older short ID instead of guessing. Scripts
should use the full ids from the `--json` output.

## Sort mail

```sh
maildo mark read 9eac029
maildo flag set 9eac029
maildo archive 9eac029             # prints the message's new id
maildo delete 20d378d              # moves it to Trash and prints its new id
maildo restore 4b7e1a0             # the id delete printed; back to the inbox
maildo move 9b1c3e2 --to Clients
```

Moving a message to another folder gives it a new id on IMAP and Outlook.
maildo prints it ("Its id is now ..."). Use that id in the next command.

To change every message a search finds, add `--all`. maildo then shows what
it would change and prints the command that does it:

```sh
maildo archive --query "from:deals@shop.example" --all
```

Run the printed command to make the change. It names the exact messages
from the preview, so mail that arrives in between is left alone.

## Write mail

```sh
maildo reply 9eac029 --body "Thanks, that works for me."
maildo draft send 3c1f2a7
```

`reply` and `forward` save a draft unless you add `--send`. A forward takes
the original's attachments along, and its preview lists them. To leave them
out, add `--include-attachments none`.

`maildo send` writes a new message:

```sh
maildo send --to anna@example.com --subject "Report" --body-file report.md --markdown --attach report.pdf --dry-run
maildo send --to anna@example.com --subject "Report" --body-file report.md --markdown --attach report.pdf
```

The first command shows the whole message and an approval code, and sends
nothing. The second one shows the same and asks before it sends. A script
or an agent cannot answer that question. It runs the command the preview
prints instead, which carries the code in `--approve`. If the message
changed after the preview, maildo refuses to send it. A code sends its
message once.

To set the name people see in the From line of your mail:

```sh
maildo config set sender_name "Ann Example" --account work
```

## More than one account

Run `maildo login` again for each account, and `maildo auth list` shows them
all. `maildo auth switch <name>` changes the active account. To use another
account for one command, add `--account <name>`. `list` and `search` also
take `--all-accounts`.

## Next

- [Search syntax](search.md)
- [Safety and policies](safety.md)
- [Using maildo from a coding agent](agents.md)
- [Troubleshooting](troubleshooting.md)
