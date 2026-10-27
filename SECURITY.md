# Security

## Reporting a security issue

Please report security issues through
[GitHub private vulnerability reporting](https://github.com/vincentsch/maildo/security/advisories/new).
Don't put exploit details in public issues, and no private data such as
passwords or mail.

Include the maildo version (`maildo --version`), your operating system and
the steps to reproduce the issue. Use test accounts and made-up messages in
examples. Remove passwords, tokens and personal mail from logs and files
before you share them.

## How maildo stores your logins

maildo saves each login in a file in its config folder, under `auth/`:

| System | Folder |
| --- | --- |
| Linux | `~/.config/maildo/` (or `$XDG_CONFIG_HOME/maildo/`) |
| macOS | `~/Library/Application Support/maildo/` |

maildo does not run on Windows yet. Windows ignores Unix file modes, and
maildo does not yet set the access control lists that would make these
files private there. So it refuses to save logins on Windows. Under WSL
(Windows Subsystem for Linux) it works the same as on Linux.

For IMAP accounts, the file holds your password or app password. For
Outlook and Gmail, it holds the refresh token. **These files are not
encrypted.** maildo creates them so that only your user can read them
(folders 0700, files 0600), and it refuses to use a folder that other users
can open. That protects them from other users on the same computer. It does
not protect them from programs that run as you, and backups of your home
folder copy them along with everything else.

To limit the damage if a file leaks:

- Use an app password where your provider offers one. You can revoke it
  without changing your main password.
- `maildo auth logout <name>` deletes a saved login. `maildo auth remove
  <name>` also removes the account.
- On a shared or managed computer, think about whether a saved mail
  password belongs there at all.

maildo does not use the system keychain or password managers yet.

## What maildo sends where

- maildo connects to your mail servers. During login it may also look up
  the servers for your address. For that it asks:
  - the domain's autoconfig file, at `autoconfig.<domain>` and at
    `<domain>/.well-known/autoconfig/`. Like Thunderbird, maildo includes
    your address in this request.
  - Thunderbird's provider database at `autoconfig.thunderbird.net`, with
    only the domain
  - DNS for the domain's records
  - the usual host names `imap.<domain>`, `smtp.<domain>` and
    `mail.<domain>`, to see whether they answer

  maildo sends no password during this lookup.
- IMAP and SMTP always use TLS, and maildo always checks the certificate.
  It never sends a password over an unencrypted connection, and it has no
  option to turn off the certificate check. `--ca-file` adds a certificate
  authority that you trust.
- There is no telemetry and no maildo server. When you run `maildo update`,
  maildo asks GitHub for new releases.

## Limits worth knowing

- maildo itself enforces the account policies (`policy.read_only`,
  `drafts_only`, `allow_recipients`, `max_bulk`). They stop mistakes, and
  a coding agent that follows maildo's instructions stays within what you
  allowed. But they are stored in your config file, so anything that
  can edit your files as you can change them. The terminal check that
  guards loosening them can also be faked. They are not a sandbox.
- Mail is untrusted input. Subjects, names and file names come from
  strangers. maildo removes terminal control characters from everything it
  prints. It saves attachments with unsafe names under safe names, and
  always inside the folder you chose. Still, the text of a message can talk
  an agent that reads it into things. maildo's agent instructions say to
  treat mail text as data and to show every send to the user first, and
  the policies limit what a careless agent can do.
- The action log (`audit/actions.jsonl`) records the recipients and
  subjects of what you sent and changed. `short-ids.json` remembers the
  subjects of messages maildo showed you. Neither holds message bodies or
  passwords, but both are private data, so protect them like your mail.
