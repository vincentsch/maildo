# Safety and policies

Scripts and coding agents use maildo as much as people do. So maildo shows
you a change before it makes it, and it asks before it sends. This page
explains those checks and the limits you can set for each account,
including what the limits cannot stop.

## Previews

Every command that changes mail takes `--dry-run`. It shows what would
happen and changes nothing in your mailbox. It may save a small file in
maildo's config folder for the command it prints. The one exception is an
Outlook reply or forward with `--send`, which [Sending](#sending) explains.

```text
$ maildo archive 20d378d --dry-run
Would archive "Project kickoff" from Clara Client (INBOX, work).
Nothing was changed. To archive it, run:
  maildo archive --account work --id imap-message/v1/SU5CT1g/1/41
```

When a command would change more than one message, it shows a preview even
without `--dry-run`. That happens with `--query ... --all`, with several ids
and with `--ids-file`. The preview lists the messages and the folder each
one is in, and prints the command that makes the change:

```text
$ maildo archive --query "from:deals@shop.example" --all
Would archive 3 messages in work:

  ID       FROM         SUBJECT              FOLDER  DATE
  20ece86  Promo Deals  Deal of the day #15  INBOX   yesterday
  ...

Nothing was changed. To archive these messages, run:
  maildo archive --account work --id imap-message/v1/SU5CT1g/1/41 --id ... --apply
```

The printed command names the account and the full ids of exactly the
messages in the preview. It does not run the search again. Mail that
arrives after the preview is not touched, and if you switch to another
account in between, the change still goes to the account from the preview.
When there are many messages, maildo writes their ids to a file in
`previews/` in its config folder, and the command reads that file with
`--ids-file`. If that file changed after the preview, or comes from the
preview of another command, maildo refuses it. Each time maildo writes such
a file, it deletes the ones that are more than a week old.

You can also make the change in one step with `--query ... --all --apply`.
Then maildo changes whatever the search finds at that moment, and you don't
get a list to check first.

When a command that changes mail uses `--query`, it leaves out Sent, Drafts,
Trash and Junk. That way a broad search cannot touch your sent mail or your
drafts. To work in one of those folders, name it with `--folder`.
`restore --query` looks only in Trash and Junk.

`--max-targets` stops a search that matches more messages than you
expected. It is 100 unless you set it, and at most 1000. In JSON, the
preview has `dry_run: true`, the `targets` with subject, sender and folder,
and the `apply_command`.

### Short ids

Tables show short ids. A short id is the first 7 characters of a hash of
the account name and the message id. When two messages maildo has shown you
start the same way, both get more characters until they differ. If a short
id matches more than one message, maildo refuses it and lists the
candidates with longer ids. It never guesses. Full ids from `--json` always
work.

maildo remembers the last 5,000 messages it showed you. When a message
drops off that list, maildo still remembers the start of its hash. A short
id that was printed for that message is then refused, and a newer message
that starts the same way gets a longer short id. So an old short id never
points at a different message.

### Deleting folders

In this beta, `folders delete` deletes only a folder that is empty and has
no subfolders. On Gmail, that is a label that no message has and that has
no nested labels. So deleting a folder never touches your mail.

- A folder with messages is refused. Move or delete the messages first.
- A folder with subfolders is refused. Delete the subfolders first.
- When the server does not say how many messages and subfolders a folder
  has, maildo also refuses, because it cannot see that the folder is empty.

`folders delete` also needs `--confirm` with the folder's id or its full
path. `--dry-run` prints the command with it.

## Sending

You cannot take back a sent message, so sending has extra steps. They apply
to `send` and `draft send`, and to `reply` and `forward` with `--send`.
Without `--send`, `reply` and `forward` only save a draft.

### The preview and the approval code

`--dry-run` shows the message as it would go out:

- the sender and every recipient, Bcc included
- the subject and the body
- each attachment with its size, inline images included
- for a reply, the Message-ID it answers, which keeps it in the
  conversation

Empty fields still get a line with "(none)", so you see that a message has
no Cc or no attachments. In JSON they are empty lists. When the From line has
only your address, the preview says how to add your name with
`maildo config set sender_name`.

In the terminal, a body longer than 40 lines is cut, and the preview says
how many lines it left out. `--full` shows all of it. With `--json`, the
preview always has the whole body.

The preview also prints an approval code and the command that sends the
message:

```text
$ maildo send --to "Clara Client <clara@client.example>" --subject "Quote for October" --body-file quote.txt --dry-run
Would send this message from work:

  From:     Me <me@work.example>
  To:       Clara Client <clara@client.example>
  Cc:       (none)
  Bcc:      (none)
  Reply-To: (none)
  Subject:  Quote for October
  Attached: (none)

  Hi Clara,

  here is the quote for October.

Nothing was sent. The approval code for this message is k7q2m9xd.
To send it, run:
  maildo send --to 'Clara Client <clara@client.example>' --subject 'Quote for October' --body-file quote.txt --account work --approve k7q2m9xd --yes
```

You can paste the printed command into a shell as it is. maildo quotes
every argument, so the shell passes on quotes, `$` signs and letters
outside ASCII unchanged. A body given with `--body` that has line breaks
makes the command span several lines, with the line breaks inside the
quotes. With `--body-file`, the command names the file and stays on one
line. When a command has characters a terminal cannot show, such as a
tab, the text output says so instead of printing it, and `--json` has it.

The code is a short hash of everything in the preview, the reply headers
included, and of the account that sends. When you run the printed command, maildo works out the code
again. If anything changed in between, for example someone edited the
draft, the code no longer matches. Then maildo refuses with `approval_mismatch`,
and nothing is sent.

maildo compares the code twice. First when the command starts, and again
right before the message leaves, with exactly what the provider is about to
send:

- Gmail sends a draft together with the raw message maildo checked.
- Outlook sends the checked copy of a draft with Graph's sendMail and then
  deletes the draft. If the draft changed in the meantime, maildo leaves it
  in Drafts and says so.
- IMAP accounts send the draft over SMTP. An IMAP message never changes: an
  edited draft is a new message with a new id.

So an edit that lands while maildo sends does not go out.

The printed command always names the account. If you switch to another
account in between, the mail still goes out from the account in the
preview.

### A code sends its message once

Right before a send, maildo writes the approval code down in
`sent-approvals.json` in its config folder. If the same command runs a
second time, maildo refuses with `approval_used` and says when the message
went out. So an agent that runs a command twice, or a command you paste
again, does not send the mail twice. When maildo cannot tell whether a send
went out (outcome `unknown`), the code counts as used. When maildo cannot
write the code down, it does not send.

To send the same message again on purpose, make a new preview. It shows
the same code, because the message is the same. The preview also says that
maildo sent this message before, and the code then works for one more send.

maildo also writes down a send without a code, but never refuses one. That
is a send a person confirms in the terminal, or a report a script sends
every week under `policy.unattended_send`.

### Who confirms a send

- In a terminal, maildo shows the whole message and asks "Send this
  message?".
- Without a terminal, for example in an agent's shell or a cron job, a send
  needs `--approve` with the code and `--yes`. `--yes` on its own is
  refused.
- `policy.unattended_send` lets a script send without a code, for example
  a report that a cron job sends every week. maildo then sends from that
  account without asking anyone, also in a terminal. Only the account's
  owner can turn it on, in a terminal. Leave it off for coding agents.

### Outlook replies and forwards

Outlook writes replies and forwards itself. It picks the recipients and adds
the quoted text when it saves the message. So on Outlook,
`reply --send --dry-run` and `forward --send --dry-run` save the message as
a draft and show you that draft. The command they print is a
`maildo draft send` for it. This is the only preview that writes something:
a draft in your Drafts folder. If you do not send it, delete it with the
command the preview prints.

### Attachments on Outlook

Graph takes about 3 MB of attachments in one send request. So on Outlook,
maildo sends at most 3 MB of attachments in one message, with `send
--attach` and with drafts. Send bigger files from Outlook itself, or share
them as a link. maildo also cannot send an Outlook draft that has an
attached email or a link to a cloud file. It says so in the preview.

## Policies

A policy is a set of limits for one account. maildo checks it every time it
opens the mailbox, for every command and no matter who runs it.

| Setting | Effect |
| --- | --- |
| `policy.read_only` | reading and searching only; maildo changes and sends nothing |
| `policy.drafts_only` | reading and drafts; no sending and no other changes |
| `policy.allow_recipients` | mail goes only to these addresses and domains |
| `policy.max_bulk` | one command changes at most this many messages |
| `policy.unattended_send` | scripts may send without `--approve` and `--yes` |

```sh
maildo config set policy.drafts_only true --account work
maildo config set policy.allow_recipients "@mycompany.example,anna@partner.example" --account work
maildo config set policy.max_bulk 50 --account work
maildo config list
```

`allow_recipients` takes addresses (`anna@partner.example`) and domains
(`@mycompany.example`). A domain also covers its subdomains. Every To, Cc
and Bcc recipient must be on the list, for replies and forwards as well as
for new mail.

### Who can change a policy

Anyone can make a policy stricter, a script included. To make it looser, a
person has to confirm it in a terminal, and without a terminal maildo
refuses. Looser means one of these:

- turning `read_only` or `drafts_only` off
- allowing more recipients
- raising `max_bulk`
- turning `unattended_send` on

An agent that gets a policy error should tell you what it wanted to do. It
should not try to change the policy.

### What policies protect against

Policies protect against mistakes, such as a search that matches more than
you meant, or a reply to the wrong person. They also hold for a coding agent
that follows maildo's instructions, because those instructions tell it to
stop at a policy error and never to loosen or work around a policy.

They do not protect against a program that sets out to get around them. The
policy is stored in maildo's config file, and your saved login is in the
same folder. Anything that runs as your user can read and edit both. The
terminal check that guards loosening can also be faked, because a program
can pretend to be a terminal. So a policy is a guard rail and not a lock.
For an agent, also limit what it may do on your computer.
[Using maildo from a coding agent](agents.md) has an example for Claude
Code.

## The action log

maildo adds a line to `audit/actions.jsonl` in its config folder for each
of these:

- every change to messages
- every send
- new and updated drafts, also replies and forwards that maildo saved as a
  draft, and files added to or removed from a draft
- changes to folders and Inbox rules

The config folder is here:

| System | Folder |
| --- | --- |
| Linux | `~/.config/maildo/` (or `$XDG_CONFIG_HOME/maildo/`) |
| macOS | `~/Library/Application Support/maildo/` |

Each line is one command. It records the messages the command touched and
how each change ended. For sends, it records the recipients and the subject,
never the body. maildo also logs a failed send, because the mail may still
have reached the server. When maildo cannot write the log, the result says
so in `action_log_status`, and in a terminal you see a warning.

## When the outcome is unknown

Sometimes maildo sends a change to the server and the connection breaks
before the answer comes back. Or the server says the change worked, but
maildo cannot read the rest of its answer. Then maildo cannot know whether
the change happened, and it tells you so instead of guessing:

- `not_applied` or `rejected`: nothing changed, and it is safe to try again.
- `unknown`: the change may have happened. Look at the mailbox before you
  try again. For a send, look in your Sent folder first, because sending
  again could deliver the mail twice.

This holds for every provider and for every kind of change, sends
included.

In a terminal you see this as a sentence after the error. JSON errors have
it in `outcome`, together with `retry_safe` and `write_may_have_landed`. A
change to many messages reports each message, and the error lists the ones
that did not change.

## Reading mail

Reading, listing and searching never change a message. maildo does not mark
mail as read when it reads it. It never follows links or loads images in
HTML mail. `read` turns HTML into plain text unless you ask for
`--body-mode html` or `--body-mode raw`.
