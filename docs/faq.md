# FAQ

### Is maildo a mail client?

Not in the usual sense. It has no inbox window, and it does not fetch mail
in the background. Each command connects, does one job and exits, the way
`gh` works for GitHub. That makes maildo easy to use from scripts and
coding agents.

### Does maildo copy my mail somewhere?

No. It reads from your provider when you run a command, and it keeps no
copy of your messages. Its config folder holds its own files: your
settings, the action log with the recipients and subjects of what maildo
changed or sent, and `short-ids.json`, which remembers the short ids and
subjects of messages it showed you. There is no maildo server and no
telemetry.

### Where does maildo store my password?

In a file in maildo's config folder that only your user can read (mode
0600). The file is not encrypted, so anyone who can read your files as you
can read it. [SECURITY.md](../SECURITY.md) explains the trade-off. App
passwords limit the damage, because you can revoke one without changing
your main password.

### Does maildo run on Windows?

Not yet. maildo stores your saved logins in files only you can read. On
Windows it does not yet set the file permissions (ACLs) that would make
them private. So instead of leaving your password readable to other users
of the computer, it stops with "maildo does not support Windows yet".
Inside WSL (Windows Subsystem for Linux) it works the same as on Linux.

### Does reading a message mark it as read?

No. Run `maildo mark read <id>` when you want that.

### Why does a message have a new id after I moved it?

IMAP numbers messages per folder. A message you move becomes a new message
in the other folder, with a new number. Outlook also gives a moved message
a new id. maildo prints the new short id ("Its id is now ..."). Use it from
then on, because the old one no longer works.

### Can I undo a change?

Most changes have an opposite command:

- `mark unread` undoes `mark read`, and `flag clear` undoes `flag set`.
- `restore` undoes `delete` and `junk`.
- For `move` and `archive`, move the message back.

`delete` moves mail to Trash. maildo removes a message for good only when it
replaces a draft with a new version or sends a draft. A sent message cannot
be taken back, which is why maildo asks before it sends.

### Why does maildo refuse to delete a folder?

In this beta, maildo deletes only empty folders without subfolders, so a
folder delete never takes mail with it. Move or delete the messages first,
and delete the subfolders first. Then delete the folder.

### Why does a script need an approval code to send?

So that no script or agent sends mail by accident, and so that the message
that goes out is the one somebody approved. Run the send with `--dry-run`.
It prints an approval code and the command that sends exactly that message.
If the message changes after the preview, the code no longer matches and
maildo sends nothing. For scripts that send on their own, such as a weekly
report, the account's owner can turn on `policy.unattended_send`; see
[Safety](safety.md).

### Does maildo work with Outlook and Gmail?

Gmail works over IMAP with an app password. maildo's IMAP code is tested
against Dovecot and GreenMail servers, but not yet against a live Gmail
account. Outlook.com and Microsoft 365 need Microsoft Graph and an app
registration of your own, which is free. Outlook was tested live with a
Microsoft 365 work account in October 2026, but not yet with a personal
Outlook.com account. maildo can also use the Gmail API with your own Google
OAuth app. Tests cover it against a fake server, but it has not been tested
against a live account yet. Please report what you find.

### Why doesn't maildo come with its own OAuth app for Gmail and Outlook?

A shared app needs a review by Google and Microsoft, and every user would
depend on one set of credentials. With your own app, your login depends on
nobody else. The [Gmail](providers/gmail.md) and [Outlook](providers/outlook.md)
guides walk you through the setup.

### Can maildo run on a server or in cron?

Yes. Log in once with `MAILDO_IMAP_PASSWORD` set. Then run commands with
`--json` and `--no-prompt`. To send from cron, you need the approval code
from a preview (`--approve <code> --yes`) or `policy.unattended_send`.

### Does maildo support POP3, JMAP or Exchange (EWS)?

No. Fastmail works over IMAP. An Exchange server with IMAP turned on should
work as an IMAP account.

### What does the short id mean?

It is the start of a hash of the account name and the message's full id.
maildo remembers the last 5,000 short ids it showed you, in
`short-ids.json` in its config folder. That way later commands can turn a
short id back into the full id.

An older short id is refused with "maildo no longer remembers which message
short id ... stood for". maildo never gives that short id to another
message, so it cannot change the wrong one. Run `maildo list` or
`maildo search` again to get a current short id.
