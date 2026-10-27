# Troubleshooting

Start with `maildo doctor`. It checks the connection one step at a time and
names the step that fails. Most error messages end with a `Next:` line that
says what to do. Add `--json` to see all the details, including the
server's own words.

## Logging in

### "the server rejected the username or password"

Many services, Gmail and iCloud among them, want an app password instead of
your normal password. The [IMAP guide](providers/imap.md) says where to
create one. GMX and WEB.DE accept your normal password, but only after you
turn on IMAP in their web settings.

### maildo says there is no terminal to ask for the password

You ran `maildo login` from a script or from an agent's shell. Run it in a
terminal, or give the password in `MAILDO_IMAP_PASSWORD` for that one
command.

### "Microsoft no longer accepts passwords over IMAP"

Outlook.com, Hotmail and Microsoft 365 need the
[Outlook setup](providers/outlook.md).

### maildo cannot find the servers

Give them with `--imap-host` and `--smtp-host`. The ports are 993 and 465
unless you set others with `--imap-port` or `--smtp-port`.

### A saved login stopped working

This happens after a password change, or when someone revoked the app
password. Log in again with:

```sh
maildo auth repair <name>
```

## Connecting

### "has a certificate maildo does not trust"

The server's certificate does not lead back to a certificate authority that
your system trusts, or it is for another host name. If the server uses its
own certificate authority, log in again with `--ca-file /path/to/ca.pem`.
maildo always checks the certificate, and there is no option to turn that
off.

### "did not start TLS"

The port expects a plain connection first, which is then switched to TLS
with STARTTLS. Use `--imap-tls starttls` (usually port 143) or
`--smtp-tls starttls` (usually port 587). Or use the TLS ports 993 and 465.

### "did not answer in time" on port 993 or 465 with STARTTLS

This is the other way round: these ports expect TLS from the first byte.
Use `--imap-tls implicit` or `--smtp-tls implicit`.

### "does not offer STARTTLS"

maildo never sends a password without encryption. Use the TLS port.

## Searching and reading

### A message is missing from search results

`search` leaves out Trash and Junk, so add `--folder Trash` to look there.
Dates count in UTC. On IMAP, the server decides how text matches, and some
servers match only whole words.

### "unknown short id"

maildo only knows the short ids of messages it showed on this computer. Run
`maildo list` or `maildo search` again, or use the full id from `--json`.

### "maildo no longer remembers which message short id ... stood for"

The short id is older than the last 5,000 messages maildo showed you.
maildo refuses it, because a newer message may start the same way. Run
`maildo list` or `maildo search` again and use the new short id, or use the
full id from `--json`.

### "the message id is out of date"

The server renumbered the folder (its UIDVALIDITY changed), or the message
moved. Search again to get a current id. Ids also change when you move or
archive a message, and maildo then prints the new one.

### Strange characters or a warning about decoding

The message breaks the MIME rules, for example with a body in an unknown
character set. maildo shows what it can decode and tells you. To see the
message as the server stores it, use `read --body-mode raw`.

## Changing and sending

### "this account has no Archive folder" (or Trash, Junk, Drafts)

Create the folder, for example with `maildo folders create Archive`.
`maildo doctor` lists the standard folders it found.

### "maildo deletes only empty folders without subfolders"

In this beta, `folders delete` deletes a folder only when it holds no
messages and has no subfolders. Move or delete the folder's messages
first. If it has subfolders, delete them first. When maildo says it could
not count the messages and subfolders, the server did not send the
counts. If that keeps happening, delete the folder in your mail program or
in the provider's web app.

### "maildo sends mail only after a confirmation"

Without a terminal, maildo cannot ask you. Run the send with `--dry-run`
first. It shows the message and prints the command that sends it, with the
message's approval code in `--approve` and with `--yes`. To let scripts send
without a code, run
`maildo config set policy.unattended_send true --account <name>` in a
terminal.

### "--yes needs --approve with the approval code from the preview"

`--yes` only skips the question. The send also needs the approval code of
exactly this message. Run the command with `--dry-run` and use the command
it prints.

### "this message is not the one that was approved"

The approval code does not match the message any more. Something in it
changed after the preview, for example the draft or a file. maildo sent
nothing. Run the preview again, check the message, and use the
new command.

### "this approved message was already sent"

The approval code already sent its message. maildo did not send it again,
so the mail does not go out twice. Look in your Sent folder. When the error
says the message "may already have been sent", maildo could not tell
whether the earlier send went out. To send the message again on purpose,
run the command with `--dry-run` for a new preview and run the command it
prints.

### "maildo could not write down this send in its config folder"

Before a send, maildo writes the approval code to `sent-approvals.json` in
its config folder, so the same command cannot send twice. It could not
write that file, so it sent nothing. Check that the config folder is
writable and has free space. If the file is damaged, move it away and
maildo starts a new one.

### A policy error

The account's policy does not allow the command. `maildo config list` shows
the policy. To loosen it, you have to be at a terminal.

### "maildo could not confirm whether the change happened"

The connection broke after the request went out. Look at the mailbox before
you try again, so that nothing happens twice. For a send, it says "maildo
could not confirm whether the mail went out" instead. Look in your Sent
folder first, or the mail may be sent twice.

### "the message changed after maildo checked it, so nothing was sent"

Someone or something changed the draft while maildo was sending it. Look at
it again with `maildo draft send <id> --dry-run`, then send it with the new
command.

### "maildo sends at most 3 MB of attachments on Outlook"

Graph takes about 3 MB of attachments in one send request. Send bigger
files from Outlook itself, or share them as a link.

### "short id 55afb6f matches 2 messages"

Two messages maildo showed you have the same short id. The error lists both
with longer ids. Use the one you mean, or the full id from `--json`.

### "the search matches more than 100 messages, which is the --max-targets limit"

A change to every match stops at `--max-targets`, which is 100 unless you
set it, and at most 1000. Narrow the search, for example with `older:30d`
or `--folder`, or raise `--max-targets`.

### A change to many messages partly failed

The error lists each message that did not change, and why. The other
messages did change. Search again for current ids before you try the rest
again.

## Still stuck

Run the failing command with `--json` and look at the `code`, the `hint`
and, when it is there, the `cause` field of the error. If you report a problem, include that output, with
personal details removed, and the output of `maildo doctor`.
